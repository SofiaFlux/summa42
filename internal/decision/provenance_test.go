package decision_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/decision"
	"github.com/SofiaFlux/summa42/internal/domain"
	_ "modernc.org/sqlite"
)

func sampleDecisionResult(decisionID domain.ID) decision.DecisionResult {
	return decision.DecisionResult{
		DecisionID:      decisionID,
		SelectedOutcome: "FAST_PATH",
		OutcomeScores: map[string]float64{
			"FAST_PATH":     0.90,
			"STANDARD_PATH": 0.10,
		},
		Confidence:     0.90,
		ConfidenceKind: decision.ConfidenceCalibratedProbability,
		Abstained:      false,
		Provider: decision.ProviderInfo{
			Kind:    decision.ProviderKindRule,
			Name:    "fast-rule-provider",
			Version: "1.0",
		},
		Latency: 5 * time.Millisecond,
		Usage: decision.DecisionUsage{
			TokensIn:       0,
			TokensOut:      0,
			CostMicroCents: 10,
		},
		ProjectionHash: "canon_hash_123",
		DecidedAt:      time.Now().UTC(),
	}
}

func TestInMemoryProvenanceStore_RecordAndOutcomeLinkage(t *testing.T) {
	ctx := context.Background()
	store := decision.NewInMemoryProvenanceStore()

	req := baseTestRequest()
	res := sampleDecisionResult(req.DecisionID)

	// 1. Record decision
	err := store.RecordDecision(ctx, req, res)
	if err != nil {
		t.Fatalf("failed to record decision: %v", err)
	}

	// 2. Query record
	rec, err := store.Get(ctx, req.DecisionID)
	if err != nil {
		t.Fatalf("failed to get decision record: %v", err)
	}
	if rec.DecisionID != req.DecisionID {
		t.Errorf("expected DecisionID %s, got %s", req.DecisionID, rec.DecisionID)
	}
	if rec.SelectedOutcome != "FAST_PATH" {
		t.Errorf("expected FAST_PATH, got %s", rec.SelectedOutcome)
	}
	if rec.OutcomeLinkedAt != nil {
		t.Errorf("expected OutcomeLinkedAt to be nil before linking")
	}

	// 3. Link downstream observable outcome
	err = store.RecordOutcome(ctx, req.DecisionID, "FAST_PATH", 1500, true)
	if err != nil {
		t.Fatalf("failed to link outcome: %v", err)
	}

	// 4. Verify linked outcome data
	recAfter, err := store.Get(ctx, req.DecisionID)
	if err != nil {
		t.Fatalf("failed to get decision record after linking: %v", err)
	}
	if recAfter.ActualOutcome != "FAST_PATH" {
		t.Errorf("expected actual outcome FAST_PATH, got %s", recAfter.ActualOutcome)
	}
	if recAfter.DownstreamCost != 1500 {
		t.Errorf("expected downstream cost 1500, got %d", recAfter.DownstreamCost)
	}
	if recAfter.Success == nil || !*recAfter.Success {
		t.Errorf("expected success true, got %v", recAfter.Success)
	}
	if recAfter.OutcomeLinkedAt == nil {
		t.Errorf("expected non-nil OutcomeLinkedAt")
	}
}

func TestSQLiteProvenanceStore_PersistenceAndQuery(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite memory db: %v", err)
	}
	defer db.Close()

	store := decision.NewSQLiteProvenanceStore(db)
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("failed to initialize schema: %v", err)
	}

	req := baseTestRequest()
	res := sampleDecisionResult(req.DecisionID)

	// Record
	if err := store.RecordDecision(ctx, req, res); err != nil {
		t.Fatalf("failed to record decision: %v", err)
	}

	// Fetch
	rec, err := store.Get(ctx, req.DecisionID)
	if err != nil {
		t.Fatalf("failed to get recorded decision: %v", err)
	}
	if rec.DecisionID != req.DecisionID {
		t.Errorf("mismatched decision ID: got %s, want %s", rec.DecisionID, req.DecisionID)
	}
	if rec.SelectedOutcome != "FAST_PATH" {
		t.Errorf("mismatched outcome: got %s, want FAST_PATH", rec.SelectedOutcome)
	}

	// Link outcome
	if err := store.RecordOutcome(ctx, req.DecisionID, "FAST_PATH", 250, true); err != nil {
		t.Fatalf("failed to record outcome: %v", err)
	}

	recLinked, err := store.Get(ctx, req.DecisionID)
	if err != nil {
		t.Fatalf("failed to get linked decision: %v", err)
	}
	if recLinked.ActualOutcome != "FAST_PATH" || recLinked.DownstreamCost != 250 || recLinked.Success == nil || !*recLinked.Success {
		t.Errorf("unexpected linked outcome: %+v", recLinked)
	}

	// Filter listing
	list, err := store.List(ctx, decision.ProvenanceFilter{
		DecisionType: decision.DecisionTypeRouting,
	})
	if err != nil {
		t.Fatalf("failed to list records: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 record, got %d", len(list))
	}
}

func TestDecisionEngine_ProvenanceRecordingAndTelemetry(t *testing.T) {
	ctx := context.Background()
	store := decision.NewInMemoryProvenanceStore()
	emitter := decision.NewRecordingTelemetryEmitter()

	engine := decision.NewEngine()
	engine.SetProvenanceStore(store)
	engine.SetTelemetryEmitter(emitter)

	ruleProvider := decision.NewRuleDecisionProvider("rule-engine")
	ruleProvider.AddRule(decision.NewExactMatchRule("r1", "complexity", "low", "FAST_PATH", "low complexity"))
	engine.RegisterProvider(decision.DecisionTypeRouting, ruleProvider)

	req := baseTestRequest()
	res, err := engine.Decide(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.SelectedOutcome != "FAST_PATH" {
		t.Errorf("expected FAST_PATH, got %s", res.SelectedOutcome)
	}

	// Verify store captured the decision
	rec, err := store.Get(ctx, req.DecisionID)
	if err != nil {
		t.Fatalf("decision was not recorded in store: %v", err)
	}
	if rec.SelectedOutcome != "FAST_PATH" {
		t.Errorf("expected stored outcome FAST_PATH, got %s", rec.SelectedOutcome)
	}

	// Verify telemetry emitter received event
	events := emitter.Events()
	if len(events) != 1 {
		t.Fatalf("expected 1 telemetry event, got %d", len(events))
	}
	if events[0].DecisionID != req.DecisionID {
		t.Errorf("telemetry event decision ID mismatch: %s vs %s", events[0].DecisionID, req.DecisionID)
	}
}

func TestReplayHarness_AlternativeProviderComparison(t *testing.T) {
	ctx := context.Background()

	// Prepare historical fixtures
	fixtures := []decision.ReplayFixture{
		{
			FixtureID:   "fix-01",
			Description: "Simple safe task",
			Request: decision.DecisionRequest{
				DecisionID:      domain.NewID("dec"),
				DecisionType:    decision.DecisionTypeRouting,
				AllowedOutcomes: []string{"FAST_PATH", "STANDARD_PATH"},
				ContextProjection: map[string]any{
					"complexity": "low",
				},
				RiskProfile: decision.RiskProfileLow,
			},
			RecordedResult: decision.DecisionResult{
				SelectedOutcome: "STANDARD_PATH", // Original provider was cautious
			},
			ActualOutcome: "FAST_PATH", // In reality, fast path was sufficient
			Success:       true,
		},
		{
			FixtureID:   "fix-02",
			Description: "High complexity task",
			Request: decision.DecisionRequest{
				DecisionID:      domain.NewID("dec"),
				DecisionType:    decision.DecisionTypeRouting,
				AllowedOutcomes: []string{"FAST_PATH", "STANDARD_PATH"},
				ContextProjection: map[string]any{
					"complexity": "high",
				},
				RiskProfile: decision.RiskProfileLow,
			},
			RecordedResult: decision.DecisionResult{
				SelectedOutcome: "STANDARD_PATH",
			},
			ActualOutcome: "STANDARD_PATH",
			Success:       true,
		},
	}

	// Challenger provider: Rule provider that picks FAST_PATH when complexity == "low"
	challenger := decision.NewRuleDecisionProvider("challenger-rules")
	challenger.AddRule(decision.NewExactMatchRule("r-low", "complexity", "low", "FAST_PATH", "fast route"))
	challenger.AddRule(decision.NewExactMatchRule("r-high", "complexity", "high", "STANDARD_PATH", "standard route"))

	harness := decision.NewReplayHarness()
	report, err := harness.Run(ctx, fixtures, challenger)
	if err != nil {
		t.Fatalf("replay harness failed: %v", err)
	}

	if report.TotalFixtures != 2 {
		t.Errorf("expected 2 fixtures, got %d", report.TotalFixtures)
	}
	// On fixture 1: original was STANDARD_PATH, challenger was FAST_PATH (disagreed)
	// On fixture 2: both were STANDARD_PATH (agreed)
	if report.AgreementCount != 1 {
		t.Errorf("expected 1 agreement, got %d", report.AgreementCount)
	}
	if report.AgreementRate != 0.5 {
		t.Errorf("expected agreement rate 0.5, got %f", report.AgreementRate)
	}
	// Challenger got both correct against ActualOutcome (FAST_PATH and STANDARD_PATH)
	if report.ChallengerAccuracy != 1.0 {
		t.Errorf("expected challenger accuracy 1.0, got %f", report.ChallengerAccuracy)
	}
	// Original got fixture 2 correct, fixture 1 suboptimal
	if report.OriginalAccuracy != 0.5 {
		t.Errorf("expected original accuracy 0.5, got %f", report.OriginalAccuracy)
	}
}
