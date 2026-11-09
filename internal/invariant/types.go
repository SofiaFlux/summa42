package invariant

import (
	"context"
	"errors"
	"time"

	"github.com/SofiaFlux/summa42/internal/attention"
	"github.com/SofiaFlux/summa42/internal/containment"
	"github.com/SofiaFlux/summa42/internal/domain"
)

var (
	ErrInvariantNotFound   = errors.New("business invariant not found")
	ErrInvalidRuleType     = errors.New("invalid invariant rule type")
	ErrInvalidTargetKind   = errors.New("invalid invariant target kind")
	ErrInvalidOperator     = errors.New("invalid invariant rule operator")
	ErrMissingRuleConfig   = errors.New("missing or incomplete rule configuration")
	ErrMissingActionConfig = errors.New("missing or incomplete action configuration")
)

type RuleType string

const (
	RuleThreshold     RuleType = "THRESHOLD"
	RuleExpiration    RuleType = "EXPIRATION"
	RuleRedundancy    RuleType = "REDUNDANCY"
	RuleExposureLimit RuleType = "EXPOSURE_LIMIT"
	RuleCustom        RuleType = "CUSTOM"
)

func (r RuleType) Valid() bool {
	switch r {
	case RuleThreshold, RuleExpiration, RuleRedundancy, RuleExposureLimit, RuleCustom:
		return true
	default:
		return false
	}
}

type Severity string

const (
	SeverityWarning  Severity = "WARNING"
	SeverityBreach   Severity = "BREACH"
	SeverityCritical Severity = "CRITICAL"
)

type ActionType string

const (
	ActionRemediationTask  ActionType = "REMEDIATION_TASK"
	ActionEscalationNotice ActionType = "ESCALATION_NOTICE"
	ActionContainmentAlert ActionType = "CONTAINMENT_ALERT"
	ActionCombined         ActionType = "COMBINED"
)

type EvaluationStatus string

const (
	StatusNominal           EvaluationStatus = "OK"
	StatusApproachingBreach EvaluationStatus = "APPROACHING_BREACH"
	StatusBreached          EvaluationStatus = "BREACHED"
)

type RuleConfig struct {
	MetricPath              string  `json:"metric_path,omitempty"`
	Operator                string  `json:"operator,omitempty"` // "<", "<=", ">", ">=", "==", "!="
	Threshold               float64 `json:"threshold,omitempty"`
	LeadWindowSeconds       int64   `json:"lead_window_seconds,omitempty"`
	MinRedundancy           int     `json:"min_redundancy,omitempty"`
	SevereExposureThreshold int64   `json:"severe_exposure_threshold,omitempty"`
}

type ActionConfig struct {
	RemediationTaskTitle          string                     `json:"remediation_task_title,omitempty"`
	RemediationTaskStatement      string                     `json:"remediation_task_statement,omitempty"`
	RemediationTaskTimeoutSeconds int                        `json:"remediation_task_timeout_seconds,omitempty"`
	RemediationAcceptanceCriteria []string                   `json:"remediation_acceptance_criteria,omitempty"`
	AttentionCategory             attention.DecisionCategory `json:"attention_category,omitempty"`
	AttentionUrgency              attention.UrgencyLevel     `json:"attention_urgency,omitempty"`
	ContainmentStep               containment.LadderStep     `json:"containment_step,omitempty"`
	ContainmentSignalName         string                     `json:"containment_signal_name,omitempty"`
}

type BusinessInvariant struct {
	InvariantID  domain.ID    `json:"invariant_id"`
	Name         string       `json:"name"`
	Description  string       `json:"description"`
	TargetKind   string       `json:"target_kind"` // "COMPANY" or company.EntityKind or "*"
	RuleType     RuleType     `json:"rule_type"`
	RuleConfig   RuleConfig   `json:"rule_config"`
	Severity     Severity     `json:"severity"`
	ActionType   ActionType   `json:"action_type"`
	ActionConfig ActionConfig `json:"action_config"`
	Enabled      bool         `json:"enabled"`
	CreatedAt    time.Time    `json:"created_at"`
	UpdatedAt    time.Time    `json:"updated_at"`
}

type EvaluationRecord struct {
	EvaluationID      domain.ID        `json:"evaluation_id"`
	InvariantID       domain.ID        `json:"invariant_id"`
	EntityID          domain.ID        `json:"entity_id,omitempty"`
	Status            EvaluationStatus `json:"status"`
	ObservedValue     map[string]any   `json:"observed_value"`
	EvidenceID        domain.ID        `json:"evidence_id,omitempty"`
	TriggeredTaskID   domain.ID        `json:"triggered_task_id,omitempty"`
	TriggeredPacketID domain.ID        `json:"triggered_packet_id,omitempty"`
	ContainmentSignal string           `json:"containment_signal,omitempty"`
	EvaluatedAt       time.Time        `json:"evaluated_at"`
}

// ContainmentEmitter allows the invariant engine to emit containment signals
// without coupling Kernel containment internals to company domain types.
type ContainmentEmitter interface {
	ApplySignal(ctx context.Context, attemptID, taskID domain.ID, signalName, enforcedBy string, step containment.LadderStep, details map[string]any) error
}

// AttentionEmitter allows the invariant engine to emit owner attention packets.
type AttentionEmitter interface {
	SubmitDecisionGroup(ctx context.Context, req attention.SubmitGroupRequest) (attention.DecisionPacket, error)
}
