// Package modelrun is the one place a model CLI process is launched. Every
// model process (triage, review, plan and implementation adapters, the Copilot
// executor) runs as a dedicated, unprivileged OS user through sudo, so it cannot
// read the Box's secrets. There is no setting that skips the user switch.
package modelrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"sort"
	"strconv"
	"strings"
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
	shellBinary   = "/bin/sh"
	defaultPATH   = "/usr/local/bin:/usr/bin:/bin"
	defaultLocale = "C.UTF-8"
)

// KillAfter is how long the model-side timeout waits between SIGTERM and
// SIGKILL; DefaultWaitDelay is how long the Box waits for the launcher after
// cancelling before it abandons it.
const (
	KillAfter        = 15 * time.Second
	DefaultWaitDelay = KillAfter + 15*time.Second
)

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ResolveRunAsUser validates the dedicated model user: it must be named, exist,
// and be neither the current user nor root. This is the fail-closed gate that
// keeps a model process from ever sharing a uid with the Box's secrets.
func ResolveRunAsUser(name string) (*user.User, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("model run-as user is required: set --model-user or SUMMA42_MODEL_USER to a dedicated OS user that cannot read the Box secrets (see scripts/setup-model-user.sh)")
	}
	target, err := lookupUser(name)
	if err != nil {
		return nil, fmt.Errorf("model run-as user %q is not usable: %w", name, err)
	}
	if name == "root" || target.Uid == "0" {
		return nil, fmt.Errorf("model run-as user %q is root; models must run as a dedicated unprivileged user", name)
	}
	self, err := currentUser()
	if err != nil {
		return nil, fmt.Errorf("determine the current user: %w", err)
	}
	if self.Uid == target.Uid {
		return nil, fmt.Errorf("model run-as user %q is the Box's own user; models must run as a different, dedicated user", name)
	}
	if target.HomeDir == "" || !strings.HasPrefix(target.HomeDir, "/") {
		return nil, fmt.Errorf("model run-as user %q has no absolute home directory", name)
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
	Dir string
	// Allowlist names the Box environment variables forwarded to the model.
	// Values travel in the launcher's environment (never argv), so they are not
	// shown in ps or logged by sudo. HOME, PATH, TMPDIR and LANG are fixed.
	Allowlist []string
	// Env gives explicit values (taking precedence over the Box environment)
	// for variables forwarded the same way. PATH and LANG here replace the
	// defaults.
	Env map[string]string
	// Timeout is used when the context has no deadline; a context deadline wins.
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
func Command(ctx context.Context, spec Spec) (*exec.Cmd, error) {
	if spec.RunAs == nil || spec.RunAs.HomeDir == "" {
		return nil, errors.New("model run-as user is not resolved")
	}
	if !strings.HasPrefix(spec.Binary, "/") {
		return nil, fmt.Errorf("model binary %q must be an absolute path", spec.Binary)
	}
	budget := spec.Timeout
	if deadline, ok := ctx.Deadline(); ok {
		budget = time.Until(deadline)
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
	path, lang := envOr("PATH", defaultPATH), envOr("LANG", defaultLocale)
	if v := spec.Env["PATH"]; v != "" {
		path = v
	}
	if v := spec.Env["LANG"]; v != "" {
		lang = v
	}

	values := map[string]string{}
	for _, name := range spec.Allowlist {
		if !envNameRE.MatchString(name) {
			return nil, fmt.Errorf("invalid environment variable name %q", name)
		}
		if value, ok := os.LookupEnv(name); ok {
			values[name] = value
		}
	}
	for name, value := range spec.Env {
		if !envNameRE.MatchString(name) {
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
	// SUDO_*, MAIL, LOGNAME, USER, SHELL are dropped with everything else).
	script := `dir=$1; home=$2; path=$3; lang=$4; shift 4; exec /usr/bin/env -i -C "$dir" HOME="$home" PATH="$path" LANG="$lang" TMPDIR="$home/tmp" ` +
		strings.Join(assigns, " ") + ` "$@"`
	argv := launchPrefix(spec.RunAs.Username, preserved)
	argv = append(argv, timeoutBinary, "--kill-after="+strconv.FormatInt(int64(KillAfter/time.Second), 10)+"s", strconv.FormatInt(seconds, 10)+"s",
		shellBinary, "-c", script, "sh", dir, home, path, lang, spec.Binary)
	argv = append(argv, spec.Args...)

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// The launcher carries only PATH (to find sudo) and the allowlisted values.
	// "/" because the Box's cwd may not be reachable by the model user.
	cmd.Env = env
	cmd.Dir = "/"
	cmd.WaitDelay = spec.WaitDelay
	if cmd.WaitDelay <= 0 {
		cmd.WaitDelay = DefaultWaitDelay
	}
	isolateProcessTree(cmd)
	return cmd, nil
}

func envOr(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return fallback
}
