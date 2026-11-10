package epistemic_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/epistemic"
)

func TestConsolidation_SleepDeduplicationAndExperienceSynthesis(t *testing.T) {
	ctx := context.Background()
	runner := epistemic.NewSleepDreamRunner()

	attempt1 := domain.NewID("attempt1")
	attempt2 := domain.NewID("attempt2")
	now := time.Now().UTC()

	src1 := epistemic.SourceRef{
		ID:                "src-1",
		Kind:              epistemic.SourceKindAttempt,
		URI:               "attempt://" + string(attempt1),
		AuthorID:          "worker-a",
		CapturedAt:        now,
		IndependenceGroup: "worker-env-1",
	}

	src2 := epistemic.SourceRef{
		ID:                "src-2",
		Kind:              epistemic.SourceKindAttempt,
		URI:               "attempt://" + string(attempt2),
		AuthorID:          "worker-b",
		CapturedAt:        now,
		IndependenceGroup: "worker-env-2",
	}

	// Two attempts encounter the identical lock contention friction
	obs1 := epistemic.NewObservation(src1, "database.locks", "lock_wait_timeout", "Row lock wait timeout after 5000ms", []string{"ev-lock-1"}, now)
	obs2 := epistemic.NewObservation(src2, "database.locks", "lock_wait_timeout", "Row lock wait timeout after 5000ms", []string{"ev-lock-2"}, now)

	delta1 := epistemic.KnowledgeDelta{
		ID:              domain.NewID("kdelta"),
		SourceAttemptID: attempt1,
		Observations:    []epistemic.Observation{obs1},
		ProposedClaims: []epistemic.Claim{
			epistemic.NewClaim("database.locks", "mitigation", "use_optimistic_locking", 0.85, now, []domain.ID{obs1.ID}, []string{"ev-lock-1"}, []epistemic.SourceRef{src1}),
		},
		CreatedAt: now,
	}

	delta2 := epistemic.KnowledgeDelta{
		ID:              domain.NewID("kdelta"),
		SourceAttemptID: attempt2,
		Observations:    []epistemic.Observation{obs2},
		ProposedClaims: []epistemic.Claim{
			epistemic.NewClaim("database.locks", "mitigation", "use_optimistic_locking", 0.90, now, []domain.ID{obs2.ID}, []string{"ev-lock-2"}, []epistemic.SourceRef{src2}),
		},
		CreatedAt: now,
	}

	budget := epistemic.CognitiveBudget{
		MaxDeltas:   10,
		MaxDuration: 5 * time.Second,
	}

	run, err := runner.RunSleep(ctx, []epistemic.KnowledgeDelta{delta1, delta2}, budget, epistemic.TriggerSurpriseAccumulation)
	if err != nil {
		t.Fatalf("unexpected error running sleep consolidation: %v", err)
	}

	if run.Mode != epistemic.ConsolidationModeSleep {
		t.Errorf("expected mode SLEEP, got %s", run.Mode)
	}
	if run.DeltasProcessed != 2 {
		t.Errorf("expected 2 deltas processed, got %d", run.DeltasProcessed)
	}
	if len(run.ExperiencesSynthesized) == 0 {
		t.Fatal("expected at least one synthesized experience across the 2 recurring attempts")
	}

	exp := run.ExperiencesSynthesized[0]
	if exp.CorroborationCount != 2 {
		t.Errorf("expected corroboration count 2, got %d", exp.CorroborationCount)
	}
	if len(exp.SourceAttemptIDs) != 2 {
		t.Errorf("expected 2 source attempt IDs linked, got %d", len(exp.SourceAttemptIDs))
	}
	if !strings.Contains(strings.ToLower(exp.Pattern), "lock") {
		t.Errorf("expected experience pattern to reference lock: %s", exp.Pattern)
	}
}

func TestConsolidation_DreamingProducesNonAuthoritativeHypotheses(t *testing.T) {
	ctx := context.Background()
	runner := epistemic.NewSleepDreamRunner()

	exp := epistemic.SynthesizedExperience{
		ID:                   domain.NewID("synexp"),
		Pattern:              "Recurring lock wait timeout on ledger update",
		RootCause:            "Contention on hot account rows under high batch concurrency",
		RecommendedProcedure: "Batch writes by destination account with deterministic ordering",
		SourceAttemptIDs:     []domain.ID{domain.NewID("att1"), domain.NewID("att2")},
		EvidenceIDs:          []string{"ev-1", "ev-2"},
		CorroborationCount:   2,
	}

	budget := epistemic.CognitiveBudget{
		MaxHypotheses: 5,
		MaxDuration:   2 * time.Second,
	}

	run, err := runner.RunDream(ctx, []epistemic.SynthesizedExperience{exp}, budget, epistemic.TriggerIdleCapacity)
	if err != nil {
		t.Fatalf("unexpected dreaming error: %v", err)
	}

	if run.Mode != epistemic.ConsolidationModeDream {
		t.Errorf("expected mode DREAM, got %s", run.Mode)
	}
	if len(run.HypothesesGenerated) == 0 {
		t.Fatal("expected at least one generated hypothesis")
	}

	hyp := run.HypothesesGenerated[0]

	// HARD INVARIANT: Dream outputs must NEVER be marked as canonical truth!
	if hyp.IsCanonical {
		t.Errorf("VIOLATION OF HARD INVARIANT: Dream output hypothesis must not be canonical")
	}
	if strings.TrimSpace(hyp.Statement) == "" {
		t.Error("expected non-empty hypothesis statement")
	}
	if strings.TrimSpace(hyp.CounterfactualScenario) == "" {
		t.Error("expected counterfactual scenario in hypothesis")
	}

	// Must generate an experiment proposal to empirically test the hypothesis
	if len(run.ExperimentProposals) == 0 {
		t.Fatal("expected experiment proposal paired with hypothesis")
	}
	prop := run.ExperimentProposals[0]
	if prop.HypothesisID != hyp.ID {
		t.Errorf("expected proposal to reference hypothesis ID %s, got %s", hyp.ID, prop.HypothesisID)
	}
	if strings.TrimSpace(prop.SafetyBounds) == "" {
		t.Error("expected safety bounds on experiment proposal")
	}
}

func TestConsolidation_CognitiveBudgetEnforcement(t *testing.T) {
	ctx := context.Background()
	runner := epistemic.NewSleepDreamRunner()

	// Provide 10 deltas but budget only allows 2
	var deltas []epistemic.KnowledgeDelta
	now := time.Now().UTC()
	for i := 0; i < 10; i++ {
		attID := domain.NewID("att")
		deltas = append(deltas, epistemic.KnowledgeDelta{
			ID:              domain.NewID("kdelta"),
			SourceAttemptID: attID,
			CreatedAt:       now,
		})
	}

	budget := epistemic.CognitiveBudget{
		MaxDeltas:   2,
		MaxDuration: 1 * time.Second,
	}

	run, err := runner.RunSleep(ctx, deltas, budget, epistemic.TriggerExplicitBudget)
	if err != nil {
		t.Fatalf("sleep run error: %v", err)
	}

	if run.DeltasProcessed > 2 {
		t.Errorf("expected at most 2 deltas processed per budget limit, got %d", run.DeltasProcessed)
	}
}
