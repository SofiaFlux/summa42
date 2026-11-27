package domain

import "errors"

var (
	ErrStaleAttempt    = errors.New("stale attempt")
	ErrLeaseInactive   = errors.New("lease inactive")
	ErrPolicyDenied    = errors.New("policy denied")
	ErrInvalidPurpose  = errors.New("invalid purpose")
	ErrIntentConflict  = errors.New("intent conflict")
	ErrBudgetExceeded  = errors.New("budget exceeded")
	ErrOutcomeUnknown  = errors.New("external operation outcome unknown")
	ErrProjectPaused   = errors.New("project is paused")
	ErrProjectArchived = errors.New("project is archived")
)
