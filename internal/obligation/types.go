package obligation

import (
	"errors"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

var (
	ErrObligationNotFound         = errors.New("obligation not found")
	ErrObligationAlreadyFulfilled = errors.New("obligation is already fulfilled")
	ErrObligationAlreadyBreached  = errors.New("obligation is already breached")
	ErrInvalidObligation          = errors.New("invalid obligation parameters")
)

type ObligationState string

const (
	StatePending   ObligationState = "PENDING"
	StateScheduled ObligationState = "SCHEDULED"
	StateActive    ObligationState = "ACTIVE"
	StateFulfilled ObligationState = "FULFILLED"
	StateBreached  ObligationState = "BREACHED"
	StateCancelled ObligationState = "CANCELLED"
)

func (s ObligationState) Terminal() bool {
	return s == StateFulfilled || s == StateBreached || s == StateCancelled
}

type Consequences struct {
	FinancialPenalty int64  `json:"financial_penalty,omitempty"`
	SLABreach        bool   `json:"sla_breach,omitempty"`
	SuspensionRisk   bool   `json:"suspension_risk,omitempty"`
	Description      string `json:"description,omitempty"`
}

type Obligation struct {
	ID                domain.ID       `json:"obligation_id"`
	Source            string          `json:"source"`
	Statement         string          `json:"statement"`
	Active            bool            `json:"active"`
	DueAt             time.Time       `json:"due_at"`
	GracePeriod       time.Duration   `json:"grace_period"`
	Consequences      Consequences    `json:"consequences"`
	CostOfDelayRate   int64           `json:"cost_of_delay_rate"`
	LeadTime          time.Duration   `json:"lead_time"`
	LifecycleState    ObligationState `json:"lifecycle_state"`
	SynthesizedTaskID domain.ID       `json:"synthesized_task_id,omitempty"`
	EntityID          domain.ID       `json:"entity_id,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
	ResolvedAt        *time.Time      `json:"resolved_at,omitempty"`
}
