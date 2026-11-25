package modelrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// The isolation probe: before the first model launch for a user, check, as
// that user and through the same launcher, that the Box's secrets cannot be
// read. A misconfigured sudo rule, a wrong group, a loosened mode or a Box home
// shared with the model user then stops the Box instead of leaking silently.
// It runs for every dedicated user (model and validation), per user and path.
var probe struct {
	mu           sync.Mutex
	required     []string
	optional     []string
	producerHome string
	reviewerHome string
	results      map[string]error
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

// RegisterRoleHomes registers the producer and reviewer home directories so
// every launch as one role probes that the opposite role's home cannot be read
// or traversed. Passing empty strings clears the registration.
func RegisterRoleHomes(producerHome, reviewerHome string) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	probe.producerHome = producerHome
	probe.reviewerHome = reviewerHome
	probe.results = map[string]error{}
}

type probePath struct {
	path           string
	optional       bool
	checkTraversal bool
}

var roleLoginRelPaths = []string{
	".claude/.credentials.json",
	".claude.json",
	".codex/auth.json",
	".commandcode/auth.json",
	".gemini/oauth_creds.json",
	".copilot/config.json",
	".config/github-copilot/hosts.json",
	".config/github-copilot/apps.json",
}

func probeIsolation(ctx context.Context, spec Spec) error {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	var paths []probePath
	for _, p := range probe.required {
		paths = append(paths, probePath{path: p})
	}
	appendOppositeHome := func(home string) {
		paths = append(paths, probePath{path: home, checkTraversal: true})
		for _, rel := range roleLoginRelPaths {
			paths = append(paths, probePath{path: filepath.Join(home, rel), optional: true})
		}
	}
	if probe.producerHome != "" && spec.RunAs.HomeDir != probe.producerHome {
		appendOppositeHome(probe.producerHome)
	}
	if probe.reviewerHome != "" && spec.RunAs.HomeDir != probe.reviewerHome {
		appendOppositeHome(probe.reviewerHome)
	}
	for _, p := range spec.Unreadable {
		paths = append(paths, probePath{path: p, checkTraversal: true})
	}
	for _, p := range probe.optional {
		paths = append(paths, probePath{path: p, optional: true})
	}
	if len(paths) == 0 {
		return nil
	}
	if probe.results == nil {
		probe.results = map[string]error{}
	}
	name := spec.RunAs.Username
	// Positive control: a failing launcher would also make `test -r` fail, which
	// must not read as "unreadable".
	if err := cachedProbe(ctx, name, "", func() error {
		code, err := probeTest(ctx, spec, "/etc/passwd", false)
		if err != nil || code != 0 {
			return fmt.Errorf("isolation probe: cannot run commands as user %q (exit %d, %v): check the sudoers rule from scripts/setup-model-user.sh", name, code, err)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, p := range paths {
		key := p.path
		if p.checkTraversal {
			key = "dir:" + p.path
		}
		if err := cachedProbe(ctx, name, key, func() error { return checkUnreadable(ctx, spec, p) }); err != nil {
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

func probeTest(ctx context.Context, spec Spec, path string, checkDirTraversal bool) (int, error) {
	args := []string{"-r", path}
	if checkDirTraversal {
		args = []string{"-r", path, "-o", "-x", path}
	}
	cmd, err := build(ctx, Spec{RunAs: spec.RunAs, Binary: "/usr/bin/test", Args: args, Timeout: 30 * time.Second}, false)
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
	info, err := os.Lstat(p.path)
	isDir := false
	if err != nil {
		if p.optional && os.IsNotExist(err) {
			return nil
		}
		if !p.optional || !os.IsPermission(err) {
			return fmt.Errorf("isolation probe: %s cannot be checked: %w", p.path, err)
		}
		// An optional login file inside another role's 0700 home cannot be statted
		// by the Box user; still probe it as the target user so any unexpected
		// traversal+read succeeds in tripping code == 0.
	} else {
		isDir = info.IsDir()
		if !isDir && info.Mode()&os.ModeSymlink != 0 {
			if target, statErr := os.Stat(p.path); statErr == nil && target.IsDir() {
				isDir = true
			}
		}
	}
	code, err := probeTest(ctx, spec, p.path, p.checkTraversal && isDir)
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
