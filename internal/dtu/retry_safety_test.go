package dtu_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/dtu"
)

func TestHostileAckLossTwin_LifecycleAndDroppedAck(t *testing.T) {
	ctx := context.Background()

	twin := dtu.NewHostileAckLossTwin(dtu.ProviderMetadata{
		ServiceID:  "payment-gateway",
		ProviderID: "twin-payment-hostile",
		Version:    "1.0.0",
		Fidelity:   dtu.FidelityT2Adversarial,
	}, dtu.HostileAckLossConfig{
		DropAckActions: map[string]bool{
			"charge": true,
		},
	})

	if err := twin.Reset(ctx, 42); err != nil {
		t.Fatalf("reset failed: %v", err)
	}
	if twin.Seed() != 42 {
		t.Errorf("expected seed 42, got %d", twin.Seed())
	}

	// 1. Initial state
	if twin.ExecutionCount("slot_charge_01") != 0 {
		t.Fatalf("expected 0 executions initially")
	}

	// 2. Dispatch non-idempotent charge
	req := dtu.InvocationRequest{
		Action:         "charge",
		IdempotencyKey: "slot_charge_01",
		Payload: map[string]any{
			"amount_cents": 5000,
			"meta": map[string]any{
				"tag": "initial",
			},
		},
	}

	_, err := twin.Execute(ctx, req)
	// Execution MUST return ErrAckDropped
	if !errors.Is(err, dtu.ErrAckDropped) {
		t.Fatalf("expected ErrAckDropped, got: %v", err)
	}

	// Remote execution count MUST be exactly 1 despite dropped ack
	if twin.ExecutionCount("slot_charge_01") != 1 {
		t.Errorf("expected remote execution count 1, got %d", twin.ExecutionCount("slot_charge_01"))
	}
	if twin.DroppedCount() != 1 {
		t.Errorf("expected 1 dropped ack, got %d", twin.DroppedCount())
	}

	// Snapshot and Restore round-trip
	snap, err := twin.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot failed: %v", err)
	}

	restoredTwin := dtu.NewHostileAckLossTwin(twin.Metadata(), dtu.HostileAckLossConfig{})
	if err := restoredTwin.Restore(ctx, snap); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	if restoredTwin.ExecutionCount("slot_charge_01") != 1 {
		t.Errorf("expected restored execution count 1, got %d", restoredTwin.ExecutionCount("slot_charge_01"))
	}
	if restoredTwin.DroppedCount() != 1 {
		t.Errorf("expected restored dropped count 1, got %d", restoredTwin.DroppedCount())
	}

	// 3. Query reconciliation
	recReq := dtu.InvocationRequest{
		Action: "reconcile",
		Payload: map[string]any{
			"target_key": "slot_charge_01",
		},
	}
	recRes, err := restoredTwin.Execute(ctx, recReq)
	if err != nil {
		t.Fatalf("reconcile query error: %v", err)
	}
	if !recRes.Success {
		t.Fatalf("expected reconcile query to succeed")
	}
	if recRes.Data["status"] != "APPLIED" {
		t.Errorf("expected remote status APPLIED, got %v", recRes.Data["status"])
	}
	if recRes.Data["execution_count"] != 1 {
		t.Errorf("expected execution_count 1, got %v", recRes.Data["execution_count"])
	}
}

func TestRetrySafetyGuard_BlocksRetryWhileOutcomeUnknown(t *testing.T) {
	guard := dtu.NewRetrySafetyGuard()

	taskID := domain.NewID("task")
	att1 := domain.NewID("att-1")
	att2 := domain.NewID("att-2")
	opID := domain.NewID("op-1")
	logicalKey := "slot_transfer_999"

	// 1. Prepare and dispatch under Attempt 1
	if err := guard.Prepare(logicalKey, opID, taskID, att1); err != nil {
		t.Fatalf("prepare failed: %v", err)
	}
	if err := guard.RecordDispatched(logicalKey); err != nil {
		t.Fatalf("record dispatched failed: %v", err)
	}

	// 2. Simulate acknowledgement drop -> mark OUTCOME_UNKNOWN
	if err := guard.RecordOutcomeUnknown(logicalKey); err != nil {
		t.Fatalf("record outcome unknown failed: %v", err)
	}

	// Invariant: OUTCOME_UNKNOWN does not grant retry permission
	canDispatch, err := guard.CanDispatch(logicalKey)
	if canDispatch {
		t.Errorf("expected CanDispatch to be false while OUTCOME_UNKNOWN")
	}
	if !errors.Is(err, dtu.ErrUnresolvedOperationBlocked) {
		t.Errorf("expected ErrUnresolvedOperationBlocked, got: %v", err)
	}

	// 3. Replacement Attempt 2 attempts to prepare the same logical effect
	opID2 := domain.NewID("op-2")
	err = guard.Prepare(logicalKey, opID2, taskID, att2)
	if !errors.Is(err, dtu.ErrUnresolvedOperationBlocked) {
		t.Errorf("replacement attempt should be blocked on unresolved operation, got: %v", err)
	}
}

func TestRetrySafetyGuard_PreparedClaimFencing(t *testing.T) {
	guard := dtu.NewRetrySafetyGuard()

	taskID := domain.NewID("task")
	att1 := domain.NewID("att-1")
	att2 := domain.NewID("att-2")
	op1 := domain.NewID("op-1")
	op2 := domain.NewID("op-2")
	slot := "slot_fencing_check"

	// Attempt 1 claims slot in PREPARED state
	if err := guard.Prepare(slot, op1, taskID, att1); err != nil {
		t.Fatalf("attempt 1 prepare failed: %v", err)
	}

	// Attempt 2 attempts to steal/overwrite slot while still PREPARED by Attempt 1
	err := guard.Prepare(slot, op2, taskID, att2)
	if !errors.Is(err, dtu.ErrUnresolvedOperationBlocked) {
		t.Fatalf("expected ErrUnresolvedOperationBlocked when attempting to claim active PREPARED slot, got %v", err)
	}

	// Attempt 1 can re-prepare idempotently
	if err := guard.Prepare(slot, op1, taskID, att1); err != nil {
		t.Errorf("same attempt should be able to re-prepare idempotently: %v", err)
	}

	// Dispatch with stale owner rejected
	err = guard.RecordDispatched(slot, op2)
	if !errors.Is(err, dtu.ErrStaleOperationOwner) {
		t.Errorf("expected ErrStaleOperationOwner when dispatching with stale opID, got %v", err)
	}

	// Dispatch with legitimate owner succeeds
	if err := guard.RecordDispatched(slot, op1); err != nil {
		t.Fatalf("legitimate dispatch failed: %v", err)
	}
}

func TestRetrySafetyGuard_ReconciliationIndeterminatePreservesUnknown(t *testing.T) {
	ctx := context.Background()
	guard := dtu.NewRetrySafetyGuard()
	slot := "slot_indeterminate"

	mockProvider := dtu.NewMockEnvironmentProvider(dtu.ProviderMetadata{
		ServiceID:  "mock-svc",
		ProviderID: "mock-prov",
		Version:    "1.0.0",
		Fidelity:   dtu.FidelityT1Behavioral,
	}, dtu.EnvModeTwin)

	// Configure mock to return indeterminate status
	mockProvider.SetResponse("reconcile", dtu.InvocationResponse{
		Success: true,
		Data: map[string]any{
			"status": "PENDING",
		},
	})

	guard.Prepare(slot, domain.NewID("op"), domain.NewID("task"), domain.NewID("att"))
	guard.RecordDispatched(slot)
	guard.RecordOutcomeUnknown(slot)

	// Reconcile returns indeterminate outcome
	_, err := guard.Reconcile(ctx, slot, mockProvider)
	if !errors.Is(err, dtu.ErrReconciliationIndeterminate) {
		t.Fatalf("expected ErrReconciliationIndeterminate, got: %v", err)
	}

	// Invariant: OUTCOME_UNKNOWN MUST remain and block dispatch
	can, err := guard.CanDispatch(slot)
	if can || !errors.Is(err, dtu.ErrUnresolvedOperationBlocked) {
		t.Errorf("slot must remain blocked after indeterminate reconcile: can=%v, err=%v", can, err)
	}
	op, _ := guard.Get(slot)
	if op.State != dtu.OpStatusOutcomeUnknown || !op.ReconciliationRequired {
		t.Errorf("expected state OUTCOME_UNKNOWN with reconciliation_required=true, got %+v", op)
	}
}

func TestHostileRetryScenario_CanonicalFlow(t *testing.T) {
	ctx := context.Background()

	// Step 1: Initialize twin and retry safety guard
	twin := dtu.NewHostileAckLossTwin(dtu.ProviderMetadata{
		ServiceID:  "billing",
		ProviderID: "billing-hostile-twin",
		Version:    "1.0.0",
		Fidelity:   dtu.FidelityT2Adversarial,
	}, dtu.HostileAckLossConfig{
		DropAckActions: map[string]bool{"charge_card": true},
	})

	guard := dtu.NewRetrySafetyGuard()

	taskID := domain.NewID("task")
	attempt1 := domain.NewID("att-1")
	attempt2 := domain.NewID("att-2")
	opID1 := domain.NewID("op-1")
	logicalSlotKey := "billing_slot_invoice_88"

	// Step 2: Attempt 1 prepares and dispatches effect
	if err := guard.Prepare(logicalSlotKey, opID1, taskID, attempt1); err != nil {
		t.Fatalf("attempt 1 prepare failed: %v", err)
	}
	if err := guard.RecordDispatched(logicalSlotKey, opID1); err != nil {
		t.Fatalf("attempt 1 record dispatched failed: %v", err)
	}

	// Step 3: Remote effect executes, but acknowledgement is dropped
	req := dtu.InvocationRequest{
		Action:         "charge_card",
		IdempotencyKey: logicalSlotKey,
		Payload: map[string]any{
			"amount": 2500,
			"nested": map[string]any{
				"item": "sku-1",
			},
		},
	}
	_, err := twin.Execute(ctx, req)
	if !errors.Is(err, dtu.ErrAckDropped) {
		t.Fatalf("expected dropped ack error, got %v", err)
	}

	// Step 4: Outcome is recorded as OUTCOME_UNKNOWN with reconciliation_required = true
	if err := guard.RecordOutcomeUnknown(logicalSlotKey); err != nil {
		t.Fatalf("record outcome unknown failed: %v", err)
	}

	// Step 5: Worker 1 terminates. Replacement Attempt 2 starts and tries to re-execute
	opID2 := domain.NewID("op-2")
	err = guard.Prepare(logicalSlotKey, opID2, taskID, attempt2)
	if !errors.Is(err, dtu.ErrUnresolvedOperationBlocked) {
		t.Fatalf("expected Attempt 2 to be blocked from re-dispatching, got: %v", err)
	}

	// Step 6: Verify invariant: remote execution count remains EXACTLY 1 while unresolved
	if twin.ExecutionCount(logicalSlotKey) != 1 {
		t.Fatalf("INVARIANT VIOLATION: external execution count is %d, want exactly 1", twin.ExecutionCount(logicalSlotKey))
	}

	// Step 7: Explicit external reconciliation occurs
	reconciledOp, err := guard.Reconcile(ctx, logicalSlotKey, twin)
	if err != nil {
		t.Fatalf("reconciliation failed: %v", err)
	}
	if reconciledOp.State != dtu.OpStatusSucceeded {
		t.Errorf("expected reconciled state SUCCEEDED, got %s", reconciledOp.State)
	}
	if reconciledOp.ReconciliationRequired {
		t.Errorf("reconciliation_required should be cleared after reconciliation")
	}

	// Step 8: Subsequent dispatch check confirms effect already succeeded
	canDispatch, err := guard.CanDispatch(logicalSlotKey)
	if canDispatch {
		t.Errorf("already succeeded operation cannot be re-dispatched")
	}
	if !errors.Is(err, dtu.ErrOperationAlreadySucceeded) {
		t.Errorf("expected ErrOperationAlreadySucceeded, got: %v", err)
	}

	// Step 9: Final check: external execution count remained 1 throughout the entire lifecycle
	if twin.ExecutionCount(logicalSlotKey) != 1 {
		t.Errorf("final execution count should still be 1, got %d", twin.ExecutionCount(logicalSlotKey))
	}
}

func TestHostileRetryScenario_ReconciledFailureAllowsGovernedRetry(t *testing.T) {
	ctx := context.Background()

	// Twin configured NOT to apply effect on "invalid_action"
	twin := dtu.NewHostileAckLossTwin(dtu.ProviderMetadata{
		ServiceID:  "storage",
		ProviderID: "storage-twin",
		Version:    "1.0.0",
		Fidelity:   dtu.FidelityT2Adversarial,
	}, dtu.HostileAckLossConfig{})

	guard := dtu.NewRetrySafetyGuard()
	logicalKey := "slot_write_missing"

	if err := guard.Prepare(logicalKey, domain.NewID("op"), domain.NewID("task"), domain.NewID("att-1")); err != nil {
		t.Fatalf("prepare failed: %v", err)
	}
	if err := guard.RecordDispatched(logicalKey); err != nil {
		t.Fatalf("record dispatched failed: %v", err)
	}
	if err := guard.RecordOutcomeUnknown(logicalKey); err != nil {
		t.Fatalf("record outcome unknown failed: %v", err)
	}

	// Reconcile against twin (which has not recorded this effect -> NOT_FOUND)
	reconciledOp, err := guard.Reconcile(ctx, logicalKey, twin)
	if err != nil {
		t.Fatalf("reconciliation failed: %v", err)
	}

	// Confirmed that remote effect was NOT applied -> state becomes FAILED
	if reconciledOp.State != dtu.OpStatusFailed {
		t.Errorf("expected state FAILED, got %s", reconciledOp.State)
	}

	// Now that it is reconciled as FAILED, it is eligible for re-prepare and governed retry
	canDispatch, err := guard.CanDispatch(logicalKey)
	if !canDispatch || err != nil {
		t.Errorf("reconciled failed operation should be eligible for new governed dispatch: can=%v, err=%v", canDispatch, err)
	}

	// Replacement attempt re-prepares and dispatches successfully
	newOpID := domain.NewID("op-retry")
	newAttID := domain.NewID("att-retry")
	if err := guard.Prepare(logicalKey, newOpID, domain.NewID("task"), newAttID); err != nil {
		t.Fatalf("re-prepare after reconciled failure failed: %v", err)
	}
	if err := guard.RecordDispatched(logicalKey, newOpID); err != nil {
		t.Fatalf("dispatch after re-prepare failed: %v", err)
	}
}

func TestRetrySafetyGuard_InvalidTransitionsAndNotFound(t *testing.T) {
	ctx := context.Background()
	guard := dtu.NewRetrySafetyGuard()
	unknownKey := "missing_slot"

	// Operations on unregistered key
	if err := guard.RecordDispatched(unknownKey); !errors.Is(err, dtu.ErrOperationNotFound) {
		t.Errorf("expected ErrOperationNotFound on RecordDispatched, got %v", err)
	}
	if err := guard.RecordOutcomeUnknown(unknownKey); !errors.Is(err, dtu.ErrOperationNotFound) {
		t.Errorf("expected ErrOperationNotFound on RecordOutcomeUnknown, got %v", err)
	}
	if _, err := guard.Reconcile(ctx, unknownKey, nil); err == nil {
		t.Errorf("expected error on nil provider")
	}

	// Invalid transition: RecordOutcomeUnknown from PREPARED
	slot := "invalid_trans_slot"
	guard.Prepare(slot, domain.NewID("op"), domain.NewID("task"), domain.NewID("att"))
	if err := guard.RecordOutcomeUnknown(slot); !errors.Is(err, dtu.ErrInvalidTransition) {
		t.Errorf("expected ErrInvalidTransition when transitioning from PREPARED directly to OUTCOME_UNKNOWN, got %v", err)
	}
}

func TestRetrySafetyGuard_ConcurrentAccess(t *testing.T) {
	guard := dtu.NewRetrySafetyGuard()
	logicalKey := "concurrent_slot"

	if err := guard.Prepare(logicalKey, domain.NewID("op"), domain.NewID("task"), domain.NewID("att-1")); err != nil {
		t.Fatalf("prepare failed: %v", err)
	}
	if err := guard.RecordDispatched(logicalKey); err != nil {
		t.Fatalf("record dispatched failed: %v", err)
	}
	if err := guard.RecordOutcomeUnknown(logicalKey); err != nil {
		t.Fatalf("record outcome unknown failed: %v", err)
	}

	const concurrency = 25
	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func(workerID int) {
			defer wg.Done()
			canDispatch, err := guard.CanDispatch(logicalKey)
			if canDispatch || !errors.Is(err, dtu.ErrUnresolvedOperationBlocked) {
				t.Errorf("worker %d allowed dispatch on unresolved operation: canDispatch=%v, err=%v", workerID, canDispatch, err)
			}

			// Attempting to prepare new operation under different attempt must also fail
			err = guard.Prepare(logicalKey, domain.NewID("op-rep"), domain.NewID("task"), domain.NewID("att-rep"))
			if !errors.Is(err, dtu.ErrUnresolvedOperationBlocked) {
				t.Errorf("worker %d prepare should fail with ErrUnresolvedOperationBlocked, got %v", workerID, err)
			}
		}(i)
	}

	wg.Wait()
}
