package dtu_test

import (
	"context"
	"errors"
	"testing"

	"github.com/SofiaFlux/summa42/internal/dtu"
	"github.com/SofiaFlux/summa42/internal/replay"
	"github.com/SofiaFlux/summa42/internal/teb"
)

func TestAdversarialScenarioSuite_StandardFixtures(t *testing.T) {
	suite := dtu.NewAdversarialScenarioSuite()

	expectedInvariants := map[string]func(inv replay.ExpectedInvariants) bool{
		dtu.ScenarioDeniedCapabilityProbing: func(inv replay.ExpectedInvariants) bool { return inv.RequireAuthorityCeilingHeld },
		dtu.ScenarioStaleAuthorityReuse:     func(inv replay.ExpectedInvariants) bool { return inv.RequireNoStaleAttemptAction },
		dtu.ScenarioUnauthorizedEgress:      func(inv replay.ExpectedInvariants) bool { return inv.RequireZeroAmbientNetwork },
		dtu.ScenarioAuditSuppression:        func(inv replay.ExpectedInvariants) bool { return inv.RequireAuditTamperEvident },
	}

	for name, checkInv := range expectedInvariants {
		t.Run(name, func(t *testing.T) {
			scen, err := suite.GetScenario(name)
			if err != nil {
				t.Fatalf("failed to get scenario %q: %v", name, err)
			}

			fixture := scen.Fixture()
			if err := fixture.Validate(); err != nil {
				t.Fatalf("scenario fixture invalid: %v", err)
			}
			if fixture.Version != replay.FixtureVersion {
				t.Errorf("expected version %d, got %d", replay.FixtureVersion, fixture.Version)
			}

			if !checkInv(fixture.Invariants) {
				t.Errorf("scenario %s missing expected specific invariant", name)
			}
		})
	}
}

func TestAdversarialScenario_ContainmentInvariant_DetectionBypassedHardBoundaryHolds(t *testing.T) {
	ctx := context.Background()
	suite := dtu.NewAdversarialScenarioSuite()

	enforcedProfile := teb.EnforcedOfflineProfile()

	scenarios := []string{
		dtu.ScenarioDeniedCapabilityProbing,
		dtu.ScenarioStaleAuthorityReuse,
		dtu.ScenarioUnauthorizedEgress,
		dtu.ScenarioAuditSuppression,
	}

	for _, name := range scenarios {
		t.Run(name, func(t *testing.T) {
			scen, err := suite.GetScenario(name)
			if err != nil {
				t.Fatalf("scenario %s not found: %v", name, err)
			}

			// 1. Nominal case: detection functional, boundary blocks
			resNominal, err := scen.SimulateExecution(ctx, enforcedProfile, false)
			if err != nil {
				t.Fatalf("simulation failed: %v", err)
			}
			if !resNominal.HardBoundaryBlocked {
				t.Errorf("expected hard boundary to block under nominal conditions")
			}
			if !resNominal.DetectionTriggered {
				t.Errorf("expected detection to trigger when detector is functional")
			}
			if resNominal.ConsequenceEscaped {
				t.Errorf("consequence must not escape under enforced profile")
			}

			// 2. Adversarial worst-case: detection FAILS / BYPASSED, hard boundary MUST STILL BLOCK
			resDetectionBypassed, err := scen.SimulateExecution(ctx, enforcedProfile, true /* detectionBypassed */)
			if err != nil {
				t.Fatalf("simulation with bypassed detection failed: %v", err)
			}
			if resDetectionBypassed.DetectionTriggered {
				t.Errorf("expected detectionTriggered to be false when bypassed")
			}
			if !resDetectionBypassed.HardBoundaryBlocked {
				t.Fatalf("CONTAINMENT INVARIANT VIOLATION: when detection was bypassed, hard TEB boundary failed to prevent consequence escape!")
			}
			if resDetectionBypassed.ConsequenceEscaped {
				t.Fatalf("CONTAINMENT INVARIANT VIOLATION: unauthorized consequence escaped containment!")
			}
		})
	}
}

func TestAdversarialScenario_NegativeControl_UnenforcedBoundaryAllowsEscapeWhenDetectionBypassed(t *testing.T) {
	ctx := context.Background()
	suite := dtu.NewAdversarialScenarioSuite()

	unenforcedProfile := teb.UnenforcedProfile("test-unenforced-teb")

	scen, err := suite.GetScenario(dtu.ScenarioUnauthorizedEgress)
	if err != nil {
		t.Fatalf("scenario not found: %v", err)
	}

	// Negative control verification:
	// If the hard boundary is unenforced/missing, and detection is bypassed, the consequence MUST escape!
	res, err := scen.SimulateExecution(ctx, unenforcedProfile, true /* detectionBypassed */)
	if err != nil {
		t.Fatalf("simulation error: %v", err)
	}
	if res.HardBoundaryBlocked {
		t.Errorf("negative control failed: unenforced profile must not report hard boundary blocked")
	}
	if !res.ConsequenceEscaped {
		t.Errorf("negative control failed: consequence must escape when boundary is unenforced and detection is bypassed")
	}
}

func TestAdversarialScenario_ContextCancellation(t *testing.T) {
	suite := dtu.NewAdversarialScenarioSuite()
	scen, _ := suite.GetScenario(dtu.ScenarioUnauthorizedEgress)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := scen.SimulateExecution(ctx, teb.EnforcedOfflineProfile(), false)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestAdversarialScenario_ExportReplayFixture(t *testing.T) {
	suite := dtu.NewAdversarialScenarioSuite()

	scen, err := suite.GetScenario(dtu.ScenarioUnauthorizedEgress)
	if err != nil {
		t.Fatalf("get scenario failed: %v", err)
	}

	fixture := scen.Fixture()
	if !fixture.Invariants.RequireZeroAmbientNetwork {
		t.Errorf("expected RequireZeroAmbientNetwork for unauthorized egress scenario")
	}

	// Verify the fixture executes cleanly under replay.Runner
	runner := replay.NewRunner()
	runner.RegisterExecutor(fixture.Execution.ExecutorKind, func(ctx context.Context, f replay.Fixture) (replay.ExecutionResult, error) {
		return replay.ExecutionResult{
			Success:              true,
			TokensConsumed:       150,
			ExternalExposure:     0,
			NetworkEgressBlocked: true, // hard boundary held!
			AuditUntampered:      true,
			AttemptedAuthority:   []string{"storage.read"},
		}, nil
	})

	report, err := runner.Execute(context.Background(), fixture)
	if err != nil {
		t.Fatalf("replay runner execution failed: %v", err)
	}
	if !report.Passed {
		t.Errorf("expected replay report to pass when hard boundary holds: failed invariants: %v", report.FailedInvariants)
	}
}

func TestAdversarialScenarioSuite_UnknownScenario(t *testing.T) {
	suite := dtu.NewAdversarialScenarioSuite()
	_, err := suite.GetScenario("non_existent_scenario")
	if !errors.Is(err, dtu.ErrScenarioNotFound) {
		t.Errorf("expected ErrScenarioNotFound, got %v", err)
	}
}
