// Package modelrun is the one place a model CLI process is launched. Every
// model process (triage, review, plan and implementation adapters, the Copilot
// executor) runs as a dedicated, unprivileged OS user through sudo, so it cannot
// read the Box's secrets. There is no setting that skips the user switch.
package modelrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Seams for the user database and the privilege-drop launcher. Production uses
// the real ones; tests substitute them through modelruntest. They are
// unexported on purpose.
var (
	lookupUser   = user.Lookup
	currentUser  = user.Current
	launchPrefix = func(runAs string, preserve []string) []string {
		argv := []string{"sudo", "-n"}
		if len(preserve) > 0 {
			argv = append(argv, "--preserve-env="+strings.Join(preserve, ","))
		}
		return append(argv, "-u", runAs, "--")
	}
	timeoutBinary = "/usr/bin/timeout"
	killBinary    = "/bin/kill"
	killAllBinary = "/bin/kill"
	killGrace     = 10 * time.Second
	shellBinary   = "/bin/sh"
	defaultPATH   = DefaultPATH
	defaultLocale = "C.UTF-8"
)

// KillAfter is how long the model-side timeout waits between SIGTERM and
// SIGKILL; DefaultWaitDelay is how long the Box waits for the launcher after
// cancelling before it abandons it.
const (
	KillAfter        = 15 * time.Second
	DefaultWaitDelay = KillAfter + 15*time.Second
)

// DefaultPATH is the model user's fixed PATH: the Box's own PATH is never used.
const DefaultPATH = "/usr/local/bin:/usr/bin:/bin"

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedPrefix is the namespace of the launcher script's own shell
// variables; a forwarded name in it could shadow them.
const reservedPrefix = "__s42_"

// pgidMarker is the first stderr line the model side prints: the process group
// of the timeout wrapper, which the Box needs to cancel the call (it cannot
// signal another uid's processes, nor sudo, which runs as root).
const pgidMarker = "__S42_PGID="

// ResolveRunAsUser validates the dedicated model user: it must be named, exist,
// and be neither the current user nor root. This is the fail-closed gate that
// keeps a model process from ever sharing a uid with the Box's secrets.
func ResolveRunAsUser(name string) (*user.User, error) {
	return resolveUser(name, "model run-as user", "set --model-user or SUMMA42_MODEL_USER to a dedicated OS user that cannot read the Box secrets (see scripts/setup-model-user.sh)")
}

// ResolveValidationUser is the same gate for the user that runs validation
// commands (candidate code). It must also differ from the model user: the
// model user holds the CLI logins, which candidate code must never reach.
func ResolveValidationUser(name string, modelUser *user.User) (*user.User, error) {
	target, err := resolveUser(name, "validation user", "set --validation-user or SUMMA42_VALIDATION_USER to a dedicated OS user with no credentials (see scripts/setup-model-user.sh)")
	if err != nil {
		return nil, err
	}
	if modelUser == nil || modelUser.Uid == target.Uid || modelUser.Username == target.Username {
		return nil, fmt.Errorf("validation user %q must differ from the model user: candidate code must not run with the model's logins", name)
	}
	return target, nil
}

func resolveUser(name, role, hint string) (*user.User, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%s is required: %s", role, hint)
	}
	target, err := lookupUser(name)
	if err != nil {
		return nil, fmt.Errorf("%s %q is not usable: %w", role, name, err)
	}
	if name == "root" || target.Uid == "0" {
		return nil, fmt.Errorf("%s %q is root; processes must run as a dedicated unprivileged user", role, name)
	}
	self, err := currentUser()
	if err != nil {
		return nil, fmt.Errorf("determine the current user: %w", err)
	}
	if self.Uid == target.Uid {
		return nil, fmt.Errorf("%s %q is the Box's own user; it must be a different, dedicated user", role, name)
	}
	if target.HomeDir == "" || !strings.HasPrefix(target.HomeDir, "/") {
		return nil, fmt.Errorf("%s %q has no absolute home directory", role, name)
	}
	return target, nil
}

// Spec describes one model process.
type Spec struct {
	// RunAs is a user returned by ResolveRunAsUser.
	RunAs *user.User
	// Binary is an absolute path.
	Binary string
	Args   []string
	// Dir is the model's working directory; empty means the model user's home.
	// It must be readable by the model user, so it is never a Box-owned
	// directory. Use Scratch when the process needs a cwd of its own.
	Dir string
	// Scratch gives the call a private directory the model-side script creates
	// with mktemp under the model's ~/tmp. It is the cwd and TMPDIR, and is
	// removed when the call ends. Stale ones (older than a day, e.g. after a
	// SIGKILL) are swept at the start of the next scratch call. Not with Dir.
	Scratch bool
	// Unreadable lists extra paths this user must not be able to read (for
	// example another model user's home). They are probed like the registered
	// Box paths, once per user and path, and a missing one refuses.
	Unreadable []string
	// Allowlist names the Box environment variables forwarded to the model.
	// Values travel in the launcher's environment (never argv), so they are not
	// shown in ps or logged by sudo. HOME, PATH, TMPDIR and LANG are fixed.
	Allowlist []string
	// Env gives explicit values (taking precedence over the Box environment)
	// for variables forwarded the same way. PATH and LANG here replace the
	// defaults.
	Env map[string]string
	// Timeout bounds the call; when the context also has a deadline the earlier
	// of the two applies.
	Timeout time.Duration
	// WaitDelay bounds how long the Box waits for the launcher after cancel.
	// Zero means DefaultWaitDelay.
	WaitDelay time.Duration
}

// Command builds the launch command:
//
//	sudo -n --preserve-env=NAMES -u U -- timeout --kill-after=15s T /bin/sh -c SCRIPT sh ... env -i ... binary args
//
// The deadline is enforced on the model user's side by coreutils timeout (the
// Box uid cannot signal another uid's processes) and again from the Box by
// WaitDelay. Secret values are only ever in cmd.Env.
func Command(ctx context.Context, spec Spec) (*Cmd, error) {
	return build(ctx, spec, true)
}

func build(ctx context.Context, spec Spec, probed bool) (*Cmd, error) {
	if spec.RunAs == nil || spec.RunAs.HomeDir == "" {
		return nil, errors.New("model run-as user is not resolved")
	}
	if !strings.HasPrefix(spec.Binary, "/") {
		return nil, fmt.Errorf("model binary %q must be an absolute path", spec.Binary)
	}
	if probed {
		if err := probeIsolation(ctx, spec); err != nil {
			return nil, err
		}
	}
	if spec.Scratch && spec.Dir != "" {
		return nil, errors.New("Scratch and Dir are mutually exclusive")
	}
	budget := spec.Timeout
	if deadline, ok := ctx.Deadline(); ok {
		if left := time.Until(deadline); budget <= 0 || left < budget {
			budget = left
		}
	}
	if budget <= 0 {
		return nil, errors.New("model call has no time left")
	}
	seconds := int64((budget + time.Second - 1) / time.Second)
	home := spec.RunAs.HomeDir
	dir := spec.Dir
	if dir == "" {
		dir = home
	}
	path, lang := defaultPATH, defaultLocale
	if v := spec.Env["PATH"]; v != "" {
		path = v
	}
	if v := spec.Env["LANG"]; v != "" {
		lang = v
	}

	values := map[string]string{}
	for _, name := range spec.Allowlist {
		if !validEnvName(name) {
			return nil, fmt.Errorf("invalid environment variable name %q", name)
		}
		if value, ok := os.LookupEnv(name); ok {
			values[name] = value
		}
	}
	for name, value := range spec.Env {
		if !validEnvName(name) {
			return nil, fmt.Errorf("invalid environment variable name %q", name)
		}
		values[name] = value
	}
	names := make([]string, 0, len(values))
	for name := range values {
		switch name {
		case "HOME", "PATH", "TMPDIR", "LANG":
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	preserved, assigns := []string{}, []string{}
	env := []string{"PATH=" + path}
	for _, name := range names {
		preserved = append(preserved, name)
		assigns = append(assigns, name+`="$`+name+`"`)
		env = append(env, name+"="+values[name])
	}
	// The script re-reads each preserved value from its own environment, so the
	// argv carries names only. env -i then gives the model nothing else (sudo's
	// SUDO_*, MAIL, LOGNAME, USER, SHELL are dropped with everything else). The
	// script's own variables carry the reserved prefix so no forwarded name
	// can shadow them.
	envCall := `/usr/bin/env -i -C "$__s42_dir" HOME="$__s42_home" PATH="$__s42_path" LANG="$__s42_lang" TMPDIR="$__s42_tmp" ` +
		strings.Join(assigns, " ") + ` "$@"`
	script := `__s42_dir=$1; __s42_home=$2; __s42_path=$3; __s42_lang=$4; shift 4; __s42_tmp="$__s42_home/tmp"; exec ` + envCall
	if spec.Scratch {
		script = `__s42_dir=$1; __s42_home=$2; __s42_path=$3; __s42_lang=$4; shift 4; ` +
			`find "$__s42_home/tmp" -maxdepth 1 -type d -name 'run.*' -mtime +0 -exec rm -rf -- {} + >/dev/null 2>&1; ` +
			`__s42_dir=$(mktemp -d "$__s42_home/tmp/run.XXXXXX") || exit 125; __s42_tmp=$__s42_dir; ` +
			`trap 'chmod -R u+w -- "$__s42_dir" 2>/dev/null; rm -rf -- "$__s42_dir"' EXIT; trap 'exit 143' HUP INT TERM; ` +
			envCall + `; exit $?`
	}
	// The outermost process prints its pid (which timeout keeps through exec and
	// makes the process group leader) so the Box can later signal the group
	// through sudo.
	prologue := `printf '` + pgidMarker + `%s\n' "$$" >&2; exec "$@"`
	argv := launchPrefix(spec.RunAs.Username, preserved)
	argv = append(argv, shellBinary, "-c", prologue, "sh",
		timeoutBinary, "--kill-after="+strconv.FormatInt(int64(KillAfter/time.Second), 10)+"s", strconv.FormatInt(seconds, 10)+"s",
		shellBinary, "-c", script, "sh", dir, home, path, lang, spec.Binary)
	argv = append(argv, spec.Args...)

	c := &Cmd{Cmd: exec.CommandContext(ctx, argv[0], argv[1:]...), runAs: spec.RunAs.Username, done: make(chan struct{})}
	cmd := c.Cmd
	// The launcher carries only PATH (to find sudo) and the allowlisted values.
	// "/" because the Box's cwd may not be reachable by the model user.
	cmd.Env = env
	cmd.Dir = "/"
	cmd.WaitDelay = spec.WaitDelay
	if cmd.WaitDelay <= 0 {
		cmd.WaitDelay = DefaultWaitDelay
	}
	isolateProcessTree(cmd)
	local := cmd.Cancel
	cmd.Cancel = func() error {
		if pgid := c.pgid.Load(); pgid > 1 {
			c.killModelGroup(pgid)
		}
		if local != nil {
			return local()
		}
		return nil
	}
	return c, nil
}

func validEnvName(name string) bool {
	return envNameRE.MatchString(name) && !strings.HasPrefix(name, reservedPrefix)
}

// Cmd is an exec.Cmd whose cancellation reaches the model user's processes.
// Run and Start/Wait must be used, so the pgid marker is read; the promoted
// Output and CombinedOutput would skip the marker filter and are shadowed with
// methods that refuse.
type Cmd struct {
	*exec.Cmd
	runAs    string
	pgid     atomic.Int64
	done     chan struct{}
	doneOnce sync.Once
	filter   *markerFilter
}

// Start wraps the caller's stderr to strip the pgid marker line.
func (c *Cmd) Start() error {
	dst := c.Cmd.Stderr
	if dst == nil {
		dst = io.Discard
	}
	c.filter = &markerFilter{dst: dst, found: func(pgid int64) { c.pgid.Store(pgid) }}
	c.Cmd.Stderr = c.filter
	if err := c.Cmd.Start(); err != nil {
		c.doneOnce.Do(func() { close(c.done) })
		return err
	}
	return nil
}

func (c *Cmd) Wait() error {
	err := c.Cmd.Wait()
	c.doneOnce.Do(func() { close(c.done) })
	if c.filter != nil {
		c.filter.flush()
	}
	if c.pgid.Load() == 0 {
		log.Printf("modelrun: no process-group marker from the launcher for user %s: an early cancel falls back to local signals and may not stop its processes", c.runAs)
	}
	return err
}

// Output is not supported: it would bypass the marker filter and leave
// cancellation without a process group to signal. Set Stdout and call Run.
func (c *Cmd) Output() ([]byte, error) {
	return nil, errors.New("modelrun: Output is not supported; set Stdout and use Run")
}

// CombinedOutput is not supported either; see Output.
func (c *Cmd) CombinedOutput() ([]byte, error) {
	return nil, errors.New("modelrun: CombinedOutput is not supported; set Stdout and Stderr and use Run")
}

func (c *Cmd) Run() error {
	if err := c.Start(); err != nil {
		return err
	}
	return c.Wait()
}

// killModelGroup signals the model-side process group as the model user:
// TERM now, KILL after killGrace unless the call has ended. The sudoers rule
// lets the Box act as the model user, which is the only way across the uid
// boundary (a direct signal to root's sudo is EPERM).
func (c *Cmd) killModelGroup(pgid int64) {
	// The seams are read now, while this call still owns them. The KILL goroutine
	// can outlive the call, and it must not read package state a later test writes.
	grace := killGrace
	prefix := launchPrefix(c.runAs, nil)
	bin := killBinary
	send := func(sig string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		argv := append(prefix, bin, "-"+sig, "--", "-"+strconv.FormatInt(pgid, 10))
		kill := exec.CommandContext(ctx, argv[0], argv[1:]...)
		kill.Env = []string{"PATH=" + defaultPATH}
		kill.Dir = "/"
		_ = kill.Run()
	}
	send("TERM")
	go func() {
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-c.done:
		case <-timer.C:
			send("KILL")
		}
	}()
}

// sudoPrefix starts every line sudo itself prints (for example "sudo: unable
// to resolve host ..."). Only sudo runs before the prologue, so such lines
// ahead of the marker are sudo's, never the model's.
const sudoPrefix = "sudo: "

// markerFilter removes the pgid marker line, which is the first line the
// launcher's prologue prints; lines sudo printed before it pass through.
type markerFilter struct {
	dst     io.Writer
	found   func(int64)
	decided bool
	buf     []byte
	mu      sync.Mutex
}

func (f *markerFilter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.decided {
		return f.dst.Write(p)
	}
	f.buf = append(f.buf, p...)
	for {
		line, rest, ok := bytes.Cut(f.buf, []byte("\n"))
		if !ok {
			compatible := strings.HasPrefix(pgidMarker, string(f.buf)) || bytes.HasPrefix(f.buf, []byte(pgidMarker))
			if compatible && len(f.buf) < 32 {
				return len(p), nil
			}
			if (strings.HasPrefix(sudoPrefix, string(f.buf)) || bytes.HasPrefix(f.buf, []byte(sudoPrefix))) && len(f.buf) < 1024 {
				return len(p), nil
			}
			return len(p), f.release()
		}
		if bytes.HasPrefix(line, []byte(sudoPrefix)) {
			if _, err := f.dst.Write(f.buf[:len(line)+1]); err != nil {
				return len(p), err
			}
			f.buf = append([]byte(nil), rest...)
			continue
		}
		f.decided = true
		if bytes.HasPrefix(line, []byte(pgidMarker)) {
			if pgid, err := strconv.ParseInt(string(line[len(pgidMarker):]), 10, 64); err == nil {
				f.found(pgid)
				f.buf = nil
				if len(rest) > 0 {
					_, err := f.dst.Write(rest)
					return len(p), err
				}
				return len(p), nil
			}
		}
		return len(p), f.release()
	}
}

// release passes the held bytes through unchanged.
func (f *markerFilter) release() error {
	f.decided = true
	held := f.buf
	f.buf = nil
	_, err := f.dst.Write(held)
	return err
}

func (f *markerFilter) flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.decided && len(f.buf) > 0 {
		_ = f.release()
	}
}

// Sweep is the backstop after a run as a dedicated user: it kills every
// process the user still has (an escaped double-fork included) and removes the
// user's scratch directories. It is only safe for a user that has no other
// work in flight, so callers serialize their runs per user. Best effort: the
// error is for logging.
func Sweep(ctx context.Context, runAs *user.User) error {
	if runAs == nil || runAs.HomeDir == "" {
		return errors.New("sweep: user is not resolved")
	}
	run := func(args ...string) ([]byte, error) {
		argv := append(launchPrefix(runAs.Username, nil), args...)
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = []string{"PATH=" + defaultPATH}
		cmd.Dir = "/"
		return cmd.CombinedOutput()
	}
	var errs []error
	// kill -1 exits 1 when the user has nothing left, which is the normal case
	// and not an error: older kills say "No such process", procps-ng 4 says
	// nothing. A sudo failure always prints a message.
	if out, err := run(killAllBinary, "-KILL", "-1"); err != nil && len(bytes.TrimSpace(out)) > 0 && !bytes.Contains(out, []byte("No such process")) {
		errs = append(errs, fmt.Errorf("kill all processes of %s: %w: %s", runAs.Username, err, strings.TrimSpace(string(out))))
	}
	const clean = `for d in "$1"/run.*; do [ -e "$d" ] || continue; chmod -R u+rwX -- "$d" 2>/dev/null; rm -rf -- "$d"; done`
	if out, err := run("/usr/bin/env", "-i", "PATH="+defaultPATH, shellBinary, "-c", clean, "sh", runAs.HomeDir+"/tmp"); err != nil {
		errs = append(errs, fmt.Errorf("remove scratch directories of %s: %w: %s", runAs.Username, err, strings.TrimSpace(string(out))))
	}
	return errors.Join(errs...)
}
