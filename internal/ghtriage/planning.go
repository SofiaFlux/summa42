package ghtriage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/verification"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

// PlanningCandidate is a triaged case and the accepted decision that made it
// eligible to enter planning.
type PlanningCandidate struct {
	Case               workflowcase.Case
	Decision           Decision
	DecisionEvidenceID domain.ID
}

// PlanningGate supplies candidates for a planner. A planner must recheck these
// conditions when committing work, because a newer revision can register after
// this read.
type PlanningGate struct {
	cases    *workflowcase.Service
	exec     *execution.Service
	verify   *verification.Service
	evidence *evidence.Store
}

func NewPlanningGate(cases *workflowcase.Service, exec *execution.Service, verify *verification.Service, store *evidence.Store) *PlanningGate {
	return &PlanningGate{cases: cases, exec: exec, verify: verify, evidence: store}
}

// List requires an ACTIVE case, a SUCCEEDED triage Task with its accepted
// ready-to-plan decision, and no newer registered revision of the same issue.
func (g *PlanningGate) List(ctx context.Context, missionID domain.ID) ([]PlanningCandidate, error) {
	if g == nil || g.cases == nil || g.exec == nil || g.verify == nil || g.evidence == nil {
		return nil, errors.New("planning gate is not configured")
	}
	active, err := g.cases.ListActive(ctx, missionID)
	if err != nil {
		return nil, err
	}
	result := make([]PlanningCandidate, 0)
	for _, c := range active {
		if c.Source != SourceGitHub || c.CurrentWorkID == "" {
			continue
		}
		revisions, err := g.cases.ListByObject(ctx, missionID, c.Source, c.ObjectID)
		if err != nil {
			return nil, err
		}
		latest := true
		for _, other := range revisions {
			if parseRevision(other.RevisionID).After(parseRevision(c.RevisionID)) {
				latest = false
				break
			}
		}
		if !latest {
			continue
		}
		task, found, err := g.exec.FindByIdempotencyKey(ctx, string(c.CurrentWorkID))
		if err != nil {
			return nil, err
		}
		if !found || task.State != domain.TaskSucceeded || task.TaskClass != TaskClass {
			continue
		}
		acceptance, found, err := g.verify.FindAcceptance(ctx, task.ID)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("case %s has a succeeded triage task without an acceptance", c.ID)
		}
		raw, found, err := findAcceptedIndex(ctx, g.evidence, c)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("case %s has a succeeded triage task without an accepted index", c.ID)
		}
		var accepted acceptedIndex
		if err := json.Unmarshal(raw, &accepted); err != nil {
			return nil, fmt.Errorf("decode accepted index for case %s: %w", c.ID, err)
		}
		if accepted.CaseID != c.ID || accepted.TaskID != c.CurrentWorkID || accepted.Revision != c.RevisionID {
			return nil, fmt.Errorf("accepted index identity differs from case %s", c.ID)
		}
		if !acceptance.CriteriaMet || acceptance.AttemptID != task.CurrentAttemptID ||
			acceptance.VerifierID != DriverVerifierID || acceptance.VerifierType != DriverVerifierType ||
			len(acceptance.EvidenceIDs) != 1 || acceptance.EvidenceIDs[0] != accepted.DecisionEvidenceID {
			return nil, fmt.Errorf("task acceptance does not back the indexed decision for case %s", c.ID)
		}
		object, decisionRaw, err := g.evidence.Get(ctx, accepted.DecisionEvidenceID)
		if err != nil {
			return nil, err
		}
		if object.Kind != KindDecision {
			return nil, fmt.Errorf("accepted index for case %s points at kind %q", c.ID, object.Kind)
		}
		var decision Decision
		if err := json.Unmarshal(decisionRaw, &decision); err != nil {
			return nil, fmt.Errorf("decode accepted decision for case %s: %w", c.ID, err)
		}
		if decision.Revision != c.RevisionID {
			return nil, fmt.Errorf("accepted decision revision differs from case %s", c.ID)
		}
		if err := decision.Validate(); err != nil {
			return nil, fmt.Errorf("accepted decision for case %s: %w", c.ID, err)
		}
		if decision.FinalDisposition() != DispositionReadyToPlan {
			continue
		}
		result = append(result, PlanningCandidate{Case: c, Decision: decision, DecisionEvidenceID: object.ID})
	}
	return result, nil
}
