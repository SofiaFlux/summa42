package ghtriage_test

import (
	"context"
	"testing"

	"github.com/SofiaFlux/summa42/internal/ghtriage"
)

func TestPlanningCandidatesRequireAcceptedReadyDecisionAndLatestRevision(t *testing.T) {
	f := newDriverFixture(t)
	f.registerRevision(t, fixtureRevision)
	gate := ghtriage.NewPlanningGate(f.cases, f.execSvc, f.verifSvc, f.evidenceStore)
	check := func(want int) {
		t.Helper()
		got, err := gate.List(context.Background(), f.missionID)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != want {
			t.Fatalf("planning candidates = %+v, want %d", got, want)
		}
	}
	check(0) // ACTIVE after intake is not a triage decision.
	f.completeTaskWithDecision(t, fixtureRevision, readyToPlanDecision(42, fixtureRevision))
	check(0) // A completed Task is not yet accepted.
	result, err := f.driver.Tick(f.ctx, f.missionID)
	if err != nil || len(result.Failures) != 0 {
		t.Fatalf("triage tick: %+v, %v", result, err)
	}
	check(1)
	if _, err := f.store.DB().ExecContext(f.ctx, `UPDATE tasks SET task_class = 'unrelated' WHERE task_id = ?`, f.tasksByRev[fixtureRevision].ID); err != nil {
		t.Fatal(err)
	}
	check(0) // A succeeded task of another class is not a triage task.
	if _, err := f.store.DB().ExecContext(f.ctx, `UPDATE tasks SET task_class = ? WHERE task_id = ?`, ghtriage.TaskClass, f.tasksByRev[fixtureRevision].ID); err != nil {
		t.Fatal(err)
	}
	f.registerRevision(t, "2026-09-28T11:00:00Z")
	check(0) // Registration alone supersedes the accepted older revision.
}

func TestPlanningCandidatesRejectAcceptedNonReadyDecisionDuringCrashWindow(t *testing.T) {
	f := newDriverFixture(t)
	f.simulateRestartAfterAcceptance(t, fixtureIssue, fixtureRevision)
	gate := ghtriage.NewPlanningGate(f.cases, f.execSvc, f.verifSvc, f.evidenceStore)
	got, err := gate.List(f.ctx, f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("planning candidates = %+v, want none for accepted not-actionable decision", got)
	}
}

func TestPlanningCandidatesRequireDurableAcceptanceOfTheirDecision(t *testing.T) {
	f := newDriverFixture(t)
	f.completeTaskWithDecision(t, fixtureRevision, readyToPlanDecision(42, fixtureRevision))
	result, err := f.driver.Tick(f.ctx, f.missionID)
	if err != nil || len(result.Failures) != 0 {
		t.Fatalf("triage tick: %+v, %v", result, err)
	}
	if _, err := f.store.DB().ExecContext(f.ctx, `DELETE FROM acceptance_records WHERE task_id = ?`, f.tasksByRev[fixtureRevision].ID); err != nil {
		t.Fatal(err)
	}
	gate := ghtriage.NewPlanningGate(f.cases, f.execSvc, f.verifSvc, f.evidenceStore)
	if got, err := gate.List(f.ctx, f.missionID); err == nil {
		t.Fatalf("planning candidates = %+v, want an error for SUCCEEDED task without acceptance", got)
	}
}
