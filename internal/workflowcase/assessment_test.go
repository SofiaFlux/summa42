package workflowcase

import (
	"context"
	"reflect"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/workflow"
)

func assessmentFixture(t *testing.T) (*Service, context.Context, Case) {
	t.Helper()
	svc, _, missionID, ctx := setupEnsure(t)
	observation := sampleObservation(missionID)
	observation.Grant = workflow.Grant{Capabilities: []string{"read", "write"}, Actions: []string{"comment"}}
	c, err := svc.Ensure(ctx, observation)
	if err != nil {
		t.Fatal(err)
	}
	return svc, ctx, c
}

func continueRequest(c Case) AssessmentRequest {
	return AssessmentRequest{CaseID: c.ID, WorkID: c.CurrentWorkID, RemainingBudget: 4,
		ProgressSignature: "reviewed", Assessment: workflow.Assessment{Verdict: workflow.Continue,
			EvidenceIDs: []string{"review-evidence"}, Next: &workflow.WorkProposal{Kind: "publish",
				RequiredCapabilities: []string{"write"}, AuthorityCeiling: []string{"write"}, ProposedActions: []string{"comment"}}}}
}

func assessmentCount(t *testing.T, svc *Service, ctx context.Context, caseID domain.ID) int {
	t.Helper()
	var count int
	if err := svc.store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM workflow_assessments WHERE case_id = ?", caseID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestAssessContinueThenReady(t *testing.T) {
	svc, ctx, initial := assessmentFixture(t)
	first, err := svc.Assess(ctx, continueRequest(initial))
	if err != nil {
		t.Fatal(err)
	}
	if first.Decision.Outcome != workflow.OutcomeContinue || first.Case.State != Active || first.Case.CurrentWorkID == initial.CurrentWorkID || first.Case.NextWork.Kind != "publish" || first.Case.CompletedSteps != 1 || first.Case.RemainingBudget != 4 {
		t.Fatalf("unexpected continue result: %+v", first)
	}
	second, err := svc.Assess(ctx, AssessmentRequest{CaseID: initial.ID, WorkID: first.Case.CurrentWorkID,
		Assessment: workflow.Assessment{Verdict: workflow.Ready, EvidenceIDs: []string{"publication-evidence"}}, RemainingBudget: 2, ProgressSignature: "published"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Decision.Outcome != workflow.OutcomeReady || second.Case.State != ReadyForVerification || second.Case.CurrentWorkID != "" || second.Case.NextWork.Kind != "" || second.Case.CompletedSteps != 2 || second.Case.RemainingBudget != 2 {
		t.Fatalf("unexpected ready result: %+v", second)
	}
}

func TestAssessReplayAndStaleWork(t *testing.T) {
	svc, ctx, initial := assessmentFixture(t)
	request := continueRequest(initial)
	first, err := svc.Assess(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Assess(ctx, AssessmentRequest{CaseID: initial.ID, WorkID: first.Case.CurrentWorkID,
		Assessment: workflow.Assessment{Verdict: workflow.Ready, EvidenceIDs: []string{"publication-evidence"}}, RemainingBudget: 2})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := svc.Assess(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replay, first) {
		t.Fatalf("replay changed original snapshot: first=%+v replay=%+v", first, replay)
	}
	if assessmentCount(t, svc, ctx, initial.ID) != 2 {
		t.Fatal("replay inserted another assessment")
	}
	changed := request
	changed.RemainingBudget = 3
	if _, err := svc.Assess(ctx, changed); err == nil {
		t.Fatal("changed request was accepted")
	}
	stale := request
	stale.WorkID = domain.NewID("work")
	if _, err := svc.Assess(ctx, stale); err == nil {
		t.Fatal("stale work was accepted")
	}
}

func TestAssessCannotIncreaseBudget(t *testing.T) {
	svc, ctx, initial := assessmentFixture(t)
	request := continueRequest(initial)
	request.RemainingBudget = 6
	if _, err := svc.Assess(ctx, request); err == nil {
		t.Fatal("budget increase was accepted")
	}
	if assessmentCount(t, svc, ctx, initial.ID) != 0 {
		t.Fatal("rejected assessment was recorded")
	}
}

func TestAssessLatestRejectsSupersededRevision(t *testing.T) {
	svc, _, missionID, ctx := setupEnsure(t)
	firstObservation := sampleObservation(missionID)
	firstObservation.Source = "github"
	firstObservation.RevisionID = "2026-09-29T10:00:00Z"
	first, err := svc.Ensure(ctx, firstObservation)
	if err != nil {
		t.Fatal(err)
	}
	newer := firstObservation
	newer.RevisionID = "2026-09-29T10:00:00.5Z"
	if _, err := svc.Ensure(ctx, newer); err != nil {
		t.Fatal(err)
	}
	request := continueRequest(first)
	request.RequireLatestRevision = true
	if _, err := svc.Assess(ctx, request); err == nil {
		t.Fatal("assessed an older revision after a newer one was registered")
	}
	if got := assessmentCount(t, svc, ctx, first.ID); got != 0 {
		t.Fatalf("older case assessments = %d, want none", got)
	}
}

func TestAssessFailedDecisionRollsBack(t *testing.T) {
	svc, ctx, initial := assessmentFixture(t)
	request := continueRequest(initial)
	request.Assessment.Next.ProposedActions = []string{"delete"}
	if _, err := svc.Assess(ctx, request); err == nil {
		t.Fatal("unauthorized action was accepted")
	}
	if assessmentCount(t, svc, ctx, initial.ID) != 0 {
		t.Fatal("failed decision inserted assessment")
	}
	var state State
	var workID domain.ID
	var steps int
	var budget int64
	if err := svc.store.DB().QueryRowContext(ctx, "SELECT state,current_work_id,completed_steps,remaining_budget FROM workflow_cases WHERE case_id=?", initial.ID).Scan(&state, &workID, &steps, &budget); err != nil {
		t.Fatal(err)
	}
	if state != Active || workID != initial.CurrentWorkID || steps != 0 || budget != 5 {
		t.Fatalf("failed decision changed case: %s %s %d %d", state, workID, steps, budget)
	}
}

func TestAssessInsertFailureRollsBackCaseUpdate(t *testing.T) {
	svc, ctx, initial := assessmentFixture(t)
	_, err := svc.store.DB().ExecContext(ctx, `CREATE TRIGGER reject_workflow_assessment BEFORE INSERT ON workflow_assessments BEGIN SELECT RAISE(ABORT, 'forced assessment failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Assess(ctx, continueRequest(initial)); err == nil {
		t.Fatal("forced insert failure was accepted")
	}
	if assessmentCount(t, svc, ctx, initial.ID) != 0 {
		t.Fatal("failed transaction inserted assessment")
	}
	var workID domain.ID
	var steps int
	if err := svc.store.DB().QueryRowContext(ctx, "SELECT current_work_id, completed_steps FROM workflow_cases WHERE case_id=?", initial.ID).Scan(&workID, &steps); err != nil {
		t.Fatal(err)
	}
	if workID != initial.CurrentWorkID || steps != 0 {
		t.Fatalf("failed transaction changed case: work=%s steps=%d", workID, steps)
	}
}

func TestAssessBlockedClearsWork(t *testing.T) {
	svc, ctx, initial := assessmentFixture(t)
	request := continueRequest(initial)
	request.RemainingBudget = 0
	result, err := svc.Assess(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision.Outcome != workflow.OutcomeBlocked || result.Case.State != Blocked || result.Case.CurrentWorkID != "" || result.Case.NextWork.Kind != "" {
		t.Fatalf("blocked case retained work: %+v", result)
	}
	var workID, workJSON string
	if err := svc.store.DB().QueryRowContext(ctx, "SELECT current_work_id, next_work_json FROM workflow_cases WHERE case_id=?", initial.ID).Scan(&workID, &workJSON); err != nil {
		t.Fatal(err)
	}
	if workID != "" || workJSON != "{}" {
		t.Fatalf("blocked work stored as id=%q json=%q", workID, workJSON)
	}
}

func TestAssessRejectsInactiveMission(t *testing.T) {
	svc, ctx, initial := assessmentFixture(t)
	if err := purpose.New(svc.store, svc.clock).DeactivateMission(ctx, initial.MissionID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Assess(ctx, continueRequest(initial)); err == nil {
		t.Fatal("inactive Mission was accepted")
	}
	if assessmentCount(t, svc, ctx, initial.ID) != 0 {
		t.Fatal("inactive Mission assessment was recorded")
	}
}
