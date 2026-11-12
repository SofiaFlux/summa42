package ghtriage_test

import (
	"context"
	"strings"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/ghtriage/fakemodel"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

// Card M1-T1: bounded automatic repair after REVISE. These tests drive the
// real planner, plan reviewer, implementer and code reviewer over a real
// store; only the model answers are scripted, in this test file.

// giveSteps raises the case's step limit: the driver fixture allows three
// steps, and a repaired case takes more.
func giveSteps(t *testing.T, f *driverFixture, id domain.ID, steps int) {
	t.Helper()
	if _, err := f.store.DB().ExecContext(f.ctx, `UPDATE workflow_cases SET max_steps = ? WHERE case_id = ?`, steps, id); err != nil {
		t.Fatal(err)
	}
}

func caseAt(t *testing.T, f *driverFixture, id domain.ID) workflowcase.Case {
	t.Helper()
	c, err := f.cases.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// auditMatches runs the triage reviewer's audit of the case history and
// reports whether it finds the case's state consistent with that history.
func auditMatches(t *testing.T, f *driverFixture) bool {
	t.Helper()
	model := fakemodel.New()
	model.ScriptedReview = []bool{true}
	index := ghtriage.NewReviewIndex(f.store, f.clock)
	rf := &reviewerFixture{driver: f, model: model, index: index, review: ghtriage.NewReviewer(f.cases, f.evidenceStore, index, model, f.clock)}
	if _, err := rf.review.Tick(context.Background(), f.missionID); err != nil {
		t.Fatal(err)
	}
	return rf.lastVerdict(t).Structural.StateMatchesDisposition
}

func TestReviseStartsBoundedRepair(t *testing.T) {
	f, cfg, plannerModel, id := repairablePlan(t)
	review := &planReviewModel{output: ghtriage.PlanReviewOutput{Verdict: "REVISE", Reason: "the plan does not say which test proves the fix"}}
	planReviewer := func() *ghtriage.PlanReviewer {
		return ghtriage.NewPlanReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, review).WithMaxRepairs(2)
	}
	before := caseAt(t, f, id)

	out, err := planReviewer().Tick(f.ctx, f.missionID)
	if err != nil || out.Reviewed != 1 || out.Held != 0 || len(out.Failures) != 0 {
		t.Fatalf("REVISE with repairs left: %+v %v", out, err)
	}
	repair := caseAt(t, f, id)
	if repair.State != workflowcase.Active || repair.NextWork.Kind != ghtriage.PlanRepairWorkKind || repair.CurrentWorkID == before.CurrentWorkID {
		t.Fatalf("case after REVISE: state %s, work kind %q", repair.State, repair.NextWork.Kind)
	}
	if !auditMatches(t, f) {
		t.Fatal("the history audit reports a case at plan repair as a violation")
	}

	// The planner writes a new plan for the repair work, with the reviewer's
	// reason as data and a question that says what it is.
	plannerModel.output.Summary = "Fix the save crash and name the regression test"
	planner := ghtriage.NewPlanner(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, plannerModel)
	if err := planner.SetGrounding(cfg); err != nil {
		t.Fatal(err)
	}
	planned, err := planner.Tick(f.ctx, f.missionID)
	if err != nil || planned.Planned != 1 || len(planned.Failures) != 0 {
		t.Fatalf("re-plan: %+v %v", planned, err)
	}
	if got := plannerModel.input.PriorReviews; len(got) != 1 || got[0].Stage != ghtriage.RepairOfPlanReview || got[0].Reason != "the plan does not say which test proves the fix" {
		t.Fatalf("planner input prior_reviews = %+v", got)
	}
	if !strings.Contains(plannerModel.input.Question, "prior_reviews") || !strings.Contains(plannerModel.input.Question, "untrusted data") {
		t.Fatalf("the planner's question does not explain prior_reviews: %q", plannerModel.input.Question)
	}
	replanned := caseAt(t, f, id)
	if replanned.State != workflowcase.Active || replanned.NextWork.Kind != ghtriage.PlanReviewWorkKind {
		t.Fatalf("case after re-plan: state %s, work kind %q", replanned.State, replanned.NextWork.Kind)
	}
	if !auditMatches(t, f) {
		t.Fatal("the history audit reports a re-planned case as a violation")
	}

	// The new plan gets a new review; ACCEPT hands the case to implementation.
	review.output = ghtriage.PlanReviewOutput{Verdict: "ACCEPT", Reason: "scope and test are named"}
	out, err = planReviewer().Tick(f.ctx, f.missionID)
	if err != nil || out.Reviewed != 1 || out.Held != 0 || len(out.Failures) != 0 {
		t.Fatalf("review of the repaired plan: %+v %v", out, err)
	}
	if review.calls != 2 {
		t.Fatalf("review model calls = %d, want one per plan", review.calls)
	}
	if review.input.Plan.Summary != "Fix the save crash and name the regression test" {
		t.Fatalf("the second review read plan %q, not the repaired one", review.input.Plan.Summary)
	}
	accepted := caseAt(t, f, id)
	if accepted.State != workflowcase.Active || accepted.NextWork.Kind != ghtriage.ImplementationWorkKind {
		t.Fatalf("case after ACCEPT: state %s, work kind %q", accepted.State, accepted.NextWork.Kind)
	}

	// The repaired chain carries through implementation and code review
	// without any manual step.
	implemented, err := newImplementer(t, f, &implementationModel{}).Prepare(f.ctx, id, cfg.Source.LocalPath, t.TempDir())
	if err != nil || implemented.Record == nil {
		t.Fatalf("implementation of the repaired plan: %+v %v", implemented, err)
	}
	result, err := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, &codeReviewModel{verdict: "ACCEPT"}).Review(f.ctx, id, cfg.Source.LocalPath)
	if err != nil || !result.Accepted {
		t.Fatalf("code review of the repaired case: %+v %v", result, err)
	}
}

// repairablePlan is a planned, authorized case with room for repairs.
func repairablePlan(t *testing.T) (*driverFixture, ghtriage.GroundingConfig, *plannerModel, domain.ID) {
	t.Helper()
	f, plannerModel := readyPlannerFixture(t)
	cfg := groundingFixture(t)
	cfg.ValidationCommands = [][]string{{"test", "-f", "code.go"}}
	id := f.casesByRev[fixtureRevision].ID
	if _, err := f.store.DB().ExecContext(f.ctx, `UPDATE workflow_cases SET grant_json=? WHERE case_id=?`, `{"Capabilities":["github.issue.read","workspace.repo.write","workspace.test"],"Actions":["github.issue.read","workspace.repo.write","workspace.test"]}`, id); err != nil {
		t.Fatal(err)
	}
	giveSteps(t, f, id, 20)
	planner := ghtriage.NewPlanner(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, plannerModel)
	if err := planner.SetGrounding(cfg); err != nil {
		t.Fatal(err)
	}
	if out, err := planner.Tick(f.ctx, f.missionID); err != nil || out.Planned != 1 {
		t.Fatalf("plan %+v %v", out, err)
	}
	return f, cfg, plannerModel, id
}

func TestThirdReviseBlocksCase(t *testing.T) {
	f, cfg, plannerModel, id := repairablePlan(t)
	review := &planReviewModel{output: ghtriage.PlanReviewOutput{Verdict: "REVISE", Reason: "still not specific"}}
	for round := 1; round <= 3; round++ {
		review.output.Reason = "still not specific, round " + string(rune('0'+round))
		out, err := ghtriage.NewPlanReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, review).WithMaxRepairs(2).Tick(f.ctx, f.missionID)
		if err != nil || out.Reviewed != 1 || len(out.Failures) != 0 {
			t.Fatalf("round %d: %+v %v", round, out, err)
		}
		c := caseAt(t, f, id)
		if round == 3 {
			if out.Held != 1 || c.State != workflowcase.Blocked {
				t.Fatalf("the third REVISE did not hold the case: held=%d state=%s", out.Held, c.State)
			}
			assertAssessed(t, f, id, workflowcase.Blocked, "plan-review:REVISE: still not specific, round 3")
			break
		}
		if c.State != workflowcase.Active || c.NextWork.Kind != ghtriage.PlanRepairWorkKind {
			t.Fatalf("round %d: state %s, work kind %q", round, c.State, c.NextWork.Kind)
		}
		plannerModel.output.Summary = "Fix the save crash, attempt " + string(rune('1'+round))
		planner := ghtriage.NewPlanner(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, plannerModel)
		if err := planner.SetGrounding(cfg); err != nil {
			t.Fatal(err)
		}
		if planned, err := planner.Tick(f.ctx, f.missionID); err != nil || planned.Planned != 1 {
			t.Fatalf("round %d re-plan: %+v %v", round, planned, err)
		}
		if got := len(plannerModel.input.PriorReviews); got != round {
			t.Fatalf("round %d: planner saw %d prior reviews", round, got)
		}
	}
	if !auditMatches(t, f) {
		t.Fatal("the history audit reports a case held after its repairs as a violation")
	}
}

func TestReviseWithoutRepairsHoldsAsBefore(t *testing.T) {
	f, _, _, id := repairablePlan(t)
	review := &planReviewModel{output: ghtriage.PlanReviewOutput{Verdict: "REVISE", Reason: "not specific"}}
	out, err := ghtriage.NewPlanReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, review).Tick(f.ctx, f.missionID)
	if err != nil || out.Held != 1 || caseAt(t, f, id).State != workflowcase.Blocked {
		t.Fatalf("REVISE with no repairs configured: %+v %v", out, err)
	}
}

func TestBlockVerdictIsNeverRepaired(t *testing.T) {
	f, _, _, id := repairablePlan(t)
	review := &planReviewModel{output: ghtriage.PlanReviewOutput{Verdict: "BLOCK", Reason: "the issue asks for something unsafe"}}
	out, err := ghtriage.NewPlanReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, review).WithMaxRepairs(2).Tick(f.ctx, f.missionID)
	if err != nil || out.Held != 1 || caseAt(t, f, id).State != workflowcase.Blocked {
		t.Fatalf("BLOCK with repairs configured: %+v %v", out, err)
	}
}

func TestRepairRespectsEnvelope(t *testing.T) {
	t.Run("step limit", func(t *testing.T) {
		f, _, _, id := repairablePlan(t)
		// One step is used (the plan); a limit of one leaves no step for a repair.
		giveSteps(t, f, id, 1)
		review := &planReviewModel{output: ghtriage.PlanReviewOutput{Verdict: "REVISE", Reason: "not specific"}}
		out, err := ghtriage.NewPlanReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, review).WithMaxRepairs(2).Tick(f.ctx, f.missionID)
		if err != nil || out.Held != 1 {
			t.Fatalf("%+v %v", out, err)
		}
		assertAssessed(t, f, id, workflowcase.Blocked, "maximum steps exhausted")
	})
	t.Run("budget", func(t *testing.T) {
		f, _, _, id := repairablePlan(t)
		if _, err := f.store.DB().ExecContext(f.ctx, `UPDATE workflow_cases SET remaining_budget = 0 WHERE case_id = ?`, id); err != nil {
			t.Fatal(err)
		}
		review := &planReviewModel{output: ghtriage.PlanReviewOutput{Verdict: "REVISE", Reason: "not specific"}}
		out, err := ghtriage.NewPlanReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, review).WithMaxRepairs(2).Tick(f.ctx, f.missionID)
		if err != nil || out.Held != 1 {
			t.Fatalf("%+v %v", out, err)
		}
		assertAssessed(t, f, id, workflowcase.Blocked, "remaining budget exhausted")
	})
}

func TestRepairCannotReuseOldAcceptance(t *testing.T) {
	f, cfg, id, first := implementedFixture(t)
	giveSteps(t, f, id, 20)
	oldWork := caseAt(t, f, id).CurrentWorkID
	reviewer := func(verdict string) (*ghtriage.CodeReviewer, *codeReviewModel) {
		model := &codeReviewModel{verdict: verdict}
		return ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model).WithMaxRepairs(2), model
	}

	revise, _ := reviewer("REVISE")
	rejected, err := revise.Review(f.ctx, id, cfg.Source.LocalPath)
	if err != nil || rejected.Accepted {
		t.Fatalf("REVISE: %+v %v", rejected, err)
	}
	repair := caseAt(t, f, id)
	if repair.State != workflowcase.Active || repair.NextWork.Kind != ghtriage.ImplementationWorkKind || repair.CurrentWorkID == oldWork {
		t.Fatalf("case after a code review REVISE: state %s, kind %q, same work %v", repair.State, repair.NextWork.Kind, repair.CurrentWorkID == oldWork)
	}
	if !auditMatches(t, f) {
		t.Fatal("the history audit reports a case at implementation repair as a violation")
	}

	// The rejected candidate exists and passed its validation, but nothing of
	// it counts for the new work: a reviewer that would accept has nothing to
	// review until a new candidate is produced, and it is not even asked.
	accept, acceptModel := reviewer("ACCEPT")
	if result, err := accept.Review(f.ctx, id, cfg.Source.LocalPath); err == nil {
		t.Fatalf("a code review ran on the repair work before a new candidate existed: %+v", result)
	}
	if acceptModel.calls.Load() != 0 {
		t.Fatal("the reviewer model was called for work that has no candidate")
	}
	oldTask, found, err := f.execSvc.FindByIdempotencyKey(f.ctx, string(oldWork))
	if err != nil || !found || oldTask.State == domain.TaskSucceeded {
		t.Fatalf("the rejected implementation Task: found=%v state=%s err=%v; it must not be accepted", found, oldTask.State, err)
	}

	// A new candidate is produced for the new work, with the reviewer's reason.
	implementer := &implementationModel{}
	var seen []ghtriage.PriorReview
	var question string
	implementer.onCall = func(in ghtriage.ImplementationInput) { seen, question = in.PriorReviews, in.Question }
	second, err := newImplementer(t, f, implementer).Prepare(f.ctx, id, cfg.Source.LocalPath, t.TempDir())
	if err != nil || second.Record == nil {
		t.Fatalf("repair implementation: %+v %v", second, err)
	}
	if implementer.calls.Load() != 1 {
		t.Fatalf("implementation model calls = %d: the repair must produce a new candidate, not replay the old one", implementer.calls.Load())
	}
	if len(seen) != 1 || seen[0].Stage != ghtriage.RepairOfCodeReview || seen[0].Reason != "independent assessment" || !strings.Contains(question, "prior_reviews") {
		t.Fatalf("implementer input: prior_reviews=%+v question=%q", seen, question)
	}
	if second.EvidenceID == first.EvidenceID || second.Record.WorkID != repair.CurrentWorkID || second.Record.TaskID == first.Record.TaskID {
		t.Fatalf("the repair reused the rejected attempt: first %+v second %+v", first.Record, second.Record)
	}

	accepted, err := accept.Review(f.ctx, id, cfg.Source.LocalPath)
	if err != nil || !accepted.Accepted || acceptModel.calls.Load() != 1 {
		t.Fatalf("review of the new candidate: %+v %v calls=%d", accepted, err, acceptModel.calls.Load())
	}
	if accepted.EvidenceID == rejected.EvidenceID || accepted.Record.ImplementationEvidenceID != second.EvidenceID {
		t.Fatalf("the acceptance does not bind the new candidate: %+v", accepted.Record)
	}
	oldTask, _, _ = f.execSvc.FindByIdempotencyKey(f.ctx, string(oldWork))
	if oldTask.State == domain.TaskSucceeded {
		t.Fatal("accepting the new candidate accepted the rejected implementation Task")
	}
}
