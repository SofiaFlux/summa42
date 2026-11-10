package epistemic_test

import (
	"context"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/epistemic"
)

func TestPostAttemptReflection_NoMaterialLearningWhenNominal(t *testing.T) {
	ctx := context.Background()
	reflector := epistemic.NewPostAttemptReflector()

	input := epistemic.ReflectionInput{
		AttemptID:             domain.NewID("attempt"),
		SummandID:             "worker-summand-42",
		Expected:              "Unit tests pass on first try",
		Observed:              "Unit tests pass on first try",
		Surprise:              "",
		PlanDeviation:         "",
		Adaptation:            "",
		Outcome:               "SUCCESS",
		CandidateLesson:       "",
		SupportingEvidenceIDs: []string{"ev-test-pass"},
	}

	delta, err := reflector.Reflect(ctx, input)
	if err != nil {
		t.Fatalf("unexpected error during nominal reflection: %v", err)
	}

	if !delta.IsNoMaterialLearning {
		t.Error("expected IsNoMaterialLearning=true when execution was completely nominal without surprises")
	}
	if len(delta.Observations) != 0 || len(delta.ProposedClaims) != 0 {
		t.Errorf("expected empty observations and claims on NO_MATERIAL_LEARNING, got %d obs, %d claims",
			len(delta.Observations), len(delta.ProposedClaims))
	}
}

func TestPostAttemptReflection_ExtractsCandidateKnowledgeOnSurprise(t *testing.T) {
	ctx := context.Background()
	reflector := epistemic.NewPostAttemptReflector()

	attemptID := domain.NewID("attempt")
	input := epistemic.ReflectionInput{
		AttemptID:       attemptID,
		SummandID:       "worker-summand-42",
		Expected:        "Migration completes within 200ms",
		Observed:        "Migration timed out after 500ms due to unindexed foreign key lock contention",
		Surprise:        "Lock contention occurred on legacy table under concurrent read traffic",
		PlanDeviation:   "Switched to chunked batch migration with 50ms pauses",
		Adaptation:      "Added chunked iterator and pre-warmed covering index",
		Outcome:         "RECOVERED",
		CandidateLesson: "High-concurrency migrations require chunked iterations to prevent table lock starvation",
		SupportingEvidenceIDs: []string{
			"ev-lock-dump-101",
			"ev-recovery-trace-102",
		},
	}

	delta, err := reflector.Reflect(ctx, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if delta.IsNoMaterialLearning {
		t.Fatal("expected material learning when surprise and adaptation occurred")
	}

	if len(delta.Observations) == 0 {
		t.Fatal("expected at least one Observation in delta")
	}
	obs := delta.Observations[0]
	if obs.Subject == "" || obs.Predicate == "" || obs.Value == "" {
		t.Errorf("observation fields incomplete: %+v", obs)
	}
	if len(obs.EvidenceIDs) != 2 {
		t.Errorf("expected 2 supporting evidence IDs, got %d", len(obs.EvidenceIDs))
	}

	if len(delta.ProposedClaims) == 0 {
		t.Fatal("expected at least one proposed Claim in delta")
	}
	claim := delta.ProposedClaims[0]
	if claim.Status != epistemic.ClaimStatusProposed {
		t.Errorf("expected candidate claim status PROPOSED, got %s", claim.Status)
	}
	if len(claim.SupportingObservations) == 0 {
		t.Errorf("expected claim to reference supporting observations")
	}
}

func TestPostAttemptReflection_NonFatalOnMalformedInput(t *testing.T) {
	ctx := context.Background()
	reflector := epistemic.NewPostAttemptReflector()

	// Empty attempt ID and blank fields
	input := epistemic.ReflectionInput{}

	delta, err := reflector.Reflect(ctx, input)
	if err != nil {
		t.Fatalf("reflection protocol must complete cleanly without returning a fatal error: %v", err)
	}

	if !delta.IsNoMaterialLearning {
		t.Error("expected fallback to NO_MATERIAL_LEARNING on malformed reflection input")
	}
}

func TestPostAttemptReflection_ZeroDirectMutationAuthority(t *testing.T) {
	ctx := context.Background()
	engine := epistemic.NewEngine()
	reflector := epistemic.NewPostAttemptReflector()

	attemptID := domain.NewID("attempt")
	input := epistemic.ReflectionInput{
		AttemptID:             attemptID,
		SummandID:             "unprivileged-summand",
		Expected:              "Normal run",
		Observed:              "Surprise anomaly",
		Surprise:              "Unpredicted deadlock",
		CandidateLesson:       "Deadlock avoided with sorted resource acquisition",
		SupportingEvidenceIDs: []string{"ev-trace"},
	}

	delta, err := reflector.Reflect(ctx, input)
	if err != nil {
		t.Fatalf("reflect: %v", err)
	}

	// 1. Reflector itself does NOT write to engine
	_, err = engine.GetClaim(ctx, delta.ProposedClaims[0].ID)
	if err != epistemic.ErrClaimNotFound {
		t.Errorf("expected claim to NOT exist in engine prior to explicit collective intake, got %v", err)
	}

	// 2. Only an authorized process calling engine.RecordDelta integrates it
	if err := engine.RecordDelta(ctx, delta); err != nil {
		t.Fatalf("engine record delta: %v", err)
	}

	claim, err := engine.GetClaim(ctx, delta.ProposedClaims[0].ID)
	if err != nil {
		t.Fatalf("expected claim to exist after explicit engine record: %v", err)
	}
	if claim.Status != epistemic.ClaimStatusProposed {
		t.Errorf("expected proposed status, got %s", claim.Status)
	}
}

func TestPostAttemptReflection_DeterministicDeltaHashing(t *testing.T) {
	ctx := context.Background()
	reflector := epistemic.NewPostAttemptReflector()

	fixedTime := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	attemptID := domain.ID("attempt_fixed_id")

	input := epistemic.ReflectionInput{
		AttemptID:             attemptID,
		SummandID:             "summand-1",
		Expected:              "fast run",
		Observed:              "slow run",
		Surprise:              "network jitter",
		CandidateLesson:       "add retry backoff",
		SupportingEvidenceIDs: []string{"ev-1"},
		Timestamp:             fixedTime,
	}

	d1, err := reflector.Reflect(ctx, input)
	if err != nil {
		t.Fatalf("reflect 1: %v", err)
	}
	d2, err := reflector.Reflect(ctx, input)
	if err != nil {
		t.Fatalf("reflect 2: %v", err)
	}

	if d1.DeltaHash == "" || d2.DeltaHash == "" {
		t.Fatal("expected non-empty DeltaHash")
	}
	if d1.DeltaHash != d2.DeltaHash {
		t.Errorf("expected deterministic DeltaHash, got %s vs %s", d1.DeltaHash, d2.DeltaHash)
	}
}
