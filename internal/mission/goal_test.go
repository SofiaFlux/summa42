package mission_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/mission"
)

func sampleActiveMission() mission.Mission {
	return mission.Mission{
		ID:      domain.NewID("mission"),
		Version: 2,
		State:   mission.MissionStateActive,
		Purpose: "Provide a secure, highly reliable financial ledger with verified regulatory compliance.",
		DesiredOutcomes: []string{
			"Sub-100ms p99 transaction confirmation latency",
			"Zero double-spending and deterministic reconciliation",
			"Continuous automated GDPR data sovereignty compliance",
		},
		Obligations: []string{
			"Maintain immutable append-only audit logs",
			"Comply with PCI-DSS data storage mandates",
		},
		Constraints: []string{
			"Maximum single-operation compute cost $10.00 USD",
			"No direct external internet egress except via TEB proxy",
		},
		NonGoals: []string{
			"Speculative high-frequency trading",
			"Unfenced manual database modifications",
		},
		TradeOffRules: []mission.TradeOffRule{
			{
				Priority:    "Audit Integrity & Compliance",
				Subordinate: "Query Latency",
				Rationale:   "Compliance records cannot be compromised for performance gains",
			},
		},
		HumanTrustBoundaries: []string{
			"Financial authority transfer exceeding $1,000 requires human governor approval",
			"Production database schema drops or migrations require root human sign-off",
		},
		Assumptions: []string{
			"Underlying storage volume provides ACID transaction guarantees",
		},
		Unknowns: []string{
			"Long-term ledger growth under 100x traffic spike",
		},
		RevisionTriggers: []string{
			"Regulatory mandate amendments",
		},
	}
}

func TestGoalResearch_GenerateCandidateProposalsFromEvidence(t *testing.T) {
	ctx := context.Background()
	engine := mission.NewGoalResearchEngine()
	m := sampleActiveMission()

	worker := mission.Principal{
		ID:   "researcher-worker-01",
		Kind: mission.PrincipalKindWorker,
	}

	reality := mission.OperationalReality{
		ObservedTelemetry: []mission.TelemetryMetric{
			{
				Name:        "transaction_latency_p99_ms",
				Value:       185.0,
				Unit:        "ms",
				RecordedAt:  time.Now().UTC().Add(-10 * time.Minute),
				Description: "Transaction confirmation latency currently exceeds 100ms SLO target",
			},
		},
		ObservedEvidence: []mission.SupportingEvidenceItem{
			{
				URI:         "telemetry://metrics/p99_latency_spike_nov10",
				Kind:        mission.EvidenceKindLive,
				Description: "Live production Prometheus metric showing p99 latency at 185ms during peak load",
				CapturedAt:  time.Now().UTC().Add(-15 * time.Minute),
			},
		},
		CurrentLimitations: []string{
			"Unindexed secondary scan on transaction ledger table during balance inquiries",
		},
		CandidateOpportunities: []string{
			"Implement read-optimized covering index for transaction verification queries",
		},
	}

	proposals, err := engine.ResearchGoals(ctx, m, reality, worker)
	if err != nil {
		t.Fatalf("unexpected error researching goals: %v", err)
	}

	if len(proposals) == 0 {
		t.Fatal("expected at least one candidate goal proposal generated")
	}

	p := proposals[0]

	// 1. Mission linkage
	if p.MissionID != m.ID {
		t.Errorf("expected mission ID %s, got %s", m.ID, p.MissionID)
	}
	if p.MissionVersion != m.Version {
		t.Errorf("expected mission version %d, got %d", m.Version, p.MissionVersion)
	}

	// 2. Field completeness
	if strings.TrimSpace(p.ProblemStatement) == "" {
		t.Error("expected non-empty ProblemStatement")
	}
	if strings.TrimSpace(p.ExpectedOutcome) == "" {
		t.Error("expected non-empty ExpectedOutcome")
	}
	if len(p.SuccessCriteria) == 0 {
		t.Error("expected non-empty SuccessCriteria")
	}
	if len(p.Assumptions) == 0 {
		t.Error("expected non-empty Assumptions")
	}
	if len(p.ReferencedMissionElements) == 0 {
		t.Error("expected referenced mission elements")
	}
	if p.ResourceEnvelope.MaxCostUSD <= 0 {
		t.Error("expected positive MaxCostUSD in ResourceEnvelope")
	}
	if p.Reversibility == "" {
		t.Error("expected non-empty Reversibility")
	}
	if p.RiskClassification == "" {
		t.Error("expected non-empty RiskClassification")
	}

	// 3. Deterministic validation succeeds on engine output
	if err := mission.ValidateGoalProposal(p, m); err != nil {
		t.Errorf("expected generated proposal to pass validation, got: %v", err)
	}
}

func TestGoalResearch_DistinguishesLiveFromSimulatedEvidence(t *testing.T) {
	ctx := context.Background()
	engine := mission.NewGoalResearchEngine()
	m := sampleActiveMission()

	worker := mission.Principal{
		ID:   "researcher-worker-01",
		Kind: mission.PrincipalKindWorker,
	}

	reality := mission.OperationalReality{
		ObservedEvidence: []mission.SupportingEvidenceItem{
			{
				URI:         "audit://prod/reconciliation_failure_42",
				Kind:        mission.EvidenceKindLive,
				Description: "Live incident report on edge-case ledger reconciliation mismatch",
				CapturedAt:  time.Now().UTC(),
			},
			{
				URI:         "dtu://simulation/twin_stress_run_99",
				Kind:        mission.EvidenceKindSimulated,
				Description: "Digital Twin hostile scenario finding race condition under network partition",
				CapturedAt:  time.Now().UTC(),
			},
		},
		CurrentLimitations: []string{
			"Ledger reconciliation retry window is too narrow under temporary network partitions",
		},
	}

	proposals, err := engine.ResearchGoals(ctx, m, reality, worker)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(proposals) == 0 {
		t.Fatal("expected proposal generated")
	}

	var foundLive, foundSimulated bool
	for _, ev := range proposals[0].SupportingEvidence {
		if ev.Kind == mission.EvidenceKindLive {
			foundLive = true
		}
		if ev.Kind == mission.EvidenceKindSimulated {
			foundSimulated = true
		}
	}

	if !foundLive || !foundSimulated {
		t.Errorf("expected both live and simulated evidence tracked distinctly, live=%v, simulated=%v", foundLive, foundSimulated)
	}
}

func TestGoalProposal_ValidationRejection_BypassingHumanTrustBoundary(t *testing.T) {
	m := sampleActiveMission()

	// Proposal touches production schema migration (which requires human sign-off per mission boundary)
	// but claims RequiresHumanApproval = false
	proposal := mission.GoalProposal{
		ID:               domain.NewID("goalprop"),
		MissionID:        m.ID,
		MissionVersion:   m.Version,
		Title:            "Automated Production Database Schema Migration",
		ProblemStatement: "Table structure requires migration to support new indexing columns.",
		SupportingEvidence: []mission.SupportingEvidenceItem{
			{
				URI:         "telemetry://live/db_stats",
				Kind:        mission.EvidenceKindLive,
				Description: "Live DB statistics",
				CapturedAt:  time.Now().UTC(),
			},
		},
		Assumptions:        []string{"Migration scripts are idempotent"},
		ExpectedOutcome:    "Production schema updated with new columns",
		SuccessCriteria:    []string{"Schema migration returns exit code 0"},
		ResourceEnvelope:   mission.ResourceEnvelope{MaxCostUSD: 5.0, MaxTokens: 50000, MaxDuration: time.Hour},
		Reversibility:      mission.ReversibilityPartial,
		RiskClassification: mission.RiskHigh,
		ReferencedMissionElements: []string{
			"Sub-100ms p99 transaction confirmation latency",
		},
		AuthorityCeiling: mission.AuthorityCeiling{
			RequiresHumanApproval: false, // VIOLATION: touches human trust boundary but claims false!
			TouchedTrustBoundaries: []string{
				"Production database schema drops or migrations require root human sign-off",
			},
		},
	}

	err := mission.ValidateGoalProposal(proposal, m)
	if err != mission.ErrHumanTrustBoundaryBypassed {
		t.Errorf("expected ErrHumanTrustBoundaryBypassed, got %v", err)
	}
}

func TestGoalProposal_ValidationRejection_ExpandingAuthorityCeiling(t *testing.T) {
	m := sampleActiveMission()

	// Mission constraint is: "Maximum single-operation compute cost $10.00 USD"
	// Proposal requests $500.00 USD compute budget
	proposal := mission.GoalProposal{
		ID:               domain.NewID("goalprop"),
		MissionID:        m.ID,
		MissionVersion:   m.Version,
		Title:            "Massive Re-Indexing Compute Job",
		ProblemStatement: "Index generation requires large distributed cluster run.",
		SupportingEvidence: []mission.SupportingEvidenceItem{
			{
				URI:         "live://evidence",
				Kind:        mission.EvidenceKindLive,
				Description: "Live evidence",
				CapturedAt:  time.Now().UTC(),
			},
		},
		Assumptions:     []string{"Storage available"},
		ExpectedOutcome: "Re-indexed cluster",
		SuccessCriteria: []string{"Index operational"},
		ResourceEnvelope: mission.ResourceEnvelope{
			MaxCostUSD:  500.0, // VIOLATION: exceeds $10.00 constraint
			MaxTokens:   1000000,
			MaxDuration: time.Hour,
		},
		Reversibility:      mission.ReversibilityFull,
		RiskClassification: mission.RiskMedium,
		ReferencedMissionElements: []string{
			"Sub-100ms p99 transaction confirmation latency",
		},
		AuthorityCeiling: mission.AuthorityCeiling{
			RequiresHumanApproval: true,
		},
	}

	err := mission.ValidateGoalProposal(proposal, m)
	if err != mission.ErrAuthorityExpansion {
		t.Errorf("expected ErrAuthorityExpansion, got %v", err)
	}
}

func TestGoalProposal_FieldCompleteness(t *testing.T) {
	m := sampleActiveMission()

	// Missing problem statement
	p := mission.GoalProposal{
		ID:             domain.NewID("goalprop"),
		MissionID:      m.ID,
		MissionVersion: m.Version,
		Title:          "Incomplete Proposal",
	}

	err := mission.ValidateGoalProposal(p, m)
	if err != mission.ErrIncompleteGoalProposal {
		t.Errorf("expected ErrIncompleteGoalProposal, got %v", err)
	}
}

func TestGoalProposal_InactiveMissionRejection(t *testing.T) {
	m := sampleActiveMission()
	m.State = mission.MissionStateDraft // Inactive!

	p := mission.GoalProposal{
		ID:             domain.NewID("goalprop"),
		MissionID:      m.ID,
		MissionVersion: m.Version,
		Title:          "Goal on Draft Mission",
	}

	err := mission.ValidateGoalProposal(p, m)
	if err != mission.ErrInactiveMission {
		t.Errorf("expected ErrInactiveMission, got %v", err)
	}
}
