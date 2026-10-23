package climodel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"os/user"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/modelrun"
)

// ResolveRunAsUser is the fail-closed dedicated-user gate shared by every model
// launcher; see modelrun.
func ResolveRunAsUser(name string) (*user.User, error) { return modelrun.ResolveRunAsUser(name) }

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
	// CommandCodeModel selects the Command Code (cmd) backend with this model
	// id; the adapter then builds the whole argv. AllowPaid permits a model
	// that is not free; ZDR sets CMD_ZDR=1. All three are Command Code only.
	CommandCodeModel string
	AllowPaid        bool
	ZDR              bool
}

type Adapter struct {
	runAs     *user.User
	binary    string
	args      []string
	timeout   time.Duration
	allowlist []string
	zdr       bool
	// commandCode: parse cmd's NDJSON and give it a private scratch cwd, so
	// one call cannot leave project config for the next (the reviewer's).
	commandCode bool
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
	if err := checkCommandCode(config); err != nil {
		return nil, err
	}
	if config.CommandCodeModel != "" {
		config.Args = commandCodeArgs(config.CommandCodeModel)
	}
	resolved, err := exec.LookPath(config.Binary)
	if err != nil {
		return nil, fmt.Errorf("model binary %q is not usable: %w", config.Binary, err)
	}
	return &Adapter{runAs: target, binary: resolved, args: config.Args, timeout: config.Timeout, allowlist: config.EnvAllowlist, zdr: config.ZDR, commandCode: config.CommandCodeModel != ""}, nil
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

	var stdout, stderr cappedBuffer
	cmd, err := modelrun.Command(runCtx, modelrun.Spec{
		RunAs: a.runAs, Binary: a.binary, Args: a.args, Allowlist: a.allowlist, Timeout: a.timeout, Env: a.env(),
		Scratch: a.commandCode,
	})
	if err != nil {
		return nil, err
	}
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
		if providerRefused(stderr.String() + stdout.String()) {
			return nil, fmt.Errorf("model invocation failed: %w: %w: %s", ghtriage.ErrModelUnavailable, err, stderr.String())
		}
		return nil, fmt.Errorf("model invocation failed: %w: %s", err, stderr.String())
	}
	if a.commandCode {
		return commandCodeAnswer(stdout.Bytes())
	}
	return stdout.Bytes(), nil
}

// maxModelOutput bounds what the Box buffers from a model process, so a model
// cannot exhaust the Box's memory through stdout or stderr.
const maxModelOutput = 64 << 20

// cappedBuffer fails the write past maxModelOutput, which ends the model run.
type cappedBuffer struct{ bytes.Buffer }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxModelOutput {
		return 0, fmt.Errorf("model output exceeds %d bytes", maxModelOutput)
	}
	return b.Buffer.Write(p)
}

func (a *Adapter) env() map[string]string {
	if a.zdr {
		return map[string]string{"CMD_ZDR": "1"}
	}
	return nil
}

// providerRefused reports a quota or rate-limit refusal in model CLI output.
// ponytail: substring list of the messages seen from Command Code and Codex;
// extend it when another CLI words its limit differently.
func providerRefused(output string) bool {
	lower := strings.ToLower(output)
	for _, marker := range []string{"reached today's limit", "rate limit", "usage limit", "quota exceeded"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
