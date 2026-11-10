package epistemic_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/epistemic"
)

func sampleProcessProposal(proposer epistemic.Principal) epistemic.ProcessChangeProposal {
	now := time.Now().UTC()
	return epistemic.ProcessChangeProposal{
		ID:                       domain.NewID("pcp"),
		Title:                    "Chunked Migration Protocol Upgrade",
		Description:              "Upgrade database migration procedure to use 100-row chunks with dynamic backoff.",
		TargetProcedure:          "procedure.database.migration",
		ChallengerImplementation: "scripts/chunked_migration.go",
		ProposedBy:               proposer,
		Status:                   epistemic.ProcessProposalStatusProposed,
		EvaluationContract: epistemic.EvaluationContract{
			EvaluatorID:         "independent-evaluator-gate",
			AcceptanceThreshold: 0.95,
			MandatoryHoldoutIDs: []string{"holdout_deadlock_stress", "holdout_data_loss"},
			HardInvariants: []string{
				"zero_data_loss",
				"maximum_memory_512mb",
				"bounded_budget_usd_5.00",
			},
			Immutable: true,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func TestProcessEvolution_CanonicalHappyPathLifecycle(t *testing.T) {
	ctx := context.Background()
	gate := epistemic.NewProcessEvolutionGate()

	proposer := epistemic.Principal{
		ID:   "researcher-worker-01",
		Kind: epistemic.PrincipalKindWorker,
	}

	governor := epistemic.Principal{
		ID:   "root-governor-human",
		Kind: epistemic.PrincipalKindHuman,
	}

	proposal := sampleProcessProposal(proposer)

	// 1. Propose
	if err := gate.Propose(ctx, proposal); err != nil {
		t.Fatalf("propose: %v", err)
	}

	// 2. Evaluate Challenger (independent evaluator)
	testResults := epistemic.ChallengerTestResults{
		ZeroDataLoss:     true,
		MemoryMB:         256.0,
		CostUSD:          1.50,
		PerformanceScore: 0.98,
		HoldoutsPassed:   []string{"holdout_deadlock_stress", "holdout_data_loss"},
		HoldoutFailures:  nil,
	}

	report, err := gate.EvaluateChallenger(ctx, proposal.ID, "independent-evaluator-gate", testResults)
	if err != nil {
		t.Fatalf("evaluate challenger: %v", err)
	}

	if !report.AllHardInvariantsPassed() {
		t.Errorf("expected all hard invariants to pass, violations: %v", report.HardInvariantViolations)
	}

	// Verify proposal transitioned to CHALLENGER_TESTED
	p, err := gate.GetProposal(ctx, proposal.ID)
	if err != nil {
		t.Fatalf("get proposal: %v", err)
	}
	if p.Status != epistemic.ProcessProposalStatusChallengerTested {
		t.Errorf("expected CHALLENGER_TESTED, got %s", p.Status)
	}

	// 3. Promote by external human governor
	if err := gate.Promote(ctx, governor, proposal.ID); err != nil {
		t.Fatalf("promote by governor: %v", err)
	}

	promoted, err := gate.GetProposal(ctx, proposal.ID)
	if err != nil {
		t.Fatalf("get promoted proposal: %v", err)
	}
	if promoted.Status != epistemic.ProcessProposalStatusPromoted {
		t.Errorf("expected PROMOTED, got %s", promoted.Status)
	}

	// 4. Rollback capability
	if err := gate.Rollback(ctx, governor, proposal.ID, "Temporary rollback for audit"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	rolledBack, err := gate.GetProposal(ctx, proposal.ID)
	if err != nil {
		t.Fatalf("get rolled back: %v", err)
	}
	if rolledBack.Status != epistemic.ProcessProposalStatusRolledBack {
		t.Errorf("expected ROLLED_BACK, got %s", rolledBack.Status)
	}
}

func TestProcessEvolution_ProposerCannotSelfPromote(t *testing.T) {
	ctx := context.Background()
	gate := epistemic.NewProcessEvolutionGate()

	worker := epistemic.Principal{
		ID:   "researcher-worker-01",
		Kind: epistemic.PrincipalKindWorker,
	}

	proposal := sampleProcessProposal(worker)
	if err := gate.Propose(ctx, proposal); err != nil {
		t.Fatalf("propose: %v", err)
	}

	testResults := epistemic.ChallengerTestResults{
		ZeroDataLoss:     true,
		MemoryMB:         200.0,
		CostUSD:          1.00,
		PerformanceScore: 0.99,
		HoldoutsPassed:   []string{"holdout_deadlock_stress", "holdout_data_loss"},
	}

	_, err := gate.EvaluateChallenger(ctx, proposal.ID, "independent-evaluator-gate", testResults)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	// Worker attempts self-promotion -> MUST FAIL
	err = gate.Promote(ctx, worker, proposal.ID)
	if err != epistemic.ErrUnauthorizedPromotion {
		t.Errorf("expected ErrUnauthorizedPromotion, got %v", err)
	}
}

func TestProcessEvolution_HardInvariantVetoOverridesSoftPerformanceGains(t *testing.T) {
	ctx := context.Background()
	gate := epistemic.NewProcessEvolutionGate()

	proposer := epistemic.Principal{
		ID:   "optimizer-agent",
		Kind: epistemic.PrincipalKindWorker,
	}

	proposal := sampleProcessProposal(proposer)
	if err := gate.Propose(ctx, proposal); err != nil {
		t.Fatalf("propose: %v", err)
	}

	// 200% speedup (+0.99 score), but violated zero_data_loss (1 dropped record) and budget ($12 vs $5)
	testResults := epistemic.ChallengerTestResults{
		ZeroDataLoss:     false, // HARD VETO
		MemoryMB:         800.0, // HARD VETO (> 512MB)
		CostUSD:          12.00, // HARD VETO (> $5.00)
		PerformanceScore: 0.99,  // High score!
		HoldoutsPassed:   []string{"holdout_deadlock_stress"},
		HoldoutFailures:  []string{"holdout_data_loss"},
	}

	report, err := gate.EvaluateChallenger(ctx, proposal.ID, "independent-evaluator-gate", testResults)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if report.AllHardInvariantsPassed() {
		t.Error("expected hard invariant vetoes to trigger")
	}

	if len(report.HardInvariantViolations) < 3 {
		t.Errorf("expected at least 3 hard invariant violations, got %d: %v", len(report.HardInvariantViolations), report.HardInvariantViolations)
	}

	// Status must be REJECTED, not CHALLENGER_TESTED
	p, err := gate.GetProposal(ctx, proposal.ID)
	if err != nil {
		t.Fatalf("get proposal: %v", err)
	}
	if p.Status != epistemic.ProcessProposalStatusRejected {
		t.Errorf("expected proposal state REJECTED, got %s", p.Status)
	}
}

func TestProcessEvolution_EvaluatorTamperingBlocked(t *testing.T) {
	ctx := context.Background()
	gate := epistemic.NewProcessEvolutionGate()

	worker := epistemic.Principal{
		ID:   "sneaky-worker",
		Kind: epistemic.PrincipalKindWorker,
	}

	proposal := sampleProcessProposal(worker)
	if err := gate.Propose(ctx, proposal); err != nil {
		t.Fatalf("propose: %v", err)
	}

	// Attempt to tamper with evaluation contract (lower threshold, remove holdouts)
	tamperedContract := epistemic.EvaluationContract{
		EvaluatorID:         "sneaky-worker",
		AcceptanceThreshold: 0.10, // lowered from 0.95!
		MandatoryHoldoutIDs: nil,  // stripped holdouts!
		HardInvariants:      nil,  // stripped invariants!
	}

	err := gate.UpdateContract(ctx, worker, proposal.ID, tamperedContract)
	if err != epistemic.ErrEvaluatorTampering {
		t.Errorf("expected ErrEvaluatorTampering when non-governor attempts contract mutation, got %v", err)
	}
}

func TestProcessEvolution_BlockedSuppressionOfFailures(t *testing.T) {
	ctx := context.Background()
	gate := epistemic.NewProcessEvolutionGate()

	worker := epistemic.Principal{
		ID:   "researcher-worker-01",
		Kind: epistemic.PrincipalKindWorker,
	}

	proposal := sampleProcessProposal(worker)
	if err := gate.Propose(ctx, proposal); err != nil {
		t.Fatalf("propose: %v", err)
	}

	// Wrong evaluator ID attempting to submit evaluation
	testResults := epistemic.ChallengerTestResults{
		ZeroDataLoss:     true,
		MemoryMB:         100.0,
		CostUSD:          1.00,
		PerformanceScore: 0.99,
		HoldoutsPassed:   []string{"holdout_deadlock_stress", "holdout_data_loss"},
	}

	_, err := gate.EvaluateChallenger(ctx, proposal.ID, "unauthorized-evaluator-id", testResults)
	if err == nil || !strings.Contains(err.Error(), "unauthorized evaluator") {
		t.Errorf("expected unauthorized evaluator error, got %v", err)
	}
}
