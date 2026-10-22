package modelrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// The isolation probe: before the first model launch for a user, check, as
// that user and through the same launcher, that the Box's secrets cannot be
// read. A misconfigured sudo rule, a wrong group, a loosened mode or a Box home
// shared with the model user then stops the Box instead of leaking silently.
// It runs for every dedicated user (model and validation), per user and path.
var probe struct {
	mu       sync.Mutex
	required []string
	optional []string
	results  map[string]error
}

// RequireUnreadable registers the Box paths the dedicated users must not be
// able to read. Each must exist: a missing one refuses. Calling it with no
// paths clears the registration (and any optional paths).
func RequireUnreadable(paths ...string) {
	RegisterProbe(paths, nil)
}

// RegisterProbe registers required paths (missing refuses) and optional ones
// (skipped when they do not exist). Every later Command for a user is
// preceded, once per user and path, by a probe; with nothing registered there
// is no probe.
func RegisterProbe(required, optional []string) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	probe.required = append([]string(nil), required...)
	probe.optional = append([]string(nil), optional...)
	probe.results = map[string]error{}
}

type probePath struct {
	path     string
	optional bool
}

func probeIsolation(ctx context.Context, spec Spec) error {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if len(probe.required)+len(probe.optional)+len(spec.Unreadable) == 0 {
		return nil
	}
	if probe.results == nil {
		probe.results = map[string]error{}
	}
	var paths []probePath
	for _, p := range probe.required {
		paths = append(paths, probePath{p, false})
	}
	for _, p := range spec.Unreadable {
		paths = append(paths, probePath{p, false})
	}
	for _, p := range probe.optional {
		paths = append(paths, probePath{p, true})
	}
	name := spec.RunAs.Username
	// Positive control: a failing launcher would also make `test -r` fail, which
	// must not read as "unreadable".
	if err := cachedProbe(ctx, name, "", func() error {
		code, err := probeTest(ctx, spec, "/etc/passwd")
		if err != nil || code != 0 {
			return fmt.Errorf("isolation probe: cannot run commands as user %q (exit %d, %v): check the sudoers rule from scripts/setup-model-user.sh", name, code, err)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, p := range paths {
		if err := cachedProbe(ctx, name, p.path, func() error { return checkUnreadable(ctx, spec, p) }); err != nil {
			return err
		}
	}
	return nil
}

// cachedProbe runs fn once per user and path (the caller holds probe.mu). A
// cancelled context says nothing about isolation, so its verdict is not kept.
func cachedProbe(ctx context.Context, user, path string, fn func() error) error {
	key := user + "\x00" + path
	if err, done := probe.results[key]; done {
		return err
	}
	err := fn()
	if ctx.Err() == nil {
		probe.results[key] = err
	}
	return err
}

func probeTest(ctx context.Context, spec Spec, path string) (int, error) {
	cmd, err := build(ctx, Spec{RunAs: spec.RunAs, Binary: "/usr/bin/test", Args: []string{"-r", path}, Timeout: 30 * time.Second}, false)
	if err != nil {
		return -1, err
	}
	err = cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exit):
		return exit.ExitCode(), nil
	}
	return -1, err
}

func checkUnreadable(ctx context.Context, spec Spec, p probePath) error {
	if _, err := os.Lstat(p.path); err != nil {
		if p.optional && os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("isolation probe: %s cannot be checked: %w", p.path, err)
	}
	code, err := probeTest(ctx, spec, p.path)
	switch {
	case err != nil:
		return fmt.Errorf("isolation probe on %s: %w", p.path, err)
	case code == 0:
		return fmt.Errorf("user %q can read %s: refusing to start a process as it; fix the permissions (scripts/setup-model-user.sh)", spec.RunAs.Username, p.path)
	case code != 1:
		return fmt.Errorf("isolation probe on %s: unexpected exit %d", p.path, code)
	}
	return nil
}
