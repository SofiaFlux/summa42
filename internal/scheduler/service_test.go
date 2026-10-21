package scheduler_test

import (
	"context"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/resources"
	"github.com/SofiaFlux/summa42/internal/scheduler"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

func TestNextFiltersIneligibleTasksAndRanksDeadlineFirst(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC))
	purposes := purpose.New(store, clk)
	execSvc := execution.New(store, clk, purposes)
	resourceSvc := resources.New(store, clk)
	svc := scheduler.New(store, clk, purposes, execSvc, resourceSvc, time.Minute)

	makeEnvelope := func(limit int64) domain.ID {
		t.Helper()
		id := domain.NewID("envelope")
		if _, err := store.DB().ExecContext(ctx,
			`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, ?, ?)`,
			id, limit, clk.Now().UTC().Format(time.RFC3339Nano),
		); err != nil {
			t.Fatal(err)
		}
		return id
	}
	makeTask := func(name string, envelope domain.ID, priority int, earliest, deadline time.Time, caps []string, enforcement domain.EnforcementLevel) domain.Task {
		t.Helper()
		task, err := execSvc.CreateTask(ctx, execution.TaskRequest{
			Purpose:              domain.PurposeRef{Kind: domain.PurposeOwnerDirective, ID: domain.ID("owner-" + name)},
			AcceptanceCriteria:   []string{"done"},
			RequiredCapabilities: caps,
			RequiredEnforcement:  enforcement,
			AuthorityCeiling:     append([]string(nil), caps...),
			ResourceEnvelopeID:   envelope,
			Priority:             priority,
			EarliestStart:        earliest,
			Deadline:             deadline,
		})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}

	capacity := scheduler.CapacitySnapshot{Capabilities: map[string]scheduler.CapabilityCapacity{
		"shell": {Accessible: true, Enforcement: domain.EnforcementEnforced},
	}}

	future := makeTask("future", makeEnvelope(100), 50, clk.Now().Add(time.Hour), time.Time{}, []string{"shell"}, domain.EnforcementEnforced)
	missing := makeTask("missing", makeEnvelope(100), 50, time.Time{}, time.Time{}, []string{"gpu"}, domain.EnforcementEnforced)
	weak := makeTask("weak", makeEnvelope(100), 50, time.Time{}, time.Time{}, []string{"shell"}, domain.EnforcementEnforced)
	zeroBudget := makeTask("zero-budget", makeEnvelope(0), 50, time.Time{}, time.Time{}, []string{"shell"}, domain.EnforcementEnforced)
	dependency := makeTask("dependency", makeEnvelope(100), 1, time.Time{}, time.Time{}, []string{"shell"}, domain.EnforcementEnforced)
	dependent := makeTask("dependent", makeEnvelope(100), 90, time.Time{}, time.Time{}, []string{"shell"}, domain.EnforcementEnforced)
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO task_dependencies(task_id, depends_on_task_id) VALUES (?, ?)`, dependent.ID, dependency.ID,
	); err != nil {
		t.Fatal(err)
	}

	missionID, err := purposes.CreateMission(ctx, "temporary mission")
	if err != nil {
		t.Fatal(err)
	}
	invalidPurpose, err := execSvc.CreateTask(ctx, execution.TaskRequest{
		Purpose:              domain.PurposeRef{Kind: domain.PurposeMission, ID: missionID},
		AcceptanceCriteria:   []string{"done"},
		RequiredCapabilities: []string{"shell"},
		RequiredEnforcement:  domain.EnforcementEnforced,
		AuthorityCeiling:     []string{"shell"},
		ResourceEnvelopeID:   makeEnvelope(100),
		Priority:             80,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := purposes.DeactivateMission(ctx, missionID); err != nil {
		t.Fatal(err)
	}

	deadline := makeTask("deadline", makeEnvelope(100), 1, time.Time{}, clk.Now().Add(30*time.Minute), []string{"shell"}, domain.EnforcementEnforced)
	ordinary := makeTask("ordinary", makeEnvelope(100), 100, time.Time{}, time.Time{}, []string{"shell"}, domain.EnforcementEnforced)

	weakCapacity := scheduler.CapacitySnapshot{Capabilities: map[string]scheduler.CapabilityCapacity{
		"shell": {Accessible: true, Enforcement: domain.EnforcementPartial},
	}}
	if candidate, err := svc.Next(ctx, weakCapacity); err != nil {
		t.Fatal(err)
	} else if candidate != nil {
		t.Fatalf("candidate %s was eligible on insufficient PARTIAL enforcement", candidate.Task.ID)
	}

	candidate, err := svc.Next(ctx, capacity)
	if err != nil {
		t.Fatal(err)
	}
	if candidate == nil {
		t.Fatal("expected an eligible task")
	}
	if candidate.Task.ID != deadline.ID {
		t.Fatalf("selected %s, want deadline-bound task %s", candidate.Task.ID, deadline.ID)
	}

	for _, rejected := range []domain.ID{future.ID, missing.ID, weak.ID, zeroBudget.ID, dependent.ID, invalidPurpose.ID} {
		if candidate.Task.ID == rejected {
			t.Fatalf("selected ineligible task %s", rejected)
		}
	}
	if candidate.Task.ID == ordinary.ID {
		t.Fatal("opaque priority outweighed explicit deadline urgency")
	}
}

func TestSchedulerSkipsTasksOfInactiveMission(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC))
	purposes := purpose.New(store, clk)
	execSvc := execution.New(store, clk, purposes)
	svc := scheduler.New(store, clk, purposes, execSvc, resources.New(store, clk), time.Minute)
	missionID, err := purposes.CreateMission(ctx, "to be superseded")
	if err != nil {
		t.Fatal(err)
	}
	envelope := domain.NewID("envelope")
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, ?, ?)`,
		envelope, 100, clk.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	task, err := execSvc.CreateTask(ctx, execution.TaskRequest{
		Purpose:              domain.PurposeRef{Kind: domain.PurposeMission, ID: missionID},
		AcceptanceCriteria:   []string{"done"},
		RequiredCapabilities: []string{"shell"},
		RequiredEnforcement:  domain.EnforcementEnforced,
		AuthorityCeiling:     []string{"shell"},
		ResourceEnvelopeID:   envelope,
		Priority:             50,
	})
	if err != nil {
		t.Fatal(err)
	}
	capacity := scheduler.CapacitySnapshot{Capabilities: map[string]scheduler.CapabilityCapacity{
		"shell": {Accessible: true, Enforcement: domain.EnforcementEnforced},
	}}
	if c, err := svc.Next(ctx, capacity); err != nil || c == nil || c.Task.ID != task.ID {
		t.Fatalf("active mission task not selected: %v %v", c, err)
	}
	if err := purposes.DeactivateMission(ctx, missionID); err != nil {
		t.Fatal(err)
	}
	if c, err := svc.Next(ctx, capacity); err != nil || c != nil {
		t.Fatalf("inactive mission task selected: %v %v", c, err)
	}
	var state string
	if err := store.DB().QueryRowContext(ctx, `SELECT state FROM tasks WHERE task_id = ?`, task.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(domain.TaskEligible) {
		t.Fatalf("task state %s, want it left ELIGIBLE", state)
	}
}
