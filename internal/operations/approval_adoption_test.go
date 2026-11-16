package operations_test

import (
	"testing"
	"time"

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
