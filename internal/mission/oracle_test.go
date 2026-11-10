package mission_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/mission"
)

func TestMissionOracle_MultiDimensionalCategoricalScoring(t *testing.T) {
	ctx := context.Background()
	m := sampleActiveMission()
	oracle := mission.NewMissionOracle()

	goal := mission.GoalProposal{
		ID:               domain.NewID("goalprop"),
		MissionID:        m.ID,
		MissionVersion:   m.Version,
		Title:            "Implement Read-Optimized Covering Index",
		ProblemStatement: "Slow unindexed queries degrade p99 latency to 185ms.",
		SupportingEvidence: []mission.SupportingEvidenceItem{
			{
				URI:         "telemetry://live/p99_latency",
				Kind:        mission.EvidenceKindLive,
				Description: "Live metric p99 latency",
				CapturedAt:  time.Now().UTC(),
			},
		},
		Assumptions:        []string{"Storage disk has 20GB free space for indexes"},
		ExpectedOutcome:    "p99 latency drops below 80ms satisfying mission SLA",
		SuccessCriteria:    []string{"p99 query latency < 100ms on benchmark suite"},
		ResourceEnvelope:   mission.ResourceEnvelope{MaxCostUSD: 5.0, MaxTokens: 10000, MaxDuration: time.Hour},
		Reversibility:      mission.ReversibilityFull,
		RiskClassification: mission.RiskLow,
		ReferencedMissionElements: []string{
			"Sub-100ms p99 transaction confirmation latency",
		},
		AuthorityCeiling: mission.AuthorityCeiling{
			RequiresHumanApproval: false,
		},
		Status: mission.GoalProposalStatusProposed,
	}

	eval, err := oracle.Evaluate(ctx, m, goal)
	if err != nil {
		t.Fatalf("unexpected evaluation error: %v", err)
	}

	// 1. Categorical scoring: NO collapsed scalar score!
	if eval.DesiredOutcomes.Status != mission.DimensionStatusSatisfied {
		t.Errorf("expected DesiredOutcomes SATISFIED, got %s", eval.DesiredOutcomes.Status)
	}
	if eval.Obligations.Status != mission.DimensionStatusSatisfied {
		t.Errorf("expected Obligations SATISFIED, got %s", eval.Obligations.Status)
	}
	if eval.Constraints.Status != mission.DimensionStatusSatisfied {
		t.Errorf("expected Constraints SATISFIED, got %s", eval.Constraints.Status)
	}
	if eval.NonGoals.Status != mission.DimensionStatusSatisfied {
		t.Errorf("expected NonGoals SATISFIED, got %s", eval.NonGoals.Status)
	}
	if eval.TradeOffCompliance.Status != mission.DimensionStatusSatisfied {
		t.Errorf("expected TradeOffCompliance SATISFIED, got %s", eval.TradeOffCompliance.Status)
	}
	if eval.EvidenceQuality.Status != mission.DimensionStatusSatisfied {
		t.Errorf("expected EvidenceQuality SATISFIED, got %s", eval.EvidenceQuality.Status)
	}
	if eval.Reversibility.Status != mission.DimensionStatusSatisfied {
		t.Errorf("expected Reversibility SATISFIED, got %s", eval.Reversibility.Status)
	}

	// Clean proposal should be accepted
	if eval.Decision != mission.EvaluationDecisionAccept {
		t.Errorf("expected Decision ACCEPT, got %s (reasons: %v)", eval.Decision, eval.RejectionReasons)
	}
}

func TestMissionOracle_NonGoalViolation_StrictRejection(t *testing.T) {
	ctx := context.Background()
	m := sampleActiveMission() // NonGoals include "Unfenced manual database modifications"
	oracle := mission.NewMissionOracle()

	// High positive outcome, but violates NonGoal
	goal := mission.GoalProposal{
		ID:               domain.NewID("goalprop"),
		MissionID:        m.ID,
		MissionVersion:   m.Version,
		Title:            "Direct Database Surgery for Performance",
		ProblemStatement: "Need immediate index and column adjustments without standard migration scripts.",
		SupportingEvidence: []mission.SupportingEvidenceItem{
			{
				URI:         "telemetry://live/perf",
				Kind:        mission.EvidenceKindLive,
				Description: "Slow queries",
				CapturedAt:  time.Now().UTC(),
			},
		},
		Assumptions:        []string{"DB connection available"},
		ExpectedOutcome:    "Instant 10x query speedup",
		SuccessCriteria:    []string{"p99 latency < 20ms"},
		ResourceEnvelope:   mission.ResourceEnvelope{MaxCostUSD: 1.0, MaxTokens: 1000, MaxDuration: time.Minute},
		Reversibility:      mission.ReversibilityIrreversible,
		RiskClassification: mission.RiskCritical,
		ReferencedMissionElements: []string{
			"Sub-100ms p99 transaction confirmation latency",
		},
		AuthorityCeiling: mission.AuthorityCeiling{
			RequiresHumanApproval: false,
		},
		Status: mission.GoalProposalStatusProposed,
	}

	eval, err := oracle.Evaluate(ctx, m, goal)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Must be rejected regardless of high outcome metric
	if eval.Decision != mission.EvaluationDecisionReject {
		t.Errorf("expected Decision REJECT for NonGoal violation, got %s", eval.Decision)
	}
	if eval.NonGoals.Status != mission.DimensionStatusViolated {
		t.Errorf("expected NonGoals dimension to be VIOLATED, got %s", eval.NonGoals.Status)
	}
	if len(eval.RejectionReasons) == 0 {
		t.Error("expected explicit rejection reasons")
	}
}

func TestMissionOracle_ObligationViolation_StrictRejection(t *testing.T) {
	ctx := context.Background()
	m := sampleActiveMission() // Obligations include "Maintain immutable append-only audit logs"
	oracle := mission.NewMissionOracle()

	// Goal disables audit logging to improve throughput
	goal := mission.GoalProposal{
		ID:               domain.NewID("goalprop"),
		MissionID:        m.ID,
		MissionVersion:   m.Version,
		Title:            "Bypass Audit Logging for Fast Writes",
		ProblemStatement: "Audit log append locks throttle transaction throughput.",
		SupportingEvidence: []mission.SupportingEvidenceItem{
			{
				URI:         "telemetry://live/io_bottleneck",
				Kind:        mission.EvidenceKindLive,
				Description: "IO bottleneck",
				CapturedAt:  time.Now().UTC(),
			},
		},
		Assumptions:        []string{"Audit logs can be discarded"},
		ExpectedOutcome:    "Throughput quadruples",
		SuccessCriteria:    []string{"Throughput > 10,000 tx/sec"},
		ResourceEnvelope:   mission.ResourceEnvelope{MaxCostUSD: 2.0, MaxTokens: 5000, MaxDuration: time.Hour},
		Reversibility:      mission.ReversibilityIrreversible,
		RiskClassification: mission.RiskCritical,
		ReferencedMissionElements: []string{
			"Sub-100ms p99 transaction confirmation latency",
		},
		AuthorityCeiling: mission.AuthorityCeiling{
			RequiresHumanApproval: false,
		},
		Status: mission.GoalProposalStatusProposed,
	}

	eval, err := oracle.Evaluate(ctx, m, goal)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Must be rejected
	if eval.Decision != mission.EvaluationDecisionReject {
		t.Errorf("expected Decision REJECT for obligation breach, got %s", eval.Decision)
	}
	if eval.Obligations.Status != mission.DimensionStatusViolated {
		t.Errorf("expected Obligations dimension to be VIOLATED, got %s", eval.Obligations.Status)
	}
}

func TestMissionOracle_HoldoutScenario_ProxyGamingRejection(t *testing.T) {
	ctx := context.Background()
	m := sampleActiveMission()
	oracle := mission.NewMissionOracle()

	// Proposal that games "100% success rate" by refusing/rejecting unverified transactions
	goal := mission.GoalProposal{
		ID:               domain.NewID("goalprop"),
		MissionID:        m.ID,
		MissionVersion:   m.Version,
		Title:            "Enforce Zero Errors via Selective Traffic Refusal",
		ProblemStatement: "Edge case transactions cause occasional errors.",
		SupportingEvidence: []mission.SupportingEvidenceItem{
			{
				URI:         "telemetry://live/error_log",
				Kind:        mission.EvidenceKindLive,
				Description: "Error logs",
				CapturedAt:  time.Now().UTC(),
			},
		},
		Assumptions:        []string{"Users will retry later"},
		ExpectedOutcome:    "Zero error metric achieved by returning 503 on high-risk traffic",
		SuccessCriteria:    []string{"0 errors recorded"},
		ResourceEnvelope:   mission.ResourceEnvelope{MaxCostUSD: 1.0, MaxTokens: 5000, MaxDuration: time.Hour},
		Reversibility:      mission.ReversibilityFull,
		RiskClassification: mission.RiskHigh,
		ReferencedMissionElements: []string{
			"Zero double-spending and deterministic reconciliation",
		},
		AuthorityCeiling: mission.AuthorityCeiling{
			RequiresHumanApproval: false,
		},
		Status: mission.GoalProposalStatusProposed,
	}

	// Run evaluation including built-in holdout scenarios
	eval, err := oracle.Evaluate(ctx, m, goal, mission.HoldoutTrafficRejectionGaming())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if eval.Decision != mission.EvaluationDecisionReject {
		t.Errorf("expected Decision REJECT from holdout scenario, got %s", eval.Decision)
	}

	var foundHoldoutFailure bool
	for _, res := range eval.HoldoutResults {
		if !res.Passed && strings.Contains(strings.ToLower(res.HoldoutName), "rejection") {
			foundHoldoutFailure = true
		}
	}
	if !foundHoldoutFailure {
		t.Errorf("expected holdout failure result for traffic rejection gaming, got: %+v", eval.HoldoutResults)
	}
}

func TestMissionOracle_HumanTrustBoundary_Escalation(t *testing.T) {
	ctx := context.Background()
	m := sampleActiveMission() // Trust boundaries include schema migrations and >$1000 transfers
	oracle := mission.NewMissionOracle()

	goal := mission.GoalProposal{
		ID:               domain.NewID("goalprop"),
		MissionID:        m.ID,
		MissionVersion:   m.Version,
		Title:            "Production Database Schema Upgrade for Partitioning",
		ProblemStatement: "Large table volume requires partition schema migration.",
		SupportingEvidence: []mission.SupportingEvidenceItem{
			{
				URI:         "telemetry://live/table_size",
				Kind:        mission.EvidenceKindLive,
				Description: "Table size metric",
				CapturedAt:  time.Now().UTC(),
			},
		},
		Assumptions:        []string{"Migration scripts verified on staging"},
		ExpectedOutcome:    "Database schema migrated cleanly",
		SuccessCriteria:    []string{"Partitions active without downtime"},
		ResourceEnvelope:   mission.ResourceEnvelope{MaxCostUSD: 8.0, MaxTokens: 20000, MaxDuration: 2 * time.Hour},
		Reversibility:      mission.ReversibilityPartial,
		RiskClassification: mission.RiskHigh,
		ReferencedMissionElements: []string{
			"Sub-100ms p99 transaction confirmation latency",
		},
		AuthorityCeiling: mission.AuthorityCeiling{
			RequiresHumanApproval: true,
			TouchedTrustBoundaries: []string{
				"Production database schema drops or migrations require root human sign-off",
			},
		},
		Status: mission.GoalProposalStatusProposed,
	}

	eval, err := oracle.Evaluate(ctx, m, goal)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if eval.Decision != mission.EvaluationDecisionEscalateToHuman {
		t.Errorf("expected Decision ESCALATE_TO_HUMAN, got %s", eval.Decision)
	}
}

func TestMissionOracle_QuerySurface(t *testing.T) {
	ctx := context.Background()
	m := sampleActiveMission()
	oracle := mission.NewMissionOracle()

	// 1. Constraining Obligations query
	obligations, err := oracle.QueryConstrainingObligations(ctx, m, []string{"audit", "pci"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(obligations) != 2 {
		t.Errorf("expected 2 obligations, got %d", len(obligations))
	}

	// 2. Applicable Trade-offs query
	tradeOffs, err := oracle.QueryApplicableTradeOffs(ctx, m, [2]string{"audit", "latency"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tradeOffs) == 0 {
		t.Error("expected matching trade-off rule for audit vs latency")
	}

	// 3. Required Human Judgments query
	judgments, err := oracle.QueryRequiredHumanJudgments(ctx, m, []string{
		"Perform database schema drop",
		"Transfer $5,000 to external provider",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(judgments) < 2 {
		t.Errorf("expected at least 2 human judgment gates, got %d", len(judgments))
	}
}
