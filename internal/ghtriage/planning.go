package ghtriage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/ghissue"
	"github.com/SofiaFlux/summa42/internal/verification"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

// PlanningCandidate is a triaged case and the accepted decision that made it
// eligible to enter planning.
type PlanningCandidate struct {
	Case               workflowcase.Case
	Decision           Decision
	DecisionEvidenceID domain.ID
	Severity           int
	// PriorReviews holds the plan reviewer's reasons when the case is at plan
	// repair work; empty for a first plan.
	PriorReviews []PriorReview
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

// CaseError is an integrity problem with one case; it never aborts the mission.
type CaseError struct {
	CaseID domain.ID
	Err    error
}

func (e CaseError) Error() string { return fmt.Sprintf("case %s: %v", e.CaseID, e.Err) }

// List requires an ACTIVE case, a SUCCEEDED triage Task with its accepted
// ready-to-plan decision, and no newer registered revision of the same issue.
// A problem with one case is returned as a CaseError and the rest are still listed.
func (g *PlanningGate) List(ctx context.Context, missionID domain.ID) ([]PlanningCandidate, []CaseError, error) {
	if g == nil || g.cases == nil || g.exec == nil || g.verify == nil || g.evidence == nil {
		return nil, nil, errors.New("planning gate is not configured")
	}
	active, err := g.cases.ListActive(ctx, missionID)
	if err != nil {
		return nil, nil, err
	}
	result := make([]PlanningCandidate, 0)
	var caseErrs []CaseError
	for _, c := range active {
		if c.Source != SourceGitHub || c.CurrentWorkID == "" || (c.NextWork.Kind != TaskClass && c.NextWork.Kind != PlanRepairWorkKind) {
			continue
		}
		candidate, ok, err := g.candidate(ctx, missionID, c)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			caseErrs = append(caseErrs, CaseError{CaseID: c.ID, Err: err})
			continue
		}
		if ok {
			result = append(result, candidate)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Severity != result[j].Severity {
			return result[i].Severity > result[j].Severity
		}
		return result[i].Case.ID < result[j].Case.ID
	})
	return result, caseErrs, nil
}

func (g *PlanningGate) candidate(ctx context.Context, missionID domain.ID, c workflowcase.Case) (PlanningCandidate, bool, error) {
	none := PlanningCandidate{}
	revisions, err := g.cases.ListByObject(ctx, missionID, c.Source, c.ObjectID)
	if err != nil {
		return none, false, err
	}
	latest := true
	for _, other := range revisions {
		if parseRevision(other.RevisionID).After(parseRevision(c.RevisionID)) {
			latest = false
			break
		}
	}
	if !latest {
		return none, false, nil
	}
	// The triage work is the case's current work for a first plan. At plan
	// repair work it is the work the accepted index names, and the repair must
	// be backed by a REVISE plan review.
	triageWorkID := c.CurrentWorkID
	var priorReviews []PriorReview
	if c.NextWork.Kind == PlanRepairWorkKind {
		records, err := g.cases.ListAssessments(ctx, c.ID)
		if err != nil {
			return none, false, err
		}
		history, err := decodeAssessments(records)
		if err != nil {
			return none, false, err
		}
		producer, err := producerOf(history, string(c.CurrentWorkID))
		if err != nil {
			return none, false, err
		}
		if producer == nil {
			return none, false, fmt.Errorf("case %s is at plan repair work that no assessment produced", c.ID)
		}
		if err := checkRepair(ctx, g.evidence, c, *producer, RepairOfPlanReview, PlanRepairWorkKind); err != nil {
			return none, false, fmt.Errorf("case %s: %w", c.ID, err)
		}
		for _, item := range history {
			if item.isRepair() && item.req.RepairOf == RepairOfPlanReview {
				priorReviews = append(priorReviews, PriorReview{Stage: RepairOfPlanReview, Reason: reviewReason(item.req.Assessment.Reason)})
			}
		}
		indexRaw, indexFound, err := findAcceptedIndex(ctx, g.evidence, c)
		if err != nil {
			return none, false, err
		}
		if !indexFound {
			return none, false, fmt.Errorf("case %s is at plan repair work without an accepted triage index", c.ID)
		}
		var index acceptedIndex
		if err := json.Unmarshal(indexRaw, &index); err != nil {
			return none, false, fmt.Errorf("decode accepted index for case %s: %w", c.ID, err)
		}
		triageWorkID = index.TaskID
	}
	task, found, err := g.exec.FindByIdempotencyKey(ctx, string(triageWorkID))
	if err != nil {
		return none, false, err
	}
	if !found || task.State != domain.TaskSucceeded || task.TaskClass != TaskClass {
		return none, false, nil
	}
	acceptance, found, err := g.verify.FindAcceptance(ctx, task.ID)
	if err != nil {
		return none, false, err
	}
	if !found {
		return none, false, fmt.Errorf("case %s has a succeeded triage task without an acceptance", c.ID)
	}
	raw, found, err := findAcceptedIndex(ctx, g.evidence, c)
	if err != nil {
		return none, false, err
	}
	if !found {
		return none, false, fmt.Errorf("case %s has a succeeded triage task without an accepted index", c.ID)
	}
	var accepted acceptedIndex
	if err := json.Unmarshal(raw, &accepted); err != nil {
		return none, false, fmt.Errorf("decode accepted index for case %s: %w", c.ID, err)
	}
	if accepted.CaseID != c.ID || accepted.TaskID != triageWorkID || accepted.Revision != c.RevisionID {
		return none, false, fmt.Errorf("accepted index identity differs from case %s", c.ID)
	}
	if !acceptance.CriteriaMet || acceptance.AttemptID != task.CurrentAttemptID ||
		acceptance.VerifierID != DriverVerifierID || acceptance.VerifierType != DriverVerifierType ||
		len(acceptance.EvidenceIDs) != 1 || acceptance.EvidenceIDs[0] != accepted.DecisionEvidenceID {
		return none, false, fmt.Errorf("task acceptance does not back the indexed decision for case %s", c.ID)
	}
	object, decisionRaw, err := g.evidence.Get(ctx, accepted.DecisionEvidenceID)
	if err != nil {
		return none, false, err
	}
	if object.Kind != KindDecision {
		return none, false, fmt.Errorf("accepted index for case %s points at kind %q", c.ID, object.Kind)
	}
	var decision Decision
	if err := json.Unmarshal(decisionRaw, &decision); err != nil {
		return none, false, fmt.Errorf("decode accepted decision for case %s: %w", c.ID, err)
	}
	if decision.Revision != c.RevisionID {
		return none, false, fmt.Errorf("accepted decision revision differs from case %s", c.ID)
	}
	if err := decision.Validate(); err != nil {
		return none, false, fmt.Errorf("accepted decision for case %s: %w", c.ID, err)
	}
	if decision.FinalDisposition() != DispositionReadyToPlan {
		return none, false, nil
	}
	sev := 0
	snapID := decision.SnapshotEvidenceID
	if snapID == "" {
		snapID = c.ObservationEvidenceID
	}
	if snapID != "" {
		if _, snapRaw, err := g.evidence.Get(ctx, domain.ID(snapID)); err == nil {
			var snap struct {
				Labels []string `json:"labels"`
			}
			if err := json.Unmarshal(snapRaw, &snap); err == nil {
				sev = ghissue.ExtractSeverity(snap.Labels)
			}
		}
	}
	return PlanningCandidate{Case: c, Decision: decision, DecisionEvidenceID: object.ID, Severity: sev, PriorReviews: priorReviews}, true, nil
}
