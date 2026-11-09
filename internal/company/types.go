package company

import (
	"errors"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

var (
	ErrEntityNotFound          = errors.New("company entity not found")
	ErrInvalidEntityKind       = errors.New("invalid company entity kind")
	ErrMissingEvidence         = errors.New("state transition requires verified task evidence")
	ErrTaskNotAccepted         = errors.New("state transition requires accepted task completion")
	ErrAcceptanceVerdictFailed = errors.New("state transition requires a PASS acceptance verdict")
)

type EntityKind string

const (
	KindCustomer       EntityKind = "CUSTOMER"
	KindContract       EntityKind = "CONTRACT"
	KindCommitment     EntityKind = "COMMITMENT"
	KindAsset          EntityKind = "ASSET"
	KindPolicy         EntityKind = "POLICY"
	KindProductService EntityKind = "PRODUCT_SERVICE"
	KindRisk           EntityKind = "RISK"
)

func (k EntityKind) Valid() bool {
	switch k {
	case KindCustomer, KindContract, KindCommitment, KindAsset,
		KindPolicy, KindProductService, KindRisk:
		return true
	default:
		return false
	}
}

type Entity struct {
	EntityID       domain.ID      `json:"entity_id"`
	Kind           EntityKind     `json:"kind"`
	Name           string         `json:"name"`
	Status         string         `json:"status"`
	Attributes     map[string]any `json:"attributes"`
	ExposureAmount int64          `json:"exposure_amount"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

type StateTransition struct {
	TransitionID   domain.ID      `json:"transition_id"`
	EntityID       domain.ID      `json:"entity_id"`
	TaskID         domain.ID      `json:"task_id"`
	EvidenceID     domain.ID      `json:"evidence_id"`
	TransitionKind string         `json:"transition_kind"`
	PreviousState  map[string]any `json:"previous_state,omitempty"`
	NewState       map[string]any `json:"new_state"`
	RecordedAt     time.Time      `json:"recorded_at"`
}

type ExternalProjection struct {
	ProjectionID domain.ID      `json:"projection_id"`
	SourceSystem string         `json:"source_system"`
	ExternalID   string         `json:"external_id"`
	EntityID     domain.ID      `json:"entity_id"`
	Payload      map[string]any `json:"payload"`
	SyncedAt     time.Time      `json:"synced_at"`
}

type CompanySummary struct {
	TotalEntities       int                `json:"total_entities"`
	EntitiesByKind      map[EntityKind]int `json:"entities_by_kind"`
	TotalExposureAmount int64              `json:"total_exposure_amount"`
	ActiveCommitments   int                `json:"active_commitments"`
	ActiveContracts     int                `json:"active_contracts"`
	ActiveRisks         int                `json:"active_risks"`
}
