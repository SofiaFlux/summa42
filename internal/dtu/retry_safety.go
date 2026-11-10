package dtu

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

var (
	// ErrUnresolvedOperationBlocked is returned when an operation is in OUTCOME_UNKNOWN, DISPATCHED,
	// or PREPARED-by-another-attempt state, blocking retry or replacement dispatch until resolved.
	ErrUnresolvedOperationBlocked = errors.New("unresolved operation in OUTCOME_UNKNOWN state blocks retry/dispatch until reconciled")

	// ErrOperationAlreadySucceeded is returned when an operation has already reached SUCCEEDED status.
	ErrOperationAlreadySucceeded = errors.New("operation has already succeeded and cannot be re-dispatched")

	// ErrOperationNotFound is returned when an operation key is not registered.
	ErrOperationNotFound = errors.New("operation not found")

	// ErrInvalidTransition is returned when a requested state transition is illegal.
	ErrInvalidTransition = errors.New("invalid operation lifecycle state transition")

	// ErrStaleOperationOwner is returned when an attempt/operation ID does not match the active claim owner.
	ErrStaleOperationOwner = errors.New("operation ownership is stale or mismatched")

	// ErrReconciliationIndeterminate is returned when external reconciliation cannot definitively confirm or deny remote effect application.
	ErrReconciliationIndeterminate = errors.New("reconciliation returned indeterminate outcome; operation remains OUTCOME_UNKNOWN")
)

// OpStatus defines the state lifecycle of a logical external operation.
type OpStatus string

const (
	OpStatusPrepared       OpStatus = "PREPARED"
	OpStatusDispatched     OpStatus = "DISPATCHED"
	OpStatusOutcomeUnknown OpStatus = "OUTCOME_UNKNOWN"
	OpStatusSucceeded      OpStatus = "SUCCEEDED"
	OpStatusFailed         OpStatus = "FAILED"
)

// LogicalOperation tracks lifecycle and reconciliation invariants of an external operation.
type LogicalOperation struct {
	LogicalKey             string    `json:"logical_key"`
	OperationID            domain.ID `json:"operation_id"`
	TaskID                 domain.ID `json:"task_id"`
	AttemptID              domain.ID `json:"attempt_id"`
	State                  OpStatus  `json:"state"`
	ReconciliationRequired bool      `json:"reconciliation_required"`
	DispatchedAt           time.Time `json:"dispatched_at,omitempty"`
	ReconciledAt           time.Time `json:"reconciled_at,omitempty"`
}

// RetrySafetyGuard enforces that OUTCOME_UNKNOWN operations cannot be re-dispatched until reconciled.
type RetrySafetyGuard struct {
	mu         sync.RWMutex
	operations map[string]*LogicalOperation
}

// NewRetrySafetyGuard constructs a new thread-safe RetrySafetyGuard.
func NewRetrySafetyGuard() *RetrySafetyGuard {
	return &RetrySafetyGuard{
		operations: make(map[string]*LogicalOperation),
	}
}

// Prepare attempts to register or claim a logical slot for dispatch by a task attempt.
// Invariant: replacement attempts cannot overwrite or claim a slot that is in PREPARED (by another attempt),
// DISPATCHED, or OUTCOME_UNKNOWN until it has been explicitly reconciled or failed.
func (g *RetrySafetyGuard) Prepare(logicalKey string, opID domain.ID, taskID domain.ID, attemptID domain.ID) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	op, exists := g.operations[logicalKey]
	if exists {
		if op.ReconciliationRequired || op.State == OpStatusOutcomeUnknown || op.State == OpStatusDispatched {
			return ErrUnresolvedOperationBlocked
		}
		if op.State == OpStatusPrepared && op.AttemptID != attemptID {
			// Fencing: slot is already claimed by an active attempt that hasn't dispatched or yielded yet
			return ErrUnresolvedOperationBlocked
		}
		if op.State == OpStatusSucceeded {
			return ErrOperationAlreadySucceeded
		}
	}

	g.operations[logicalKey] = &LogicalOperation{
		LogicalKey:             logicalKey,
		OperationID:            opID,
		TaskID:                 taskID,
		AttemptID:              attemptID,
		State:                  OpStatusPrepared,
		ReconciliationRequired: false,
	}
	return nil
}

// RecordDispatched marks the logical operation as in-flight dispatched.
// Optional expectedOwner (operationID) provides fencing against superseded workers.
func (g *RetrySafetyGuard) RecordDispatched(logicalKey string, expectedOwner ...domain.ID) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	op, exists := g.operations[logicalKey]
	if !exists {
		return ErrOperationNotFound
	}
	if len(expectedOwner) > 0 && op.OperationID != expectedOwner[0] {
		return ErrStaleOperationOwner
	}
	if op.State == OpStatusOutcomeUnknown {
		return ErrUnresolvedOperationBlocked
	}
	if op.State == OpStatusSucceeded {
		return ErrOperationAlreadySucceeded
	}
	if op.State != OpStatusPrepared && op.State != OpStatusDispatched {
		return fmt.Errorf("%w: cannot dispatch from state %s", ErrInvalidTransition, op.State)
	}

	op.State = OpStatusDispatched
	op.DispatchedAt = time.Now()
	return nil
}

// RecordOutcomeUnknown transitions the operation into OUTCOME_UNKNOWN with reconciliation_required = true.
func (g *RetrySafetyGuard) RecordOutcomeUnknown(logicalKey string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	op, exists := g.operations[logicalKey]
	if !exists {
		return ErrOperationNotFound
	}
	if op.State == OpStatusSucceeded {
		return ErrOperationAlreadySucceeded
	}
	if op.State != OpStatusDispatched && op.State != OpStatusOutcomeUnknown {
		return fmt.Errorf("%w: cannot transition to OUTCOME_UNKNOWN from %s", ErrInvalidTransition, op.State)
	}

	op.State = OpStatusOutcomeUnknown
	op.ReconciliationRequired = true
	return nil
}

// CanDispatch evaluates whether a logical operation is eligible for dispatch or retry.
func (g *RetrySafetyGuard) CanDispatch(logicalKey string) (bool, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	op, exists := g.operations[logicalKey]
	if !exists {
		return true, nil
	}
	if op.ReconciliationRequired || op.State == OpStatusOutcomeUnknown || op.State == OpStatusDispatched {
		return false, ErrUnresolvedOperationBlocked
	}
	if op.State == OpStatusSucceeded {
		return false, ErrOperationAlreadySucceeded
	}
	return true, nil
}

// Get returns a defensive copy of the logical operation state.
func (g *RetrySafetyGuard) Get(logicalKey string) (LogicalOperation, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	op, exists := g.operations[logicalKey]
	if !exists {
		return LogicalOperation{}, false
	}
	return *op, true
}

// Reconcile performs external reconciliation against the environment provider for the given logical effect.
// It executes provider I/O outside the write lock to prevent deadlocks and blocking reads.
func (g *RetrySafetyGuard) Reconcile(ctx context.Context, logicalKey string, provider EnvironmentProvider) (*LogicalOperation, error) {
	if provider == nil {
		return nil, errors.New("provider cannot be nil")
	}

	// 1. Validate operation exists under read lock
	g.mu.RLock()
	_, exists := g.operations[logicalKey]
	g.mu.RUnlock()
	if !exists {
		return nil, ErrOperationNotFound
	}

	// 2. Perform provider I/O outside guard lock
	req := InvocationRequest{
		Action: "reconcile",
		Payload: map[string]any{
			"target_key": logicalKey,
		},
	}
	res, err := provider.Execute(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("reconciliation provider execution failed: %w", err)
	}

	// 3. Re-acquire write lock and evaluate definitive outcome
	g.mu.Lock()
	defer g.mu.Unlock()

	op, exists := g.operations[logicalKey]
	if !exists {
		return nil, ErrOperationNotFound
	}

	status, _ := res.Data["status"].(string)

	// Invariant: ONLY explicit confirmation of application or definitive absence of effect transitions state.
	// Indeterminate responses MUST keep OUTCOME_UNKNOWN and reconciliation_required = true.
	if !res.Success {
		return nil, fmt.Errorf("%w: reconciliation query response indicated failure: %s", ErrReconciliationIndeterminate, res.Error)
	}

	switch status {
	case "APPLIED", "SUCCEEDED":
		op.State = OpStatusSucceeded
		op.ReconciliationRequired = false
		op.ReconciledAt = time.Now()
	case "NOT_FOUND", "FAILED", "CONFIRMED_NO_EFFECT":
		op.State = OpStatusFailed
		op.ReconciliationRequired = false
		op.ReconciledAt = time.Now()
	default:
		// Indeterminate outcome (e.g. "UNKNOWN", "PENDING", or missing status)
		op.State = OpStatusOutcomeUnknown
		op.ReconciliationRequired = true
		return nil, fmt.Errorf("%w: received status %q", ErrReconciliationIndeterminate, status)
	}

	opCopy := *op
	return &opCopy, nil
}
