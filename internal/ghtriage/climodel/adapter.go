package climodel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/SofiaFlux/summa42/internal/ghtriage"
)

type Config struct {
	Binary  string
	Args    []string
	Timeout time.Duration
}

type Adapter struct {
	binary  string
	args    []string
	timeout time.Duration
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
	if _, err := exec.LookPath(config.Binary); err != nil {
		return nil, fmt.Errorf("model binary %q is not usable: %w", config.Binary, err)
	}
	return &Adapter{binary: config.Binary, args: config.Args, timeout: config.Timeout}, nil
}

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

func (a *Adapter) run(ctx context.Context, prompt []byte) ([]byte, error) {
	if a == nil {
		return nil, errors.New("model adapter is not configured")
	}
	runCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(runCtx, a.binary, a.args...)
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
