package decision

import (
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

// DecisionType represents the semantic category of bounded micro-decision.
type DecisionType string

const (
	DecisionTypeRouting               DecisionType = "ROUTING"
	DecisionTypeAcceptanceTriage      DecisionType = "ACCEPTANCE_TRIAGE"
	DecisionTypeRetryEvaluation       DecisionType = "RETRY_EVALUATION"
	DecisionTypeEscalationCheck       DecisionType = "ESCALATION_CHECK"
	DecisionTypeAnomalyClassification DecisionType = "ANOMALY_CLASSIFICATION"
)

func (d DecisionType) Valid() bool {
	switch d {
	case DecisionTypeRouting,
		DecisionTypeAcceptanceTriage,
		DecisionTypeRetryEvaluation,
		DecisionTypeEscalationCheck,
		DecisionTypeAnomalyClassification:
		return true
	default:
		return false
	}
}

// RiskProfile represents the financial or operational risk of the decision.
type RiskProfile string

const (
	RiskProfileLow      RiskProfile = "LOW"
	RiskProfileMedium   RiskProfile = "MEDIUM"
	RiskProfileHigh     RiskProfile = "HIGH"
	RiskProfileCritical RiskProfile = "CRITICAL"
)

func (r RiskProfile) Valid() bool {
	switch r {
	case RiskProfileLow, RiskProfileMedium, RiskProfileHigh, RiskProfileCritical:
		return true
	default:
		return false
	}
}

// ConfidenceKind distinguishes self-reported model guesses from calibrated statistical probabilities.
type ConfidenceKind string

const (
	ConfidenceNone                  ConfidenceKind = "NONE"
	ConfidenceHeuristicScore        ConfidenceKind = "HEURISTIC_SCORE"
	ConfidenceSelfReported          ConfidenceKind = "SELF_REPORTED"
	ConfidenceCalibratedProbability ConfidenceKind = "CALIBRATED_PROBABILITY"
)

func (c ConfidenceKind) Valid() bool {
	switch c {
	case ConfidenceNone,
		ConfidenceHeuristicScore,
		ConfidenceSelfReported,
		ConfidenceCalibratedProbability:
		return true
	default:
		return false
	}
}

// ProviderKind categorizes the underlying cognitive mechanism making the decision.
type ProviderKind string

const (
	ProviderKindRule            ProviderKind = "RULE"
	ProviderKindClassicalML     ProviderKind = "CLASSICAL_ML"
	ProviderKindSpecialistModel ProviderKind = "SPECIALIST_MODEL"
	ProviderKindConstrainedLLM  ProviderKind = "CONSTRAINED_LLM"
)

func (p ProviderKind) Valid() bool {
	switch p {
	case ProviderKindRule,
		ProviderKindClassicalML,
		ProviderKindSpecialistModel,
		ProviderKindConstrainedLLM:
		return true
	default:
		return false
	}
}

// ProviderInfo tracks the provenance of the provider that produced the decision.
type ProviderInfo struct {
	Kind    ProviderKind `json:"kind"`
	Name    string       `json:"name"`
	Version string       `json:"version,omitempty"`
}

// SubjectRef identifies the entity being decided upon (e.g. Task, Attempt, Operation).
type SubjectRef struct {
	Kind string    `json:"kind"`
	ID   domain.ID `json:"id"`
}

// ProviderEligibility sets boundaries on which classes of decision providers are permitted.
type ProviderEligibility struct {
	AllowProbabilistic          bool `json:"allow_probabilistic"`
	AllowRemoteLLM              bool `json:"allow_remote_llm"`
	RequireCalibratedConfidence bool `json:"require_calibrated_confidence"`
}

// DecisionUsage captures the resource and cost footprint of evaluating a decision.
type DecisionUsage struct {
	TokensIn       int   `json:"tokens_in,omitempty"`
	TokensOut      int   `json:"tokens_out,omitempty"`
	CostMicroCents int64 `json:"cost_micro_cents,omitempty"`
}

// DecisionRequest encapsulates a bounded, provider-neutral micro-decision operation.
type DecisionRequest struct {
	DecisionID        domain.ID           `json:"decision_id"`
	DecisionType      DecisionType        `json:"decision_type"`
	SubjectRef        SubjectRef          `json:"subject_ref"`
	AllowedOutcomes   []string            `json:"allowed_outcomes"`
	ContextProjection map[string]any      `json:"context_projection"`
	RiskProfile       RiskProfile         `json:"risk_profile"`
	CostBudgetMax     int64               `json:"cost_budget_max,omitempty"` // in micro-cents
	LatencyBudget     time.Duration       `json:"latency_budget,omitempty"`
	Eligibility       ProviderEligibility `json:"eligibility"`
	CreatedAt         time.Time           `json:"created_at"`
}

// DecisionResult represents the outcome chosen by a decision provider within the allowed envelope.
type DecisionResult struct {
	DecisionID      domain.ID          `json:"decision_id"`
	SelectedOutcome string             `json:"selected_outcome,omitempty"`
	OutcomeScores   map[string]float64 `json:"outcome_scores,omitempty"`
	Confidence      float64            `json:"confidence"`
	ConfidenceKind  ConfidenceKind     `json:"confidence_kind"`
	Abstained       bool               `json:"abstained"`
	AbstainReason   string             `json:"abstain_reason,omitempty"`
	Provider        ProviderInfo       `json:"provider"`
	Latency         time.Duration      `json:"latency,omitempty"`
	Usage           DecisionUsage      `json:"usage"`
	ProjectionHash  string             `json:"projection_hash,omitempty"`
	DecidedAt       time.Time          `json:"decided_at"`
}
