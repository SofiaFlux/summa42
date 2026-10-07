package ghtriage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/repoworkspace"
	"github.com/SofiaFlux/summa42/internal/verification"
	"github.com/SofiaFlux/summa42/internal/workflow"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

const ImplementationWorkKind = "github.issue.implement"
const KindPlanReview = "github.issue.plan.review"
const PlanReviewVersion = "github.issue.plan.review.v1"

type PlanReviewInput struct {
	Schema   string                 `json:"schema"`
	Question string                 `json:"question"`
	Plan     Plan                   `json:"plan"`
	Snapshot Snapshot               `json:"snapshot"`
	Context  repoworkspace.Snapshot `json:"context"`
}
type PlanReviewOutput struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

func (o PlanReviewOutput) Validate() error {
	if (o.Verdict != "ACCEPT" && o.Verdict != "REVISE" && o.Verdict != "BLOCK") || textLength(o.Reason, 1, 1000) != nil {
		return errors.New("plan review requires ACCEPT, REVISE or BLOCK and bounded reason")
	}
	return nil
}
func ParsePlanReviewOutput(raw []byte) (PlanReviewOutput, error) {
	var out PlanReviewOutput
	if err := decodeExactlyOne(raw, &out); err != nil {
		return out, err
	}
	return out, out.Validate()
}

type PlanReviewModel interface {
	ReviewPlan(context.Context, PlanReviewInput) (PlanReviewOutput, error)
}
type PlanReviewResult struct {
	Reviewed, Held, Busy int
	Failures             []string
}
type PlanReviewer struct {
	cases    *workflowcase.Service
	exec     *execution.Service
	verify   *verification.Service
	evidence *evidence.Store
	model    PlanReviewModel
}

func NewPlanReviewer(cases *workflowcase.Service, exec *execution.Service, verify *verification.Service, store *evidence.Store, model PlanReviewModel) *PlanReviewer {
	return &PlanReviewer{cases: cases, exec: exec, verify: verify, evidence: store, model: model}
}

type PlanReviewRecord struct {
	Schema            string    `json:"schema"`
	CaseID            domain.ID `json:"case_id"`
	WorkID            domain.ID `json:"work_id"`
	PlanEvidenceID    domain.ID `json:"plan_evidence_id"`
	ContextEvidenceID domain.ID `json:"context_evidence_id"`
	BaseSHA           string    `json:"base_sha"`
	TaskID            domain.ID `json:"task_id"`
	AttemptID         domain.ID `json:"attempt_id"`
	PlanReviewOutput
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func (r *PlanReviewer) Tick(ctx context.Context, mission domain.ID) (PlanReviewResult, error) {
	if r == nil || r.cases == nil || r.exec == nil || r.verify == nil || r.evidence == nil || r.model == nil {
		return PlanReviewResult{}, errors.New("plan reviewer is not configured")
	}
	cases, err := r.cases.ListActive(ctx, mission)
	if err != nil {
		return PlanReviewResult{}, err
	}
	var out PlanReviewResult
	for _, c := range cases {
		if c.Source != SourceGitHub || c.NextWork.Kind != PlanReviewWorkKind {
			continue
		}
		held, err := r.reviewCase(ctx, c)
		if errors.Is(err, errPlanReviewBusy) {
			out.Busy++
			continue
		}
		if err != nil {
			out.Failures = append(out.Failures, fmt.Sprintf("case %s: %v", c.ID, err))
			continue
		}
		out.Reviewed++
		if held {
			out.Held++
		}
	}
	return out, nil
}

func (r *PlanReviewer) loadPlan(ctx context.Context, c workflowcase.Case) (Plan, domain.ID, Snapshot, repoworkspace.Snapshot, error) {
	var empty Plan
	fail := func(err error) (Plan, domain.ID, Snapshot, repoworkspace.Snapshot, error) {
		return empty, "", Snapshot{}, repoworkspace.Snapshot{}, err
	}
	repo, issue, err := caseIssue(c)
	if err != nil {
		return fail(err)
	}
	revisions, err := r.cases.ListByObject(ctx, c.MissionID, c.Source, c.ObjectID)
	if err != nil {
		return fail(err)
	}
	for _, other := range revisions {
		if parseRevision(other.RevisionID).After(parseRevision(c.RevisionID)) {
			return fail(errors.New("newer issue revision exists"))
		}
	}
	records, err := r.cases.ListAssessments(ctx, c.ID)
	if err != nil {
		return fail(err)
	}
	var request *workflowcase.AssessmentRequest
	for _, record := range records {
		var result workflowcase.AssessmentResult
		if err := json.Unmarshal([]byte(record.ResultJSON), &result); err != nil {
			return fail(err)
		}
		if result.Case.CurrentWorkID != c.CurrentWorkID {
			continue
		}
		if request != nil {
			return fail(errors.New("ambiguous planning assessment"))
		}
		var req workflowcase.AssessmentRequest
		if err := json.Unmarshal([]byte(record.RequestJSON), &req); err != nil {
			return fail(err)
		}
		if req.CaseID != c.ID || string(req.WorkID) != record.WorkID || result.Case.ID != c.ID || result.Decision.Outcome != workflow.OutcomeContinue || result.Decision.Next == nil || result.Decision.Next.Kind != PlanReviewWorkKind || req.Assessment.Reason != PlanningAssessmentReason || !req.RequireLatestRevision || len(req.Assessment.EvidenceIDs) != 1 {
			return fail(errors.New("current work is not backed by a planning assessment"))
		}
		request = &req
	}
	if request == nil {
		return fail(errors.New("planning assessment not found"))
	}
	planID := domain.ID(request.Assessment.EvidenceIDs[0])
	object, raw, err := r.evidence.Get(ctx, planID)
	if err != nil {
		return fail(err)
	}
	if object.Kind != KindPlan {
		return fail(errors.New("planning citation is not plan evidence"))
	}
	var plan Plan
	if err := decodeExactlyOne(raw, &plan); err != nil {
		return fail(err)
	}
	canonical, err := plan.Canonical()
	if err != nil {
		return fail(err)
	}
	if !bytes.Equal(canonical, raw) || plan.Schema != PlanSchemaV2 || plan.Source == nil || plan.CaseID != c.ID || plan.Repository != repo || plan.Issue != issue || plan.Revision != c.RevisionID || string(plan.SnapshotEvidenceID) != c.ObservationEvidenceID {
		return fail(errors.New("plan is ungrounded or differs from current case"))
	}
	indexRaw, found, err := findAcceptedIndex(ctx, r.evidence, c)
	if err != nil {
		return fail(err)
	}
	if !found {
		return fail(errors.New("accepted triage citation missing"))
	}
	var accepted acceptedIndex
	if err := json.Unmarshal(indexRaw, &accepted); err != nil {
		return fail(err)
	}
	if accepted.CaseID != c.ID || accepted.TaskID != request.WorkID || accepted.Revision != c.RevisionID || accepted.DecisionEvidenceID != plan.DecisionEvidenceID {
		return fail(errors.New("plan differs from accepted triage index"))
	}
	// The existing accepted index's task_id is the Work creation key, not the
	// generated Task ID (the planning gate uses the same lookup).
	task, taskFound, err := r.exec.FindByIdempotencyKey(ctx, string(accepted.TaskID))
	if err != nil {
		return fail(err)
	}
	if !taskFound {
		return fail(errors.New("accepted triage task not found"))
	}
	acceptance, found, err := r.verify.FindAcceptance(ctx, task.ID)
	if err != nil {
		return fail(err)
	}
	if !found || task.State != domain.TaskSucceeded || !acceptance.CriteriaMet || acceptance.AttemptID != task.CurrentAttemptID || acceptance.VerifierID != DriverVerifierID || acceptance.VerifierType != DriverVerifierType || len(acceptance.EvidenceIDs) != 1 || acceptance.EvidenceIDs[0] != plan.DecisionEvidenceID {
		return fail(errors.New("plan triage lacks exact task acceptance"))
	}
	snap, err := (&Executor{evidence: r.evidence}).loadSnapshot(ctx, triagePayload{Repository: repo, Issue: issue, Revision: c.RevisionID, SnapshotEvidenceID: plan.SnapshotEvidenceID})
	if err != nil {
		return fail(err)
	}
	contextObject, contextRaw, err := r.evidence.Get(ctx, plan.Source.ContextEvidenceID)
	if err != nil {
		return fail(err)
	}
	if contextObject.Kind != KindSourceContext || contextObject.ContentHash != plan.Source.ContextHash {
		return fail(errors.New("source context citation hash or kind differs"))
	}
	var source repoworkspace.Snapshot
	if err := decodeExactlyOne(contextRaw, &source); err != nil {
		return fail(err)
	}
	sourceCanonical, err := source.Canonical()
	if err != nil {
		return fail(err)
	}
	if !bytes.Equal(sourceCanonical, contextRaw) || source.Repository != repo || source.Commit != plan.Source.BaseSHA {
		return fail(errors.New("source context identity differs from plan"))
	}
	return plan, planID, snap, source, nil
}

func (r *PlanReviewer) reviewCase(ctx context.Context, c workflowcase.Case) (bool, error) {
	plan, planID, snap, source, err := r.loadPlan(ctx, c)
	if err != nil {
		return false, err
	}
	record, object, err := r.obtainReview(ctx, c, plan, planID, snap, source)
	if err != nil {
		return false, err
	}
	output := record.PlanReviewOutput
	assessment := workflow.Assessment{Verdict: workflow.Unknown, Reason: "plan-review:" + output.Verdict + ": " + output.Reason, EvidenceIDs: []string{string(object.ID)}}
	if output.Verdict == "ACCEPT" {
		caps := []string{"workspace.repo.write", "workspace.test"}
		authorized := true
		for _, cap := range caps {
			if !contains(c.Grant.Capabilities, cap) || !contains(c.Grant.Actions, cap) {
				authorized = false
			}
		}
		if authorized {
			assessment.Verdict = workflow.Continue
			assessment.Next = &workflow.WorkProposal{Kind: ImplementationWorkKind, RequiredCapabilities: append([]string(nil), caps...), AuthorityCeiling: append([]string(nil), caps...), ProposedActions: append([]string(nil), caps...)}
		} else {
			assessment.Reason = "plan-review:implementation-grant-missing"
		}
	}
	result, err := r.cases.Assess(ctx, workflowcase.AssessmentRequest{CaseID: c.ID, WorkID: c.CurrentWorkID, Assessment: assessment, RemainingBudget: c.RemainingBudget, ProgressSignature: object.ContentHash, RequireLatestRevision: true})
	if err != nil {
		return false, err
	}
	return result.Decision.Outcome == workflow.OutcomeBlocked, nil
}
