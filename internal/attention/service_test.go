package attention_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/attention"
	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type fixedClock struct {
	now time.Time
}

func (c *fixedClock) Now() time.Time {
	return c.now
}

func (c *fixedClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
}

func setupAttentionSuite(t *testing.T) (*state.Store, *fixedClock, domain.ID, *attention.Service) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(context.Background(), filepath.Join(dir, "attention_test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.DB().Close() })

	clk := &fixedClock{now: time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)}
	ownerID := domain.ID("owner_p1")
	svc := attention.NewService(store, clk, ownerID)

	return store, clk, ownerID, svc
}

func TestAttentionService_AutonomousDelegationVsSovereign(t *testing.T) {
	ctx := context.Background()
	_, _, _, svc := setupAttentionSuite(t)

	// Case 1: Delegated sub-decision (e.g. routine retry or low exposure)
	// Must be marked as DELEGATED_AUTONOMOUS and auto-resolved without consuming Owner attention
	sub1 := []attention.SubDecision{
		{ID: "sub_1", Type: "OPERATION_RETRY", Summary: "Retry transient timeout", RiskScore: 10},
	}
	pkt1, err := svc.SubmitDecisionGroup(ctx, attention.SubmitGroupRequest{
		Title:                   "Routine operation retry",
		Category:                attention.CategoryDelegatedAutonomous,
		Urgency:                 attention.UrgencyLow,
		EstimatedAttentionUnits: 5,
		Summary:                 "Routine retry within quota",
		SubDecisions:            sub1,
		ProposedAction:          attention.ProposedAction{ActionType: "RETRY_OPERATION"},
	})
	if err != nil {
		t.Fatalf("submit delegated group: %v", err)
	}

	if pkt1.SovereignRequired {
		t.Fatal("expected sovereign_required to be false for delegated autonomous decision")
	}
	if pkt1.Status != attention.StatusAutonomouslyResolved {
		t.Fatalf("expected StatusAutonomouslyResolved, got %s", pkt1.Status)
	}
	if pkt1.EstimatedAttentionUnits != 5 {
		t.Fatalf("expected 5 estimated attention units, got %d", pkt1.EstimatedAttentionUnits)
	}

	// Case 2: Sovereign required decision (e.g. Constitutional change or major exposure)
	sub2 := []attention.SubDecision{
		{ID: "sub_2", Type: "CONSTITUTION_AMENDMENT", Summary: "Update mission directive", RiskScore: 90},
	}
	pkt2, err := svc.SubmitDecisionGroup(ctx, attention.SubmitGroupRequest{
		Title:                   "Amend foundational mission directive",
		Category:                attention.CategoryConstitutional,
		Urgency:                 attention.UrgencyCritical,
		EstimatedAttentionUnits: 20,
		Summary:                 "Requires sovereign Owner approval",
		SubDecisions:            sub2,
		ProposedAction:          attention.ProposedAction{ActionType: "UPDATE_CONSTITUTION"},
	})
	if err != nil {
		t.Fatalf("submit sovereign group: %v", err)
	}

	if !pkt2.SovereignRequired {
		t.Fatal("expected sovereign_required to be true for constitutional decision")
	}
	if pkt2.Status != attention.StatusPending {
		t.Fatalf("expected StatusPending, got %s", pkt2.Status)
	}
}

func TestAttentionService_BudgetDeductionAndRejection(t *testing.T) {
	ctx := context.Background()
	_, _, ownerID, svc := setupAttentionSuite(t)

	// Initial budget is 100 units
	budget, err := svc.GetAttentionBudget(ctx)
	if err != nil {
		t.Fatalf("get budget: %v", err)
	}
	if budget.DailyBudgetUnits != 100 || budget.UsedBudgetUnits != 0 {
		t.Fatalf("unexpected initial budget: %+v", budget)
	}

	// Submit sovereign packet costing 30 units
	pkt, err := svc.SubmitDecisionGroup(ctx, attention.SubmitGroupRequest{
		Title:                   "High exposure capital spend",
		Category:                attention.CategoryExposureThreshold,
		Urgency:                 attention.UrgencyHigh,
		EstimatedAttentionUnits: 30,
		Summary:                 "Exceeds exposure ceiling",
		SubDecisions:            []attention.SubDecision{{ID: "s1", Type: "CAPITAL_SPEND", Summary: "Hardware acquisition", RiskScore: 70}},
		ProposedAction:          attention.ProposedAction{ActionType: "AUTHORIZE_SPEND"},
	})
	if err != nil {
		t.Fatalf("submit sovereign group: %v", err)
	}

	// Owner resolves/approves the decision
	err = svc.ResolveDecision(ctx, pkt.PacketID, ownerID, attention.StatusApproved)
	if err != nil {
		t.Fatalf("resolve decision: %v", err)
	}

	// Used budget should now be 30 units
	budget, err = svc.GetAttentionBudget(ctx)
	if err != nil {
		t.Fatalf("get budget after approval: %v", err)
	}
	if budget.UsedBudgetUnits != 30 {
		t.Fatalf("expected used budget 30, got %d", budget.UsedBudgetUnits)
	}

	// Submit another sovereign packet costing 20 units and test REJECTION
	pktReject, err := svc.SubmitDecisionGroup(ctx, attention.SubmitGroupRequest{
		Title:                   "Risky proposal",
		Category:                attention.CategoryStrategicMission,
		Urgency:                 attention.UrgencyMedium,
		EstimatedAttentionUnits: 20,
		Summary:                 "Will be rejected by owner",
	})
	if err != nil {
		t.Fatalf("submit pktReject: %v", err)
	}

	err = svc.ResolveDecision(ctx, pktReject.PacketID, ownerID, attention.StatusRejected)
	if err != nil {
		t.Fatalf("resolve decision rejection: %v", err)
	}

	// Used budget should now be 50 units (30 + 20)
	budget, err = svc.GetAttentionBudget(ctx)
	if err != nil {
		t.Fatalf("get budget after rejection: %v", err)
	}
	if budget.UsedBudgetUnits != 50 {
		t.Fatalf("expected used budget 50, got %d", budget.UsedBudgetUnits)
	}
}

func TestAttentionService_AuthorizationAndEdgeCases(t *testing.T) {
	ctx := context.Background()
	_, _, ownerID, svc := setupAttentionSuite(t)

	// Create sovereign packet
	pkt, err := svc.SubmitDecisionGroup(ctx, attention.SubmitGroupRequest{
		Title:                   "Sovereign packet",
		Category:                attention.CategorySafetyOverride,
		Urgency:                 attention.UrgencyHigh,
		EstimatedAttentionUnits: 15,
		Summary:                 "Safety check",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// 1. Unauthorized caller
	err = svc.ResolveDecision(ctx, pkt.PacketID, "random_imposter", attention.StatusApproved)
	if !errors.Is(err, attention.ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}

	// 1b. Invalid resolution value
	err = svc.ResolveDecision(ctx, pkt.PacketID, ownerID, attention.StatusPending)
	if err == nil {
		t.Fatal("expected error for invalid resolution value StatusPending, got nil")
	}

	// 2. Empty caller
	err = svc.ResolveDecision(ctx, pkt.PacketID, "", attention.StatusApproved)
	if !errors.Is(err, attention.ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized for empty caller, got %v", err)
	}

	// 3. Packet not found
	err = svc.ResolveDecision(ctx, "pkt_nonexistent", ownerID, attention.StatusApproved)
	if !errors.Is(err, attention.ErrPacketNotFound) {
		t.Fatalf("expected ErrPacketNotFound, got %v", err)
	}

	// 4. Authorized resolution succeeds
	err = svc.ResolveDecision(ctx, pkt.PacketID, ownerID, attention.StatusApproved)
	if err != nil {
		t.Fatalf("resolve decision: %v", err)
	}

	// 5. Already resolved
	err = svc.ResolveDecision(ctx, pkt.PacketID, ownerID, attention.StatusApproved)
	if !errors.Is(err, attention.ErrAlreadyResolved) {
		t.Fatalf("expected ErrAlreadyResolved, got %v", err)
	}

	// 6. Delegated autonomous cannot be manually resolved
	delPkt, err := svc.SubmitDecisionGroup(ctx, attention.SubmitGroupRequest{
		Title:                   "Autonomous task",
		Category:                attention.CategoryDelegatedAutonomous,
		Urgency:                 attention.UrgencyLow,
		EstimatedAttentionUnits: 5,
		Summary:                 "Auto handled",
	})
	if err != nil {
		t.Fatalf("submit delegated: %v", err)
	}
	err = svc.ResolveDecision(ctx, delPkt.PacketID, ownerID, attention.StatusApproved)
	// Status was already AUTONOMOUSLY_RESOLVED so it triggers already resolved
	if !errors.Is(err, attention.ErrAlreadyResolved) {
		t.Fatalf("expected ErrAlreadyResolved for delegated item, got %v", err)
	}
}

func TestAttentionService_BudgetCeilingAndRollover(t *testing.T) {
	ctx := context.Background()
	_, clk, ownerID, svc := setupAttentionSuite(t)

	// Daily budget is 100 units. Submit packet requiring 80 units
	pkt1, err := svc.SubmitDecisionGroup(ctx, attention.SubmitGroupRequest{
		Title:                   "Heavy sovereign packet",
		Category:                attention.CategoryConstitutional,
		Urgency:                 attention.UrgencyCritical,
		EstimatedAttentionUnits: 80,
		Summary:                 "Large item",
	})
	if err != nil {
		t.Fatalf("submit pkt1: %v", err)
	}

	if err := svc.ResolveDecision(ctx, pkt1.PacketID, ownerID, attention.StatusApproved); err != nil {
		t.Fatalf("resolve pkt1: %v", err)
	}

	// Second packet requiring 30 units (80 + 30 > 100)
	pkt2, err := svc.SubmitDecisionGroup(ctx, attention.SubmitGroupRequest{
		Title:                   "Another packet",
		Category:                attention.CategoryStrategicMission,
		Urgency:                 attention.UrgencyMedium,
		EstimatedAttentionUnits: 30,
		Summary:                 "Exceeds remainder",
	})
	if err != nil {
		t.Fatalf("submit pkt2: %v", err)
	}

	// Must fail with ErrAttentionBudgetExceeded
	err = svc.ResolveDecision(ctx, pkt2.PacketID, ownerID, attention.StatusApproved)
	if !errors.Is(err, attention.ErrAttentionBudgetExceeded) {
		t.Fatalf("expected ErrAttentionBudgetExceeded, got %v", err)
	}

	// Verify budget used is still 80
	b, err := svc.GetAttentionBudget(ctx)
	if err != nil {
		t.Fatalf("get budget: %v", err)
	}
	if b.UsedBudgetUnits != 80 {
		t.Fatalf("expected used budget 80, got %d", b.UsedBudgetUnits)
	}

	// Advance clock by 25 hours (daily period rollover)
	clk.Advance(25 * time.Hour)

	// Now resolving pkt2 should succeed because daily budget rolled over
	err = svc.ResolveDecision(ctx, pkt2.PacketID, ownerID, attention.StatusApproved)
	if err != nil {
		t.Fatalf("resolve pkt2 after rollover: %v", err)
	}

	// Budget used should now be 30
	b, err = svc.GetAttentionBudget(ctx)
	if err != nil {
		t.Fatalf("get budget: %v", err)
	}
	if b.UsedBudgetUnits != 30 {
		t.Fatalf("expected used budget 30 after rollover, got %d", b.UsedBudgetUnits)
	}
}

func TestAttentionService_OwnerSummaryProjection(t *testing.T) {
	ctx := context.Background()
	store, _, _, svc := setupAttentionSuite(t)
	now := "2026-11-09T08:00:00Z"

	// Seed basic operational state: Mission & Workflow Cases (ACTIVE, BLOCKED, CLOSED)
	db := store.DB()
	_, _ = db.Exec(`INSERT INTO missions(mission_id, statement, active, created_at) VALUES ('m_alpha', 'Build autonomous company', 1, ?)`, now)
	_, _ = db.Exec(`
		INSERT INTO workflow_cases(
			case_id, mission_id, source, object_id, revision_id, observation_evidence_id,
			initial_request_json, grant_json, state, current_work_id, next_work_json,
			completed_steps, max_steps, remaining_budget, progress_signature, created_at, updated_at
		) VALUES
		('case_1', 'm_alpha', 'intake', 'issue_1', 'rev_1', 'ev_obs', '{}', '{}', 'ACTIVE', 'work_1', '{}', 1, 5, 10, 'sig_1', ?, ?),
		('case_2', 'm_alpha', 'intake', 'issue_2', 'rev_1', 'ev_obs', '{}', '{}', 'BLOCKED', 'work_2', '{}', 2, 5, 10, 'sig_2', ?, ?),
		('case_3', 'm_alpha', 'intake', 'issue_3', 'rev_1', 'ev_obs', '{}', '{}', 'CLOSED', 'work_3', '{}', 5, 5, 0, 'sig_3', ?, ?)`,
		now, now, now, now, now, now)

	// Submit one sovereign pending item and one auto-resolved item
	_, _ = svc.SubmitDecisionGroup(ctx, attention.SubmitGroupRequest{
		Title:                   "Pending sovereign security override",
		Category:                attention.CategorySafetyOverride,
		Urgency:                 attention.UrgencyCritical,
		EstimatedAttentionUnits: 15,
		Summary:                 "Security perimeter waiver",
		SubDecisions:            []attention.SubDecision{{ID: "sub_sec", Type: "OVERRIDE", Summary: "Waiver", RiskScore: 85}},
		ProposedAction:          attention.ProposedAction{ActionType: "APPROVE_WAIVER"},
	})
	_, _ = svc.SubmitDecisionGroup(ctx, attention.SubmitGroupRequest{
		Title:                   "Routine task completion",
		Category:                attention.CategoryDelegatedAutonomous,
		Urgency:                 attention.UrgencyLow,
		EstimatedAttentionUnits: 2,
		Summary:                 "Completed within budget",
		SubDecisions:            []attention.SubDecision{{ID: "sub_rtn", Type: "TASK", Summary: "Done", RiskScore: 5}},
		ProposedAction:          attention.ProposedAction{ActionType: "AUTO_CLOSE"},
	})

	proj, err := svc.GetOwnerSummary(ctx)
	if err != nil {
		t.Fatalf("get owner summary projection: %v", err)
	}

	if proj.CompanyHealth.MissionStatement != "Build autonomous company" {
		t.Fatalf("unexpected mission statement: %s", proj.CompanyHealth.MissionStatement)
	}
	if proj.CompanyHealth.ActiveWorkflowCases != 2 {
		t.Fatalf("expected 2 active workflow cases, got %d", proj.CompanyHealth.ActiveWorkflowCases)
	}
	if len(proj.PendingSovereignItems) != 1 {
		t.Fatalf("expected 1 pending sovereign item, got %d", len(proj.PendingSovereignItems))
	}
	if proj.AutonomousResolutions != 1 {
		t.Fatalf("expected 1 autonomous resolution, got %d", proj.AutonomousResolutions)
	}
	if proj.AvailableAttention != 100 {
		t.Fatalf("expected 100 available attention units, got %d", proj.AvailableAttention)
	}
}
