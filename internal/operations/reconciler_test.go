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
	now := time.Date(2026, 9, 15, 15, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
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
	if _, err := reconciler.ReconcilePending(h.ctx, 0); err != nil {
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

func forceDispatched(t *testing.T, h *harness, id domain.ID, at time.Time) {
	t.Helper()
	if _, err := h.store.DB().ExecContext(h.ctx, `
		UPDATE external_operations
		SET state = 'DISPATCHED', dispatcher_claim = 'crash-' || operation_id, dispatch_policy_decision_id = 'd',
		    dispatch_policy_set_id = 'policy_test', dispatch_policy_set_hash = 'policy-v1',
		    dispatch_policy_capabilities_hash = 'caps_test', dispatched_at = ?
		WHERE operation_id = ?`, at.UTC().Format(time.RFC3339Nano), id,
	); err != nil {
		t.Fatal(err)
	}
}

func opState(t *testing.T, h *harness, id domain.ID) (domain.OperationState, domain.ReservationState) {
	t.Helper()
	var st domain.OperationState
	var rs domain.ReservationState
	if err := h.store.DB().QueryRowContext(h.ctx, `
		SELECT o.state, r.state FROM external_operations o
		JOIN resource_reservations r ON r.reservation_id = o.reservation_id WHERE o.operation_id = ?`, id,
	).Scan(&st, &rs); err != nil {
		t.Fatal(err)
	}
	return st, rs
}

func TestReconcilePendingSkipsUnregisteredProviderAndContinues(t *testing.T) {
	h := newHarness(t)
	old := h.clock.Now().Add(-time.Hour)
	ghost := preparePurchase(t, h, h.attempt.ID, "ghost", 1)
	known := preparePurchase(t, h, h.attempt.ID, "known", 2)
	forceDispatched(t, h, ghost.ID, old)
	forceDispatched(t, h, known.ID, old)
	if _, err := h.store.DB().ExecContext(h.ctx, `UPDATE external_operations SET provider = 'ghost' WHERE operation_id = ?`, ghost.ID); err != nil {
		t.Fatal(err)
	}
	h.provider.outcomes[known.ID] = operations.ProviderOutcome{State: domain.OperationConfirmedEffect, ProviderReference: "r", ActualCost: 20}

	res, err := operations.NewReconciler(h.svc).ReconcilePending(h.ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 1 || res.Reconciled != 1 {
		t.Fatalf("result = %+v, want 1 skipped 1 reconciled", res)
	}
	if st, rs := opState(t, h, ghost.ID); st != domain.OperationDispatched || rs != domain.ReservationHeld {
		t.Fatalf("ghost = %s/%s, want DISPATCHED/HELD (untouched)", st, rs)
	}
	if st, _ := opState(t, h, known.ID); st != domain.OperationConfirmedEffect {
		t.Fatalf("known = %s, want CONFIRMED_EFFECT", st)
	}
	if _, err := operations.NewReconciler(h.svc).Reconcile(h.ctx, ghost.ID); !errors.Is(err, operations.ErrProviderNotRegistered) {
		t.Fatalf("Reconcile ghost = %v, want ErrProviderNotRegistered", err)
	}
	if st, rs := opState(t, h, ghost.ID); st != domain.OperationDispatched || rs != domain.ReservationHeld {
		t.Fatalf("ghost after Reconcile = %s/%s, want untouched", st, rs)
	}
}

func TestReconcilePendingSkipsFreshDispatchedWithinGrace(t *testing.T) {
	h := newHarness(t)
	op := preparePurchase(t, h, h.attempt.ID, "fresh", 1)
	forceDispatched(t, h, op.ID, h.clock.Now().Add(-time.Minute))
	rec := operations.NewReconciler(h.svc)

	res, err := rec.ReconcilePending(h.ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 1 || res.Reconciled != 0 || h.provider.LookupCount() != 0 {
		t.Fatalf("result = %+v lookups = %d, want skipped without lookup", res, h.provider.LookupCount())
	}
	if st, rs := opState(t, h, op.ID); st != domain.OperationDispatched || rs != domain.ReservationHeld {
		t.Fatalf("fresh op = %s/%s, want untouched", st, rs)
	}
	h.clock.Advance(2 * time.Hour)
	if res, err = rec.ReconcilePending(h.ctx, time.Hour); err != nil || res.Skipped != 0 {
		t.Fatalf("after grace: %+v %v", res, err)
	}
	if st, _ := opState(t, h, op.ID); st != domain.OperationOutcomeUnknown {
		t.Fatalf("after grace state = %s, want OUTCOME_UNKNOWN (no outcome known)", st)
	}
}
