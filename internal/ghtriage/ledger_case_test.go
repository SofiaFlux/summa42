package ghtriage_test

import (
	"context"
	"testing"

	"github.com/SofiaFlux/summa42/internal/capacity"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
)

// One model adapter serves every case, so each stage hands the case it is
// working on to the model call through the context, where the ledger writer
// reads it. These tests pin that for the stages that loop over a mission's cases.

type caseSeeingPlanner struct {
	*plannerModel
	caseID *string
}

func (m caseSeeingPlanner) Plan(ctx context.Context, in ghtriage.PlanInput) (ghtriage.PlanOutput, error) {
	*m.caseID = capacity.CaseIDFrom(ctx)
	return m.plannerModel.Plan(ctx, in)
}

func TestPlannerModelCallCarriesTheCaseID(t *testing.T) {
	f, model := readyPlannerFixture(t)
	var got string
	planner := ghtriage.NewPlanner(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, caseSeeingPlanner{model, &got})
	if _, err := planner.Tick(f.ctx, f.missionID); err != nil {
		t.Fatal(err)
	}
	if want := string(f.casesByRev[fixtureRevision].ID); got == "" || got != want {
		t.Fatalf("plan call case ID = %q, want %q", got, want)
	}
}

type caseSeeingPlanReviewer struct {
	*planReviewModel
	caseID *string
}

func (m caseSeeingPlanReviewer) ReviewPlan(ctx context.Context, in ghtriage.PlanReviewInput) (ghtriage.PlanReviewOutput, error) {
	*m.caseID = capacity.CaseIDFrom(ctx)
	return m.planReviewModel.ReviewPlan(ctx, in)
}

func TestPlanReviewModelCallCarriesTheCaseID(t *testing.T) {
	f, model := readyPlanReview(t, true)
	var got string
	reviewer := ghtriage.NewPlanReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, caseSeeingPlanReviewer{model, &got})
	if _, err := reviewer.Tick(f.ctx, f.missionID); err != nil {
		t.Fatal(err)
	}
	if want := string(f.casesByRev[fixtureRevision].ID); got == "" || got != want {
		t.Fatalf("plan review call case ID = %q, want %q", got, want)
	}
}

type caseSeeingReviewer struct {
	ghtriage.ReviewerModel
	caseID *string
}

func (m caseSeeingReviewer) Review(ctx context.Context, in ghtriage.ReviewInput) (bool, error) {
	*m.caseID = capacity.CaseIDFrom(ctx)
	return m.ReviewerModel.Review(ctx, in)
}

func TestTriageReviewModelCallCarriesTheCaseID(t *testing.T) {
	rf := newReviewerFixture(t)
	rf.model.ScriptedReview = []bool{true}
	var got string
	reviewer := ghtriage.NewReviewer(rf.driver.cases, rf.driver.evidenceStore, rf.index, caseSeeingReviewer{rf.model, &got}, rf.driver.clock)
	if _, err := reviewer.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	if want := string(rf.driver.casesByRev[fixtureRevision].ID); got == "" || got != want {
		t.Fatalf("triage review call case ID = %q, want %q", got, want)
	}
}
