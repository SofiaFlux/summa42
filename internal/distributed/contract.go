package distributed

import (
	"context"
	"errors"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

var (
	// ErrCarrierUnavailable indicates the distributed carrier is offline or unreachable.
	ErrCarrierUnavailable = errors.New("distributed carrier unavailable")
	// ErrStaleFence indicates the attempt's fence generation is behind the canonical active fence.
	ErrStaleFence = errors.New("stale attempt rejected: fence generation is outdated")
	// ErrLeaseExpired indicates the attempt's lease deadline has elapsed.
	ErrLeaseExpired = errors.New("lease expired: dispatch rejected")
	// ErrBillingExposureBlocked indicates an unresolved billing exposure prevents committing further spend.
	ErrBillingExposureBlocked = errors.New("unresolved billing exposure blocks overcommit")
	// ErrDuplicateCommit indicates a carrier retry was blocked to prevent duplicate external side-effects.
	ErrDuplicateCommit = errors.New("blind retry rejected: external operation already confirmed")
	// ErrIncompleteManifest indicates a crash occurred before the completion manifest was recorded.
	ErrIncompleteManifest = errors.New("crash before completion manifest: execution incomplete")
)

// DispatchAck represents carrier acknowledgement of message delivery.
type DispatchAck struct {
	CarrierID    string
	DispatchedAt time.Time
	MessageID    string
	Accepted     bool
}

// AttemptDispatchEnvelope encapsulates an attempt for distributed carrier transmission.
type AttemptDispatchEnvelope struct {
	CollectiveID    domain.ID
	CubeID          domain.ID
	TaskID          domain.ID
	AttemptID       domain.ID
	FenceGeneration int64
	LeaseDeadline   time.Time
	IdempotencyKey  string
	Payload         []byte
}

// EffectStatus represents the state of an external side-effect slot.
type EffectStatus string

const (
	EffectStatusPrepared   EffectStatus = "PREPARED"
	EffectStatusDispatched EffectStatus = "DISPATCHED"
	EffectStatusConfirmed  EffectStatus = "CONFIRMED"
	EffectStatusUnknown    EffectStatus = "OUTCOME_UNKNOWN"
	EffectStatusFailed     EffectStatus = "FAILED"
)

// EffectSlotRecord tracks an idempotent external operation slot across attempt lifecycles.
type EffectSlotRecord struct {
	SlotKey       string
	TaskID        domain.ID
	AttemptID     domain.ID
	Status        EffectStatus
	CostCents     int64
	ExecutionHash string
	ConfirmedAt   *time.Time
}

// EffectReconciliationResult records the outcome of reconciling an unsettled external effect.
type EffectReconciliationResult struct {
	SlotKey       string
	PriorStatus   EffectStatus
	NewStatus     EffectStatus
	Reconciled    bool
	ExecutionHash string
}

// DistributedCarrier defines the provider-neutral transport and activation seam.
// Invariant: "Meeseek decides; Carrier carries".
type DistributedCarrier interface {
	Name() string
	Available(ctx context.Context) bool
	Dispatch(ctx context.Context, env AttemptDispatchEnvelope) (DispatchAck, error)
	ReconcilePendingEffects(ctx context.Context, taskID domain.ID) ([]EffectReconciliationResult, error)
}
