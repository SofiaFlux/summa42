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
		got, _, err := gate.List(context.Background(), f.missionID)
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
	got, _, err := gate.List(f.ctx, f.missionID)
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
	got, caseErrs, err := gate.List(f.ctx, f.missionID)
	if err != nil || len(got) != 0 || len(caseErrs) != 1 {
		t.Fatalf("candidates=%+v caseErrs=%+v err=%v, want one CaseError for SUCCEEDED task without acceptance", got, caseErrs, err)
	}
}

func TestPlanningGateIsolatesBadCase(t *testing.T) {
	f, _ := readyPlannerFixture(t)
	const second = "2026-09-28T10:30:00Z"
	f.registerIssueRevision(t, "o/r#43", second)
	f.completeTaskWithDecision(t, second, readyToPlanDecision(43, second))
	if result, err := f.driver.Tick(f.ctx, f.missionID); err != nil || len(result.Failures) != 0 {
		t.Fatalf("triage tick: %+v, %v", result, err)
	}
	if _, err := f.store.DB().ExecContext(f.ctx, `DELETE FROM acceptance_records WHERE task_id = ?`, f.tasksByRev[second].ID); err != nil {
		t.Fatal(err)
	}
	gate := ghtriage.NewPlanningGate(f.cases, f.execSvc, f.verifSvc, f.evidenceStore)
	got, caseErrs, err := gate.List(f.ctx, f.missionID)
	if err != nil || len(got) != 1 || got[0].Case.ID != f.casesByRev[fixtureRevision].ID ||
		len(caseErrs) != 1 || caseErrs[0].CaseID != f.casesByRev[second].ID {
		t.Fatalf("candidates=%+v caseErrs=%+v err=%v", got, caseErrs, err)
	}
}

func TestPlanningGateRequiresTriageKind(t *testing.T) {
	f, _ := readyPlannerFixture(t)
	gate := ghtriage.NewPlanningGate(f.cases, f.execSvc, f.verifSvc, f.evidenceStore)
	if got, _, err := gate.List(f.ctx, f.missionID); err != nil || len(got) != 1 {
		t.Fatalf("before: %+v %v", got, err)
	}
	if _, err := f.store.DB().ExecContext(f.ctx, `UPDATE workflow_cases SET next_work_json = '{}' WHERE case_id = ?`, f.casesByRev[fixtureRevision].ID); err != nil {
		t.Fatal(err)
	}
	if got, _, err := gate.List(f.ctx, f.missionID); err != nil || len(got) != 0 {
		t.Fatalf("empty NextWork was listed: %+v %v", got, err)
	}
}
