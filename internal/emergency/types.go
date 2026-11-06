package emergency

import (
	"errors"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

var (
	// ErrBreakGlassFrozen is returned when execution is attempted while Break Glass is FROZEN.
	ErrBreakGlassFrozen = errors.New("system is frozen under emergency break glass")

	// ErrSafeModeRestricted is returned when restricted operations are attempted under Dead Man SAFE_MODE.
	ErrSafeModeRestricted = errors.New("operation prohibited under safe mode profile")

	// ErrUnauthorizedEmergencyCaller is returned when an entity other than Owner/Root attempts emergency control.
	ErrUnauthorizedEmergencyCaller = errors.New("caller is not authorized to invoke emergency controls")

	// ErrInvalidEmergencyState is returned when an invalid state transition is attempted.
	ErrInvalidEmergencyState = errors.New("invalid emergency control state transition")
)

type BreakGlassState string

const (
	BreakGlassArmed    BreakGlassState = "ARMED"
	BreakGlassFrozen   BreakGlassState = "FROZEN"
	BreakGlassRestored BreakGlassState = "RESTORED"
)

type DeadManState string

const (
	DeadManNominal  DeadManState = "NOMINAL"
	DeadManSafeMode DeadManState = "SAFE_MODE"
)

type EmergencyEventType string

const (
	EventBreakGlassTriggered EmergencyEventType = "BREAK_GLASS_TRIGGERED"
	EventSystemResumed       EmergencyEventType = "SYSTEM_RESUMED"
	EventHeartbeatRecorded   EmergencyEventType = "HEARTBEAT_RECORDED"
	EventSafeModeTriggered   EmergencyEventType = "SAFE_MODE_TRIGGERED"
	EventSafeModeCleared     EmergencyEventType = "SAFE_MODE_CLEARED"
)

type BreakGlassStatus struct {
	State               BreakGlassState
	FrozenAt            time.Time
	FrozenByPrincipalID domain.ID
	FreezeReason        string
	RestoredAt          time.Time
	RestoredByPrincipal domain.ID
	UpdatedAt           time.Time
}

type DeadManStatus struct {
	State               DeadManState
	LastHeartbeatAt     time.Time
	HeartbeatTimeoutSec int
	SafeModeTriggeredAt time.Time
	UpdatedAt           time.Time
}

type EmergencyAuditEvent struct {
	EventID     domain.ID
	EventType   EmergencyEventType
	PrincipalID domain.ID
	Reason      string
	CreatedAt   time.Time
}
