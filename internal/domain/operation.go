package domain

import "time"

type ExternalOperation struct {
	ID                ID
	TaskID            ID
	AttemptID         ID
	EffectSlotID      ID
	State             OperationState
	IntentFingerprint string
	IntentRevision    int64
	Provider          string
	ProviderReference string
	ReservationID     ID
	ApprovalID        ID
	// CanonicalIntent is the effect slot's canonical intent JSON; Attributes are the persisted caller policy attributes.
	CanonicalIntent []byte
	Attributes      map[string]any
	CreatedAt       time.Time
	DispatchedAt    time.Time
	SettledAt       time.Time
}
