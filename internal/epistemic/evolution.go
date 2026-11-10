package epistemic

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

var (
	// ErrUnauthorizedPromotion is returned when an unauthorized worker principal attempts to self-promote a process change.
	ErrUnauthorizedPromotion = errors.New("unauthorized: process promotion requires sovereign human/root authority")

	// ErrEvaluatorTampering is returned when a proposer attempts to alter evaluation contracts or thresholds.
	ErrEvaluatorTampering = errors.New("evaluator contract tampering detected: contract is immutable once sealed")

	// ErrHardInvariantVeto is returned when candidate changes violate hard safety, correctness, or budget invariants.
	ErrHardInvariantVeto = errors.New("hard invariant veto: candidate violated critical safety, correctness, or budget invariants")

	// ErrProposalNotFound is returned when the requested proposal does not exist.
	ErrProposalNotFound = errors.New("process change proposal not found")

	// ErrInvalidProposalState is returned when attempting an illegal lifecycle transition.
	ErrInvalidProposalState = errors.New("invalid process change proposal lifecycle state transition")
)

// PrincipalKind categorizes governance actors in the collective.
type PrincipalKind string

const (
	PrincipalKindHuman  PrincipalKind = "HUMAN"
	PrincipalKindRoot   PrincipalKind = "ROOT"
	PrincipalKindWorker PrincipalKind = "WORKER"
)

// Principal represents the caller executing a governance or evaluation action.
type Principal struct {
	ID   string        `json:"id"`
	Kind PrincipalKind `json:"kind"`
}

// IsAuthorizedGovernance evaluates whether the principal possesses sovereign promotion authority.
func (p Principal) IsAuthorizedGovernance() bool {
	return p.Kind == PrincipalKindHuman || p.Kind == PrincipalKindRoot
}

// ProcessProposalStatus represents the lifecycle state machine of a process change proposal.
type ProcessProposalStatus string

const (
	ProcessProposalStatusProposed         ProcessProposalStatus = "PROPOSED"
	ProcessProposalStatusEvaluating       ProcessProposalStatus = "EVALUATING"
	ProcessProposalStatusChallengerTested ProcessProposalStatus = "CHALLENGER_TESTED"
	ProcessProposalStatusPromoted         ProcessProposalStatus = "PROMOTED"
	ProcessProposalStatusRejected         ProcessProposalStatus = "REJECTED"
	ProcessProposalStatusRolledBack       ProcessProposalStatus = "ROLLED_BACK"
)

// EvaluationContract defines the immutable evaluation parameters and holdout requirements.
type EvaluationContract struct {
	EvaluatorID         string   `json:"evaluator_id"`
	AcceptanceThreshold float64  `json:"acceptance_threshold"`
	MandatoryHoldoutIDs []string `json:"mandatory_holdout_ids"`
	HardInvariants      []string `json:"hard_invariants"`
	Immutable           bool     `json:"immutable"`
}

// ChallengerTestResults captures empirical metrics from DTU replay, regression suites, and holdout scenarios.
type ChallengerTestResults struct {
	ZeroDataLoss     bool     `json:"zero_data_loss"`
	MemoryMB         float64  `json:"memory_mb"`
	CostUSD          float64  `json:"cost_usd"`
	PerformanceScore float64  `json:"performance_score"`
	HoldoutsPassed   []string `json:"holdouts_passed"`
	HoldoutFailures  []string `json:"holdout_failures,omitempty"`
}

// HoldoutScenarioResult records the empirical pass/fail outcome against a holdout scenario.
type HoldoutScenarioResult struct {
	HoldoutID   string `json:"holdout_id"`
	HoldoutName string `json:"holdout_name"`
	Passed      bool   `json:"passed"`
	Reason      string `json:"reason,omitempty"`
}

// PromotionEvaluationReport details multi-dimensional evaluation results and hard invariant vetoes.
type PromotionEvaluationReport struct {
	CorrectnessPassed       bool                    `json:"correctness_passed"`
	SafetyPassed            bool                    `json:"safety_passed"`
	BudgetPassed            bool                    `json:"budget_passed"`
	PerformanceScore        float64                 `json:"performance_score"`
	HardInvariantViolations []string                `json:"hard_invariant_violations,omitempty"`
	HoldoutResults          []HoldoutScenarioResult `json:"holdout_results,omitempty"`
	EvaluatedBy             string                  `json:"evaluated_by"`
	EvaluatedAt             time.Time               `json:"evaluated_at"`
}

// AllHardInvariantsPassed checks whether all critical correctness, safety, and budget invariants were satisfied.
func (r PromotionEvaluationReport) AllHardInvariantsPassed() bool {
	return r.CorrectnessPassed && r.SafetyPassed && r.BudgetPassed && len(r.HardInvariantViolations) == 0
}

// ProcessChangeProposal encapsulates an operational or procedural improvement candidate.
type ProcessChangeProposal struct {
	ID                       domain.ID                  `json:"id"`
	Title                    string                     `json:"title"`
	Description              string                     `json:"description"`
	TargetProcedure          string                     `json:"target_procedure"`
	ChallengerImplementation string                     `json:"challenger_implementation"`
	ProposedBy               Principal                  `json:"proposed_by"`
	Status                   ProcessProposalStatus      `json:"status"`
	EvaluationContract       EvaluationContract         `json:"evaluation_contract"`
	EvaluationReport         *PromotionEvaluationReport `json:"evaluation_report,omitempty"`
	RollbackReason           string                     `json:"rollback_reason,omitempty"`
	CreatedAt                time.Time                  `json:"created_at"`
	UpdatedAt                time.Time                  `json:"updated_at"`
}

// ProcessEvolutionGate enforces generator/evaluator separation, multi-dimensional holdouts, and external promotion gates.
type ProcessEvolutionGate struct {
	mu        sync.RWMutex
	proposals map[domain.ID]ProcessChangeProposal
}

// NewProcessEvolutionGate initializes a ProcessEvolutionGate.
func NewProcessEvolutionGate() *ProcessEvolutionGate {
	return &ProcessEvolutionGate{
		proposals: make(map[domain.ID]ProcessChangeProposal),
	}
}

// Propose submits a new ProcessChangeProposal in PROPOSED state.
func (g *ProcessEvolutionGate) Propose(ctx context.Context, p ProcessChangeProposal) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	p.Status = ProcessProposalStatusProposed
	p.EvaluationContract.Immutable = true
	g.proposals[p.ID] = p
	return nil
}

// GetProposal retrieves a proposal by ID.
func (g *ProcessEvolutionGate) GetProposal(ctx context.Context, id domain.ID) (ProcessChangeProposal, error) {
	if err := ctx.Err(); err != nil {
		return ProcessChangeProposal{}, err
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	p, ok := g.proposals[id]
	if !ok {
		return ProcessChangeProposal{}, ErrProposalNotFound
	}
	return p, nil
}

// UpdateContract blocks attempts by non-governors to mutate sealed evaluation contracts.
func (g *ProcessEvolutionGate) UpdateContract(
	ctx context.Context,
	principal Principal,
	id domain.ID,
	contract EvaluationContract,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	p, ok := g.proposals[id]
	if !ok {
		return ErrProposalNotFound
	}

	// Generator/evaluator isolation: Proposers cannot modify evaluation contracts
	if !principal.IsAuthorizedGovernance() {
		return ErrEvaluatorTampering
	}

	p.EvaluationContract = contract
	p.UpdatedAt = time.Now().UTC()
	g.proposals[id] = p
	return nil
}

// EvaluateChallenger executes independent multi-dimensional evaluation enforcing hard invariant vetoes.
func (g *ProcessEvolutionGate) EvaluateChallenger(
	ctx context.Context,
	id domain.ID,
	evaluatorID string,
	results ChallengerTestResults,
) (PromotionEvaluationReport, error) {
	if err := ctx.Err(); err != nil {
		return PromotionEvaluationReport{}, err
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	p, ok := g.proposals[id]
	if !ok {
		return PromotionEvaluationReport{}, ErrProposalNotFound
	}

	// Verify evaluator authority against sealed contract
	if p.EvaluationContract.EvaluatorID != "" && p.EvaluationContract.EvaluatorID != evaluatorID {
		return PromotionEvaluationReport{}, fmt.Errorf("unauthorized evaluator %q: contract designated %q",
			evaluatorID, p.EvaluationContract.EvaluatorID)
	}

	now := time.Now().UTC()
	var violations []string

	// Hard Invariant 1: Zero Data Loss
	correctnessPassed := results.ZeroDataLoss
	if !results.ZeroDataLoss {
		violations = append(violations, "Critical correctness violation: zero_data_loss invariant failed")
	}

	// Hard Invariant 2: Safety & Memory Limits
	safetyPassed := results.MemoryMB <= 512.0
	if results.MemoryMB > 512.0 {
		violations = append(violations, fmt.Sprintf("Safety invariant violation: memory usage (%.1f MB) exceeded ceiling 512.0 MB", results.MemoryMB))
	}

	// Hard Invariant 3: Cost Bounds
	budgetPassed := results.CostUSD <= 5.00
	if results.CostUSD > 5.00 {
		violations = append(violations, fmt.Sprintf("Budget invariant violation: cost ($%.2f) exceeded bound $5.00", results.CostUSD))
	}

	// Check holdout failures
	var holdoutResults []HoldoutScenarioResult
	for _, h := range p.EvaluationContract.MandatoryHoldoutIDs {
		passed := true
		for _, f := range results.HoldoutFailures {
			if f == h {
				passed = false
				violations = append(violations, fmt.Sprintf("Mandatory holdout %q failed", h))
				break
			}
		}
		holdoutResults = append(holdoutResults, HoldoutScenarioResult{
			HoldoutID:   h,
			HoldoutName: h,
			Passed:      passed,
		})
	}

	report := PromotionEvaluationReport{
		CorrectnessPassed:       correctnessPassed,
		SafetyPassed:            safetyPassed,
		BudgetPassed:            budgetPassed,
		PerformanceScore:        results.PerformanceScore,
		HardInvariantViolations: violations,
		HoldoutResults:          holdoutResults,
		EvaluatedBy:             evaluatorID,
		EvaluatedAt:             now,
	}

	p.EvaluationReport = &report
	p.UpdatedAt = now

	// Strict Veto: If any hard invariant failed, reject immediately regardless of performance
	if !report.AllHardInvariantsPassed() || results.PerformanceScore < p.EvaluationContract.AcceptanceThreshold {
		p.Status = ProcessProposalStatusRejected
	} else {
		p.Status = ProcessProposalStatusChallengerTested
	}

	g.proposals[id] = p
	return report, nil
}

// Promote promotes a tested challenger to PROMOTED state. Requires sovereign human/root authority.
func (g *ProcessEvolutionGate) Promote(ctx context.Context, principal Principal, id domain.ID) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	p, ok := g.proposals[id]
	if !ok {
		return ErrProposalNotFound
	}

	// Sovereign authority check
	if !principal.IsAuthorizedGovernance() {
		return ErrUnauthorizedPromotion
	}

	if p.Status != ProcessProposalStatusChallengerTested {
		return fmt.Errorf("%w: proposal status is %s, expected CHALLENGER_TESTED", ErrInvalidProposalState, p.Status)
	}

	now := time.Now().UTC()
	p.Status = ProcessProposalStatusPromoted
	p.UpdatedAt = now
	g.proposals[id] = p
	return nil
}

// Rollback restores prior baseline procedure upon post-promotion degradation or operator veto.
func (g *ProcessEvolutionGate) Rollback(ctx context.Context, principal Principal, id domain.ID, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	p, ok := g.proposals[id]
	if !ok {
		return ErrProposalNotFound
	}

	if !principal.IsAuthorizedGovernance() {
		return ErrUnauthorizedPromotion
	}

	now := time.Now().UTC()
	p.Status = ProcessProposalStatusRolledBack
	p.RollbackReason = reason
	p.UpdatedAt = now
	g.proposals[id] = p
	return nil
}
