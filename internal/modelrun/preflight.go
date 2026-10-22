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
var probe struct {
	mu      sync.Mutex
	paths   []string
	results map[string]error
}

// RequireUnreadable registers the Box paths (database, keys directory) the model
// user must not be able to read. Every later Command for a user is preceded,
// once per user, by a probe; with no paths registered there is no probe.
func RequireUnreadable(paths ...string) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	probe.paths = append([]string(nil), paths...)
	probe.results = map[string]error{}
}

func probeIsolation(ctx context.Context, spec Spec) error {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if len(probe.paths) == 0 {
		return nil
	}
	name := spec.RunAs.Username
	if err, done := probe.results[name]; done {
		return err
	}
	err := runProbe(ctx, spec)
	// A cancelled context says nothing about isolation; try again next time.
	if ctx.Err() == nil {
		probe.results[name] = err
	}
	return err
}

func runProbe(ctx context.Context, spec Spec) error {
	check := func(path string) (int, error) {
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
	// Positive control: a failing launcher would also make `test -r` fail, which
	// must not read as "unreadable".
	if code, err := check("/etc/passwd"); err != nil || code != 0 {
		return fmt.Errorf("isolation probe: cannot run commands as model user %q (exit %d, %v): check the sudoers rule from scripts/setup-model-user.sh", spec.RunAs.Username, code, err)
	}
	for _, path := range probe.paths {
		if _, err := os.Lstat(path); err != nil {
			return fmt.Errorf("isolation probe: %s cannot be checked: %w", path, err)
		}
		code, err := check(path)
		switch {
		case err != nil:
			return fmt.Errorf("isolation probe on %s: %w", path, err)
		case code == 0:
			return fmt.Errorf("model user %q can read %s: refusing to start a model; fix the permissions (scripts/setup-model-user.sh)", spec.RunAs.Username, path)
		case code != 1:
			return fmt.Errorf("isolation probe on %s: unexpected exit %d", path, code)
		}
	}
	return nil
}
