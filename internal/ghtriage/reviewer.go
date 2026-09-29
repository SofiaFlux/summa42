package ghtriage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

type Structural struct {
	Stage2OnlyIfUnresolved         bool   `json:"stage2_only_if_unresolved"`
	SchemaConformant               bool   `json:"schema_conformant"`
	RuleMatchesRecomputation       string `json:"rule_matches_recomputation"`
	StateMatchesDisposition        bool   `json:"state_matches_disposition"`
	ClassificationAgreesWithIntake bool   `json:"classification_agrees_with_intake"`
}

type ReviewVerdict struct {
	Schema             string     `json:"schema"`
	ReviewerVersion    string     `json:"reviewer_version"`
	DecisionEvidenceID string     `json:"decision_evidence_id"`
	Repository         string     `json:"repository"`
	Issue              int64      `json:"issue"`
	Revision           string     `json:"revision"`
	CaseState          string     `json:"case_state"`
	LatestAssessmentID *string    `json:"latest_assessment_id"`
	Structural         Structural `json:"structural"`
	Plausible          bool       `json:"plausible"`
}

type ReviewResult struct {
	Reviewed int
	Skipped  int
	Failed   int
}

type Reviewer struct {
	cases    *workflowcase.Service
	evidence *evidence.Store
	index    ReviewIndexStore
	model    ReviewerModel
	clock    clock.Clock
}

func NewReviewer(cases *workflowcase.Service, store *evidence.Store, index ReviewIndexStore, model ReviewerModel, clk clock.Clock) *Reviewer {
	return &Reviewer{cases: cases, evidence: store, index: index, model: model, clock: clk}
}

// Tick reviews every decision whose case state it has not already observed. It
// changes nothing, gates nothing and blocks nothing.
func (r *Reviewer) Tick(ctx context.Context, missionID domain.ID) (ReviewResult, error) {
	if r == nil || r.cases == nil || r.evidence == nil || r.index == nil || r.model == nil {
		return ReviewResult{}, errors.New("triage reviewer is not configured")
	}
	var result ReviewResult
	cases, err := r.cases.ListBySource(ctx, missionID, SourceGitHub)
	if err != nil {
		return result, err
	}
	for _, c := range cases {
		outcome, err := r.reviewCase(ctx, c)
		if err != nil {
			result.Failed++
			fmt.Fprintf(os.Stderr, "triage review of case %s failed: %v\n", c.ID, err)
			continue
		}
		if outcome {
			result.Reviewed++
			continue
		}
		result.Skipped++
	}
	return result, nil
}

func (r *Reviewer) reviewCase(ctx context.Context, c workflowcase.Case) (bool, error) {
	records, err := r.cases.ListAssessments(ctx, c.ID)
	if err != nil {
		return false, err
	}
	fingerprint, latest := stateFingerprint(c, records)
	reason := latestReason(records, latest)

	// A task that exhausted its retries has no decision to review, and the
	// driver already reported it. Recording "review unavailable" as a verdict
	// would mark it reviewed without performing any check.
	if reason == ReasonTriageFailed {
		return false, nil
	}
	decision, decisionID, err := r.findDecision(ctx, c, records)
	if err != nil {
		return false, err
	}
	if decision == nil {
		return false, nil
	}
	linked, err := r.index.ReviewLinked(ctx, decisionID, ReviewerVersion, fingerprint)
	if err != nil {
		return false, err
	}
	if linked {
		return false, nil
	}

	snap, err := r.snapshot(ctx, *decision)
	if err != nil {
		return false, err
	}
	plausible, err := r.model.Review(ctx, ReviewInput{
		Schema:   ReviewSchema,
		Title:    snap.Title,
		Body:     snap.Body,
		Question: ReviewerQuestion,
		Decision: *decision,
	})
	if err != nil {
		// No verdict document and no index row, so the next tick retries.
		return false, fmt.Errorf("reviewer model call: %w", err)
	}

	verdict := ReviewVerdict{
		Schema:             ReviewSchema,
		ReviewerVersion:    ReviewerVersion,
		DecisionEvidenceID: string(decisionID),
		Repository:         decision.Repository,
		Issue:              decision.Issue,
		Revision:           decision.Revision,
		CaseState:          string(c.State),
		LatestAssessmentID: latest,
		Structural:         r.structural(ctx, c, records, latest, *decision),
		Plausible:          plausible,
	}
	raw, err := json.Marshal(verdict)
	if err != nil {
		return false, err
	}
	object, err := r.evidence.Put(ctx, strings.NewReader(string(raw)), evidence.Metadata{
		MediaType: DecisionMediaType,
		Kind:      KindReview,
	})
	if err != nil {
		return false, err
	}
	stored, err := r.index.LinkReview(ctx, decisionID, ReviewerVersion, fingerprint, object.ID)
	if err != nil {
		return false, err
	}
	if !stored {
		// A concurrent tick got there first, so this document is not the one the
		// index points at. Reporting it anyway would put a verdict on stderr that
		// no reader can find through the index, and counting it would claim a
		// review this tick did not record. The document itself stays: it is
		// content-addressed and unreferenced, which costs one blob and keeps the
		// loser's work out of the loop's own account of itself.
		return false, nil
	}
	fmt.Fprintf(os.Stderr,
		"triage review %s#%d at %s: plausible=%v structural=%+v verdict=%s\n",
		decision.Repository, decision.Issue, decision.Revision, plausible, verdict.Structural, object.ID)
	return true, nil
}

// structural re-derives what can be re-derived from stored inputs. It never
// re-runs the classifier.
func (r *Reviewer) structural(ctx context.Context, c workflowcase.Case, records []workflowcase.AssessmentRecord, latest *string, decision Decision) Structural {
	return Structural{
		Stage2OnlyIfUnresolved:         decision.Stage1.Resolved() == (decision.Stage2 == nil),
		SchemaConformant:               decision.Validate() == nil,
		RuleMatchesRecomputation:       r.recompute(ctx, decision),
		StateMatchesDisposition:        r.stateMatches(c, records, latest, decision),
		ClassificationAgreesWithIntake: r.classificationAgrees(ctx, decision),
	}
}

// recompute re-derives the outcome. A record written by a newer rules version
// is reported as not-applicable, not as a mismatch: the reviewer simply does
// not implement those rules, and calling history violating would be wrong.
func (r *Reviewer) recompute(ctx context.Context, decision Decision) string {
	if decision.Stage1.Resolved() {
		if decision.TriageRulesVersion != TriageRulesVersion {
			return "not-applicable"
		}
		snap, err := r.snapshot(ctx, decision)
		if err != nil {
			return "mismatch"
		}
		recomputed := Stage1(snap)
		if recomputed.Disposition == decision.Stage1.Disposition && recomputed.Rule == decision.Stage1.Rule {
			return "match"
		}
		return "mismatch"
	}
	if decision.DispositionRulesVersion != DispositionRulesVersion {
		return "not-applicable"
	}
	if decision.Stage2 == nil || decision.Stage3 == nil {
		return "mismatch"
	}
	recomputed := Stage3(*decision.Stage2)
	if recomputed.Disposition == decision.Stage3.Disposition && recomputed.Rule == decision.Stage3.Rule {
		return "match"
	}
	return "mismatch"
}

// stateMatches treats a superseded case as an allowed terminal state, so a
// correctly superseded revision is never reported as a violation.
func (r *Reviewer) stateMatches(c workflowcase.Case, records []workflowcase.AssessmentRecord, latest *string, decision Decision) bool {
	reason := latestReason(records, latest)
	if strings.HasPrefix(reason, supersededReasonHead) {
		return c.State == workflowcase.Blocked
	}
	switch decision.FinalDisposition() {
	case DispositionReadyToPlan:
		return c.State == workflowcase.Active && reason == ""
	default:
		return c.State == workflowcase.Blocked && reason == string(decision.FinalDisposition())
	}
}

// classificationAgrees recomputes ClassifyTriage and the exact stage 1 widening
// rule from the snapshot, rather than accepting any difference merely because
// the recorded value is question or duplicate.
func (r *Reviewer) classificationAgrees(ctx context.Context, decision Decision) bool {
	snap, err := r.snapshot(ctx, decision)
	if err != nil {
		return false
	}
	return Stage1(snap).Triage == decision.Stage1.Triage
}

func (r *Reviewer) snapshot(ctx context.Context, decision Decision) (Snapshot, error) {
	_, raw, err := r.evidence.Get(ctx, domain.ID(decision.SnapshotEvidenceID))
	if err != nil {
		return Snapshot{}, fmt.Errorf("load snapshot %s: %w", decision.SnapshotEvidenceID, err)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("decode snapshot %s: %w", decision.SnapshotEvidenceID, err)
	}
	return snap, nil
}

// findDecision locates the decision of a case: the assessment of its own task
// first, and the driver's accepted index otherwise, which covers the window
// between AcceptTask and Assess. A cited id that decodes to a decision of
// another revision is passed over rather than accepted, on both paths - see
// loadDecision. Supersession is what makes that necessary rather than tidy: the
// assessment that closes an older revision deliberately cites the newer
// revision's decision as the evidence for closing it, so a case that took the
// first decision it could decode would resolve to the newer revision's, review
// it a second time and record the result under this case's state.
func (r *Reviewer) findDecision(ctx context.Context, c workflowcase.Case, records []workflowcase.AssessmentRecord) (*Decision, domain.ID, error) {
	for i := len(records) - 1; i >= 0; i-- {
		for _, evidenceID := range assessmentEvidence(records[i]) {
			decision, id, ok, err := r.loadDecision(ctx, c.RevisionID, evidenceID)
			if err != nil {
				return nil, domain.ID(""), err
			}
			if ok {
				return decision, id, nil
			}
		}
	}
	return r.findIndexedDecision(ctx, c)
}

func (r *Reviewer) findIndexedDecision(ctx context.Context, c workflowcase.Case) (*Decision, domain.ID, error) {
	raw, found, err := findAcceptedIndex(ctx, r.evidence, c)
	if err != nil || !found {
		return nil, domain.ID(""), err
	}
	var record acceptedIndex
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, domain.ID(""), fmt.Errorf("decode accepted index: %w", err)
	}
	decision, id, ok, err := r.loadDecision(ctx, c.RevisionID, string(record.DecisionEvidenceID))
	if err != nil || !ok {
		return nil, domain.ID(""), err
	}
	return decision, id, nil
}

// loadDecision reads a decision document and reports whether it is this case's
// own. Decoding to KindDecision is not enough: the driver makes the same check
// on both of the paths it reads a decision on - readDecision and
// recoverAccepted - because a decision belonging to another revision is not
// this case's record, and a verdict written against one is a statement about a
// decision the case never made.
func (r *Reviewer) loadDecision(ctx context.Context, revision, evidenceID string) (*Decision, domain.ID, bool, error) {
	object, raw, err := r.evidence.Get(ctx, domain.ID(evidenceID))
	if err != nil {
		return nil, domain.ID(""), false, err
	}
	if object.Kind != KindDecision {
		return nil, domain.ID(""), false, nil
	}
	var decision Decision
	if err := json.Unmarshal(raw, &decision); err != nil {
		return nil, domain.ID(""), false, fmt.Errorf("decode decision %s: %w", evidenceID, err)
	}
	if decision.Revision != revision {
		return nil, domain.ID(""), false, nil
	}
	return &decision, domain.ID(evidenceID), true, nil
}

// findAcceptedIndex returns the driver's accepted index for a case, using the
// bounded candidate scan because the evidence store carries no case column. The
// walk matches the case in each candidate and passes over the rest, for the
// reason Driver.findAccepted gives: several cases of one mission each write a
// record of this kind, and only this case's record carries this case's identity.
// The reviewer has no task to match on - the task is gone once the case has been
// assessed - so the case ID is the whole of the identity it can check.
func findAcceptedIndex(ctx context.Context, store *evidence.Store, c workflowcase.Case) ([]byte, bool, error) {
	if store == nil {
		return nil, false, errors.New("evidence store is not configured")
	}
	matches := func(_ evidence.EvidenceObject, raw []byte) bool {
		var record acceptedIndex
		if err := json.Unmarshal(raw, &record); err != nil {
			return false
		}
		return record.CaseID == c.ID
	}
	_, raw, found, err := store.FindByKind(ctx, KindAccepted, acceptedIndexScanLimit, matches)
	if err != nil || !found {
		return nil, false, err
	}
	return raw, true, nil
}

// stateFingerprint is the canonical pair of the case state and its latest
// assessment, so a later transition schedules a new structural review.
func stateFingerprint(c workflowcase.Case, records []workflowcase.AssessmentRecord) (string, *string) {
	if len(records) == 0 {
		return string(c.State) + "|none", nil
	}
	latest := records[len(records)-1].ID
	return string(c.State) + "|" + latest, &latest
}

func latestReason(records []workflowcase.AssessmentRecord, latest *string) string {
	if latest == nil {
		return ""
	}
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].ID != *latest {
			continue
		}
		var result workflowcase.AssessmentResult
		if err := json.Unmarshal([]byte(records[i].ResultJSON), &result); err != nil {
			return ""
		}
		return result.Decision.Reason
	}
	return ""
}

// assessmentEvidence is the evidence the assessment cited. It is the request
// that carries it, not the result: workflow.Decision records what was decided,
// and the decision.EvidenceIDs an assessment was given are on the request the
// case service stored beside it.
func assessmentEvidence(record workflowcase.AssessmentRecord) []string {
	var request workflowcase.AssessmentRequest
	if err := json.Unmarshal([]byte(record.RequestJSON), &request); err != nil {
		return nil
	}
	return request.Assessment.EvidenceIDs
}
