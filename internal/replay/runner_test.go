package replay_test

import (
	"context"
	"testing"

	"github.com/SofiaFlux/summa42/internal/replay"
)

func validTestFixture() replay.Fixture {
	return replay.Fixture{
		Version:   replay.FixtureVersion,
		FixtureID: "fix_harness_01",
		Task: replay.TaskSemantics{
			Objective:            "Compute aggregated metrics",
			TaskClass:            "batch_compute",
			RequiredCapabilities: []string{"calc.compute"},
			AcceptanceCriteria:   []string{"metrics aggregated"},
			TaskPayloadHash:      "hash_payload_123",
		},
		Execution: replay.ExecutionMetadata{
			ExecutorKind:          "mock_executor",
			ExecutorModel:         "deterministic-v1",
			TEBProfile:            "strict_oci",
			ContextProjectionHash: "proj_hash_1",
		},
		Invariants: replay.ExpectedInvariants{
			RequireAuthorityCeilingHeld: true,
			RequireZeroAmbientNetwork:   true,
			RequireAuditTamperEvident:   true,
			RequireNoStaleAttemptAction: true,
		},
		CostEnvelope: replay.CostEnvelope{
			MaxTokens:   5000,
			MaxExposure: 100,
		},
		Sanitization: replay.SanitizationProvenance{
			SanitizerVersion: replay.SanitizerVersion,
			RulesetHash:      "hash1",
			PassedChecks:     []string{"ALL"},
		},
	}
}

func TestReplayRunner_OfflineSuccess(t *testing.T) {
	fixture := validTestFixture()
	runner := replay.NewRunner()

	// Register simulated execution outcome satisfying invariants
	runner.RegisterExecutor("mock_executor", func(ctx context.Context, f replay.Fixture) (replay.ExecutionResult, error) {
		return replay.ExecutionResult{
			Success:              true,
			TokensConsumed:       1200,
			ExternalExposure:     0,
			AttemptedAuthority:   []string{"calc.compute"},
			NetworkEgressBlocked: true,
			AuditUntampered:      true,
			StaleAttemptBlocked:  true,
		}, nil
	})

	report, err := runner.Execute(context.Background(), fixture)
	if err != nil {
		t.Fatalf("replay execution error: %v", err)
	}

	if !report.Passed {
		t.Fatalf("expected replay to pass, failed invariants: %v", report.FailedInvariants)
	}
	if report.TokensConsumed != 1200 {
		t.Fatalf("expected 1200 tokens consumed, got %d", report.TokensConsumed)
	}
}

func TestReplayRunner_FailsOnInvariantViolation(t *testing.T) {
	fixture := validTestFixture()
	runner := replay.NewRunner()

	// Simulated rogue behavior: authority ceiling breached
	runner.RegisterExecutor("mock_executor", func(ctx context.Context, f replay.Fixture) (replay.ExecutionResult, error) {
		return replay.ExecutionResult{
			Success:              true,
			TokensConsumed:       1500,
			AttemptedAuthority:   []string{"calc.compute", "host.root_exec"}, // Exceeds ceiling
			NetworkEgressBlocked: true,
			AuditUntampered:      true,
			StaleAttemptBlocked:  true,
		}, nil
	})

	report, err := runner.Execute(context.Background(), fixture)
	if err != nil {
		t.Fatalf("replay execution error: %v", err)
	}

	if report.Passed {
		t.Fatal("expected replay to fail due to authority ceiling violation")
	}

	hasAuthorityFailure := false
	for _, inv := range report.FailedInvariants {
		if inv == "AUTHORITY_CEILING_BREACHED" {
			hasAuthorityFailure = true
			break
		}
	}
	if !hasAuthorityFailure {
		t.Fatalf("expected AUTHORITY_CEILING_BREACHED in failed invariants, got: %v", report.FailedInvariants)
	}
}

func TestReplayRunner_FailsOnCostEnvelopeThreshold(t *testing.T) {
	fixture := validTestFixture()
	fixture.CostEnvelope.MaxTokens = 2000
	runner := replay.NewRunner()

	// Exceeds token budget
	runner.RegisterExecutor("mock_executor", func(ctx context.Context, f replay.Fixture) (replay.ExecutionResult, error) {
		return replay.ExecutionResult{
			Success:              true,
			TokensConsumed:       2500, // Exceeds 2000 limit
			AttemptedAuthority:   []string{"calc.compute"},
			NetworkEgressBlocked: true,
			AuditUntampered:      true,
			StaleAttemptBlocked:  true,
		}, nil
	})

	report, err := runner.Execute(context.Background(), fixture)
	if err != nil {
		t.Fatalf("replay execution error: %v", err)
	}

	if report.Passed {
		t.Fatal("expected replay to fail due to token limit exceedance")
	}

	hasCostFailure := false
	for _, inv := range report.FailedInvariants {
		if inv == "TOKEN_BUDGET_EXCEEDED" {
			hasCostFailure = true
			break
		}
	}
	if !hasCostFailure {
		t.Fatalf("expected TOKEN_BUDGET_EXCEEDED in failed invariants, got: %v", report.FailedInvariants)
	}
}

func TestReplayRunner_FailsOnStaleAttemptOrEgressLeak(t *testing.T) {
	fixture := validTestFixture()
	runner := replay.NewRunner()

	// Network egress was not strictly blocked
	runner.RegisterExecutor("mock_executor", func(ctx context.Context, f replay.Fixture) (replay.ExecutionResult, error) {
		return replay.ExecutionResult{
			Success:              true,
			TokensConsumed:       1000,
			AttemptedAuthority:   []string{"calc.compute"},
			NetworkEgressBlocked: false, // Invariant violation
			AuditUntampered:      true,
			StaleAttemptBlocked:  false, // Invariant violation
		}, nil
	})

	report, err := runner.Execute(context.Background(), fixture)
	if err != nil {
		t.Fatalf("replay execution error: %v", err)
	}

	if report.Passed {
		t.Fatal("expected replay to fail due to ambient network and stale attempt violations")
	}

	if len(report.FailedInvariants) < 2 {
		t.Fatalf("expected at least 2 invariant failures, got: %v", report.FailedInvariants)
	}
}

func TestReplayRunner_FailsOnUnsuccessfulExecution(t *testing.T) {
	fixture := validTestFixture()
	runner := replay.NewRunner()

	// All security invariants held, but execution failed (e.g. task logic error)
	runner.RegisterExecutor("mock_executor", func(ctx context.Context, f replay.Fixture) (replay.ExecutionResult, error) {
		return replay.ExecutionResult{
			Success:              false,
			TokensConsumed:       500,
			AttemptedAuthority:   []string{"calc.compute"},
			NetworkEgressBlocked: true,
			AuditUntampered:      true,
			StaleAttemptBlocked:  true,
		}, nil
	})

	report, err := runner.Execute(context.Background(), fixture)
	if err != nil {
		t.Fatalf("replay execution error: %v", err)
	}
	if report.Passed {
		t.Fatal("expected replay to fail when execution outcome was unsuccessful")
	}
	hasFail := false
	for _, inv := range report.FailedInvariants {
		if inv == "EXECUTION_UNSUCCESSFUL" {
			hasFail = true
			break
		}
	}
	if !hasFail {
		t.Fatalf("expected EXECUTION_UNSUCCESSFUL in failed invariants, got: %v", report.FailedInvariants)
	}
}

func TestReplayRunner_CompareExecutors(t *testing.T) {
	fixture := validTestFixture()
	runner := replay.NewRunner()

	runner.RegisterExecutor("adapter_a", func(ctx context.Context, f replay.Fixture) (replay.ExecutionResult, error) {
		return replay.ExecutionResult{
			Success:              true,
			TokensConsumed:       800,
			AttemptedAuthority:   []string{"calc.compute"},
			NetworkEgressBlocked: true,
			AuditUntampered:      true,
			StaleAttemptBlocked:  true,
		}, nil
	})

	runner.RegisterExecutor("adapter_b", func(ctx context.Context, f replay.Fixture) (replay.ExecutionResult, error) {
		return replay.ExecutionResult{
			Success:              true,
			TokensConsumed:       1200,
			AttemptedAuthority:   []string{"calc.compute"},
			NetworkEgressBlocked: true,
			AuditUntampered:      true,
			StaleAttemptBlocked:  true,
		}, nil
	})

	reports, err := runner.CompareExecutors(context.Background(), fixture, "adapter_a", "adapter_b")
	if err != nil {
		t.Fatalf("compare executors error: %v", err)
	}

	if len(reports) != 2 {
		t.Fatalf("expected 2 reports, got %d", len(reports))
	}
	if reports["adapter_a"].TokensConsumed != 800 {
		t.Errorf("expected 800 tokens for adapter_a, got %d", reports["adapter_a"].TokensConsumed)
	}
	if reports["adapter_b"].TokensConsumed != 1200 {
		t.Errorf("expected 1200 tokens for adapter_b, got %d", reports["adapter_b"].TokensConsumed)
	}
}
