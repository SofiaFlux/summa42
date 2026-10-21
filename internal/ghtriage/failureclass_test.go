package ghtriage_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

type failingReviewModel struct {
	planReviewModel
	errs []error
}

func (m *failingReviewModel) ReviewPlan(ctx context.Context, in ghtriage.PlanReviewInput) (ghtriage.PlanReviewOutput, error) {
	if len(m.errs) > 0 {
		err := m.errs[0]
		m.errs = m.errs[1:]
		m.calls++
		return ghtriage.PlanReviewOutput{}, err
	}
	return m.planReviewModel.ReviewPlan(ctx, in)
}

func tickReview(t *testing.T, f *driverFixture, model ghtriage.PlanReviewModel, n int) {
	t.Helper()
	reviewer := ghtriage.NewPlanReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model)
	for i := 0; i < n; i++ {
		if _, err := reviewer.Tick(f.ctx, f.missionID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTransientModelFailuresDoNotBlockPermanently(t *testing.T) {
	f, model := readyPlanReview(t, true)
	timeout := fmt.Errorf("model invocation timed out: %w", context.DeadlineExceeded)
	m := &failingReviewModel{planReviewModel: *model, errs: []error{timeout, timeout}}
	tickReview(t, f, m, 3)
	if m.calls != 3 {
		t.Fatalf("calls = %d, want 3", m.calls)
	}
	var blocked, succeeded int
	if err := f.store.DB().QueryRowContext(f.ctx, `SELECT count(*) FROM tasks WHERE state='BLOCKED'`).Scan(&blocked); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRowContext(f.ctx, `SELECT count(*) FROM tasks WHERE state='SUCCEEDED' AND objective LIKE 'Independently review plan%'`).Scan(&succeeded); err != nil {
		t.Fatal(err)
	}
	if blocked != 0 || succeeded != 1 {
		t.Fatalf("blocked=%d succeeded review tasks=%d", blocked, succeeded)
	}
}

func TestRepeatedTimeoutsHoldCase(t *testing.T) {
	f, model := readyPlanReview(t, true)
	timeout := fmt.Errorf("model invocation timed out: %w", context.DeadlineExceeded)
	m := &failingReviewModel{planReviewModel: *model, errs: []error{timeout, timeout, timeout}}
	tickReview(t, f, m, 4)
	if m.calls != 3 {
		t.Fatalf("held case was reviewed again: calls=%d", m.calls)
	}
	if f.caseState(t, fixtureRevision) != string(workflowcase.Blocked) ||
		!hasAssessmentReason(t, f, "plan-review-failed:timeout") {
		t.Fatalf("state=%s", f.caseState(t, fixtureRevision))
	}
}

func TestRepeatedParseFailureBlocksAndHolds(t *testing.T) {
	f, model := readyPlanReview(t, true)
	_, parseErr := ghtriage.ParsePlanReviewOutput([]byte("not json"))
	if parseErr == nil {
		t.Fatal("expected parse error")
	}
	m := &failingReviewModel{planReviewModel: *model, errs: []error{parseErr, parseErr}}
	tickReview(t, f, m, 1)
	if f.caseState(t, fixtureRevision) != string(workflowcase.Active) {
		t.Fatalf("held after first parse failure: %s", f.caseState(t, fixtureRevision))
	}
	tickReview(t, f, m, 2)
	if m.calls != 2 || f.caseState(t, fixtureRevision) != string(workflowcase.Blocked) ||
		!hasAssessmentReason(t, f, "plan-review-failed:parse") {
		t.Fatalf("calls=%d state=%s", m.calls, f.caseState(t, fixtureRevision))
	}
	var blocked int
	if err := f.store.DB().QueryRowContext(f.ctx, `SELECT count(*) FROM tasks WHERE state='BLOCKED'`).Scan(&blocked); err != nil || blocked != 1 {
		t.Fatalf("blocked tasks = %d %v", blocked, err)
	}
}

// hasAssessmentReason avoids latestAssessmentReason, whose created_at ordering ties under the fixture clock.
func hasAssessmentReason(t *testing.T, f *driverFixture, reason string) bool {
	t.Helper()
	records, err := f.cases.ListAssessments(f.ctx, f.casesByRev[fixtureRevision].ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		var result workflowcase.AssessmentResult
		if err := json.Unmarshal([]byte(r.ResultJSON), &result); err != nil {
			t.Fatal(err)
		}
		if result.Decision.Reason == reason {
			return true
		}
	}
	return false
}
