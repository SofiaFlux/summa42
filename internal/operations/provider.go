package operations

import (
	"context"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/resources"
)

// IntentDescriptor is a provider-owned, typed description of a consequential
// effect. Providers must reject descriptor types they do not own rather than
// accepting arbitrary model-authored JSON as authoritative intent.
type IntentDescriptor interface {
	DescriptorType() string
}

type CostProfile struct {
	MaxExposure    int64
	Enforceability resources.Enforceability
}

type ProviderOutcome struct {
	State             domain.OperationState
	ProviderReference string
	ActualCost        int64
}

type ProviderDispatchRequest struct {
	OperationID       domain.ID
	TaskID            domain.ID
	EffectSlotID      domain.ID
	IntentFingerprint string
	IntentRevision    int64
	CanonicalIntent   []byte
}

// Provider separates effectful Dispatch from read-only LookupOutcome. This is
// what lets recovery reconcile an uncertain effect without repeating it.
type Provider interface {
	Name() string
	Capability() string
	EnforcementLevel() domain.EnforcementLevel
	AdapterVersion() string
	AdapterVersionSemanticallyRelevant() bool
	CanonicalIntent(IntentDescriptor) ([]byte, error)
	CostProfile(IntentDescriptor) (CostProfile, error)
	Dispatch(context.Context, ProviderDispatchRequest) (ProviderOutcome, error)
	// LookupOutcome is read-only. It MUST NOT return CONFIRMED_NO_EFFECT unless
	// the provider can prove no delayed write can still land (for example a
	// rejected idempotency key); absence of a resource is not such a proof.
	LookupOutcome(context.Context, ProviderDispatchRequest) (ProviderOutcome, error)
	// SettleWindow is how long after dispatch a CONFIRMED_NO_EFFECT lookup is
	// distrusted and the operation stays OUTCOME_UNKNOWN. Zero means the
	// provider can prove absence.
	SettleWindow() time.Duration
}
