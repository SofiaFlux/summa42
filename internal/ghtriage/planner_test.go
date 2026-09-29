package ghtriage_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

type plannerModel struct {
	calls  int
	input  ghtriage.PlanInput
	output ghtriage.PlanOutput
	err    error
	onCall func()
}

func (m *plannerModel) Plan(_ context.Context, input ghtriage.PlanInput) (ghtriage.PlanOutput, error) {
	m.calls++
	m.input = input
	if m.onCall != nil {
		m.onCall()
	}
	return m.output, m.err
}

func validPlannerOutput() ghtriage.PlanOutput {
	return ghtriage.PlanOutput{Summary: "Fix the save crash", Steps: []ghtriage.PlanStep{
		{Objective: "Reproduce the crash", DoneWhen: "A regression test fails before the fix"},
		{Objective: "Fix save", DoneWhen: "The regression test passes after the fix"},
	}}
}

func readyPlannerFixture(t *testing.T) (*driverFixture, *plannerModel) {
	t.Helper()
	f := newDriverFixture(t)
	f.completeTaskWithDecision(t, fixtureRevision, readyToPlanDecision(42, fixtureRevision))
	result, err := f.driver.Tick(f.ctx, f.missionID)
	if err != nil || len(result.Failures) != 0 {
		t.Fatalf("triage tick: result=%+v err=%v", result, err)
	}
	return f, &plannerModel{output: validPlannerOutput()}
}

func TestPlannerStoresPlanAndAdvancesReadyCaseOnce(t *testing.T) {
	f, model := readyPlannerFixture(t)
	planner := ghtriage.NewPlanner(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model)
	before := f.casesByRev[fixtureRevision]
	result, err := planner.Tick(f.ctx, f.missionID)
	if err != nil || len(result.Failures) != 0 || result.Planned != 1 {
		t.Fatalf("planner tick: result=%+v err=%v", result, err)
	}
	if model.calls != 1 || model.input.Question != ghtriage.PlanQuestion || model.input.Snapshot.Title != "Crash on save" {
		t.Fatalf("model calls=%d input=%+v", model.calls, model.input)
	}
	current, err := f.cases.Get(f.ctx, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != workflowcase.Active || current.CurrentWorkID == before.CurrentWorkID ||
		current.NextWork.Kind != ghtriage.PlanReviewWorkKind || current.CompletedSteps != 1 {
		t.Fatalf("case after planning = %+v", current)
	}
	if _, found, err := f.execSvc.FindByIdempotencyKey(f.ctx, string(current.CurrentWorkID)); err != nil || found {
		t.Fatalf("review Work was prematurely materialized: found=%v err=%v", found, err)
	}
	records, err := f.cases.ListAssessments(f.ctx, before.ID)
	if err != nil || len(records) != 1 {
		t.Fatalf("assessments=%+v err=%v", records, err)
	}
	var request workflowcase.AssessmentRequest
	if err := json.Unmarshal([]byte(records[0].RequestJSON), &request); err != nil {
		t.Fatal(err)
	}
	if request.Assessment.Reason != ghtriage.PlanningAssessmentReason || !request.RequireLatestRevision || len(request.Assessment.EvidenceIDs) != 1 {
		t.Fatalf("planning assessment = %+v", request)
	}
	object, raw, err := f.evidenceStore.Get(f.ctx, domain.ID(request.Assessment.EvidenceIDs[0]))
	if err != nil || object.Kind != ghtriage.KindPlan {
		t.Fatalf("plan evidence = %+v, %v", object, err)
	}
	var plan ghtriage.Plan
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.CaseID != before.ID || plan.Revision != before.RevisionID || plan.Summary != model.output.Summary {
		t.Fatalf("stored plan = %+v", plan)
	}
	second, err := planner.Tick(f.ctx, f.missionID)
	if err != nil || second.Planned != 0 || model.calls != 1 {
		t.Fatalf("repeat tick=%+v err=%v calls=%d", second, err, model.calls)
	}
}

func TestPlannerRejectsRevisionRegisteredDuringModelCall(t *testing.T) {
	f, model := readyPlannerFixture(t)
	model.onCall = func() { f.registerRevision(t, "2026-09-28T10:00:00.5Z") }
	planner := ghtriage.NewPlanner(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model)
	result, err := planner.Tick(f.ctx, f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Planned != 0 || len(result.Failures) != 1 {
		t.Fatalf("race result = %+v", result)
	}
	if n := f.assessmentCount(t, fixtureRevision); n != 0 {
		t.Fatalf("stale case assessments = %d", n)
	}
}

func TestPlannerModelFailureLeavesReadyCaseUntouched(t *testing.T) {
	f, model := readyPlannerFixture(t)
	model.err = errors.New("model unavailable")
	planner := ghtriage.NewPlanner(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model)
	result, err := planner.Tick(f.ctx, f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Planned != 0 || len(result.Failures) != 1 {
		t.Fatalf("model failure result = %+v", result)
	}
	if n := f.assessmentCount(t, fixtureRevision); n != 0 {
		t.Fatalf("failed plan assessments = %d", n)
	}
}
