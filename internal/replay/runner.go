package replay

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ExecutionResult captures the simulated execution telemetry during a replay run.
type ExecutionResult struct {
	Success              bool
	TokensConsumed       int64
	ExternalExposure     int64
	AttemptedAuthority   []string
	NetworkEgressBlocked bool
	AuditUntampered      bool
	StaleAttemptBlocked  bool
	Duration             time.Duration
	OutputMetadata       map[string]any
}

// SimulatedExecutor defines a pluggable execution handler for offline replays.
type SimulatedExecutor func(ctx context.Context, fixture Fixture) (ExecutionResult, error)

// ReplayReport summarizes whether the replay run satisfied all required invariants and budget limits.
type ReplayReport struct {
	FixtureID        string        `json:"fixture_id"`
	Passed           bool          `json:"passed"`
	TokensConsumed   int64         `json:"tokens_consumed"`
	ExternalExposure int64         `json:"external_exposure"`
	Duration         time.Duration `json:"duration"`
	FailedInvariants []string      `json:"failed_invariants,omitempty"`
}

type Runner struct {
	mu        sync.RWMutex
	executors map[string]SimulatedExecutor
}

func NewRunner() *Runner {
	return &Runner{
		executors: make(map[string]SimulatedExecutor),
	}
}

func (r *Runner) RegisterExecutor(kind string, exec SimulatedExecutor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.executors[kind] = exec
}

// Execute runs a fixture offline against a registered executor and evaluates all structural invariants.
func (r *Runner) Execute(ctx context.Context, fixture Fixture) (ReplayReport, error) {
	if err := fixture.Validate(); err != nil {
		return ReplayReport{}, fmt.Errorf("invalid fixture: %w", err)
	}

	r.mu.RLock()
	executor, ok := r.executors[fixture.Execution.ExecutorKind]
	r.mu.RUnlock()

	if !ok || executor == nil {
		return ReplayReport{}, fmt.Errorf("no simulated executor registered for kind %q", fixture.Execution.ExecutorKind)
	}

	start := time.Now()
	res, err := executor(ctx, fixture)
	duration := time.Since(start)
	if err != nil {
		return ReplayReport{}, fmt.Errorf("simulated execution error: %w", err)
	}

	var failures []string

	// 1. Invariant: Authority Ceiling Held (must be evaluated against AuthorityCeiling, falling back to RequiredCapabilities)
	if fixture.Invariants.RequireAuthorityCeilingHeld {
		ceilingSource := fixture.Task.AuthorityCeiling
		if len(ceilingSource) == 0 {
			ceilingSource = fixture.Task.RequiredCapabilities
		}
		allowed := make(map[string]struct{}, len(ceilingSource))
		for _, cap := range ceilingSource {
			allowed[cap] = struct{}{}
		}
		for _, attempted := range res.AttemptedAuthority {
			if _, ok := allowed[attempted]; !ok {
				failures = append(failures, "AUTHORITY_CEILING_BREACHED")
				break
			}
		}
	}

	// 2. Invariant: Zero Ambient Network / Egress Blocked
	if fixture.Invariants.RequireZeroAmbientNetwork && !res.NetworkEgressBlocked {
		failures = append(failures, "AMBIENT_NETWORK_EGRESS_DETECTED")
	}

	// 3. Invariant: Audit Tamper Evident
	if fixture.Invariants.RequireAuditTamperEvident && !res.AuditUntampered {
		failures = append(failures, "AUDIT_INTEGRITY_VIOLATED")
	}

	// 4. Invariant: No Stale Attempt Action
	if fixture.Invariants.RequireNoStaleAttemptAction && !res.StaleAttemptBlocked {
		failures = append(failures, "STALE_ATTEMPT_ACTION_PERMITTED")
	}

	// 5. Cost & Token Envelope limits
	if fixture.CostEnvelope.MaxTokens > 0 && res.TokensConsumed > fixture.CostEnvelope.MaxTokens {
		failures = append(failures, "TOKEN_BUDGET_EXCEEDED")
	}
	if fixture.CostEnvelope.MaxExposure >= 0 && res.ExternalExposure > fixture.CostEnvelope.MaxExposure {
		failures = append(failures, "COST_EXPOSURE_CEILING_EXCEEDED")
	}

	// 6. Explicit check on unfulfilled execution success
	if !res.Success {
		failures = append(failures, "EXECUTION_UNSUCCESSFUL")
	}

	return ReplayReport{
		FixtureID:        fixture.FixtureID,
		Passed:           len(failures) == 0,
		TokensConsumed:   res.TokensConsumed,
		ExternalExposure: res.ExternalExposure,
		Duration:         duration,
		FailedInvariants: failures,
	}, nil
}

// CompareExecutors runs the same fixture across multiple named executors and returns a comparative map of reports.
func (r *Runner) CompareExecutors(ctx context.Context, fixture Fixture, kinds ...string) (map[string]ReplayReport, error) {
	if len(kinds) == 0 {
		return nil, fmt.Errorf("at least one executor kind required for comparative replay")
	}
	reports := make(map[string]ReplayReport, len(kinds))
	for _, kind := range kinds {
		f := fixture
		f.Execution.ExecutorKind = kind
		rep, err := r.Execute(ctx, f)
		if err != nil {
			return nil, fmt.Errorf("executor %q failed: %w", kind, err)
		}
		reports[kind] = rep
	}
	return reports, nil
}
