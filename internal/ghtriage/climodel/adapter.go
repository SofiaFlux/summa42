package climodel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/ghtriage"
)

// Seams for the user database and the privilege-drop launcher. Production uses
// the real ones; the package tests substitute a fake sudo so no real user switch
// is needed. They are unexported on purpose: there is no setting that lets a
// deployment skip the dedicated user.
var (
	lookupUser    = user.Lookup
	currentUser   = user.Current
	launchPrefix  = func(runAs string) []string { return []string{"sudo", "-n", "-u", runAs, "--"} }
	defaultPATH   = "/usr/local/bin:/usr/bin:/bin"
	defaultLocale = "C.UTF-8"
)

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

type Config struct {
	// RunAsUser is the dedicated OS user model processes run as, through
	// sudo -n -u. Required: construction fails when it is unset, root or the
	// Box's own user.
	RunAsUser string
	Binary    string
	Args      []string
	Timeout   time.Duration
	// EnvAllowlist names the Box environment variables forwarded to the model
	// process (its own auth variables). Everything else is withheld, so an
	// agentic model cannot reuse GitHub or Box credentials.
	EnvAllowlist []string
}

type Adapter struct {
	runAs     string
	home      string
	binary    string
	args      []string
	timeout   time.Duration
	allowlist []string
}

// New validates the model configuration at construction, so an unusable
// configuration is a startup error rather than a per-tick error.
func New(config Config) (*Adapter, error) {
	if config.Binary == "" {
		return nil, errors.New("model binary is required")
	}
	if config.Timeout <= 0 {
		return nil, errors.New("model timeout must be positive")
	}
	target, err := ResolveRunAsUser(config.RunAsUser)
	if err != nil {
		return nil, err
	}
	resolved, err := exec.LookPath(config.Binary)
	if err != nil {
		return nil, fmt.Errorf("model binary %q is not usable: %w", config.Binary, err)
	}
	return &Adapter{runAs: target.Username, home: target.HomeDir, binary: resolved, args: config.Args, timeout: config.Timeout, allowlist: config.EnvAllowlist}, nil
}

// Timeout is the per-call model timeout.
func (a *Adapter) Timeout() time.Duration { return a.timeout }

func (a *Adapter) Classify(ctx context.Context, input ghtriage.Stage2Input) (ghtriage.Stage2Output, error) {
	prompt, err := json.Marshal(input)
	if err != nil {
		return ghtriage.Stage2Output{}, fmt.Errorf("encode stage 2 input: %w", err)
	}
	raw, err := a.run(ctx, prompt)
	if err != nil {
		return ghtriage.Stage2Output{}, err
	}
	return ghtriage.ParseStage2Output(raw)
}

func (a *Adapter) Review(ctx context.Context, input ghtriage.ReviewInput) (bool, error) {
	prompt, err := json.Marshal(input)
	if err != nil {
		return false, fmt.Errorf("encode review input: %w", err)
	}
	raw, err := a.run(ctx, prompt)
	if err != nil {
		return false, err
	}
	return ghtriage.ParseReviewVerdict(raw)
}

func (a *Adapter) Plan(ctx context.Context, input ghtriage.PlanInput) (ghtriage.PlanOutput, error) {
	prompt, err := json.Marshal(input)
	if err != nil {
		return ghtriage.PlanOutput{}, fmt.Errorf("encode plan input: %w", err)
	}
	raw, err := a.run(ctx, prompt)
	if err != nil {
		return ghtriage.PlanOutput{}, err
	}
	return ghtriage.ParsePlanOutput(raw)
}

func (a *Adapter) SelectFiles(ctx context.Context, input ghtriage.FileSelectionInput) (ghtriage.FileSelection, error) {
	prompt, err := json.Marshal(input)
	if err != nil {
		return ghtriage.FileSelection{}, err
	}
	raw, err := a.run(ctx, prompt)
	if err != nil {
		return ghtriage.FileSelection{}, err
	}
	return ghtriage.ParseFileSelection(raw)
}

func (a *Adapter) ReviewPlan(ctx context.Context, input ghtriage.PlanReviewInput) (ghtriage.PlanReviewOutput, error) {
	prompt, err := json.Marshal(input)
	if err != nil {
		return ghtriage.PlanReviewOutput{}, err
	}
	raw, err := a.run(ctx, prompt)
	if err != nil {
		return ghtriage.PlanReviewOutput{}, err
	}
	return ghtriage.ParsePlanReviewOutput(raw)
}

func (a *Adapter) Implement(ctx context.Context, input ghtriage.ImplementationInput) (ghtriage.ImplementationOutput, error) {
	prompt, err := json.Marshal(input)
	if err != nil {
		return ghtriage.ImplementationOutput{}, err
	}
	raw, err := a.run(ctx, prompt)
	if err != nil {
		return ghtriage.ImplementationOutput{}, err
	}
	return ghtriage.ParseImplementationOutput(raw)
}

func (a *Adapter) ReviewCode(ctx context.Context, input ghtriage.CodeReviewInput) (ghtriage.PlanReviewOutput, error) {
	prompt, err := json.Marshal(input)
	if err != nil {
		return ghtriage.PlanReviewOutput{}, err
	}
	raw, err := a.run(ctx, prompt)
	if err != nil {
		return ghtriage.PlanReviewOutput{}, err
	}
	return ghtriage.ParsePlanReviewOutput(raw)
}

func (a *Adapter) run(ctx context.Context, prompt []byte) ([]byte, error) {
	if a == nil {
		return nil, errors.New("model adapter is not configured")
	}
	runCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	argv := a.argv()
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	// The launcher itself carries no Box state: sudo resets the environment and
	// the model's own environment is given on the env command line. "/" because
	// the Box's cwd may not be reachable by the model user; env -C moves the
	// model into its own home once the user switch is done.
	cmd.Env = []string{"PATH=" + a.envValue("PATH", defaultPATH)}
	cmd.Dir = "/"
	isolateProcessTree(cmd)
	cmd.Stdin = bytes.NewReader(prompt)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Checked before the process error is wrapped, so a caller can tell a
		// model that hung from one that crashed: cancelling the subtree kills
		// it, and the wrapped signal-kill is indistinguishable from a crash by
		// eye. errors.Is against DeadlineExceeded is the only handle on it, so
		// the deadline is wrapped rather than returned bare.
		//
		// Either way the captured stderr rides along. A provider reporting a
		// rate limit or an auth failure and then stalling is exactly the case
		// worth diagnosing, and a bare deadline exceeded leaves no trace of it:
		// before this branch existed that text reached the caller, so dropping
		// it here would make a timeout quieter than a plain crash.
		if runCtx.Err() != nil {
			return nil, fmt.Errorf("model invocation timed out: %w: %s", runCtx.Err(), stderr.String())
		}
		return nil, fmt.Errorf("model invocation failed: %w: %s", err, stderr.String())
	}
	return stdout.Bytes(), nil
}

func (a *Adapter) envValue(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return fallback
}

// argv is the whole launch command: the privilege drop, then env -i so the
// model sees only what is listed here. HOME is the model user's own home and
// TMPDIR a private directory inside it; the Box's HOME never appears.
func (a *Adapter) argv() []string {
	argv := append([]string{}, launchPrefix(a.runAs)...)
	argv = append(argv, "/usr/bin/env", "-i", "-C", a.home,
		"HOME="+a.home,
		"PATH="+a.envValue("PATH", defaultPATH),
		"LANG="+a.envValue("LANG", defaultLocale),
		"TMPDIR="+a.home+"/tmp",
	)
	argv = append(argv, a.childEnv()...)
	argv = append(argv, a.binary)
	return append(argv, a.args...)
}

// childEnv is the allowlisted Box variables forwarded to the model, each only
// when present. HOME, PATH, TMPDIR and LANG are fixed by argv and cannot be
// overridden through the allowlist.
func (a *Adapter) childEnv() []string {
	env := []string{}
	for _, name := range a.allowlist {
		switch name {
		case "HOME", "PATH", "TMPDIR", "LANG":
			continue
		}
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}
