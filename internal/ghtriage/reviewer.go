package ghtriage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
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

// ReviewVerdict is the record one tick leaves behind: the decision it reviewed,
// the observation it re-derived from, the case state and latest assessment it
// observed, and the checks it ran over them.
//
// The observation is named in its own right rather than read out of the
// decision's snapshot citation, because the reviewer derives from the case's own
// observation and never trusts the citation: a verdict carrying only the
// citation would name the one document the review is not about.
type ReviewVerdict struct {
	Schema                string     `json:"schema"`
	ReviewerVersion       string     `json:"reviewer_version"`
	DecisionEvidenceID    string     `json:"decision_evidence_id"`
	ObservationEvidenceID string     `json:"observation_evidence_id"`
	Repository            string     `json:"repository"`
	Issue                 int64      `json:"issue"`
	Revision              string     `json:"revision"`
	CaseState             string     `json:"case_state"`
	LatestAssessmentID    *string    `json:"latest_assessment_id"`
	Structural            Structural `json:"structural"`
	Plausible             bool       `json:"plausible"`
}

// ReviewResult is what one tick did to one mission's cases. Skipped is the
// total of the three reasons below it, because all three mean the same thing to
// the caller - this tick reviewed nothing about this case - and only an operator
// reading the numbers needs to tell them apart: a mission in steady state
// reports nothing but AlreadyReviewed, a mission whose cases never produced a
// decision reports them all under NoDecision, and a LostRace is a tick that lost
// a race it had already paid for.
type ReviewResult struct {
	Reviewed        int
	Skipped         int
	AlreadyReviewed int
	NoDecision      int
	LostRace        int
	Failed          int
}

type Reviewer struct {
	cases    *workflowcase.Service
	evidence *evidence.Store
	index    ReviewIndexStore
	model    ReviewerModel
}

// NewReviewer builds the reviewer over the four collaborators it uses. The
// clock is taken and deliberately not kept: the verdict document is
// content-addressed and carries no timestamp of its own, and the one time this
// loop writes - the index row's created_at - belongs to the index, which stamps
// it from its own clock. The parameter stays so the construction keeps the
// driver's shape and a change that does need a time has one to take.
func NewReviewer(cases *workflowcase.Service, store *evidence.Store, index ReviewIndexStore, model ReviewerModel, _ clock.Clock) *Reviewer {
	return &Reviewer{cases: cases, evidence: store, index: index, model: model}
}

// Tick reviews every decision whose case state it has not already observed, so
// each (decision, reviewer version, state, latest assessment) is reviewed once
// and a case that later moves is reviewed again. It changes nothing, gates
// nothing and blocks nothing.
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
		switch outcome {
		case reviewed:
			result.Reviewed++
		case alreadyReviewed:
			result.Skipped++
			result.AlreadyReviewed++
		case noDecision:
			result.Skipped++
			result.NoDecision++
		case lostRace:
			result.Skipped++
			result.LostRace++
		}
	}
	return result, nil
}

// outcome is what a case contributed to the tick. All three non-review outcomes
// are counted as Skipped, because none of them is work this tick did, and
// separated because they are three different things an operator looking at the
// counters has to be able to tell: a case whose decision is already reviewed for
// this state, a case that never produced a decision of its own, and a case whose
// verdict another tick linked first. An error return carries no outcome at all -
// the caller counts a failure instead - so the error paths below return a fixed
// placeholder rather than a claim about a case the tick never judged.
type outcome int

const (
	reviewed outcome = iota
	alreadyReviewed
	noDecision
	lostRace
)

func (r *Reviewer) reviewCase(ctx context.Context, c workflowcase.Case) (outcome, error) {
	records, err := r.cases.ListAssessments(ctx, c.ID)
	if err != nil {
		return noDecision, err
	}
	fingerprint, latest := stateFingerprint(c, records)

	decision, decisionID, err := r.findDecision(ctx, c, records)
	if err != nil {
		return noDecision, err
	}
	if decision == nil {
		// A case that produced no decision of its own has nothing to review. A
		// task that exhausted its retries lands here too, the driver having
		// already reported it, so there is no separate triage-failed case to
		// make: a case either decided something of its own or it is skipped.
		return noDecision, nil
	}
	linked, err := r.index.ReviewLinked(ctx, decisionID, ReviewerVersion, fingerprint)
	if err != nil {
		return noDecision, err
	}
	if linked {
		return alreadyReviewed, nil
	}

	snap, observationID, err := r.caseSnapshot(ctx, c)
	if err != nil {
		return noDecision, err
	}
	// The decision is handed over as it was recorded, whatever identity it claims:
	// it is the document under review, and a record claiming another issue's is
	// exactly what the question should be able to see against this issue's text.
	// The disagreement is reported in the block below rather than tidied away
	// before the model is asked, because a reviewer that edited the record it is
	// reviewing would be answering a different question than the one it reports.
	plausible, err := r.model.Review(ctx, ReviewInput{
		Schema:   ReviewSchema,
		Title:    snap.Title,
		Body:     snap.Body,
		Question: ReviewerQuestion,
		Decision: *decision,
	})
	if err != nil {
		// No verdict document and no index row, so the next tick retries.
		return noDecision, fmt.Errorf("reviewer model call: %w", err)
	}

	verdict := ReviewVerdict{
		Schema:                ReviewSchema,
		ReviewerVersion:       ReviewerVersion,
		DecisionEvidenceID:    string(decisionID),
		ObservationEvidenceID: observationID,
		Repository:            decision.Repository,
		Issue:                 decision.Issue,
		Revision:              decision.Revision,
		CaseState:             string(c.State),
		LatestAssessmentID:    latest,
		Structural:            r.structural(ctx, c, records, latest, *decision, snap),
		Plausible:             plausible,
	}
	raw, err := json.Marshal(verdict)
	if err != nil {
		return noDecision, err
	}
	object, err := r.evidence.Put(ctx, strings.NewReader(string(raw)), evidence.Metadata{
		MediaType: DecisionMediaType,
		Kind:      KindReview,
	})
	if err != nil {
		return noDecision, err
	}
	stored, err := r.index.LinkReview(ctx, decisionID, ReviewerVersion, fingerprint, object.ID)
	if err != nil {
		return noDecision, err
	}
	if !stored {
		// A concurrent tick got there first, so this document is not the one the
		// index points at. Reporting it anyway would put a verdict on stderr that
		// no reader can find through the index, and counting it would claim a
		// review this tick did not record. The document itself stays: it is
		// content-addressed and unreferenced, which costs one blob and keeps the
		// loser's work out of the loop's own account of itself.
		return lostRace, nil
	}
	fmt.Fprintf(os.Stderr,
		"triage review %s#%d at %s: plausible=%v structural=%+v verdict=%s\n",
		decision.Repository, decision.Issue, decision.Revision, plausible, verdict.Structural, object.ID)
	return reviewed, nil
}

// structural re-derives what can be re-derived from stored inputs. It never
// re-runs the classifier.
//
// Every check that needs the snapshot reads the one this case was observed from
// - never the one the decision names, which a tampered record can point at
// another issue's - and the other stored documents, so a decision citing
// someone else's snapshot is reported rather than agreed with by construction.
// The snapshot is proved to be this case's own when it is loaded, and the
// decision is compared against the case's own object id, so the block cannot
// come back clean for a record that is about a different issue entirely.
//
// The limit of that is worth stating here rather than leaving to a reader of
// the verdict, because it is why the model is asked anything at all. A stage 2
// fabricated so that Stage3 of it yields exactly the recorded stage 3 rule is a
// match by construction: the record agrees with itself and no re-derivation of
// it can say otherwise. What the block does catch is a record that disagrees
// with itself - a stage 3 that does not follow from its own stage 2, a stage 1
// triage that disagrees with the snapshot, a stage 1 that claims to have
// resolved the issue and carries stage 2 anyway, a case state that does not
// match the disposition, a schema that does not validate. The plausibility
// question beside it is the one check a re-derivation cannot be.
func (r *Reviewer) structural(ctx context.Context, c workflowcase.Case, records []workflowcase.AssessmentRecord, latest *string, decision Decision, snap Snapshot) Structural {
	return Structural{
		Stage2OnlyIfUnresolved:         decision.Stage1.Resolved() == (decision.Stage2 == nil),
		SchemaConformant:               decision.Validate() == nil,
		RuleMatchesRecomputation:       r.recompute(decision, snap),
		StateMatchesDisposition:        r.stateMatches(c, records, latest, decision),
		ClassificationAgreesWithIntake: classificationAgrees(c, decision, snap),
	}
}

// recompute re-derives the outcome from the snapshot the case itself was
// observed from. A record written by a newer rules version is reported as
// not-applicable, not as a mismatch: the reviewer simply does not implement
// those rules, and calling history violating would be wrong.
func (r *Reviewer) recompute(decision Decision, snap Snapshot) string {
	if decision.Stage1.Resolved() {
		if decision.TriageRulesVersion != TriageRulesVersion {
			return "not-applicable"
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
// rule from the snapshot this case was observed from, rather than accepting any
// difference merely because the recorded value is question or duplicate.
//
// The snapshot it re-derives from is the case's own observation, never the one
// the decision names. A decision may cite some other issue's snapshot, and every
// other check would read that one too - the plausibility question would be asked
// about the other issue's title and body - so comparing against it would compare
// a decision with itself and agree with it by construction. So the citation is
// itself part of what this check says: a decision derived from anything other
// than this case's observation has not been shown to agree with intake, and the
// recorded triage is not compared at all.
//
// So is the identity the decision claims. Repository and issue are copied into a
// decision from the task payload and are nothing the reader re-establishes, so a
// record about #99 filed against this case's snapshot re-derives cleanly and is
// published as this case's verdict - a verdict about an issue nobody asked
// about. The case's own object id is what says which issue this is, and a
// decision naming a different one has not been shown to agree with intake; it is
// reported here rather than dropped, because the reviewer reports violations and
// does not decide which records exist.
func classificationAgrees(c workflowcase.Case, decision Decision, snap Snapshot) bool {
	repository, issue, err := caseIssue(c)
	if err != nil {
		return false
	}
	if decision.Repository != repository || decision.Issue != issue {
		return false
	}
	if strings.TrimSpace(decision.SnapshotEvidenceID) != strings.TrimSpace(c.ObservationEvidenceID) {
		return false
	}
	return Stage1(snap).Triage == decision.Stage1.Triage
}

// caseIssue is the issue a case is about, read out of the object id intake
// writes: "<source>:<repository>#<number>". The reviewer walks GitHub cases only,
// so the source qualifier is stripped when present and a case registered without
// one names the same issue - which is what lets a case object id be compared
// against a decision's repository and issue at all.
func caseIssue(c workflowcase.Case) (string, int64, error) {
	object := strings.TrimSpace(c.ObjectID)
	if qualifier := strings.TrimSpace(c.Source) + ":"; strings.HasPrefix(object, qualifier) {
		object = strings.TrimPrefix(object, qualifier)
	}
	repository, number, found := strings.Cut(object, "#")
	if !found {
		return "", 0, fmt.Errorf("case object id %q names no issue number", c.ObjectID)
	}
	issue, err := strconv.ParseInt(number, 10, 64)
	if err != nil || issue <= 0 {
		return "", 0, fmt.Errorf("case object id %q names no issue number", c.ObjectID)
	}
	return strings.TrimSpace(repository), issue, nil
}

// caseSnapshot reads the snapshot intake recorded for this case and proves it is
// that object: the right kind, canonical, and about the issue the case names.
// It is the only snapshot the reviewer derives anything from, and the one the
// plausibility question is asked about, so a decision citing another issue's
// snapshot cannot borrow another issue's content here.
//
// The provenance is re-established on this side of the package boundary rather
// than inherited from the writer's: the executor proves the same three things on
// its own copy of the id when it stores a decision, but the case row outlives
// that call and nothing re-checks it afterwards. Reading the field on faith would
// leave a document that decodes to an empty snapshot, and then a correct decision
// re-derived against nothing earns a reported mismatch - a false accusation, which
// is the one direction of error worse than saying nothing. So every check here
// that is not the load itself fails the review, which the tick counts as Failed
// and the next tick retries.
func (r *Reviewer) caseSnapshot(ctx context.Context, c workflowcase.Case) (Snapshot, string, error) {
	id := strings.TrimSpace(c.ObservationEvidenceID)
	if id == "" {
		return Snapshot{}, "", errors.New("the case carries no observation evidence to review against")
	}
	repository, issue, err := caseIssue(c)
	if err != nil {
		return Snapshot{}, "", err
	}
	object, raw, err := r.evidence.Get(ctx, domain.ID(id))
	if err != nil {
		return Snapshot{}, "", fmt.Errorf("load issue snapshot %s: %w", id, err)
	}
	if object.Kind != snapshotEvidenceKind {
		return Snapshot{}, "", fmt.Errorf("evidence %s is a %q, not an issue snapshot", id, object.Kind)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return Snapshot{}, "", fmt.Errorf("decode issue snapshot %s: %w", id, err)
	}
	if snap.Repo == "" || snap.Issue <= 0 {
		return Snapshot{}, "", fmt.Errorf("evidence %s is not a canonical issue snapshot", id)
	}
	if snap.Repo != repository || snap.Issue != issue {
		return Snapshot{}, "", fmt.Errorf("evidence %s is a snapshot of %s#%d, but the case is %s#%d",
			id, snap.Repo, snap.Issue, repository, issue)
	}
	return snap, id, nil
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
