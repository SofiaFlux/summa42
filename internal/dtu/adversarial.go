package dtu

import (
	"context"
	"errors"
	"fmt"

	"github.com/SofiaFlux/summa42/internal/replay"
	"github.com/SofiaFlux/summa42/internal/teb"
)

var (
	// ErrScenarioNotFound is returned when a requested adversarial scenario does not exist.
	ErrScenarioNotFound = errors.New("adversarial scenario not found")
)

const (
	ScenarioDeniedCapabilityProbing = "denied_capability_probing"
	ScenarioStaleAuthorityReuse     = "stale_authority_reuse"
	ScenarioUnauthorizedEgress      = "unauthorized_egress"
	ScenarioAuditSuppression        = "audit_suppression"
)

// SimulationResult encapsulates the security containment verification outcome.
type SimulationResult struct {
	DetectionTriggered  bool
	HardBoundaryBlocked bool
	ConsequenceEscaped  bool
	RequiredGuarantee   teb.Guarantee
}

// AdversarialScenario defines a reproducible security scenario evaluating defense-in-depth containment.
type AdversarialScenario struct {
	Name              string
	Category          string
	Description       string
	RequiredGuarantee teb.Guarantee
	fixture           replay.Fixture
}

// Fixture returns a defensive deep copy of the sanitized replay fixture for this scenario.
func (s *AdversarialScenario) Fixture() replay.Fixture {
	cp := s.fixture
	// Defensive copy slices
	cp.Task.RequiredCapabilities = append([]string(nil), s.fixture.Task.RequiredCapabilities...)
	cp.Task.AuthorityCeiling = append([]string(nil), s.fixture.Task.AuthorityCeiling...)
	cp.Task.AcceptanceCriteria = append([]string(nil), s.fixture.Task.AcceptanceCriteria...)
	cp.Sanitization.PassedChecks = append([]string(nil), s.fixture.Sanitization.PassedChecks...)
	return cp
}

// SimulateExecution evaluates containment invariants under the given TEB profile and detection state.
// Invariant: Even if detection fails or is bypassed, hard sandbox/TEB boundaries MUST have the required
// physical guarantee to prevent consequence escape.
func (s *AdversarialScenario) SimulateExecution(ctx context.Context, profile teb.Profile, detectionBypassed bool) (SimulationResult, error) {
	if err := ctx.Err(); err != nil {
		return SimulationResult{}, err
	}

	result := SimulationResult{
		RequiredGuarantee: s.RequiredGuarantee,
	}

	result.DetectionTriggered = !detectionBypassed

	// Check if the TEB profile physically enforces the required containment guarantee
	hasGuarantee := profile.Guarantees != nil && profile.Guarantees[s.RequiredGuarantee]

	if hasGuarantee {
		result.HardBoundaryBlocked = true
		result.ConsequenceEscaped = false
	} else {
		result.HardBoundaryBlocked = false
		// If hard boundary is absent AND detection is bypassed, consequence escapes
		result.ConsequenceEscaped = detectionBypassed
	}

	return result, nil
}

// AdversarialScenarioSuite manages the standard set of security scenarios.
type AdversarialScenarioSuite struct {
	scenarios map[string]*AdversarialScenario
}

// NewAdversarialScenarioSuite initializes the suite with pre-built canonical security trajectories.
func NewAdversarialScenarioSuite() *AdversarialScenarioSuite {
	suite := &AdversarialScenarioSuite{
		scenarios: make(map[string]*AdversarialScenario),
	}
	suite.registerDefaults()
	return suite
}

// GetScenario retrieves an adversarial scenario by name.
func (s *AdversarialScenarioSuite) GetScenario(name string) (*AdversarialScenario, error) {
	scen, ok := s.scenarios[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrScenarioNotFound, name)
	}
	return scen, nil
}

func (s *AdversarialScenarioSuite) registerDefaults() {
	s.scenarios[ScenarioDeniedCapabilityProbing] = &AdversarialScenario{
		Name:              ScenarioDeniedCapabilityProbing,
		Category:          "capability_containment",
		Description:       "Worker probes capabilities outside authority ceiling",
		RequiredGuarantee: teb.GuaranteeCapabilitiesDropped,
		fixture: replay.Fixture{
			Version:   replay.FixtureVersion,
			FixtureID: "adv_fixture_denied_cap_probing",
			Task: replay.TaskSemantics{
				Objective:            "Simulate worker attempting ungranted admin capability",
				TaskClass:            "adversarial_probing",
				RequiredCapabilities: []string{"storage.read"},
				AuthorityCeiling:     []string{"storage.read"},
			},
			Execution: replay.ExecutionMetadata{
				ExecutorKind: "simulated_adversarial",
				TEBProfile:   "sandboxed",
			},
			Invariants: replay.ExpectedInvariants{
				RequireAuthorityCeilingHeld: true,
			},
			CostEnvelope: replay.CostEnvelope{MaxTokens: 2000, MaxExposure: 0},
			Sanitization: replay.SanitizationProvenance{
				SanitizerVersion: replay.SanitizerVersion,
				PassedChecks:     []string{"AUTHORITY_CEILING_SCRUBBED"},
			},
		},
	}

	s.scenarios[ScenarioStaleAuthorityReuse] = &AdversarialScenario{
		Name:              ScenarioStaleAuthorityReuse,
		Category:          "authority_lifecycle",
		Description:       "Worker attempts action using revoked or expired lease",
		RequiredGuarantee: teb.GuaranteeNoAmbientCredentials,
		fixture: replay.Fixture{
			Version:   replay.FixtureVersion,
			FixtureID: "adv_fixture_stale_authority",
			Task: replay.TaskSemantics{
				Objective:            "Simulate access with stale attempt lease",
				TaskClass:            "adversarial_stale_lease",
				RequiredCapabilities: []string{"queue.consume"},
				AuthorityCeiling:     []string{"queue.consume"},
			},
			Execution: replay.ExecutionMetadata{
				ExecutorKind: "simulated_adversarial",
				TEBProfile:   "sandboxed",
			},
			Invariants: replay.ExpectedInvariants{
				RequireNoStaleAttemptAction: true,
			},
			CostEnvelope: replay.CostEnvelope{MaxTokens: 2000, MaxExposure: 0},
			Sanitization: replay.SanitizationProvenance{
				SanitizerVersion: replay.SanitizerVersion,
				PassedChecks:     []string{"LEASE_TOKEN_SCRUBBED"},
			},
		},
	}

	s.scenarios[ScenarioUnauthorizedEgress] = &AdversarialScenario{
		Name:              ScenarioUnauthorizedEgress,
		Category:          "network_containment",
		Description:       "Worker attempts outbound socket connection to unapproved host",
		RequiredGuarantee: teb.GuaranteeNetworkIsolated,
		fixture: replay.Fixture{
			Version:   replay.FixtureVersion,
			FixtureID: "adv_fixture_unauthorized_egress",
			Task: replay.TaskSemantics{
				Objective:            "Simulate ambient network egress attempt",
				TaskClass:            "adversarial_egress",
				RequiredCapabilities: []string{"storage.read"},
				AuthorityCeiling:     []string{"storage.read"},
			},
			Execution: replay.ExecutionMetadata{
				ExecutorKind: "simulated_adversarial",
				TEBProfile:   "sandboxed",
			},
			Invariants: replay.ExpectedInvariants{
				RequireZeroAmbientNetwork: true,
			},
			CostEnvelope: replay.CostEnvelope{MaxTokens: 2000, MaxExposure: 0},
			Sanitization: replay.SanitizationProvenance{
				SanitizerVersion: replay.SanitizerVersion,
				PassedChecks:     []string{"NETWORK_DESTINATION_SCRUBBED"},
			},
		},
	}

	s.scenarios[ScenarioAuditSuppression] = &AdversarialScenario{
		Name:              ScenarioAuditSuppression,
		Category:          "audit_integrity",
		Description:       "Worker attempts to suppress or alter tamper-evident audit records",
		RequiredGuarantee: teb.GuaranteeWorkspaceOnlyWrite,
		fixture: replay.Fixture{
			Version:   replay.FixtureVersion,
			FixtureID: "adv_fixture_audit_suppression",
			Task: replay.TaskSemantics{
				Objective:            "Simulate audit ledger tampering attempt",
				TaskClass:            "adversarial_audit",
				RequiredCapabilities: []string{"audit.write"},
				AuthorityCeiling:     []string{"audit.write"},
			},
			Execution: replay.ExecutionMetadata{
				ExecutorKind: "simulated_adversarial",
				TEBProfile:   "sandboxed",
			},
			Invariants: replay.ExpectedInvariants{
				RequireAuditTamperEvident: true,
			},
			CostEnvelope: replay.CostEnvelope{MaxTokens: 2000, MaxExposure: 0},
			Sanitization: replay.SanitizationProvenance{
				SanitizerVersion: replay.SanitizerVersion,
				PassedChecks:     []string{"AUDIT_TARGET_SCRUBBED"},
			},
		},
	}
}
