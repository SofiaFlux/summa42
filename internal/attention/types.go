package attention

import (
	"errors"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

var (
	ErrPacketNotFound          = errors.New("decision packet not found")
	ErrNotSovereignDecision    = errors.New("decision is within delegated authority and does not require owner sovereignty")
	ErrAttentionBudgetExceeded = errors.New("estimated attention cost exceeds available attention budget")
	ErrAlreadyResolved         = errors.New("decision packet is already resolved")
	ErrUnauthorized            = errors.New("caller is not authorized sovereign owner")
)

type DecisionCategory string

const (
	CategoryConstitutional      DecisionCategory = "CONSTITUTIONAL"
	CategoryStrategicMission    DecisionCategory = "STRATEGIC_MISSION"
	CategoryExposureThreshold   DecisionCategory = "EXPOSURE_THRESHOLD"
	CategorySafetyOverride      DecisionCategory = "SAFETY_OVERRIDE"
	CategoryDelegatedAutonomous DecisionCategory = "DELEGATED_AUTONOMOUS"
)

type UrgencyLevel string

const (
	UrgencyLow      UrgencyLevel = "LOW"
	UrgencyMedium   UrgencyLevel = "MEDIUM"
	UrgencyHigh     UrgencyLevel = "HIGH"
	UrgencyCritical UrgencyLevel = "CRITICAL"
)

type PacketStatus string

const (
	StatusPending              PacketStatus = "PENDING"
	StatusApproved             PacketStatus = "APPROVED"
	StatusRejected             PacketStatus = "REJECTED"
	StatusAutonomouslyResolved PacketStatus = "AUTONOMOUSLY_RESOLVED"
	StatusExpired              PacketStatus = "EXPIRED"
)

type SubDecision struct {
	ID        domain.ID `json:"id"`
	Type      string    `json:"type"`
	Summary   string    `json:"summary"`
	RiskScore int       `json:"risk_score"`
}

type ProposedAction struct {
	ActionType string         `json:"action_type"`
	Parameters map[string]any `json:"parameters,omitempty"`
}

type DecisionPacket struct {
	PacketID                domain.ID        `json:"packet_id"`
	Title                   string           `json:"title"`
	Category                DecisionCategory `json:"category"`
	Urgency                 UrgencyLevel     `json:"urgency"`
	SovereignRequired       bool             `json:"sovereign_required"`
	Status                  PacketStatus     `json:"status"`
	EstimatedAttentionUnits int              `json:"estimated_attention_units"` // In attention units (e.g. estimated cognitive minutes/effort)
	Summary                 string           `json:"summary"`
	EvidenceIDs             []domain.ID      `json:"evidence_ids"`
	SubDecisions            []SubDecision    `json:"sub_decisions"`
	ProposedAction          ProposedAction   `json:"proposed_action"`
	ResolvedAt              *time.Time       `json:"resolved_at,omitempty"`
	ResolvedByPrincipalID   domain.ID        `json:"resolved_by_principal_id,omitempty"`
	CreatedAt               time.Time        `json:"created_at"`
	UpdatedAt               time.Time        `json:"updated_at"`
}

type AttentionBudget struct {
	DailyBudgetUnits int       `json:"daily_budget_units"`
	UsedBudgetUnits  int       `json:"used_budget_units"`
	PeriodStart      time.Time `json:"period_start"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type CompanyHealthStatus struct {
	ActiveMissionID     domain.ID `json:"active_mission_id"`
	MissionStatement    string    `json:"mission_statement"`
	ActiveWorkflowCases int       `json:"active_workflow_cases"`
	HealthyCubes        int       `json:"healthy_cubes"`
	QuarantinedCubes    int       `json:"quarantined_cubes"`
	SafeModeActive      bool      `json:"safe_mode_active"`
	BreakGlassFrozen    bool      `json:"break_glass_frozen"`
}

type InvariantPressureMetric struct {
	Name        string `json:"name"`
	Level       string `json:"level"` // OK, ELEVATED, CRITICAL
	Description string `json:"description"`
}

type OwnerSummaryProjection struct {
	GeneratedAt           time.Time                 `json:"generated_at"`
	CompanyHealth         CompanyHealthStatus       `json:"company_health"`
	AttentionBudget       AttentionBudget           `json:"attention_budget"`
	AvailableAttention    int                       `json:"available_attention"`
	InvariantPressures    []InvariantPressureMetric `json:"invariant_pressures"`
	UnmitigatedExposure   int64                     `json:"unmitigated_exposure"` // Total unsettled exposure in base currency/units
	PendingSovereignItems []DecisionPacket          `json:"pending_sovereign_items"`
	AutonomousResolutions int                       `json:"autonomous_resolutions_count"`
}
