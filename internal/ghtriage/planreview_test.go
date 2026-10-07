package ghtriage_test

import (
	"context"
	"encoding/json"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type planReviewModel struct {
	calls  int
	input  ghtriage.PlanReviewInput
	onCall func()
	output ghtriage.PlanReviewOutput
}

func TestPlanReviewResumesRecordedResultWithoutPayingAgain(t *testing.T) {
	f, model := readyPlanReview(t, true)
	if _, err := f.store.DB().ExecContext(f.ctx, `CREATE TRIGGER interrupt_review_assessment BEFORE INSERT ON workflow_assessments BEGIN SELECT RAISE(ABORT,'injected interruption'); END`); err != nil {
		t.Fatal(err)
	}
	reviewer := ghtriage.NewPlanReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model)
	out, err := reviewer.Tick(f.ctx, f.missionID)
	if err != nil || len(out.Failures) != 1 || model.calls != 1 {
		t.Fatalf("interruption %+v calls=%d %v", out, model.calls, err)
	}
	if _, err := f.store.DB().ExecContext(f.ctx, `DROP TRIGGER interrupt_review_assessment`); err != nil {
		t.Fatal(err)
	}
	out, err = reviewer.Tick(f.ctx, f.missionID)
	if err != nil || out.Reviewed != 1 || len(out.Failures) != 0 || model.calls != 1 {
		t.Fatalf("paid again after restart: %+v calls=%d %v", out, model.calls, err)
	}
}

func (m *planReviewModel) ReviewPlan(_ context.Context, in ghtriage.PlanReviewInput) (ghtriage.PlanReviewOutput, error) {
	m.calls++
	m.input = in
	if m.onCall != nil {
		m.onCall()
	}
	return m.output, nil
}
func readyPlanReview(t *testing.T, authorize bool) (*driverFixture, *planReviewModel) {
	t.Helper()
	f, model := readyPlannerFixture(t)
	if authorize {
		if _, err := f.store.DB().ExecContext(f.ctx, `UPDATE workflow_cases SET grant_json=? WHERE case_id=?`, `{"Capabilities":["github.issue.read","workspace.repo.write","workspace.test"],"Actions":["github.issue.read","workspace.repo.write","workspace.test"]}`, f.casesByRev[fixtureRevision].ID); err != nil {
			t.Fatal(err)
		}
	}
	planner := ghtriage.NewPlanner(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model)
	if err := planner.SetGrounding(groundingFixture(t)); err != nil {
		t.Fatal(err)
	}
	out, err := planner.Tick(f.ctx, f.missionID)
	if err != nil || out.Planned != 1 {
		t.Fatalf("planning %+v %v", out, err)
	}
	return f, &planReviewModel{output: ghtriage.PlanReviewOutput{Verdict: "ACCEPT", Reason: "Scope and tests are appropriate"}}
}
func TestPlanReviewAcceptsAuthorizedPinnedPlanOnce(t *testing.T) {
	f, model := readyPlanReview(t, true)
	reviewer := ghtriage.NewPlanReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model)
	out, err := reviewer.Tick(f.ctx, f.missionID)
	if err != nil || out.Reviewed != 1 || len(out.Failures) != 0 {
		t.Fatalf("review %+v %v", out, err)
	}
	c, err := f.cases.Get(f.ctx, f.casesByRev[fixtureRevision].ID)
	if err != nil || c.NextWork.Kind != ghtriage.ImplementationWorkKind || c.State != workflowcase.Active {
		t.Fatalf("handoff %+v %v", c, err)
	}
	if model.input.Context.Files[0].Content != "package test\n" || model.input.Snapshot.Title != "Crash on save" {
		t.Fatal("review did not get original inputs")
	}
	out, err = reviewer.Tick(f.ctx, f.missionID)
	if err != nil || out.Reviewed != 0 || model.calls != 1 {
		t.Fatalf("review repeated %+v calls=%d %v", out, model.calls, err)
	}
}

type blockingPlanReviewModel struct {
	calls            atomic.Int64
	entered, release chan struct{}
}

func (m *blockingPlanReviewModel) ReviewPlan(ctx context.Context, _ ghtriage.PlanReviewInput) (ghtriage.PlanReviewOutput, error) {
	if m.calls.Add(1) == 1 {
		close(m.entered)
		select {
		case <-m.release:
		case <-ctx.Done():
			return ghtriage.PlanReviewOutput{}, ctx.Err()
		}
	}
	return ghtriage.PlanReviewOutput{Verdict: "ACCEPT", Reason: "scope fits"}, nil
}
func TestPlanReviewClaimsWorkBeforeConcurrentModelCalls(t *testing.T) {
	f, _ := readyPlanReview(t, true)
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	model := &blockingPlanReviewModel{entered: make(chan struct{}), release: make(chan struct{})}
	reviewer := ghtriage.NewPlanReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model)
	first := make(chan ghtriage.PlanReviewResult, 1)
	go func() { out, _ := reviewer.Tick(ctx, f.missionID); first <- out }()
	select {
	case <-model.entered:
	case <-ctx.Done():
		t.Fatal("first model invocation did not start")
	}
	second, err := reviewer.Tick(ctx, f.missionID)
	close(model.release)
	out := <-first
	if model.calls.Load() != 1 || err != nil || len(second.Failures) != 0 || second.Reviewed != 0 || out.Reviewed != 1 {
		t.Fatalf("duplicate paid review calls=%d first=%+v second=%+v err=%v", model.calls.Load(), out, second, err)
	}
}
func TestPlanReviewCannotGrantImplementationAuthority(t *testing.T) {
	f, model := readyPlanReview(t, false)
	out, err := ghtriage.NewPlanReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model).Tick(f.ctx, f.missionID)
	if err != nil || out.Held != 1 {
		t.Fatalf("review %+v %v", out, err)
	}
	c, _ := f.cases.Get(f.ctx, f.casesByRev[fixtureRevision].ID)
	if c.State != workflowcase.Blocked {
		t.Fatalf("read-only grant expanded %+v", c)
	}
}

func TestPlanReviewHoldsRevisionAndBlockVerdicts(t *testing.T) {
	for _, verdict := range []string{"REVISE", "BLOCK"} {
		t.Run(verdict, func(t *testing.T) {
			f, model := readyPlanReview(t, true)
			model.output.Verdict = verdict
			out, err := ghtriage.NewPlanReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model).Tick(f.ctx, f.missionID)
			if err != nil || out.Held != 1 {
				t.Fatalf("%+v %v", out, err)
			}
			c, _ := f.cases.Get(f.ctx, f.casesByRev[fixtureRevision].ID)
			if c.State != workflowcase.Blocked {
				t.Fatal("non-accept verdict advanced")
			}
		})
	}
}
func TestPlanReviewRejectsNewRevisionDuringModelCall(t *testing.T) {
	f, model := readyPlanReview(t, true)
	model.onCall = func() { f.registerRevision(t, "2026-09-28T10:00:01Z") }
	out, err := ghtriage.NewPlanReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model).Tick(f.ctx, f.missionID)
	if err != nil || len(out.Failures) != 1 || out.Reviewed != 0 {
		t.Fatalf("stale review advanced %+v %v", out, err)
	}
	c, _ := f.cases.Get(f.ctx, f.casesByRev[fixtureRevision].ID)
	if c.NextWork.Kind != ghtriage.PlanReviewWorkKind {
		t.Fatal("stale review changed work")
	}
}

func TestPlanReviewExpiredLeaseCannotAdvanceCase(t *testing.T) {
	f, model := readyPlanReview(t, true)
	model.onCall = func() { f.clock.Advance(ghtriage.PlanReviewLeaseDuration + time.Second) }
	out, err := ghtriage.NewPlanReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model).Tick(f.ctx, f.missionID)
	if err != nil || out.Reviewed != 0 || len(out.Failures) != 1 {
		t.Fatalf("expired review advanced %+v %v", out, err)
	}
	c, _ := f.cases.Get(f.ctx, f.casesByRev[fixtureRevision].ID)
	if c.NextWork.Kind != ghtriage.PlanReviewWorkKind {
		t.Fatal("expired result advanced work")
	}
}
func TestPlanReviewRejectsUnGroundedV1BeforeModel(t *testing.T) {
	f, plannerModel := readyPlannerFixture(t)
	if out, err := ghtriage.NewPlanner(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, plannerModel).Tick(f.ctx, f.missionID); err != nil || out.Planned != 1 {
		t.Fatal(err)
	}
	model := &planReviewModel{}
	out, err := ghtriage.NewPlanReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model).Tick(f.ctx, f.missionID)
	if err != nil || len(out.Failures) != 1 || model.calls != 0 {
		t.Fatalf("v1 authorized %+v %v", out, err)
	}
}
func TestParsePlanReviewStrict(t *testing.T) {
	for _, raw := range []string{`{"verdict":"ACCEPT","reason":""}`, `{"verdict":"MAYBE","reason":"x"}`, `{"verdict":"ACCEPT","reason":"x","authority":["write"]}`, `{"verdict":"ACCEPT","reason":"x"} {}`} {
		if _, err := ghtriage.ParsePlanReviewOutput([]byte(raw)); err == nil {
			t.Fatalf("invalid response accepted %s", raw)
		}
	}
	out, err := ghtriage.ParsePlanReviewOutput([]byte(`{"verdict":"REVISE","reason":"needs tests"}`))
	if err != nil || out.Verdict != "REVISE" {
		t.Fatal(err)
	}
}

func TestPlanReviewRejectsContextHashDriftBeforeModel(t *testing.T) {
	f, model := readyPlanReview(t, true)
	plan, _ := storedPlan(t, f)
	plan.Source.ContextHash = strings.Repeat("0", 64)
	raw, err := plan.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	object, err := f.evidenceStore.Put(f.ctx, strings.NewReader(string(raw)), evidence.Metadata{Kind: ghtriage.KindPlan, MediaType: "application/json"})
	if err != nil {
		t.Fatal(err)
	}
	records, _ := f.cases.ListAssessments(f.ctx, plan.CaseID)
	var req workflowcase.AssessmentRequest
	json.Unmarshal([]byte(records[0].RequestJSON), &req)
	req.Assessment.EvidenceIDs = []string{string(object.ID)}
	changed, _ := json.Marshal(req)
	if _, err := f.store.DB().ExecContext(f.ctx, `UPDATE workflow_assessments SET request_json=? WHERE assessment_id=?`, string(changed), records[0].ID); err != nil {
		t.Fatal(err)
	}
	out, err := ghtriage.NewPlanReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model).Tick(f.ctx, f.missionID)
	if err != nil || len(out.Failures) != 1 || model.calls != 0 {
		t.Fatalf("hash drift reviewed %+v calls=%d %v", out, model.calls, err)
	}
}
