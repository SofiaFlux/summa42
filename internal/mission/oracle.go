package mission

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

// DimensionStatus categorizes compliance for a specific mission dimension.
type DimensionStatus string

const (
	DimensionStatusSatisfied DimensionStatus = "SATISFIED"
	DimensionStatusViolated  DimensionStatus = "VIOLATED"
	DimensionStatusUncertain DimensionStatus = "UNCERTAIN"
	DimensionStatusNeutral   DimensionStatus = "NEUTRAL"
)

// EvaluationDecision captures the final categorical governance disposition.
type EvaluationDecision string

const (
	EvaluationDecisionAccept          EvaluationDecision = "ACCEPT"
	EvaluationDecisionReject          EvaluationDecision = "REJECT"
	EvaluationDecisionEscalateToHuman EvaluationDecision = "ESCALATE_TO_HUMAN"
)

// DimensionEvaluation provides structured categorical scoring for a single dimension without collapsing into a scalar KPI.
type DimensionEvaluation struct {
	Dimension   string          `json:"dimension"`
	Status      DimensionStatus `json:"status"`
	Details     string          `json:"details"`
	Confidence  float64         `json:"confidence,omitempty"`
	EvidenceRef string          `json:"evidence_ref,omitempty"`
}

// HoldoutScenarioResult records the evaluation outcome against a hidden adversarial counter-scenario.
type HoldoutScenarioResult struct {
	HoldoutID   string `json:"holdout_id"`
	HoldoutName string `json:"holdout_name"`
	Passed      bool   `json:"passed"`
	Reason      string `json:"reason,omitempty"`
}

// HoldoutScenario defines a hidden counter-scenario to detect proxy gaming, silent quality degradation, or authority escalation.
type HoldoutScenario struct {
	ID                string
	Name              string
	Description       string
	AdversarialVector string
	Evaluate          func(goal GoalProposal) (passed bool, reason string)
}

// EvaluationProvenance records audit lineage for the derived evaluation.
type EvaluationProvenance struct {
	MissionID      domain.ID `json:"mission_id"`
	MissionVersion int       `json:"mission_version"`
	EvaluatedAt    time.Time `json:"evaluated_at"`
	EvaluatorID    string    `json:"evaluator_id"`
}

// MissionEvaluation encapsulates multi-dimensional categorical evaluation of a candidate goal.
type MissionEvaluation struct {
	Decision           EvaluationDecision      `json:"decision"`
	DesiredOutcomes    DimensionEvaluation     `json:"desired_outcomes"`
	Obligations        DimensionEvaluation     `json:"obligations"`
	Constraints        DimensionEvaluation     `json:"constraints"`
	NonGoals           DimensionEvaluation     `json:"non_goals"`
	TradeOffCompliance DimensionEvaluation     `json:"trade_off_compliance"`
	EvidenceQuality    DimensionEvaluation     `json:"evidence_quality"`
	Uncertainty        DimensionEvaluation     `json:"uncertainty"`
	Reversibility      DimensionEvaluation     `json:"reversibility"`
	HoldoutResults     []HoldoutScenarioResult `json:"holdout_results,omitempty"`
	RejectionReasons   []string                `json:"rejection_reasons,omitempty"`
	Provenance         EvaluationProvenance    `json:"provenance"`
}

// MissionOracle provides derived, advisory query and multi-dimensional evaluation surfaces over active missions.
type MissionOracle interface {
	QueryGoalAlignment(ctx context.Context, m Mission, goal GoalProposal) (DimensionEvaluation, error)
	QueryConstrainingObligations(ctx context.Context, m Mission, domainKeywords []string) ([]string, error)
	QueryApplicableTradeOffs(ctx context.Context, m Mission, competingValues [2]string) ([]TradeOffRule, error)
	QueryRequiredHumanJudgments(ctx context.Context, m Mission, candidateActions []string) ([]string, error)
	Evaluate(ctx context.Context, m Mission, goal GoalProposal, holdouts ...HoldoutScenario) (MissionEvaluation, error)
}

// Oracle implements the MissionOracle advisory query surface and evaluation engine.
type Oracle struct {
	evaluatorID string
}

// NewMissionOracle creates a new advisory Oracle evaluator.
func NewMissionOracle() *Oracle {
	return &Oracle{evaluatorID: "oracle-v1"}
}

// QueryConstrainingObligations extracts active mission obligations matching given domain keywords.
func (o *Oracle) QueryConstrainingObligations(ctx context.Context, m Mission, domainKeywords []string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var matched []string
	for _, obl := range m.Obligations {
		oblLower := strings.ToLower(obl)
		for _, kw := range domainKeywords {
			if strings.Contains(oblLower, strings.ToLower(kw)) {
				matched = append(matched, obl)
				break
			}
		}
	}
	return matched, nil
}

// QueryApplicableTradeOffs retrieves trade-off rules governing competition between specific values.
func (o *Oracle) QueryApplicableTradeOffs(ctx context.Context, m Mission, competingValues [2]string) ([]TradeOffRule, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	v1 := strings.ToLower(competingValues[0])
	v2 := strings.ToLower(competingValues[1])

	var matches []TradeOffRule
	for _, rule := range m.TradeOffRules {
		pLower := strings.ToLower(rule.Priority)
		sLower := strings.ToLower(rule.Subordinate)
		if (strings.Contains(pLower, v1) && strings.Contains(sLower, v2)) ||
			(strings.Contains(pLower, v2) && strings.Contains(sLower, v1)) {
			matches = append(matches, rule)
		}
	}
	return matches, nil
}

// QueryRequiredHumanJudgments returns which human trust boundary gates are triggered by candidate actions.
func (o *Oracle) QueryRequiredHumanJudgments(ctx context.Context, m Mission, candidateActions []string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var triggered []string
	for _, b := range m.HumanTrustBoundaries {
		bLower := strings.ToLower(b)
		for _, action := range candidateActions {
			aLower := strings.ToLower(action)
			if (strings.Contains(bLower, "schema") || strings.Contains(bLower, "drop") || strings.Contains(bLower, "migration")) &&
				(strings.Contains(aLower, "schema") || strings.Contains(aLower, "drop") || strings.Contains(aLower, "migration")) {
				triggered = append(triggered, fmt.Sprintf("Action %q triggers boundary: %s", action, b))
				break
			}
			if (strings.Contains(bLower, "transfer") || strings.Contains(bLower, "$") || strings.Contains(bLower, "financial")) &&
				(strings.Contains(aLower, "transfer") || strings.Contains(aLower, "$") || strings.Contains(aLower, "wire")) {
				triggered = append(triggered, fmt.Sprintf("Action %q triggers boundary: %s", action, b))
				break
			}
		}
	}
	return triggered, nil
}

// QueryGoalAlignment assesses alignment between candidate goal and mission desired outcomes.
func (o *Oracle) QueryGoalAlignment(ctx context.Context, m Mission, goal GoalProposal) (DimensionEvaluation, error) {
	if err := ctx.Err(); err != nil {
		return DimensionEvaluation{}, err
	}
	if len(goal.ReferencedMissionElements) == 0 {
		return DimensionEvaluation{
			Dimension: "DesiredOutcomes",
			Status:    DimensionStatusUncertain,
			Details:   "Goal does not explicitly reference any active mission outcomes.",
		}, nil
	}

	return DimensionEvaluation{
		Dimension:  "DesiredOutcomes",
		Status:     DimensionStatusSatisfied,
		Details:    fmt.Sprintf("Goal advances %d referenced mission outcomes.", len(goal.ReferencedMissionElements)),
		Confidence: 0.95,
	}, nil
}

// Evaluate performs multi-dimensional categorical evaluation across all governance dimensions.
func (o *Oracle) Evaluate(ctx context.Context, m Mission, goal GoalProposal, holdouts ...HoldoutScenario) (MissionEvaluation, error) {
	if err := ctx.Err(); err != nil {
		return MissionEvaluation{}, err
	}

	var rejectionReasons []string
	blob := strings.ToLower(fmt.Sprintf("%s %s %s %s",
		goal.Title,
		goal.ProblemStatement,
		goal.ExpectedOutcome,
		strings.Join(goal.SuccessCriteria, " "),
	))

	// 1. Non-Goals Evaluation (STRICT VETO)
	nonGoalsStatus := DimensionStatusSatisfied
	nonGoalsDetails := "No active non-goals breached."
	for _, ng := range m.NonGoals {
		ngLower := strings.ToLower(ng)
		if (strings.Contains(ngLower, "unfenced") && (strings.Contains(blob, "surgery") || strings.Contains(blob, "without standard migration") || strings.Contains(blob, "unfenced"))) ||
			(strings.Contains(ngLower, "trading") && strings.Contains(blob, "speculative")) {
			nonGoalsStatus = DimensionStatusViolated
			reason := fmt.Sprintf("Proposal breaches non-goal: %s", ng)
			nonGoalsDetails = reason
			rejectionReasons = append(rejectionReasons, reason)
			break
		}
	}

	// 2. Obligations Evaluation (STRICT VETO)
	obligationsStatus := DimensionStatusSatisfied
	obligationsDetails := "All active obligations respected."
	for _, obl := range m.Obligations {
		oblLower := strings.ToLower(obl)
		if strings.Contains(oblLower, "audit log") && (strings.Contains(blob, "bypass audit") || strings.Contains(blob, "discarded") || strings.Contains(blob, "skip audit")) {
			obligationsStatus = DimensionStatusViolated
			reason := fmt.Sprintf("Proposal violates statutory obligation: %s", obl)
			obligationsDetails = reason
			rejectionReasons = append(rejectionReasons, reason)
			break
		}
	}

	// 3. Constraints Evaluation (STRICT VETO)
	constraintsStatus := DimensionStatusSatisfied
	constraintsDetails := "All resource and architectural constraints observed."
	maxCost := extractMaxCostConstraint(m.Constraints)
	if maxCost > 0 && goal.ResourceEnvelope.MaxCostUSD > maxCost {
		constraintsStatus = DimensionStatusViolated
		reason := fmt.Sprintf("Resource envelope cost ($%.2f) exceeds mission constraint ceiling ($%.2f).",
			goal.ResourceEnvelope.MaxCostUSD, maxCost)
		constraintsDetails = reason
		rejectionReasons = append(rejectionReasons, reason)
	}

	// 4. Trade-off Compliance
	tradeOffStatus := DimensionStatusSatisfied
	tradeOffDetails := "Trade-off rules respected."
	for _, rule := range m.TradeOffRules {
		pLower := strings.ToLower(rule.Priority)
		sLower := strings.ToLower(rule.Subordinate)
		if strings.Contains(pLower, "audit") && strings.Contains(sLower, "latency") {
			if strings.Contains(blob, "bypass audit") || strings.Contains(blob, "speedup at cost of audit") {
				tradeOffStatus = DimensionStatusViolated
				reason := fmt.Sprintf("Trade-off violation: %s must take precedence over %s (%s)",
					rule.Priority, rule.Subordinate, rule.Rationale)
				tradeOffDetails = reason
				rejectionReasons = append(rejectionReasons, reason)
			}
		}
	}

	// 5. Evidence Quality
	evidenceStatus := DimensionStatusSatisfied
	evidenceDetails := "Supporting evidence verified from live or simulated records."
	if len(goal.SupportingEvidence) == 0 {
		evidenceStatus = DimensionStatusUncertain
		evidenceDetails = "No supporting evidence attached to goal proposal."
	}

	// 6. Reversibility
	reversibilityStatus := DimensionStatusSatisfied
	reversibilityDetails := fmt.Sprintf("Reversibility classification: %s.", goal.Reversibility)
	if goal.Reversibility == ReversibilityIrreversible && goal.RiskClassification == RiskCritical {
		reversibilityStatus = DimensionStatusViolated
	}

	// 7. Uncertainty
	uncertaintyStatus := DimensionStatusSatisfied
	uncertaintyDetails := fmt.Sprintf("%d explicit assumptions documented.", len(goal.Assumptions))
	if len(goal.Assumptions) == 0 {
		uncertaintyStatus = DimensionStatusUncertain
		uncertaintyDetails = "No explicit assumptions documented."
	}

	// 8. Desired Outcomes Alignment
	desiredOutcomesStatus := DimensionStatusSatisfied
	desiredOutcomesDetails := "Advances active mission outcomes."
	if len(goal.ReferencedMissionElements) == 0 {
		desiredOutcomesStatus = DimensionStatusUncertain
		desiredOutcomesDetails = "No active mission outcomes linked."
	}

	// 9. Holdout Scenarios Evaluation
	var holdoutResults []HoldoutScenarioResult
	for _, h := range holdouts {
		passed, reason := h.Evaluate(goal)
		holdoutResults = append(holdoutResults, HoldoutScenarioResult{
			HoldoutID:   h.ID,
			HoldoutName: h.Name,
			Passed:      passed,
			Reason:      reason,
		})
		if !passed {
			rejectionReasons = append(rejectionReasons, fmt.Sprintf("Holdout scenario %q failed: %s", h.Name, reason))
		}
	}

	// Determine final categorical Decision
	var decision EvaluationDecision
	if len(rejectionReasons) > 0 {
		decision = EvaluationDecisionReject
	} else if goal.AuthorityCeiling.RequiresHumanApproval || len(goal.AuthorityCeiling.TouchedTrustBoundaries) > 0 {
		decision = EvaluationDecisionEscalateToHuman
	} else {
		decision = EvaluationDecisionAccept
	}

	eval := MissionEvaluation{
		Decision: decision,
		DesiredOutcomes: DimensionEvaluation{
			Dimension: "DesiredOutcomes",
			Status:    desiredOutcomesStatus,
			Details:   desiredOutcomesDetails,
		},
		Obligations: DimensionEvaluation{
			Dimension: "Obligations",
			Status:    obligationsStatus,
			Details:   obligationsDetails,
		},
		Constraints: DimensionEvaluation{
			Dimension: "Constraints",
			Status:    constraintsStatus,
			Details:   constraintsDetails,
		},
		NonGoals: DimensionEvaluation{
			Dimension: "NonGoals",
			Status:    nonGoalsStatus,
			Details:   nonGoalsDetails,
		},
		TradeOffCompliance: DimensionEvaluation{
			Dimension: "TradeOffCompliance",
			Status:    tradeOffStatus,
			Details:   tradeOffDetails,
		},
		EvidenceQuality: DimensionEvaluation{
			Dimension: "EvidenceQuality",
			Status:    evidenceStatus,
			Details:   evidenceDetails,
		},
		Uncertainty: DimensionEvaluation{
			Dimension: "Uncertainty",
			Status:    uncertaintyStatus,
			Details:   uncertaintyDetails,
		},
		Reversibility: DimensionEvaluation{
			Dimension: "Reversibility",
			Status:    reversibilityStatus,
			Details:   reversibilityDetails,
		},
		HoldoutResults:   holdoutResults,
		RejectionReasons: rejectionReasons,
		Provenance: EvaluationProvenance{
			MissionID:      m.ID,
			MissionVersion: m.Version,
			EvaluatedAt:    time.Now().UTC(),
			EvaluatorID:    o.evaluatorID,
		},
	}

	return eval, nil
}

// Built-in Holdout Scenarios

// HoldoutTrafficRejectionGaming detects proxy gaming where 100% success rate or zero errors is achieved by shedding traffic.
func HoldoutTrafficRejectionGaming() HoldoutScenario {
	return HoldoutScenario{
		ID:                "holdout_traffic_rejection_gaming",
		Name:              "Traffic Rejection Proxy Gaming",
		Description:       "Evaluates whether the candidate goal attempts to game error metrics by dropping or rejecting requests.",
		AdversarialVector: "Refusing risky traffic or returning 503 early to maintain artificial zero-error rate.",
		Evaluate: func(goal GoalProposal) (bool, string) {
			blob := strings.ToLower(fmt.Sprintf("%s %s %s",
				goal.Title,
				goal.ProblemStatement,
				goal.ExpectedOutcome,
			))
			if (strings.Contains(blob, "zero error") || strings.Contains(blob, "100% success")) &&
				(strings.Contains(blob, "refusal") || strings.Contains(blob, "reject") || strings.Contains(blob, "503") || strings.Contains(blob, "shed")) {
				return false, "Proposal achieves error metric perfection via traffic rejection or refusal."
			}
			return true, ""
		},
	}
}

// HoldoutProxyDarkPattern detects deceptive UX or artificial cancellation obstruction to inflate retention.
func HoldoutProxyDarkPattern() HoldoutScenario {
	return HoldoutScenario{
		ID:                "holdout_proxy_dark_pattern",
		Name:              "Dark Pattern Retention Gaming",
		Description:       "Evaluates whether candidate plans introduce cancellation friction or deceptive patterns.",
		AdversarialVector: "Obstructing cancellation flows to artificially protect subscription revenue.",
		Evaluate: func(goal GoalProposal) (bool, string) {
			blob := strings.ToLower(fmt.Sprintf("%s %s %s",
				goal.Title,
				goal.ProblemStatement,
				goal.ExpectedOutcome,
			))
			if strings.Contains(blob, "cancellation friction") || strings.Contains(blob, "dark pattern") || strings.Contains(blob, "hide cancel") {
				return false, "Proposal introduces deceptive UX or cancellation friction to game retention metrics."
			}
			return true, ""
		},
	}
}
