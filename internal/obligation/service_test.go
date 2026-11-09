package obligation_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/obligation"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/resources"
	"github.com/SofiaFlux/summa42/internal/scheduler"
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

func setupObligationSuite(t *testing.T) (*state.Store, *fixedClock, *obligation.Service) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(context.Background(), filepath.Join(dir, "obligation_test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.DB().Close() })

	clk := &fixedClock{now: time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)}
	svc := obligation.NewService(store, clk)

	// Ensure default envelope for execution tasks if not already seeded
	_, _ = store.DB().Exec(`
		INSERT OR IGNORE INTO resource_envelopes(envelope_id, hard_limit, created_at)
		VALUES ('envelope_default', 100000, '2026-11-09T08:00:00Z')`)

	return store, clk, svc
}

func TestObligationService_CreateAndModel(t *testing.T) {
	ctx := context.Background()
	_, clk, svc := setupObligationSuite(t)

	due := clk.Now().Add(5 * 24 * time.Hour)
	ob, err := svc.CreateObligation(ctx, obligation.CreateObligationRequest{
		Source:      "CUSTOMER_CONTRACT_ACME",
		Statement:   "Submit quarterly SOC2 compliance audit report",
		EntityID:    "cust_acme_1",
		DueAt:       due,
		GracePeriod: 12 * time.Hour,
		Consequences: obligation.Consequences{
			FinancialPenalty: 25000,
			SLABreach:        true,
			SuspensionRisk:   true,
			Description:      "Immediate contractual termination and penalty fine",
		},
		CostOfDelayRate: 500,
		LeadTime:        48 * time.Hour,
	})
	if err != nil {
		t.Fatalf("create obligation: %v", err)
	}

	if ob.ID == "" {
		t.Fatal("expected generated obligation ID")
	}
	if ob.LifecycleState != obligation.StatePending {
		t.Fatalf("expected PENDING state, got %s", ob.LifecycleState)
	}
	if ob.CostOfDelayRate != 500 {
		t.Fatalf("expected cost of delay rate 500, got %d", ob.CostOfDelayRate)
	}
	if ob.Consequences.FinancialPenalty != 25000 || !ob.Consequences.SLABreach {
		t.Fatalf("unexpected consequences: %+v", ob.Consequences)
	}

	fetched, err := svc.GetObligation(ctx, ob.ID)
	if err != nil {
		t.Fatalf("get obligation: %v", err)
	}
	if fetched.Statement != ob.Statement || fetched.EntityID != "cust_acme_1" {
		t.Fatalf("fetched obligation mismatch: %+v", fetched)
	}
}

func TestObligationService_LeadTimeTriggeringAndSynthesis(t *testing.T) {
	ctx := context.Background()
	store, clk, svc := setupObligationSuite(t)

	// Due in 3 days, lead time is 48h (2 days). Currently 3 days away, so outside lead time.
	due := clk.Now().Add(72 * time.Hour)
	ob, err := svc.CreateObligation(ctx, obligation.CreateObligationRequest{
		Source:          "TLS_CERTIFICATE",
		Statement:       "Renew wildcard TLS certificate *.api.summa42.io",
		DueAt:           due,
		GracePeriod:     6 * time.Hour,
		CostOfDelayRate: 100,
		LeadTime:        48 * time.Hour,
	})
	if err != nil {
		t.Fatalf("create obligation: %v", err)
	}

	// 1. Initial evaluation outside lead time: no tasks synthesized
	evaluated, tasks, err := svc.EvaluateAndSchedule(ctx)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("expected 0 synthesized tasks outside lead time, got %d", len(tasks))
	}
	if len(evaluated) != 1 || evaluated[0].LifecycleState != obligation.StatePending {
		t.Fatalf("expected pending obligation, got %+v", evaluated)
	}

	// 2. Advance clock by 25 hours -> now within 47 hours of due date (inside 48h lead time)
	clk.Advance(25 * time.Hour)

	evaluated, tasks, err = svc.EvaluateAndSchedule(ctx)
	if err != nil {
		t.Fatalf("evaluate in lead time: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 synthesized task inside lead time, got %d", len(tasks))
	}

	task := tasks[0]
	if task.Purpose.Kind != domain.PurposeObligation || task.Purpose.ID != ob.ID {
		t.Fatalf("unexpected task purpose: %+v", task.Purpose)
	}
	if task.Deadline.IsZero() || !task.Deadline.Equal(due) {
		t.Fatalf("expected deadline to equal due date %v, got %v", due, task.Deadline)
	}
	if task.Priority < obligation.BaseObligationPriority {
		t.Fatalf("expected priority >= %d, got %d", obligation.BaseObligationPriority, task.Priority)
	}

	// Verify obligation updated to SCHEDULED with link to task
	obAfter, err := svc.GetObligation(ctx, ob.ID)
	if err != nil {
		t.Fatalf("get obligation: %v", err)
	}
	if obAfter.LifecycleState != obligation.StateScheduled {
		t.Fatalf("expected SCHEDULED state, got %s", obAfter.LifecycleState)
	}
	if obAfter.SynthesizedTaskID != task.ID {
		t.Fatalf("expected synthesized task ID %s, got %s", task.ID, obAfter.SynthesizedTaskID)
	}

	// 3. Idempotent check: repeated evaluation does not synthesize duplicate task
	_, tasks2, err := svc.EvaluateAndSchedule(ctx)
	if err != nil {
		t.Fatalf("evaluate again: %v", err)
	}
	if len(tasks2) != 0 {
		t.Fatalf("expected 0 new tasks on duplicate eval, got %d", len(tasks2))
	}

	// 4. Lifecycle transition: when task SUCCEEDED -> obligation marked FULFILLED
	_, err = store.DB().Exec(`UPDATE tasks SET state = 'SUCCEEDED' WHERE task_id = ?`, task.ID)
	if err != nil {
		t.Fatalf("update task state: %v", err)
	}

	evaluated, _, err = svc.EvaluateAndSchedule(ctx)
	if err != nil {
		t.Fatalf("evaluate after task success: %v", err)
	}
	if len(evaluated) != 1 || evaluated[0].LifecycleState != obligation.StateFulfilled {
		t.Fatalf("expected obligation FULFILLED, got %+v", evaluated)
	}
	if evaluated[0].Active {
		t.Fatal("expected fulfilled obligation to be inactive (active=0)")
	}
	if evaluated[0].ResolvedAt == nil {
		t.Fatal("expected resolved_at timestamp to be set")
	}
}

func TestObligationService_BreachPastGracePeriod(t *testing.T) {
	ctx := context.Background()
	_, clk, svc := setupObligationSuite(t)

	// Due in 2 hours, grace period 1 hour.
	due := clk.Now().Add(2 * time.Hour)
	ob, err := svc.CreateObligation(ctx, obligation.CreateObligationRequest{
		Source:          "REGULATORY_FILING",
		Statement:       "Submit monthly tax statement",
		DueAt:           due,
		GracePeriod:     1 * time.Hour,
		CostOfDelayRate: 300,
		LeadTime:        5 * time.Hour,
	})
	if err != nil {
		t.Fatalf("create obligation: %v", err)
	}

	// Advance clock past due + grace period (2h + 1h + 10m = 3h10m)
	clk.Advance(3*time.Hour + 10*time.Minute)

	evaluated, _, err := svc.EvaluateAndSchedule(ctx)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(evaluated) != 1 || evaluated[0].LifecycleState != obligation.StateBreached {
		t.Fatalf("expected BREACHED state, got %+v", evaluated)
	}
	if evaluated[0].Active {
		t.Fatal("expected breached obligation to be deactivated (active=0)")
	}
	if evaluated[0].ResolvedAt == nil {
		t.Fatal("expected resolved_at to be populated on breach")
	}

	// Verify in DB
	fetched, err := svc.GetObligation(ctx, ob.ID)
	if err != nil {
		t.Fatalf("get obligation: %v", err)
	}
	if fetched.LifecycleState != obligation.StateBreached {
		t.Fatalf("expected BREACHED state in DB, got %s", fetched.LifecycleState)
	}
}

func TestObligationService_TaskFailureResetAndReAttempt(t *testing.T) {
	ctx := context.Background()
	store, clk, svc := setupObligationSuite(t)

	due := clk.Now().Add(24 * time.Hour)
	_, err := svc.CreateObligation(ctx, obligation.CreateObligationRequest{
		Source:          "BACKUP_AUDIT",
		Statement:       "Run database snapshot verification",
		DueAt:           due,
		GracePeriod:     2 * time.Hour,
		CostOfDelayRate: 50,
		LeadTime:        48 * time.Hour, // already in lead time
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// 1. Initial eval synthesizes task
	_, tasks, err := svc.EvaluateAndSchedule(ctx)
	if err != nil {
		t.Fatalf("eval 1: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	taskID := tasks[0].ID

	// 2. Task encounters terminal failure (e.g. FAILED)
	_, err = store.DB().Exec(`UPDATE tasks SET state = 'FAILED' WHERE task_id = ?`, taskID)
	if err != nil {
		t.Fatalf("set task failed: %v", err)
	}

	// 3. Next evaluation detects failed task, resets obligation to PENDING, and immediately re-synthesizes a fresh task candidate
	evaluated, tasksAfter, err := svc.EvaluateAndSchedule(ctx)
	if err != nil {
		t.Fatalf("eval 2: %v", err)
	}
	if len(evaluated) != 1 || evaluated[0].LifecycleState != obligation.StateScheduled {
		t.Fatalf("expected SCHEDULED state after re-attempt, got %+v", evaluated)
	}
	if len(tasksAfter) != 1 {
		t.Fatalf("expected 1 fresh task synthesized on retry, got %d", len(tasksAfter))
	}
	if tasksAfter[0].ID == taskID {
		t.Fatal("expected new task ID on re-attempt, got same ID")
	}
}

func TestObligationAwarePlanner_NonPreemptionByGoals(t *testing.T) {
	ctx := context.Background()
	store, clk, obSvc := setupObligationSuite(t)

	// Set up purpose, execution, resources, and scheduler services
	purposeSvc := purpose.New(store, clk)
	missionID, err := purposeSvc.CreateMission(ctx, "Accelerate Collective Engineering")
	if err != nil {
		t.Fatalf("create mission: %v", err)
	}

	_, err = purposeSvc.CreateGoal(ctx, domain.PurposeRef{Kind: domain.PurposeMission, ID: missionID}, "Optimize LLM prompt cache")
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}

	execSvc := execution.New(store, clk, purposeSvc, nil)
	resourceSvc := resources.New(store, clk)

	schedSvc := scheduler.New(store, clk, purposeSvc, execSvc, resourceSvc, 5*time.Minute)
	planner := obligation.NewPlannerAdapter(obSvc, schedSvc)

	// Seed an attractive discretionary Goal Task with high standard priority (e.g. 50) and no deadline
	goalTaskID := domain.NewID("task_goal")
	nowStr := domain.FormatTime(clk.Now())
	_, err = store.DB().Exec(`
		INSERT INTO tasks (
			task_id, purpose_kind, purpose_id, state, acceptance_criteria_json,
			required_capabilities_json, required_enforcement, authority_ceiling_json,
			resource_envelope_id, priority, created_at, updated_at
		) VALUES (?, 'MISSION', ?, 'ELIGIBLE', '["optimize cache"]', '[]', 'ENFORCED', '[]', 'envelope_default', 50, ?, ?)`,
		goalTaskID, missionID, nowStr, nowStr,
	)
	if err != nil {
		t.Fatalf("seed goal task: %v", err)
	}

	// Create approaching Obligation due in 12 hours with 24h lead time (already in lead time)
	due := clk.Now().Add(12 * time.Hour)
	_, err = obSvc.CreateObligation(ctx, obligation.CreateObligationRequest{
		Source:          "CRITICAL_SLA",
		Statement:       "Ensure enterprise customer backup parity",
		DueAt:           due,
		GracePeriod:     2 * time.Hour,
		CostOfDelayRate: 200,
		LeadTime:        24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("create obligation: %v", err)
	}

	// Planning step: PlanNext must evaluate obligations, synthesize the obligation task,
	// and select it ahead of the goal task (anti-starvation guarantee)!
	capacity := scheduler.CapacitySnapshot{
		Capabilities: map[string]scheduler.CapabilityCapacity{},
	}

	candidate, err := planner.PlanNext(ctx, capacity)
	if err != nil {
		t.Fatalf("plan next: %v", err)
	}
	if candidate == nil {
		t.Fatal("expected scheduled task candidate, got nil")
	}

	if candidate.Task.Purpose.Kind != domain.PurposeObligation {
		t.Fatalf("expected Obligation task to be scheduled first to prevent starvation, got %s (task ID %s)", candidate.Task.Purpose.Kind, candidate.Task.ID)
	}
	if candidate.Task.Priority <= 50 {
		t.Fatalf("expected obligation task priority > 50, got %d", candidate.Task.Priority)
	}
	if candidate.Task.Deadline.IsZero() {
		t.Fatal("expected deadline to be set on obligation task")
	}
}
