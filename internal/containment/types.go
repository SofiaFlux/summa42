package containment

import (
	"errors"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

var (
	// ErrAttemptSuspectedRogue is returned when operations/effects are attempted under a SUSPECTED_ROGUE attempt.
	ErrAttemptSuspectedRogue = errors.New("attempt is quarantined as SUSPECTED_ROGUE: authority revoked")

	// ErrAttemptRestricted is returned when restricted operations are attempted under RESTRICT ladder step.
	ErrAttemptRestricted = errors.New("operation prohibited under attempt restriction")

	// ErrSelfClearDenied is returned when a worker attempts to self-clear its rogue state.
	ErrSelfClearDenied = errors.New("rogue state cannot be cleared by the worker or attempt itself")

	// ErrAttemptNotFound is returned when the containment record does not exist.
	ErrAttemptNotFound = errors.New("attempt containment record not found")

	// ErrInvalidLadderTransition is returned on invalid ladder progression.
	ErrInvalidLadderTransition = errors.New("invalid containment ladder transition")
)

type LadderStep string

const (
	StepObserve   LadderStep = "OBSERVE"
	StepWarn      LadderStep = "WARN"
	StepRestrict  LadderStep = "RESTRICT"
	StepTerminate LadderStep = "TERMINATE"
)

type LadderState string

const (
	StateNominal        LadderState = "NOMINAL"
	StateWarning        LadderState = "WARNING"
	StateRestricted     LadderState = "RESTRICTED"
	StateSuspectedRogue LadderState = "SUSPECTED_ROGUE"
)

type ContainmentRecord struct {
	AttemptID           domain.ID
	TaskID              domain.ID
	LadderState         LadderState
	ViolationSignals    []string
	InvestigationTaskID domain.ID
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type ContainmentEvent struct {
	EventID     domain.ID
	AttemptID   domain.ID
	TaskID      domain.ID
	LadderStep  LadderStep
	SignalName  string
	DetailsJSON string
	EnforcedBy  string
	CreatedAt   time.Time
}
