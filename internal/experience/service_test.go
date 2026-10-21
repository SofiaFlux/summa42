package experience

import (
	"context"
	"database/sql"
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

func TestOutcomeMetricsDerivedFromLedger(t *testing.T) {
	svc, ap, clk := newExpHarness(t)
	ctx := context.Background()
	db := svc.store.DB()
	seedExperienceCanonicalTask(t, svc, "task-metrics", "repo.review", "attempt-current", "codex")
	start := clk.Now().UTC()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	nowS := formatTime(start)
	exec("UPDATE attempts SET started_at = ? WHERE attempt_id = 'attempt-old'", formatTime(start.Add(-10*time.Second)))
	exec("UPDATE attempts SET started_at = ? WHERE attempt_id = 'attempt-current'", formatTime(start.Add(-5*time.Second)))
	exec(`INSERT INTO acceptance_records(acceptance_id, task_id, attempt_id, verifier_id, verifier_type, criteria_result_json, evidence_ids_json, created_at)
		VALUES ('accept-metrics','task-metrics','attempt-current','owner','TEST','{"met":true}','[]',?)`, formatTime(start.Add(2*time.Second)))
	exec("INSERT OR IGNORE INTO policy_sets(policy_set_id,version,module_name,module,policy_hash,capabilities_hash,active,created_at) VALUES('policy_test',1,'test.rego','package test','policy-hash','caps-hash',1,?)", nowS)
	exec("INSERT INTO resource_envelopes(envelope_id,hard_limit,created_at) VALUES('env_metrics',100,?)", nowS)
	for _, r := range []struct {
		id, state string
		settled   any
	}{{"res-a", "SETTLED", 7}, {"res-b", "SETTLED", 5}, {"res-c", "RELEASED", nil}} {
		exec("INSERT INTO resource_reservations(reservation_id,envelope_id,state,reserved_amount,settled_amount,cost_control,cost_source,require_hard_cap,created_at,updated_at) VALUES(?,'env_metrics',?,10,?,'TECHNICALLY_CAPPED','test',0,?,?)",
			r.id, r.state, r.settled, nowS, nowS)
	}
	// A settled reservation of another task must not leak into this task's cost.
	exec("INSERT INTO resource_reservations(reservation_id,envelope_id,state,reserved_amount,settled_amount,cost_control,cost_source,require_hard_cap,created_at,updated_at) VALUES('res-other','env_metrics','SETTLED',10,50,'TECHNICALLY_CAPPED','test',0,?,?)", nowS, nowS)
	exec("INSERT INTO effect_slots(effect_slot_id,collective_id,task_id,trusted_slot_key,provider,descriptor_type,intent_fingerprint,intent_revision,canonical_intent_json,adapter_version,adapter_version_semantic,created_at,updated_at) VALUES('slot-1','collective','task-metrics','key','provider','test','fp',1,'{}','v1',0,?,?)", nowS, nowS)
	req, err := ap.Create(ctx, approvals.CreateRequest{
		SubjectKind: "EXTERNAL_OPERATION", SubjectID: "op-a", RequestDigest: "digest-a", PolicyDecisionID: "decision-1",
		RequiredApprovers: []domain.ID{"owner-1"}, RequestedBy: "requester", ExpiresAt: start.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ap.Approve(ctx, req.ID, "owner-1", req.RequestDigest); err != nil {
		t.Fatal(err)
	}
	for i, res := range []string{"res-a", "res-b"} {
		approval := any(nil)
		if i == 0 {
			approval = req.ID
		}
		exec("INSERT INTO external_operations(operation_id,task_id,attempt_id,effect_slot_id,operation_sequence,state,intent_fingerprint,intent_revision,provider,adapter_version,reservation_id,risk,prepare_policy_decision_id,prepare_policy_set_id,prepare_policy_set_hash,prepare_policy_capabilities_hash,approval_id,created_at) VALUES(?,'task-metrics','attempt-current','slot-1',?,'CONFIRMED_EFFECT','fp',1,'provider','v1',?,'LOW','decision-1','policy_test','policy-hash','caps-hash',?,?)",
			"op-"+res, i+1, res, approval, nowS)
	}

	if err := svc.ObserveVerifiedOutcome(ctx, VerifiedOutcome{TaskID: "task-metrics", Accepted: true}); err != nil {
		t.Fatal(err)
	}
	var human, retries int64
	var cost, latency sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT human_intervention, retry_count, cost_units, latency_ms FROM experience_outcomes WHERE task_id = 'task-metrics'").
		Scan(&human, &retries, &cost, &latency); err != nil {
		t.Fatal(err)
	}
	if human != 1 || retries != 1 || cost.Int64 != 12 || latency.Int64 != 12000 {
		t.Fatalf("human=%d retries=%d cost=%v latency=%v", human, retries, cost, latency)
	}
}
