package company_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/company"
	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type fixedClock struct {
	now time.Time
}

func (c *fixedClock) Now() time.Time {
	return c.now
}

func (c *fixedClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
}

func setupCompanySuite(t *testing.T) (*state.Store, *fixedClock, *company.Service) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(context.Background(), filepath.Join(dir, "company_test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.DB().Close() })

	clk := &fixedClock{now: time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)}
	svc := company.NewService(store, clk)

	return store, clk, svc
}

func seedAcceptedTaskWithEvidence(
	t *testing.T,
	store *state.Store,
	taskID domain.ID,
	evidenceID domain.ID,
	criteriaMet bool,
) {
	t.Helper()
	ctx := context.Background()
	db := store.DB()
	now := "2026-11-09T08:00:00Z"

	attemptID := domain.NewID("att")

	// Seed task
	_, err := db.ExecContext(ctx, `
		INSERT INTO tasks (
			task_id, purpose_kind, purpose_id, state, current_attempt_id, acceptance_criteria_json,
			required_capabilities_json, required_enforcement, authority_ceiling_json,
			resource_envelope_id, created_at, updated_at
		) VALUES (?, 'MISSION', 'm1', 'SUCCEEDED', ?, '{}', '[]', 'ENFORCED', '{}', 'env1', ?, ?)`,
		taskID, attemptID, now, now,
	)
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}

	// Seed attempt
	_, err = db.ExecContext(ctx, `
		INSERT INTO attempts (
			attempt_id, task_id, state, fence_generation, lease_state, lease_expires_at, started_at, executor_kind
		) VALUES (?, ?, 'COMPLETED', 1, 'EXPIRED', ?, ?, 'TEST')`,
		attemptID, taskID, now, now,
	)
	if err != nil {
		t.Fatalf("seed attempt: %v", err)
	}

	// Seed evidence object
	_, err = db.ExecContext(ctx, `
		INSERT INTO evidence_objects (
			evidence_id, content_hash, media_type, kind, size_bytes, created_at
		) VALUES (?, 'hash_123', 'application/json', 'task.output', 100, ?)`,
		evidenceID, now,
	)
	if err != nil {
		t.Fatalf("seed evidence object: %v", err)
	}

	// Seed acceptance record with canonical {"met": true/false}
	criteriaJSON := `{"met":true}`
	if !criteriaMet {
		criteriaJSON = `{"met":false}`
	}

	acceptanceID := domain.NewID("acc")
	_, err = db.ExecContext(ctx, `
		INSERT INTO acceptance_records (
			acceptance_id, task_id, attempt_id, verifier_id, verifier_type,
			criteria_result_json, evidence_ids_json, created_at
		) VALUES (?, ?, ?, 'verifier_1', 'INTERNAL', ?, ?, ?)`,
		acceptanceID, taskID, attemptID, criteriaJSON, `["`+string(evidenceID)+`"]`, now,
	)
	if err != nil {
		t.Fatalf("seed acceptance record: %v", err)
	}
}

func TestCompanyService_ApplyTransitionWithAcceptedTaskAndEvidence(t *testing.T) {
	ctx := context.Background()
	store, clk, svc := setupCompanySuite(t)

	task1 := domain.NewID("task")
	ev1 := domain.NewID("ev")
	seedAcceptedTaskWithEvidence(t, store, task1, ev1, true)

	entityID := domain.ID("cust_acme")

	// 1. Initial State Transition: Create Customer
	trans1, err := svc.ApplyTransition(ctx, company.ApplyTransitionRequest{
		EntityID:       entityID,
		EntityKind:     company.KindCustomer,
		Name:           "Acme International",
		TaskID:         task1,
		EvidenceID:     ev1,
		TransitionKind: "ONBOARDING_APPROVED",
		NewStatus:      "ONBOARDED",
		NewAttributes: map[string]any{
			"tier":    "ENTERPRISE",
			"country": "US",
		},
	})
	if err != nil {
		t.Fatalf("apply initial transition: %v", err)
	}

	if trans1.EntityID != entityID {
		t.Fatalf("expected entity ID %s, got %s", entityID, trans1.EntityID)
	}
	if trans1.PreviousState != nil {
		t.Fatalf("expected nil previous state on initial transition, got %+v", trans1.PreviousState)
	}

	// Verify Entity was created in durable store
	ent, err := svc.GetEntity(ctx, entityID)
	if err != nil {
		t.Fatalf("get entity: %v", err)
	}
	if ent.Name != "Acme International" || ent.Status != "ONBOARDED" {
		t.Fatalf("unexpected entity state: %+v", ent)
	}
	if ent.Attributes["country"] != "US" {
		t.Fatalf("expected country US, got %v", ent.Attributes["country"])
	}

	// 2. Secondary State Transition: Upgrade SLA and increase exposure (attribute merge)
	clk.Advance(2 * time.Hour)
	task2 := domain.NewID("task")
	ev2 := domain.NewID("ev")
	seedAcceptedTaskWithEvidence(t, store, task2, ev2, true)

	newExposure := int64(75000)
	trans2, err := svc.ApplyTransition(ctx, company.ApplyTransitionRequest{
		EntityID:          entityID,
		TaskID:            task2,
		EvidenceID:        ev2,
		TransitionKind:    "CONTRACT_EXPANSION",
		NewStatus:         "ACTIVE",
		NewExposureAmount: &newExposure,
		NewAttributes: map[string]any{
			"tier": "TIER_1_VIP", // Overwrites tier, preserves country
		},
	})
	if err != nil {
		t.Fatalf("apply secondary transition: %v", err)
	}

	if trans2.PreviousState == nil {
		t.Fatal("expected previous state on secondary transition")
	}
	if trans2.PreviousState["status"] != "ONBOARDED" {
		t.Fatalf("expected previous status ONBOARDED, got %v", trans2.PreviousState["status"])
	}

	// Verify merged attributes on entity
	entAfter, err := svc.GetEntity(ctx, entityID)
	if err != nil {
		t.Fatalf("get entity after update: %v", err)
	}
	if entAfter.Attributes["tier"] != "TIER_1_VIP" {
		t.Fatalf("expected tier TIER_1_VIP, got %v", entAfter.Attributes["tier"])
	}
	if entAfter.Attributes["country"] != "US" {
		t.Fatalf("expected preserved country US, got %v", entAfter.Attributes["country"])
	}

	// 3. Verify audit history contains both transitions in chronological order
	history, err := svc.GetAuditHistory(ctx, entityID)
	if err != nil {
		t.Fatalf("get audit history: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected 2 audit transitions, got %d", len(history))
	}
	if history[0].TransitionKind != "ONBOARDING_APPROVED" {
		t.Fatalf("expected first transition ONBOARDING_APPROVED, got %s", history[0].TransitionKind)
	}
	if history[1].TransitionKind != "CONTRACT_EXPANSION" {
		t.Fatalf("expected second transition CONTRACT_EXPANSION, got %s", history[1].TransitionKind)
	}
}

func TestCompanyService_TransitionRejectedWithoutAcceptedTask(t *testing.T) {
	ctx := context.Background()
	store, _, svc := setupCompanySuite(t)

	taskUnaccepted := domain.NewID("task_running")
	ev := domain.NewID("ev")
	now := "2026-11-09T08:00:00Z"

	// Seed task in EXECUTING state (not SUCCEEDED)
	_, _ = store.DB().ExecContext(ctx, `
		INSERT INTO tasks (
			task_id, purpose_kind, purpose_id, state, acceptance_criteria_json,
			required_capabilities_json, required_enforcement, authority_ceiling_json,
			resource_envelope_id, created_at, updated_at
		) VALUES (?, 'MISSION', 'm1', 'EXECUTING', '{}', '[]', 'ENFORCED', '{}', 'env1', ?, ?)`,
		taskUnaccepted, now, now,
	)

	_, err := svc.ApplyTransition(ctx, company.ApplyTransitionRequest{
		EntityID:       "ent_fail",
		EntityKind:     company.KindCustomer,
		Name:           "Fail Corp",
		TaskID:         taskUnaccepted,
		EvidenceID:     ev,
		TransitionKind: "UNAUTHORIZED_CHANGE",
		NewStatus:      "ACTIVE",
	})
	if !errors.Is(err, company.ErrTaskNotAccepted) {
		t.Fatalf("expected ErrTaskNotAccepted, got %v", err)
	}
}

func TestCompanyService_TransitionRejectedWhenAcceptanceVerdictFails(t *testing.T) {
	ctx := context.Background()
	store, _, svc := setupCompanySuite(t)

	taskFail := domain.NewID("task_fail")
	ev := domain.NewID("ev")
	seedAcceptedTaskWithEvidence(t, store, taskFail, ev, false)

	_, err := svc.ApplyTransition(ctx, company.ApplyTransitionRequest{
		EntityID:       "ent_fail_verdict",
		EntityKind:     company.KindCustomer,
		Name:           "Fail Verdict Corp",
		TaskID:         taskFail,
		EvidenceID:     ev,
		TransitionKind: "REJECTED_ONBOARDING",
		NewStatus:      "ACTIVE",
	})
	if !errors.Is(err, company.ErrAcceptanceVerdictFailed) {
		t.Fatalf("expected ErrAcceptanceVerdictFailed, got %v", err)
	}
}

func TestCompanyService_TransitionRejectedWithoutVerifiedEvidence(t *testing.T) {
	ctx := context.Background()
	store, _, svc := setupCompanySuite(t)

	taskAccepted := domain.NewID("task_acc")
	evLegit := domain.NewID("ev_legit")
	seedAcceptedTaskWithEvidence(t, store, taskAccepted, evLegit, true)

	// Seed another real evidence object in the DB, but unrelated to this task
	now := "2026-11-09T08:00:00Z"
	evUnrelated := domain.NewID("ev_unrelated")
	_, _ = store.DB().ExecContext(ctx, `
		INSERT INTO evidence_objects (evidence_id, content_hash, media_type, kind, size_bytes, created_at)
		VALUES (?, 'hash_unrelated', 'application/json', 'task.output', 50, ?)`,
		evUnrelated, now,
	)

	// Try transition with real-but-unrelated evidence ID
	_, err := svc.ApplyTransition(ctx, company.ApplyTransitionRequest{
		EntityID:       "ent_unverified",
		EntityKind:     company.KindCustomer,
		Name:           "Unverified Corp",
		TaskID:         taskAccepted,
		EvidenceID:     evUnrelated,
		TransitionKind: "TAMPERED_TRANSITION",
		NewStatus:      "ACTIVE",
	})
	if !errors.Is(err, company.ErrMissingEvidence) {
		t.Fatalf("expected ErrMissingEvidence for unrelated evidence, got %v", err)
	}
}

func TestCompanyService_ExternalProjectionsIngestion(t *testing.T) {
	ctx := context.Background()
	_, _, svc := setupCompanySuite(t)

	// Ingest CRM projection
	crmEnt, crmProj, err := svc.IngestExternalProjection(ctx, "CRM", "cust_123", map[string]any{
		"name":           "Global Logistics LLC",
		"status":         "ACTIVE",
		"contract_value": float64(120000),
	})
	if err != nil {
		t.Fatalf("ingest crm projection: %v", err)
	}
	if crmEnt.Kind != company.KindCustomer || crmEnt.ExposureAmount != 120000 {
		t.Fatalf("unexpected crm entity: %+v", crmEnt)
	}
	if crmProj.SourceSystem != "CRM" || crmProj.ExternalID != "cust_123" {
		t.Fatalf("unexpected crm projection: %+v", crmProj)
	}
	if crmProj.ProjectionID == "" {
		t.Fatal("expected populated ProjectionID")
	}

	// Verify an auditable transition was recorded for the projection
	hist, err := svc.GetAuditHistory(ctx, crmEnt.EntityID)
	if err != nil {
		t.Fatalf("get audit history: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("expected 1 audit transition for crm projection, got %d", len(hist))
	}
	if hist[0].TransitionKind != "EXTERNAL_PROJECTION_INGESTION" {
		t.Fatalf("expected EXTERNAL_PROJECTION_INGESTION, got %s", hist[0].TransitionKind)
	}

	// Ingest ERP projection
	erpEnt, _, err := svc.IngestExternalProjection(ctx, "ERP", "acc_999", map[string]any{
		"description":     "AWS Infrastructure Spend",
		"status":          "ACTIVE",
		"payable_balance": float64(45000),
	})
	if err != nil {
		t.Fatalf("ingest erp projection: %v", err)
	}
	if erpEnt.Kind != company.KindCommitment || erpEnt.ExposureAmount != 45000 {
		t.Fatalf("unexpected erp entity: %+v", erpEnt)
	}

	// Ingest Cloud projection
	cloudEnt, _, err := svc.IngestExternalProjection(ctx, "CLOUD", "cluster_prod_1", map[string]any{
		"resource_name": "us-east-1-k8s-prod",
		"health":        "HEALTHY",
	})
	if err != nil {
		t.Fatalf("ingest cloud projection: %v", err)
	}
	if cloudEnt.Kind != company.KindAsset {
		t.Fatalf("unexpected cloud entity kind: %s", cloudEnt.Kind)
	}

	// Ingest Git projection
	gitEnt, _, err := svc.IngestExternalProjection(ctx, "GIT", "repo_core_api", map[string]any{
		"repo_name":         "summa42/core-api",
		"compliance_status": "COMPLIANT",
	})
	if err != nil {
		t.Fatalf("ingest git projection: %v", err)
	}
	if gitEnt.Kind != company.KindProductService {
		t.Fatalf("unexpected git entity kind: %s", gitEnt.Kind)
	}

	// Idempotent re-ingestion of CRM projection with updated value
	updatedEnt, _, err := svc.IngestExternalProjection(ctx, "CRM", "cust_123", map[string]any{
		"name":           "Global Logistics LLC",
		"status":         "ACTIVE",
		"contract_value": float64(150000),
	})
	if err != nil {
		t.Fatalf("re-ingest crm projection: %v", err)
	}
	if updatedEnt.EntityID != crmEnt.EntityID {
		t.Fatalf("expected same entity ID %s, got %s", crmEnt.EntityID, updatedEnt.EntityID)
	}
	if updatedEnt.ExposureAmount != 150000 {
		t.Fatalf("expected updated exposure 150000, got %d", updatedEnt.ExposureAmount)
	}

	// Test Summary
	summary, err := svc.GetCompanySummary(ctx)
	if err != nil {
		t.Fatalf("get company summary: %v", err)
	}
	if summary.TotalEntities != 4 {
		t.Fatalf("expected 4 total entities, got %d", summary.TotalEntities)
	}
	if summary.TotalExposureAmount != 195000 { // 150000 + 45000
		t.Fatalf("expected total exposure 195000, got %d", summary.TotalExposureAmount)
	}
	if summary.ActiveCommitments != 1 {
		t.Fatalf("expected 1 active commitment, got %d", summary.ActiveCommitments)
	}
}
