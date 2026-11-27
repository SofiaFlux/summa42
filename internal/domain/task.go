package domain

import (
	"encoding/json"
	"time"
)

type PurposeKind string

const (
	PurposeMission               PurposeKind = "MISSION"
	PurposeObligation            PurposeKind = "OBLIGATION"
	PurposeCollectiveMaintenance PurposeKind = "COLLECTIVE_MAINTENANCE"
	PurposeGovernance            PurposeKind = "GOVERNANCE"
	PurposeStrategicPulse        PurposeKind = "STRATEGIC_PULSE"
	PurposeRecovery              PurposeKind = "RECOVERY"
	PurposeOwnerDirective        PurposeKind = "OWNER_DIRECTIVE"
)

func (k PurposeKind) Valid() bool {
	switch k {
	case PurposeMission,
		PurposeObligation,
		PurposeCollectiveMaintenance,
		PurposeGovernance,
		PurposeStrategicPulse,
		PurposeRecovery,
		PurposeOwnerDirective:
		return true
	default:
		return false
	}
}

type PurposeRef struct {
	Kind PurposeKind
	ID   ID
}

type Task struct {
	ID                   ID
	ParentTaskID         ID
	ProjectID            ID
	Purpose              PurposeRef
	TaskClass            string
	Objective            string
	PayloadJSON          json.RawMessage
	IdempotencyKey       string
	State                TaskState
	CurrentAttemptID     ID
	CurrentFence         int64
	AcceptanceCriteria   []string
	RequiredCapabilities []string
	RequiredEnforcement  EnforcementLevel
	AuthorityCeiling     []string
	ResourceEnvelopeID   ID
	Priority             int
	EarliestStart        time.Time
	Deadline             time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
}
