package quarantine

import (
	"errors"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

var (
	// ErrCubeQuarantined is returned when an action is denied because the Cube is quarantined.
	ErrCubeQuarantined = errors.New("cube is quarantined: capability or operation denied")

	// ErrSelfUnquarantineDenied is returned when a quarantined Cube attempts to clear its own quarantine.
	ErrSelfUnquarantineDenied = errors.New("quarantine cannot be cleared by the quarantined cube itself")

	// ErrUnauthorizedPrincipal is returned when an unauthorized actor attempts to manage quarantine.
	ErrUnauthorizedPrincipal = errors.New("principal is not authorized to manage quarantine")

	// ErrProhibitedInQuarantine is returned when a prohibited operation is attempted under quarantine.
	ErrProhibitedInQuarantine = errors.New("operation strictly prohibited under quarantine")

	// ErrCubeNotFound is returned when the specified Cube does not exist.
	ErrCubeNotFound = errors.New("cube not found")

	// ErrInvalidCubeState is returned when an invalid state transition is attempted.
	ErrInvalidCubeState = errors.New("invalid cube state transition")

	// ErrAlreadyQuarantined is returned when attempting to quarantine a Cube that is already quarantined.
	ErrAlreadyQuarantined = errors.New("cube is already quarantined")
)

type CubeState string

const (
	CubeActive      CubeState = "ACTIVE"
	CubeQuarantined CubeState = "QUARANTINED"
	CubeDraining    CubeState = "DRAINING"
	CubeTerminated  CubeState = "TERMINATED"
)

type QuarantineMode string

const (
	PassiveWait QuarantineMode = "PASSIVE_WAIT"
	ActiveRecon QuarantineMode = "ACTIVE_RECON"
)

type Cube struct {
	CubeID           domain.ID
	PrincipalID      domain.ID
	State            CubeState
	QuarantineReason string
	QuarantinedAt    time.Time
	QuarantinedBy    domain.ID
	QuarantineMode   QuarantineMode
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type QuarantineEvent struct {
	EventID               domain.ID
	CubeID                domain.ID
	Action                string // "QUARANTINE" or "UNQUARANTINE"
	Reason                string
	EnforcedByPrincipalID domain.ID
	Mode                  QuarantineMode
	CreatedAt             time.Time
}
