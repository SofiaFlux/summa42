package epistemic_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/epistemic"
)

func TestEpistemic_CryptographicContentHashing(t *testing.T) {
	now := time.Now().UTC()
	source := epistemic.SourceRef{
		ID:                "src-01",
		Kind:              epistemic.SourceKindAttempt,
		URI:               "attempt://attempt-42",
		AuthorID:          "worker-01",
		CapturedAt:        now,
		IndependenceGroup: "worker-pool-a",
	}

	obs := epistemic.NewObservation(
		source,
		"database.postgres",
		"connection_pool_exhausted",
		"Active connections: 100/100, wait time > 5000ms",
		[]string{"ev-log-991", "ev-metric-882"},
		now,
	)

	if obs.ContentHash == "" {
		t.Fatal("expected non-empty cryptographic ContentHash on Observation")
	}

	// Verify deterministic hashing
	obs2 := epistemic.NewObservation(
		source,
		"database.postgres",
		"connection_pool_exhausted",
		"Active connections: 100/100, wait time > 5000ms",
		[]string{"ev-log-991", "ev-metric-882"},
		now,
	)

	if obs.ContentHash != obs2.ContentHash {
		t.Errorf("expected deterministic hash, got %s vs %s", obs.ContentHash, obs2.ContentHash)
	}

	claim := epistemic.NewClaim(
		"database.postgres",
		"requires_connection_pool_expansion",
		"pool_size_should_increase_to_200",
		0.85,
		now,
		[]domain.ID{obs.ID},
		[]string{"ev-log-991"},
		[]epistemic.SourceRef{source},
	)

	if claim.ContentHash == "" {
		t.Fatal("expected non-empty cryptographic ContentHash on Claim")
	}
}

func TestEpistemic_NonDestructiveContradictionCoexistence(t *testing.T) {
	ctx := context.Background()
	engine := epistemic.NewEngine()
	now := time.Now().UTC()

	srcA := epistemic.SourceRef{
		ID:                "src-a",
		Kind:              epistemic.SourceKindTelemetry,
		URI:               "telemetry://region-us-east-1",
		AuthorID:          "monitor-a",
		CapturedAt:        now,
		IndependenceGroup: "dc-east",
	}

	srcB := epistemic.SourceRef{
		ID:                "src-b",
		Kind:              epistemic.SourceKindTelemetry,
		URI:               "telemetry://region-eu-west-1",
		AuthorID:          "monitor-b",
		CapturedAt:        now,
		IndependenceGroup: "dc-west",
	}

	obsA := epistemic.NewObservation(srcA, "auth_service", "status", "HEALTHY", []string{"ev-health-east"}, now)
	obsB := epistemic.NewObservation(srcB, "auth_service", "status", "DEGRADED", []string{"ev-health-west"}, now)

	if err := engine.RecordObservation(ctx, obsA); err != nil {
		t.Fatalf("record obsA: %v", err)
	}
	if err := engine.RecordObservation(ctx, obsB); err != nil {
		t.Fatalf("record obsB: %v", err)
	}

	claimA := epistemic.NewClaim("auth_service", "availability", "AVAILABLE", 0.9, now, []domain.ID{obsA.ID}, []string{"ev-health-east"}, []epistemic.SourceRef{srcA})
	claimB := epistemic.NewClaim("auth_service", "availability", "UNAVAILABLE", 0.85, now, []domain.ID{obsB.ID}, []string{"ev-health-west"}, []epistemic.SourceRef{srcB})

	if err := engine.RecordClaim(ctx, claimA); err != nil {
		t.Fatalf("record claimA: %v", err)
	}
	if err := engine.RecordClaim(ctx, claimB); err != nil {
		t.Fatalf("record claimB: %v", err)
	}

	// Record contradiction non-destructively
	err := engine.RecordContradiction(ctx, claimA.ID, claimB.ID, "ev-health-west", "Regional network split causing divergent availability readings")
	if err != nil {
		t.Fatalf("record contradiction: %v", err)
	}

	// 1. Both claims must coexist
	cA, err := engine.GetClaim(ctx, claimA.ID)
	if err != nil {
		t.Fatalf("get claimA: %v", err)
	}
	cB, err := engine.GetClaim(ctx, claimB.ID)
	if err != nil {
		t.Fatalf("get claimB: %v", err)
	}

	if cA.Status != epistemic.ClaimStatusDisputed {
		t.Errorf("expected claimA status DISPUTED, got %s", cA.Status)
	}
	if cB.Status != epistemic.ClaimStatusDisputed {
		t.Errorf("expected claimB status DISPUTED, got %s", cB.Status)
	}

	if len(cA.ContradictingClaimIDs) == 0 || cA.ContradictingClaimIDs[0] != claimB.ID {
		t.Errorf("expected claimA to reference contradicting claimB ID")
	}
	if len(cB.ContradictingClaimIDs) == 0 || cB.ContradictingClaimIDs[0] != claimA.ID {
		t.Errorf("expected claimB to reference contradicting claimA ID")
	}
}

func TestEpistemic_TemporalValidityAndNonDestructiveSupersession(t *testing.T) {
	ctx := context.Background()
	engine := epistemic.NewEngine()

	t0 := time.Now().UTC().Add(-2 * time.Hour)
	t1 := t0.Add(1 * time.Hour)
	t2 := t1.Add(30 * time.Minute)

	src := epistemic.SourceRef{
		ID:                "src-ci",
		Kind:              epistemic.SourceKindAttempt,
		URI:               "ci://job-101",
		AuthorID:          "builder",
		CapturedAt:        t0,
		IndependenceGroup: "ci-pipeline",
	}

	obs1 := epistemic.NewObservation(src, "compiler.version", "target", "go1.26", []string{"ev-build-1"}, t0)
	claim1 := epistemic.NewClaim("compiler.version", "current_standard", "go1.26", 0.95, t0, []domain.ID{obs1.ID}, []string{"ev-build-1"}, []epistemic.SourceRef{src})

	if err := engine.RecordObservation(ctx, obs1); err != nil {
		t.Fatalf("record obs1: %v", err)
	}
	if err := engine.RecordClaim(ctx, claim1); err != nil {
		t.Fatalf("record claim1: %v", err)
	}

	// Superseding claim at t1
	obs2 := epistemic.NewObservation(src, "compiler.version", "target", "go1.27", []string{"ev-build-2"}, t1)
	claim2 := epistemic.NewClaim("compiler.version", "current_standard", "go1.27", 0.99, t1, []domain.ID{obs2.ID}, []string{"ev-build-2"}, []epistemic.SourceRef{src})

	if err := engine.RecordObservation(ctx, obs2); err != nil {
		t.Fatalf("record obs2: %v", err)
	}
	if err := engine.RecordClaim(ctx, claim2); err != nil {
		t.Fatalf("record claim2: %v", err)
	}

	if err := engine.SupersedeClaim(ctx, claim1.ID, claim2.ID, "Upgraded runtime to Go 1.27", t1); err != nil {
		t.Fatalf("supersede claim: %v", err)
	}

	// Verify claim1 preserved non-destructively
	c1Updated, err := engine.GetClaim(ctx, claim1.ID)
	if err != nil {
		t.Fatalf("get updated claim1: %v", err)
	}
	if c1Updated.Status != epistemic.ClaimStatusSuperseded {
		t.Errorf("expected claim1 status SUPERSEDED, got %s", c1Updated.Status)
	}
	if c1Updated.SupersededBy == nil || *c1Updated.SupersededBy != claim2.ID {
		t.Errorf("expected claim1 SupersededBy to point to claim2")
	}
	if c1Updated.ValidTo == nil || !c1Updated.ValidTo.Equal(t1) {
		t.Errorf("expected claim1 ValidTo to equal %v, got %v", t1, c1Updated.ValidTo)
	}

	// Temporal query:
	// As of t0 + 10m: claim1 should be active
	tPast := t0.Add(10 * time.Minute)
	claimsPast, err := engine.ListClaimsBySubject(ctx, "compiler.version", &tPast)
	if err != nil {
		t.Fatalf("list claims past: %v", err)
	}
	if len(claimsPast) != 1 || claimsPast[0].Object != "go1.26" {
		t.Errorf("expected go1.26 valid in the past, got %+v", claimsPast)
	}

	// As of t2: claim2 should be active
	claimsCurrent, err := engine.ListClaimsBySubject(ctx, "compiler.version", &t2)
	if err != nil {
		t.Fatalf("list claims current: %v", err)
	}
	if len(claimsCurrent) != 1 || claimsCurrent[0].Object != "go1.27" {
		t.Errorf("expected go1.27 valid currently, got %+v", claimsCurrent)
	}
}

func TestEpistemic_WhyDoWeBelieveLineageQuery(t *testing.T) {
	ctx := context.Background()
	engine := epistemic.NewEngine()
	now := time.Now().UTC()

	// 2 independent sources supporting the claim
	src1 := epistemic.SourceRef{
		ID:                "src-dtu-1",
		Kind:              epistemic.SourceKindDTU,
		URI:               "dtu://simulation/twin-42",
		AuthorID:          "dtu-agent",
		CapturedAt:        now,
		IndependenceGroup: "simulation-cluster",
	}

	src2 := epistemic.SourceRef{
		ID:                "src-telemetry-1",
		Kind:              epistemic.SourceKindTelemetry,
		URI:               "telemetry://prod/gateway-logs",
		AuthorID:          "log-indexer",
		CapturedAt:        now,
		IndependenceGroup: "production-gateway",
	}

	obs1 := epistemic.NewObservation(src1, "rate_limiter", "token_bucket_leak", "Detected memory leak of 50KB/hr under burst traffic", []string{"ev-dtu-heap-dump"}, now)
	obs2 := epistemic.NewObservation(src2, "rate_limiter", "token_bucket_leak", "Observed RSS memory growth on pod gateway-01", []string{"ev-prod-metrics-graph"}, now)

	if err := engine.RecordObservation(ctx, obs1); err != nil {
		t.Fatalf("record obs1: %v", err)
	}
	if err := engine.RecordObservation(ctx, obs2); err != nil {
		t.Fatalf("record obs2: %v", err)
	}

	claim := epistemic.NewClaim(
		"rate_limiter",
		"has_memory_leak",
		"token_bucket_goroutine_leak",
		0.92,
		now,
		[]domain.ID{obs1.ID, obs2.ID},
		[]string{"ev-dtu-heap-dump", "ev-prod-metrics-graph"},
		[]epistemic.SourceRef{src1, src2},
	)

	if err := engine.RecordClaim(ctx, claim); err != nil {
		t.Fatalf("record claim: %v", err)
	}

	// Query: Why do we believe this claim?
	lineage, err := engine.WhyDoWeBelieve(ctx, claim.ID)
	if err != nil {
		t.Fatalf("WhyDoWeBelieve query failed: %v", err)
	}

	if lineage.Claim.ID != claim.ID {
		t.Errorf("lineage claim ID mismatch")
	}
	if lineage.IndependentSourceCount != 2 {
		t.Errorf("expected 2 independent sources, got %d", lineage.IndependentSourceCount)
	}
	if len(lineage.SupportingEvidence) != 2 {
		t.Errorf("expected 2 supporting evidence items, got %d", len(lineage.SupportingEvidence))
	}
	if len(lineage.SupportingObservations) != 2 {
		t.Errorf("expected 2 supporting observations, got %d", len(lineage.SupportingObservations))
	}
	if lineage.ValidityState != "VALID" {
		t.Errorf("expected validity state VALID, got %s", lineage.ValidityState)
	}
	if !strings.Contains(lineage.ConfidenceExplanation, "independent sources") {
		t.Errorf("expected confidence explanation to reference independent sources: %s", lineage.ConfidenceExplanation)
	}
}

func TestEpistemic_KnowledgeDeltaAndNoMaterialLearning(t *testing.T) {
	ctx := context.Background()
	engine := epistemic.NewEngine()
	now := time.Now().UTC()

	attemptID := domain.NewID("attempt")

	// 1. Delta with NO_MATERIAL_LEARNING
	deltaEmpty := epistemic.NewNoMaterialLearningDelta(attemptID, now)
	if !deltaEmpty.IsNoMaterialLearning {
		t.Error("expected IsNoMaterialLearning to be true")
	}
	if err := engine.RecordDelta(ctx, deltaEmpty); err != nil {
		t.Fatalf("record empty delta: %v", err)
	}

	// 2. Delta with rich observations and claims
	src := epistemic.SourceRef{
		ID:                "src-worker-delta",
		Kind:              epistemic.SourceKindAttempt,
		URI:               "attempt://" + string(attemptID),
		AuthorID:          "worker-reflection",
		CapturedAt:        now,
		IndependenceGroup: "worker-env",
	}

	obs := epistemic.NewObservation(src, "test_harness", "flakiness", "High timeout prevents flakiness in worker startup test", []string{"ev-startup-test"}, now)
	claim := epistemic.NewClaim("test_harness", "timeout_guideline", "worker_startup_timeout_min_5s", 0.95, now, []domain.ID{obs.ID}, []string{"ev-startup-test"}, []epistemic.SourceRef{src})

	delta := epistemic.KnowledgeDelta{
		ID:              domain.NewID("kdelta"),
		SourceAttemptID: attemptID,
		Observations:    []epistemic.Observation{obs},
		ProposedClaims:  []epistemic.Claim{claim},
		CreatedAt:       now,
	}

	if err := engine.RecordDelta(ctx, delta); err != nil {
		t.Fatalf("record delta with learning: %v", err)
	}

	// Verify observations and claims were integrated into engine
	retrievedClaim, err := engine.GetClaim(ctx, claim.ID)
	if err != nil {
		t.Fatalf("retrieved claim from delta failed: %v", err)
	}
	if retrievedClaim.Object != "worker_startup_timeout_min_5s" {
		t.Errorf("expected claim from delta to be recorded, got %s", retrievedClaim.Object)
	}
}
