package experience

import (
	"context"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/approvals"
	"github.com/SofiaFlux/summa42/internal/audit"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

type expHarness struct {
	ctx       context.Context
	svc       *Service
	approvals *approvals.Service
	clock     *testutil.Clock
	store     interface{ DB() interface{} }
}

func newExpHarness(t *testing.T) (*Service, *approvals.Service, *testutil.Clock) {
	t.Helper()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC))
	ap := approvals.New(store, clk)
	svc := New(store, clk, ap, audit.New(store, clk), "owner-1")
	return svc, ap, clk
}

func TestGrantRequiresExactOwnerApprovalAndHasNoImplicitAuthority(t *testing.T) {
	svc, ap, clk := newExpHarness(t)
	ctx := context.Background()
	req, err := svc.RequestGrant(ctx, GrantInput{
		Kind: AdaptationExecutorPreference, ScopeKey: "repo.review",
		AllowedExecutors: []string{"codex", "claude"}, MinVerifiedSamples: 3,
		MaxAcceptanceRegressionBps: 500, MaxCostRegressionBps: 1000, ExpiresAt: clk.Now().Add(24 * time.Hour),
	}, "requester-1")
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := svc.store.DB().QueryRowContext(ctx, "SELECT count(*) FROM adaptation_grants").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("grant exists before approval: %d", count)
	}
	if _, err := svc.ActivateGrant(ctx, req.ID); err == nil {
		t.Fatal("grant activated before Owner approval")
	}
	if err := ap.Approve(ctx, req.ApprovalID, "owner-1", "wrong-digest"); err == nil {
		t.Fatal("wrong digest approval succeeded")
	}
	if err := ap.Approve(ctx, req.ApprovalID, "owner-1", req.Digest); err != nil {
		t.Fatal(err)
	}
	grant, err := svc.ActivateGrant(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if grant.OwnerPrincipalID != "owner-1" || grant.Kind != AdaptationExecutorPreference {
		t.Fatalf("grant=%+v", grant)
	}
	approval, err := ap.Get(ctx, req.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	if approval.State != domain.ApprovalConsumed {
		t.Fatalf("approval state=%s", approval.State)
	}
}

func TestGrantRejectsTrustedControlKindsAndProposalRequiresEvidence(t *testing.T) {
	svc, ap, clk := newExpHarness(t)
	ctx := context.Background()
	if _, err := svc.RequestGrant(ctx, GrantInput{
		Kind: "POLICY_OVERRIDE", ScopeKey: "repo", AllowedExecutors: []string{"codex"}, MinVerifiedSamples: 1, ExpiresAt: clk.Now().Add(time.Hour),
	}, "requester"); err == nil {
		t.Fatal("policy override grant accepted")
	}
	req, err := svc.RequestGrant(ctx, GrantInput{
		Kind: AdaptationExecutorPreference, ScopeKey: "repo.review", AllowedExecutors: []string{"codex"}, MinVerifiedSamples: 1, ExpiresAt: clk.Now().Add(time.Hour),
	}, "requester")
	if err != nil {
		t.Fatal(err)
	}
	if err := ap.Approve(ctx, req.ApprovalID, "owner-1", req.Digest); err != nil {
		t.Fatal(err)
	}
	grant, err := svc.ActivateGrant(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Propose(ctx, ProposalInput{
		GrantID: grant.ID, GenericTaskClass: domain.GenericTaskReview, ScopeKey: "repo.review",
		PreferredExecutor: "codex", EvidenceObservationIDs: []domain.ID{"missing-observation"},
	}); err == nil {
		t.Fatal("proposal accepted missing evidence")
	}
	if _, err := svc.Propose(ctx, ProposalInput{
		GrantID: grant.ID, GenericTaskClass: domain.GenericTaskReview, ScopeKey: "repo.review",
		PreferredExecutor: "not-allowed", EvidenceObservationIDs: []domain.ID{"missing"},
	}); err == nil {
		t.Fatal("proposal accepted executor outside grant")
	}
}

func seedExperienceCanonicalTask(t *testing.T, svc *Service, taskID, taskClass string, currentAttempt domain.ID, executor string) {
	t.Helper()
	now := svc.clock.Now().UTC().Format(time.RFC3339Nano)
	if _, err := svc.store.DB().ExecContext(context.Background(), `
		INSERT INTO tasks(
			task_id, purpose_kind, purpose_id, task_class, state, current_attempt_id, current_fence,
			acceptance_criteria_json, required_capabilities_json, required_enforcement,
			authority_ceiling_json, resource_envelope_id, priority, created_at, updated_at
		) VALUES (?, 'OWNER_DIRECTIVE', ?, ?, 'SUCCEEDED', ?, 2, '["ok"]', '[]', 'UNENFORCED', '[]', 'env', 0, ?, ?)`,
		taskID, "purpose-"+taskID, taskClass, currentAttempt, now, now,
	); err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []struct {
		id    domain.ID
		fence int64
		kind  string
	}{
		{id: "attempt-old", fence: 1, kind: "claude"},
		{id: currentAttempt, fence: 2, kind: executor},
	} {
		if _, err := svc.store.DB().ExecContext(context.Background(), `
			INSERT OR IGNORE INTO attempts(
				attempt_id, task_id, state, fence_generation, lease_state, lease_expires_at, started_at, completed_at, executor_kind
			) VALUES (?, ?, 'COMPLETED', ?, 'REVOKED', ?, ?, ?, ?)`,
			attempt.id, taskID, attempt.fence, now, now, now, attempt.kind,
		); err != nil {
			t.Fatal(err)
		}
	}
}

func TestObserveVerifiedOutcomeDerivesScopeAndClassFromCanonicalTask(t *testing.T) {
	svc, _, _ := newExpHarness(t)
	ctx := context.Background()
	seedExperienceCanonicalTask(t, svc, "task-scope", "repo.review", "attempt-current", "codex")
	now := svc.clock.Now().UTC().Format(time.RFC3339Nano)
	if _, err := svc.store.DB().ExecContext(ctx, `
		INSERT INTO acceptance_records(
			acceptance_id, task_id, attempt_id, verifier_id, verifier_type, criteria_result_json, evidence_ids_json, created_at
		) VALUES ('accept-scope','task-scope','attempt-current','owner','TEST','{"met":true}','[]',?)`, now); err != nil {
		t.Fatal(err)
	}

	err := svc.ObserveVerifiedOutcome(ctx, VerifiedOutcome{
		TaskID: "task-scope", GenericTaskClass: domain.GenericTaskDebugging, ScopeKey: "repo.debug",
		ExecutorKind: "codex", Accepted: true,
	})
	if err == nil {
		t.Fatal("caller-provided scope/class overrode canonical TaskClass")
	}
}

func TestObserveVerifiedOutcomeIsIdempotent(t *testing.T) {
	svc, _, _ := newExpHarness(t)
	ctx := context.Background()
	seedExperienceCanonicalTask(t, svc, "task-idempotent", "repo.review", "attempt-current", "codex")
	now := svc.clock.Now().UTC().Format(time.RFC3339Nano)
	if _, err := svc.store.DB().ExecContext(ctx, `
		INSERT INTO acceptance_records(acceptance_id, task_id, attempt_id, verifier_id, verifier_type, criteria_result_json, evidence_ids_json, created_at)
		VALUES ('accept-idempotent','task-idempotent','attempt-current','owner','TEST','{"met":true}','[]',?)`, now); err != nil {
		t.Fatal(err)
	}
	input := VerifiedOutcome{TaskID: "task-idempotent", Accepted: true}
	if err := svc.ObserveVerifiedOutcome(ctx, input); err != nil {
		t.Fatal(err)
	}
	if err := svc.ObserveVerifiedOutcome(ctx, input); err != nil {
		t.Fatalf("repeat verified outcome = %v", err)
	}
}

func TestObserveVerifiedOutcomeRequiresEvidenceForExactCurrentAttempt(t *testing.T) {
	svc, _, _ := newExpHarness(t)
	ctx := context.Background()
	seedExperienceCanonicalTask(t, svc, "task-attempt", "repo.review", "attempt-current", "codex")
	now := svc.clock.Now().UTC().Format(time.RFC3339Nano)
	if _, err := svc.store.DB().ExecContext(ctx, `
		INSERT INTO acceptance_records(
			acceptance_id, task_id, attempt_id, verifier_id, verifier_type, criteria_result_json, evidence_ids_json, created_at
		) VALUES ('accept-old','task-attempt','attempt-old','owner','TEST','{"met":true}','[]',?)`, now); err != nil {
		t.Fatal(err)
	}
	if err := svc.ObserveVerifiedOutcome(ctx, VerifiedOutcome{TaskID: "task-attempt", Accepted: true}); err == nil {
		t.Fatal("acceptance from a different Attempt counted as verified outcome")
	}

	if _, err := svc.store.DB().ExecContext(ctx, "UPDATE tasks SET state = 'CHALLENGED' WHERE task_id = 'task-attempt'"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.DB().ExecContext(ctx, `
		INSERT INTO execution_events(event_id, task_id, attempt_id, event_type, details_json, created_at)
		VALUES ('event-old-challenge','task-attempt','attempt-old','TASK_CHALLENGED','{}',?)`, now); err != nil {
		t.Fatal(err)
	}
	if err := svc.ObserveVerifiedOutcome(ctx, VerifiedOutcome{TaskID: "task-attempt", Accepted: false}); err == nil {
		t.Fatal("challenge from a different Attempt counted as verified negative outcome")
	}
}
