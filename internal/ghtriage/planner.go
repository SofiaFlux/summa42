package ghtriage

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/verification"
	"github.com/SofiaFlux/summa42/internal/workflow"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

type PlannerResult struct {
	Planned  int
	Failures []string
}

type Planner struct {
	gate     *PlanningGate
	cases    *workflowcase.Service
	evidence *evidence.Store
	model    PlanModel
}

func NewPlanner(cases *workflowcase.Service, exec *execution.Service, verify *verification.Service, store *evidence.Store, model PlanModel) *Planner {
	return &Planner{gate: NewPlanningGate(cases, exec, verify, store), cases: cases, evidence: store, model: model}
}

func (p *Planner) Tick(ctx context.Context, missionID domain.ID) (PlannerResult, error) {
	if p == nil || p.cases == nil || p.evidence == nil || p.model == nil {
		return PlannerResult{}, errors.New("planner is not configured")
	}
	candidates, err := p.gate.List(ctx, missionID)
	if err != nil {
		return PlannerResult{}, err
	}
	var result PlannerResult
	for _, candidate := range candidates {
		if err := p.planCase(ctx, candidate); err != nil {
			result.Failures = append(result.Failures, fmt.Sprintf("case %s: %v", candidate.Case.ID, err))
			continue
		}
		result.Planned++
	}
	return result, nil
}

func (p *Planner) planCase(ctx context.Context, candidate PlanningCandidate) error {
	c := candidate.Case
	if c.CompletedSteps+1 >= c.MaxSteps || c.RemainingBudget <= 0 {
		return errors.New("case has no room for a planning handoff")
	}
	repo, issue, err := caseIssue(c)
	if err != nil {
		return err
	}
	if candidate.Decision.Repository != repo || candidate.Decision.Issue != issue ||
		candidate.Decision.SnapshotEvidenceID != c.ObservationEvidenceID {
		return errors.New("accepted decision does not identify this case and observation")
	}
	snap, err := (&Executor{evidence: p.evidence}).loadSnapshot(ctx, triagePayload{
		Repository: repo, Issue: issue, Revision: c.RevisionID,
		SnapshotEvidenceID: domain.ID(c.ObservationEvidenceID),
	})
	if err != nil {
		return err
	}
	output, err := p.model.Plan(ctx, PlanInput{
		Schema: PlanSchema, Question: PlanQuestion, Snapshot: snap,
		Decision: candidate.Decision, DecisionEvidenceID: candidate.DecisionEvidenceID,
	})
	if err != nil {
		return fmt.Errorf("plan model: %w", err)
	}
	plan := Plan{
		Schema: PlanSchema, CaseID: c.ID, DecisionEvidenceID: candidate.DecisionEvidenceID,
		SnapshotEvidenceID: domain.ID(c.ObservationEvidenceID), Repository: repo,
		Issue: issue, Revision: c.RevisionID, PlanOutput: output,
	}
	raw, err := plan.Canonical()
	if err != nil {
		return fmt.Errorf("validate plan: %w", err)
	}
	object, err := p.evidence.Put(ctx, bytes.NewReader(raw), evidence.Metadata{MediaType: DecisionMediaType, Kind: KindPlan})
	if err != nil {
		return fmt.Errorf("store plan: %w", err)
	}
	assessment, err := p.cases.Assess(ctx, workflowcase.AssessmentRequest{
		CaseID: c.ID, WorkID: c.CurrentWorkID, RemainingBudget: c.RemainingBudget,
		ProgressSignature: object.ContentHash, RequireLatestRevision: true,
		Assessment: workflow.Assessment{
			Verdict: workflow.Continue, Reason: PlanningAssessmentReason,
			EvidenceIDs: []string{string(object.ID)},
			Next: &workflow.WorkProposal{
				Kind:                 PlanReviewWorkKind,
				RequiredCapabilities: []string{RequiredCapability},
				AuthorityCeiling:     []string{RequiredCapability},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("assess planned case: %w", err)
	}
	if assessment.Decision.Outcome != workflow.OutcomeContinue {
		return fmt.Errorf("planning handoff had unexpected outcome %s", assessment.Decision.Outcome)
	}
	return nil
}
