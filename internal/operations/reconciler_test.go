package operations_test

import (
	"errors"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/operations"
)

func TestReconcilerConfirmsLostAcknowledgementWithoutRedispatch(t *testing.T) {
	h := newHarness(t)
	h.provider.SetLoseAcknowledgement(true)
	op := preparePurchase(t, h, h.attempt.ID, "purchase-primary", 3)

	unknown, err := h.svc.Dispatch(h.ctx, op.ID, h.attempt.ID)
	if !errors.Is(err, domain.ErrOutcomeUnknown) {
		t.Fatalf("expected unknown outcome, got %v", err)
	}
	reconciler := operations.NewReconciler(h.svc)
	resolved, err := reconciler.Reconcile(h.ctx, unknown.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != domain.OperationConfirmedEffect {
		t.Fatalf("state = %s, want CONFIRMED_EFFECT", resolved.State)
	}
	if h.provider.DispatchCount() != 1 {
		t.Fatalf("reconciliation redispatched effect; dispatch count = %d", h.provider.DispatchCount())
	}
	if h.provider.LookupCount() != 1 {
		t.Fatalf("lookup count = %d, want 1", h.provider.LookupCount())
	}
}

func TestStartupRecoveryFindsDispatchedOperationWithoutRedispatch(t *testing.T) {
	h := newHarness(t)
	op := preparePurchase(t, h, h.attempt.ID, "purchase-primary", 1)
	now := time.Date(2026, 9, 15, 16, 1, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if _, err := h.store.DB().ExecContext(h.ctx, `
		UPDATE external_operations
		SET state = 'DISPATCHED',
		    dispatcher_claim = 'crash-before-provider-call',
		    dispatch_policy_decision_id = 'decision_crash',
		    dispatch_policy_set_id = 'policy_test',
		    dispatch_policy_set_hash = 'policy-v1',
		    dispatch_policy_capabilities_hash = 'caps_test',
		    dispatched_at = ?
		WHERE operation_id = ? AND state = 'PREPARED'`, now, op.ID,
	); err != nil {
		t.Fatal(err)
	}

	reconciler := operations.NewReconciler(h.svc)
	if _, err := reconciler.ReconcilePending(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.provider.DispatchCount(); got != 0 {
		t.Fatalf("startup recovery redispatched effect; dispatch count = %d", got)
	}
	if got := h.provider.LookupCount(); got != 1 {
		t.Fatalf("lookup count = %d, want 1", got)
	}

	var stateValue domain.OperationState
	var reconciliationRequired int
	var reservationState domain.ReservationState
	if err := h.store.DB().QueryRowContext(h.ctx, `
		SELECT o.state, o.reconciliation_required, r.state
		FROM external_operations o
		JOIN resource_reservations r ON r.reservation_id = o.reservation_id
		WHERE o.operation_id = ?`, op.ID,
	).Scan(&stateValue, &reconciliationRequired, &reservationState); err != nil {
		t.Fatal(err)
	}
	if stateValue != domain.OperationOutcomeUnknown {
		t.Fatalf("state = %s, want OUTCOME_UNKNOWN", stateValue)
	}
	if reconciliationRequired != 1 {
		t.Fatalf("reconciliation_required = %d, want 1", reconciliationRequired)
	}
	if reservationState != domain.ReservationUnresolved {
		t.Fatalf("reservation state = %s, want UNRESOLVED", reservationState)
	}
}
