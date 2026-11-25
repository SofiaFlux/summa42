package operations_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/approvals"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/operations"
)

func requireOwner(h *harness, owners ...domain.ID) {
	h.policy.setDecision(domain.PolicyRequireApproval, "policy-v1", owners, nil)
}

func TestApprovalOutlivesLeaseAndIsAdoptedOnce(t *testing.T) {
	h := newHarness(t)
	owner := domain.ID("owner")
	requireOwner(h, owner)

	first := preparePurchase(t, h, h.attempt.ID, "adopt", 1)
	approval, err := h.approvals.Get(h.ctx, first.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	if want := h.clock.Now().UTC().Add(operations.DefaultApprovalTTL); !approval.ExpiresAt.Equal(want) {
		t.Fatalf("approval expires %s, want %s (the TTL, not the lease)", approval.ExpiresAt, want)
	}
	if err := h.approvals.Approve(h.ctx, approval.ID, owner, approval.RequestDigest); err != nil {
		t.Fatal(err)
	}

	second := replacementAttempt(t, h, h.attempt.ID)
	adopted := preparePurchase(t, h, second.ID, "adopt", 1)
	if adopted.ID == first.ID || adopted.ApprovalID != first.ApprovalID {
		t.Fatalf("new operation %s approval %s, want a new operation on approval %s", adopted.ID, adopted.ApprovalID, first.ApprovalID)
	}
	settled, err := h.svc.Dispatch(h.ctx, adopted.ID, second.ID)
	if err != nil || settled.State != domain.OperationConfirmedEffect || h.provider.DispatchCount() != 1 {
		t.Fatalf("dispatch %+v %v count=%d", settled, err, h.provider.DispatchCount())
	}
	if consumed, err := h.approvals.Get(h.ctx, approval.ID); err != nil || consumed.State != domain.ApprovalConsumed {
		t.Fatalf("approval %+v %v, want CONSUMED", consumed, err)
	}

	// A third Attempt sees the effect, not a second dispatch.
	third := replacementAttempt(t, h, second.ID)
	again := preparePurchase(t, h, third.ID, "adopt", 1)
	if again.ID != adopted.ID || h.provider.DispatchCount() != 1 {
		t.Fatalf("third prepare %+v count=%d, want the confirmed operation", again, h.provider.DispatchCount())
	}
}

func TestPendingApprovalIsReadoptedByEveryReplacement(t *testing.T) {
	h := newHarness(t)
	requireOwner(h, "owner")
	first := preparePurchase(t, h, h.attempt.ID, "readopt", 1)
	attempt := h.attempt.ID
	for i := 0; i < 3; i++ {
		attempt = replacementAttempt(t, h, attempt).ID
		op := preparePurchase(t, h, attempt, "readopt", 1)
		if op.ApprovalID != first.ApprovalID {
			t.Fatalf("replacement %d approval %s, want %s", i, op.ApprovalID, first.ApprovalID)
		}
		// What the Owner is shown is the live operation, not the cancelled subject.
		if shown, err := h.svc.ApprovalOperation(h.ctx, first.ApprovalID); err != nil || shown.ID != op.ID || shown.State != domain.OperationPrepared {
			t.Fatalf("approval shows %+v %v, want live operation %s", shown, err, op.ID)
		}
	}
}

func TestApprovalIsNotAdopted(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, h *harness, approvalID domain.ID){
		"rejected": func(t *testing.T, h *harness, approvalID domain.ID) {
			record, err := h.approvals.Get(h.ctx, approvalID)
			if err != nil {
				t.Fatal(err)
			}
			if err := h.approvals.Reject(h.ctx, approvalID, "owner", record.RequestDigest); err != nil {
				t.Fatal(err)
			}
		},
		"approvers changed": func(t *testing.T, h *harness, _ domain.ID) {
			requireOwner(h, "owner", "second-owner")
		},
		"too close to expiry": func(t *testing.T, h *harness, _ domain.ID) {
			h.clock.Advance(operations.DefaultApprovalTTL - 10*time.Minute)
		},
		"expired": func(t *testing.T, h *harness, _ domain.ID) {
			h.clock.Advance(operations.DefaultApprovalTTL + time.Minute)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			requireOwner(h, "owner")
			first := preparePurchase(t, h, h.attempt.ID, "no-adopt", 1)
			change(t, h, first.ApprovalID)
			next := replacementAttempt(t, h, h.attempt.ID)
			op := preparePurchase(t, h, next.ID, "no-adopt", 1)
			if op.ApprovalID == "" || op.ApprovalID == first.ApprovalID {
				t.Fatalf("approval %q, want a fresh request", op.ApprovalID)
			}
		})
	}
}

func TestApprovalIsNotAdoptedAcrossSlots(t *testing.T) {
	h := newHarness(t)
	requireOwner(h, "owner")
	first := preparePurchase(t, h, h.attempt.ID, "slot-a", 1)
	other := preparePurchase(t, h, h.attempt.ID, "slot-b", 1)
	if other.ApprovalID == first.ApprovalID {
		t.Fatal("an approval crossed effect slots")
	}
}

func TestApprovalIsNotAdoptedAcrossTasksWithSameSlotKey(t *testing.T) {
	h := newHarness(t)
	requireOwner(h, "owner")
	first := preparePurchase(t, h, h.attempt.ID, "shared-slot", 1)

	otherTaskID := domain.NewID("task")
	otherEnvelopeID := domain.NewID("envelope")
	now := h.clock.Now().UTC().Format(time.RFC3339Nano)
	if _, err := h.store.DB().ExecContext(h.ctx,
		`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, 1000, ?)`,
		otherEnvelopeID, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB().ExecContext(h.ctx, `
		INSERT INTO tasks(
			task_id, purpose_kind, purpose_id, state, current_fence,
			acceptance_criteria_json, required_capabilities_json, required_enforcement,
			authority_ceiling_json, resource_envelope_id, priority, created_at, updated_at
		) VALUES (?, 'OWNER_DIRECTIVE', 'owner-test', 'ELIGIBLE', 0, '[]', '[]', 'ENFORCED', ?, ?, 0, ?, ?)`,
		otherTaskID, `["`+h.provider.Capability()+`"]`, otherEnvelopeID, now, now,
	); err != nil {
		t.Fatal(err)
	}
	otherAttempt, err := h.exec.StartAttempt(h.ctx, otherTaskID, "other-task-exec", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	other := preparePurchase(t, h, otherAttempt.ID, "shared-slot", 1)
	if other.EffectSlotID == first.EffectSlotID || other.ApprovalID == first.ApprovalID || other.ApprovalID == "" {
		t.Fatalf("other task slot=%s approval=%s, want distinct from slot=%s approval=%s", other.EffectSlotID, other.ApprovalID, first.EffectSlotID, first.ApprovalID)
	}
}

func TestConsumedApprovalIsNotAdopted(t *testing.T) {
	h := newHarness(t)
	requireOwner(h, "owner")
	h.provider.SetLoseAcknowledgement(true)
	h.provider.lookupNoEffect = true
	h.provider.settleWindow = time.Hour

	first := preparePurchase(t, h, h.attempt.ID, "consumed-no-effect", 1)
	approval, err := h.approvals.Get(h.ctx, first.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.approvals.Approve(h.ctx, approval.ID, "owner", approval.RequestDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Dispatch(h.ctx, first.ID, h.attempt.ID); !errors.Is(err, domain.ErrOutcomeUnknown) {
		t.Fatalf("dispatch = %v, want ErrOutcomeUnknown", err)
	}
	h.clock.Advance(2 * time.Hour)
	settled, err := operations.NewReconciler(h.svc).Reconcile(h.ctx, first.ID)
	if err != nil || settled.State != domain.OperationConfirmedNoEffect {
		t.Fatalf("reconcile = %+v %v, want CONFIRMED_NO_EFFECT", settled, err)
	}

	next := replacementAttempt(t, h, h.attempt.ID)
	retried := preparePurchase(t, h, next.ID, "consumed-no-effect", 1)
	if retried.ApprovalID == "" || retried.ApprovalID == first.ApprovalID {
		t.Fatalf("retried approval %q, want a fresh request after CONSUMED", retried.ApprovalID)
	}
}

func TestSameFingerprintDifferentCanonicalJSONIsNotAdopted(t *testing.T) {
	h := newHarness(t)
	requireOwner(h, "owner")

	first := preparePurchase(t, h, h.attempt.ID, "diff-canonical", 1)
	loaded, err := h.svc.Operation(h.ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	otherDigest, err := operations.OperationApprovalDigest(
		h.provider.Name(), loaded.DescriptorType, first.EffectSlotID,
		first.IntentFingerprint, first.IntentRevision, []byte(`{"sku":"sku-1","quantity":2}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	other, err := h.approvals.Create(h.ctx, approvals.CreateRequest{
		SubjectKind: "EXTERNAL_OPERATION", SubjectID: first.ID, RequestDigest: otherDigest,
		PolicyDecisionID: "decision-other-canonical", RequiredApprovers: []domain.ID{"owner"},
		RequestedBy: h.attempt.ID, ExpiresAt: h.clock.Now().UTC().Add(operations.DefaultApprovalTTL),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB().ExecContext(h.ctx,
		`UPDATE external_operations SET approval_id = ? WHERE operation_id = ?`, other.ID, first.ID,
	); err != nil {
		t.Fatal(err)
	}

	next := replacementAttempt(t, h, h.attempt.ID)
	second := preparePurchase(t, h, next.ID, "diff-canonical", 1)
	if second.ApprovalID == "" || second.ApprovalID == other.ID || second.ApprovalID == first.ApprovalID {
		t.Fatalf("second approval %q, want a fresh request when canonical JSON digest differs", second.ApprovalID)
	}
}

func TestConsumeRaceLoserGetsFreshApproval(t *testing.T) {
	h := newHarness(t)
	requireOwner(h, "owner")

	first := preparePurchase(t, h, h.attempt.ID, "consume-race", 1)
	approval, err := h.approvals.Get(h.ctx, first.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.approvals.Approve(h.ctx, approval.ID, "owner", approval.RequestDigest); err != nil {
		t.Fatal(err)
	}

	loser := replacementAttempt(t, h, h.attempt.ID)
	loserOp := preparePurchase(t, h, loser.ID, "consume-race", 1)
	if loserOp.ApprovalID != first.ApprovalID {
		t.Fatalf("loser adopted %q, want %q", loserOp.ApprovalID, first.ApprovalID)
	}

	// Winner consumes the approval before the loser reaches DispatchWithGuard.
	if err := h.store.WithTx(h.ctx, func(tx *sql.Tx) error {
		return h.approvals.ConsumeInTx(h.ctx, tx, approval.ID, approval.RequestDigest)
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.svc.Dispatch(h.ctx, loserOp.ID, loser.ID); !errors.Is(err, domain.ErrPolicyDenied) {
		t.Fatalf("loser dispatch = %v, want ErrPolicyDenied", err)
	}
	if h.provider.DispatchCount() != 0 {
		t.Fatalf("dispatch count = %d, want 0", h.provider.DispatchCount())
	}

	retried := preparePurchase(t, h, loser.ID, "consume-race", 1)
	if retried.ApprovalID == "" || retried.ApprovalID == first.ApprovalID {
		t.Fatalf("retried approval %q, want a fresh approval for the race loser", retried.ApprovalID)
	}
}

func TestLiveAttemptDoesNotReuseExpiredApproval(t *testing.T) {
	h := newHarness(t)
	requireOwner(h, "owner")

	first := preparePurchase(t, h, h.attempt.ID, "live-expiry", 1)
	h.clock.Advance(operations.DefaultApprovalTTL - 20*time.Minute)

	// Start a 1 h Attempt while 20 min remain on the approval (above the 15 min floor).
	live := replacementAttempt(t, h, h.attempt.ID)
	adopted := preparePurchase(t, h, live.ID, "live-expiry", 1)
	if adopted.ApprovalID != first.ApprovalID {
		t.Fatalf("adopted approval %q, want %q", adopted.ApprovalID, first.ApprovalID)
	}

	// Advance past approval expiry while the Attempt's 1 h lease is still active.
	h.clock.Advance(25 * time.Minute)
	reprepared := preparePurchase(t, h, live.ID, "live-expiry", 1)
	if reprepared.ID == adopted.ID || reprepared.ApprovalID == "" || reprepared.ApprovalID == first.ApprovalID {
		t.Fatalf("reprepared %+v, want a cancelled stale op and fresh approval after expiry", reprepared)
	}
}

func TestNewerRejectedApprovalBlocksOlderPendingAdoption(t *testing.T) {
	h := newHarness(t)
	requireOwner(h, "owner")

	first := preparePurchase(t, h, h.attempt.ID, "no-resurrect", 1)

	requireOwner(h, "owner", "second-owner")
	secondAttempt := replacementAttempt(t, h, h.attempt.ID)
	second := preparePurchase(t, h, secondAttempt.ID, "no-resurrect", 1)
	if second.ApprovalID == first.ApprovalID {
		t.Fatalf("second approval %q should differ after policy change", second.ApprovalID)
	}
	record, err := h.approvals.Get(h.ctx, second.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.approvals.Reject(h.ctx, second.ApprovalID, "owner", record.RequestDigest); err != nil {
		t.Fatal(err)
	}

	// Even if policy reverts, the newer rejected request blocks resurrecting the older pending one.
	requireOwner(h, "owner")
	thirdAttempt := replacementAttempt(t, h, secondAttempt.ID)
	third := preparePurchase(t, h, thirdAttempt.ID, "no-resurrect", 1)
	if third.ApprovalID == "" || third.ApprovalID == first.ApprovalID || third.ApprovalID == second.ApprovalID {
		t.Fatalf("third approval %q, want a fresh request", third.ApprovalID)
	}
}
