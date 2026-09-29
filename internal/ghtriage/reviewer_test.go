package ghtriage_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/ghtriage/fakemodel"
	"github.com/SofiaFlux/summa42/internal/workflow"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

type reviewerFixture struct {
	driver *driverFixture
	model  *fakemodel.Fake
	index  ghtriage.ReviewIndexStore
	review *ghtriage.Reviewer
}

// newReviewerFixture builds the standard one-revision fixture. An older revision
// passed here is registered first and left pending, so the driver's tick
// supersedes it against the fixture revision: that is the shape in which the
// assessment that closes the older case cites a decision belonging to the newer
// one.
func newReviewerFixture(t *testing.T, olderRevisions ...string) *reviewerFixture {
	t.Helper()
	return newFixtureDeciding(t, readyToPlanDecision, olderRevisions...)
}

// newNotActionableReviewerFixture is the same fixture on the other disposition.
// The driver blocks the case and records an assessment, so the reviewer reads a
// case that was closed with a reason rather than one left open with none.
func newNotActionableReviewerFixture(t *testing.T) *reviewerFixture {
	t.Helper()
	return newFixtureDeciding(t, notActionableDecision)
}

func newFixtureDeciding(t *testing.T, decide func(int64, string) ghtriage.Decision, olderRevisions ...string) *reviewerFixture {
	t.Helper()
	f := newDriverFixture(t)
	for _, revision := range olderRevisions {
		f.registerRevision(t, revision)
	}
	f.registerRevision(t, fixtureRevision)
	// A ready-to-plan decision leaves the case ACTIVE, which is the state the
	// reviewer has to be able to read a decision from and the one it can then
	// see superseded. The driver must run once so the task is accepted and the
	// accepted index is written; the reviewer reads state the driver produced.
	f.completeTaskWithDecision(t, fixtureRevision, decide(42, fixtureRevision))
	if _, err := f.driver.Tick(context.Background(), f.missionID); err != nil {
		t.Fatal(err)
	}
	model := fakemodel.New()
	index := ghtriage.NewReviewIndex(f.store, f.clock)
	return &reviewerFixture{
		driver: f,
		model:  model,
		index:  index,
		review: ghtriage.NewReviewer(f.cases, f.evidenceStore, index, model, f.clock),
	}
}

// reviewWithIndex rebuilds the reviewer over another index, so a test can
// observe what the loop does with what the index answers.
func (rf *reviewerFixture) reviewWithIndex(index ghtriage.ReviewIndexStore) {
	rf.review = ghtriage.NewReviewer(rf.driver.cases, rf.driver.evidenceStore, index, rf.model, rf.driver.clock)
}

func (rf *reviewerFixture) verdictCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := rf.driver.store.DB().QueryRowContext(rf.driver.ctx,
		`SELECT count(*) FROM evidence_objects WHERE kind = ?`, ghtriage.KindReview).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// verdicts reads every verdict the index points at, keyed by the revision the
// verdict names, so a test over two revisions can say which one it is looking
// at instead of trusting an ordering.
func (rf *reviewerFixture) verdicts(t *testing.T) map[string]ghtriage.ReviewVerdict {
	t.Helper()
	// The ids are collected before any document is read: the store's pool is
	// small and a verdict read while this cursor is open would wait on it.
	rows, err := rf.driver.store.DB().QueryContext(rf.driver.ctx,
		`SELECT verdict_evidence_id FROM github_issue_triage_reviews`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []domain.ID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		ids = append(ids, domain.ID(id))
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}

	byRevision := map[string]ghtriage.ReviewVerdict{}
	for _, id := range ids {
		_, raw, err := rf.driver.evidenceStore.Get(rf.driver.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		var verdict ghtriage.ReviewVerdict
		if err := json.Unmarshal(raw, &verdict); err != nil {
			t.Fatal(err)
		}
		if _, seen := byRevision[verdict.Revision]; seen {
			t.Fatalf("revision %s has more than one linked verdict", verdict.Revision)
		}
		byRevision[verdict.Revision] = verdict
	}
	return byRevision
}

// rewriteSnapshotCitation stores the decision again with snapshot_evidence_id
// pointing at evidence the case was never observed from, which is the shape of a
// decision that was derived from some other issue.
func (rf *reviewerFixture) rewriteSnapshotCitation(t *testing.T, snapshotEvidenceID string) {
	t.Helper()
	rf.rewriteDecision(t, func(d *ghtriage.Decision) { d.SnapshotEvidenceID = snapshotEvidenceID })
}

func (rf *reviewerFixture) reviewLinkCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := rf.driver.store.DB().QueryRowContext(rf.driver.ctx,
		`SELECT count(*) FROM github_issue_triage_reviews`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// lastVerdict reads the verdict of the most recently linked review. A test that
// reviews twice advances the clock between the two ticks, so created_at is an
// honest order and the tiebreaks below are only there to make a same-instant
// read deterministic.
func (rf *reviewerFixture) lastVerdict(t *testing.T) ghtriage.ReviewVerdict {
	t.Helper()
	var id string
	if err := rf.driver.store.DB().QueryRowContext(rf.driver.ctx,
		`SELECT verdict_evidence_id FROM github_issue_triage_reviews
		 ORDER BY created_at DESC, decision_evidence_id DESC, verdict_evidence_id DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	_, raw, err := rf.driver.evidenceStore.Get(rf.driver.ctx, domain.ID(id))
	if err != nil {
		t.Fatal(err)
	}
	var verdict ghtriage.ReviewVerdict
	if err := json.Unmarshal(raw, &verdict); err != nil {
		t.Fatal(err)
	}
	return verdict
}

// rewriteLatestAssessmentReason edits the reason in the assessment the driver
// recorded, which is the only way to reach a case closed as not-actionable under
// some other reason: Assess refuses a case that is not ACTIVE, so a blocked case
// cannot be assessed a second time. The assessment id is left alone, so the
// fingerprint - and therefore the review this reaches - is the one the matching
// case earned with the same case state and the same assessment.
func (rf *reviewerFixture) rewriteLatestAssessmentReason(t *testing.T, reason string) {
	t.Helper()
	createdCase := rf.driver.casesByRev[fixtureRevision]
	records, err := rf.driver.cases.ListAssessments(rf.driver.ctx, createdCase.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Fatal("the case has no assessment to rewrite")
	}
	latest := records[len(records)-1]
	var result workflowcase.AssessmentResult
	if err := json.Unmarshal([]byte(latest.ResultJSON), &result); err != nil {
		t.Fatal(err)
	}
	result.Decision.Reason = reason
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rf.driver.store.DB().ExecContext(rf.driver.ctx,
		`UPDATE workflow_assessments SET result_json = ? WHERE assessment_id = ?`,
		string(raw), latest.ID); err != nil {
		t.Fatal(err)
	}
}

// rewriteDecision stores a new decision document and repoints the driver's
// accepted index at it, so the reviewer reads a tampered record.
func (rf *reviewerFixture) rewriteDecision(t *testing.T, mutate func(*ghtriage.Decision)) {
	t.Helper()
	original := rf.driver.decisionFor(t, fixtureRevision)
	mutate(&original)
	raw, err := original.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	object, err := rf.driver.evidenceStore.Put(rf.driver.ctx, strings.NewReader(string(raw)), evidence.Metadata{
		MediaType: ghtriage.DecisionMediaType, Kind: ghtriage.KindDecision,
	})
	if err != nil {
		t.Fatal(err)
	}
	rf.driver.repointAccepted(t, fixtureRevision, object.ID)
}

func (rf *reviewerFixture) corruptStage3Rule(t *testing.T, rule string) {
	t.Helper()
	rf.rewriteDecision(t, func(d *ghtriage.Decision) { d.Stage3.Rule = rule })
}

// repointObservation rewrites the case's own observation evidence id. Intake
// writes it once and nothing re-checks it, so this is the only way to reach a
// case whose observation is not the snapshot its decision names - the shape a
// case falls into when something other than a canonical issue snapshot is
// recorded against it.
// decisionOfVerdict loads the decision document a verdict names. A tampered
// fixture leaves the record it replaced on the store as well, so the decision a
// verdict is about is the one named by the verdict and not the newest document
// carrying the revision.
func (rf *reviewerFixture) decisionOfVerdict(t *testing.T, verdict ghtriage.ReviewVerdict) ghtriage.Decision {
	t.Helper()
	_, raw, err := rf.driver.evidenceStore.Get(rf.driver.ctx, domain.ID(verdict.DecisionEvidenceID))
	if err != nil {
		t.Fatal(err)
	}
	var decision ghtriage.Decision
	if err := json.Unmarshal(raw, &decision); err != nil {
		t.Fatal(err)
	}
	return decision
}

func (rf *reviewerFixture) repointObservation(t *testing.T, evidenceID string) {
	t.Helper()
	createdCase := rf.driver.casesByRev[fixtureRevision]
	if _, err := rf.driver.store.DB().ExecContext(rf.driver.ctx,
		`UPDATE workflow_cases SET observation_evidence_id = ? WHERE case_id = ?`,
		evidenceID, createdCase.ID); err != nil {
		t.Fatal(err)
	}
}

// supersedeCase blocks the case with a superseded reason, which the reviewer
// must treat as an allowed terminal state rather than a violation. The work ID
// is the case's own current work, which is the task's idempotency key and not
// the task row's ID.
func (rf *reviewerFixture) supersedeCase(t *testing.T) {
	t.Helper()
	createdCase := rf.driver.casesByRev[fixtureRevision]
	if _, err := rf.driver.cases.Assess(rf.driver.ctx, workflowcase.AssessmentRequest{
		CaseID: createdCase.ID, WorkID: createdCase.CurrentWorkID,
		Assessment: workflow.Assessment{
			Verdict:     workflow.Unknown,
			Reason:      "superseded-by:2026-09-29T09:00:00Z",
			EvidenceIDs: []string{string(rf.driver.decisionEvidenceID(t))},
		},
		RemainingBudget: createdCase.RemainingBudget,
	}); err != nil {
		t.Fatal(err)
	}
}

// The accepting direction of the whole structural block, which nothing pinned
// before: four of the five checks had only tests that they flag a violation, and
// a check hardcoded to false passes every one of those. A block that reported
// every decision as a violation would have stayed green, so the fixture here is
// a correct, untampered decision on a case behaving as that decision says, and
// the assertion is that all five fields agree with it and the plausibility
// question still ran.
func TestReviewerAcceptsACleanDecisionWhoseCaseStateMatchesIt(t *testing.T) {
	rf := newReviewerFixture(t)
	rf.model.ScriptedReview = []bool{true}

	if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	verdict := rf.lastVerdict(t)
	// The precondition, stated rather than assumed: nothing was tampered with,
	// the case is the state the disposition leaves behind, and the decision is
	// the one the case itself made.
	if verdict.DecisionEvidenceID != string(rf.driver.decisionEvidenceID(t)) {
		t.Fatalf("verdict decision = %s, want the case's own %s",
			verdict.DecisionEvidenceID, rf.driver.decisionEvidenceID(t))
	}
	if got := rf.driver.decisionFor(t, fixtureRevision); got.Stage3.Rule != "actionable-without-repro-small-or-medium" {
		t.Fatalf("fixture decision stage 3 rule = %q, want the one Stage3 derives", got.Stage3.Rule)
	}
	if verdict.CaseState != string(workflowcase.Active) {
		t.Fatalf("case state = %q, want ACTIVE", verdict.CaseState)
	}
	if !verdict.Plausible {
		t.Fatal("the scripted plausible verdict was not carried into the document")
	}

	want := ghtriage.Structural{
		Stage2OnlyIfUnresolved:         true,
		SchemaConformant:               true,
		RuleMatchesRecomputation:       "match",
		StateMatchesDisposition:        true,
		ClassificationAgreesWithIntake: true,
	}
	if verdict.Structural != want {
		t.Fatalf("a correct decision on a matching case was reported as %+v, want %+v",
			verdict.Structural, want)
	}
	// The model question is not one of the five, and it is the check the block
	// cannot stand in for: it ran, and the answer is in the document beside them.
	if got := len(rf.model.ReviewInputs); got != 1 {
		t.Fatalf("model calls = %d, want exactly the reviewer's own question", got)
	}
}

// The same accepting direction on the other branch of the re-derivation. A
// decision stage 1 resolved is re-derived by re-running Stage1 over the
// snapshot, and that branch's "match" site had no accepting test at all - a
// mutation there returns "mismatch" for every record and stays green. The
// snapshot has to carry a real duplicate-of label, because a fixture that
// asserted a resolution Stage1 would not derive would prove nothing.
func TestReviewerAcceptsADecisionStageOneResolvedAndAgreesWithTheSnapshot(t *testing.T) {
	f := newDriverFixture(t)
	f.registerSnapshotRevision(t, fixtureIssue, fixtureRevision, ghtriage.Snapshot{
		Title: "Crash on save", Body: "it crashes", Triage: "bug", Labels: []string{"duplicate-of:41"},
	})
	f.completeTaskWithDecision(t, fixtureRevision, duplicateDecision(42, fixtureRevision))
	if _, err := f.driver.Tick(context.Background(), f.missionID); err != nil {
		t.Fatal(err)
	}
	if got := f.latestAssessmentReason(t, fixtureRevision); got != string(ghtriage.DispositionDuplicate) {
		t.Fatalf("assessment reason = %q, want %q: the driver closed the case as duplicate", got, ghtriage.DispositionDuplicate)
	}
	model := fakemodel.New()
	rf := &reviewerFixture{driver: f, model: model}
	rf.review = ghtriage.NewReviewer(f.cases, f.evidenceStore,
		ghtriage.NewReviewIndex(f.store, f.clock), model, f.clock)
	model.ScriptedReview = []bool{true}

	if _, err := rf.review.Tick(context.Background(), f.missionID); err != nil {
		t.Fatal(err)
	}
	verdict := rf.lastVerdict(t)
	// The precondition the branch depends on: stage 1 resolved, so there is no
	// stage 2 or stage 3 to re-derive and no model to ask for one.
	if got := rf.driver.decisionFor(t, fixtureRevision); got.Stage1.Resolved() != true ||
		got.Stage2 != nil || got.Stage3 != nil {
		t.Fatalf("fixture decision = %+v, want a stage 1 that resolved the issue", got)
	}
	if verdict.Structural.RuleMatchesRecomputation != "match" {
		t.Fatalf("a stage 1 Stage1 re-derives to the same rule and was reported %q, want match",
			verdict.Structural.RuleMatchesRecomputation)
	}
	want := ghtriage.Structural{
		Stage2OnlyIfUnresolved:         true,
		SchemaConformant:               true,
		RuleMatchesRecomputation:       "match",
		StateMatchesDisposition:        true,
		ClassificationAgreesWithIntake: true,
	}
	if verdict.Structural != want {
		t.Fatalf("a correct stage 1 decision was reported as %+v, want %+v", verdict.Structural, want)
	}
	if got := len(rf.model.ReviewInputs); got != 1 {
		t.Fatalf("model calls = %d, want only the reviewer's own question", got)
	}
}

func TestReviewerMakesNoSecondModelCallWhileTheCaseStateIsUnchanged(t *testing.T) {
	rf := newReviewerFixture(t)
	rf.model.ScriptedReview = []bool{true, true}

	if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	// The count is len(ReviewInputs) and not the fake's own call counter: that one
	// counts scripted responses consumed, so it reads 1 after two calls whose
	// second response was never scripted, and a test written against it cannot
	// tell one call from two.
	first := len(rf.model.ReviewInputs)
	if first != 1 {
		t.Fatalf("model calls on the first tick = %d, want 1", first)
	}
	if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	if got := len(rf.model.ReviewInputs); got != first {
		t.Fatalf("an unchanged case triggered %d extra model calls", got-first)
	}
	if got := rf.verdictCount(t); got != 1 {
		t.Fatalf("verdict documents = %d, want exactly 1", got)
	}
	// The steady state, named as such: a mission whose cases are all reviewed
	// reports these counters and nothing else, which is what makes a mission of
	// undecided cases - the same three integers, one number different - readable.
	steady, err := rf.review.Tick(context.Background(), rf.driver.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if steady.Reviewed != 0 || steady.Skipped != 1 || steady.Failed != 0 ||
		steady.AlreadyReviewed != 1 || steady.NoDecision != 0 || steady.LostRace != 0 {
		t.Fatalf("steady-state tick = %+v, want one already-reviewed case and nothing else", steady)
	}
}

func TestReviewerReviewsAgainAfterSupersessionChangesTheFingerprint(t *testing.T) {
	rf := newReviewerFixture(t)
	rf.model.ScriptedReview = []bool{true, true}

	if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	rf.supersedeCase(t)
	// The clock moves, so the second review is later than the first by a time
	// rather than by a tiebreak on the fingerprint - which is what lets
	// lastVerdict order by recency and still read the second one.
	rf.driver.clock.Advance(time.Second)
	if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	if got := len(rf.model.ReviewInputs); got != 2 {
		t.Fatalf("model calls = %d, want a second call after the fingerprint changed", got)
	}
	if got := rf.verdictCount(t); got != 2 {
		t.Fatalf("verdict documents = %d, want 2", got)
	}
	// The verdict of the later tick is the one over the superseded state, and
	// reading it is what makes the helper's ordering load-bearing rather than a
	// tiebreak that happens to land the right way.
	if got := rf.lastVerdict(t).CaseState; got != string(workflowcase.Blocked) {
		t.Fatalf("latest verdict case state = %q, want %q: the review after the supersession", got, workflowcase.Blocked)
	}
}

// Supersession closes the older case by citing the newer revision's decision: the
// evidence for "this revision is out of date" is the thing that replaced it. So
// the older case's newest assessment names a decision that is not its own, and a
// reviewer that took the first decision it could decode from an assessment
// resolved to the newer revision's - reviewing the same decision a second time,
// under a state it was never in. What the older case is entitled to is nothing:
// it was never triaged, so it made no decision and there is nothing of its own
// to check, which is what Skipped says.
func TestReviewerReviewsOnlyTheDecisionTheCaseItselfMade(t *testing.T) {
	const older = "2026-09-28T09:00:00Z"
	rf := newReviewerFixture(t, older)
	rf.model.ScriptedReview = []bool{true, true}

	if got := rf.driver.caseState(t, older); got != string(workflowcase.Blocked) {
		t.Fatalf("older case state = %q, want BLOCKED: the driver superseded it", got)
	}
	if got := rf.driver.latestAssessmentEvidence(t, older); len(got) != 1 ||
		string(got[0]) != string(rf.driver.decisionEvidenceID(t)) {
		t.Fatalf("older assessment evidence = %v, want the newer revision's decision %s",
			got, rf.driver.decisionEvidenceID(t))
	}

	result, err := rf.review.Tick(context.Background(), rf.driver.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reviewed != 1 || result.Skipped != 1 || result.Failed != 0 {
		t.Fatalf("result = %+v, want one review and one skip over two cases", result)
	}
	if result.NoDecision != 1 || result.AlreadyReviewed != 0 || result.LostRace != 0 {
		t.Fatalf("result = %+v, want the skip attributed to the case that made no decision", result)
	}
	if got := len(rf.model.ReviewInputs); got != 1 {
		t.Fatalf("model calls = %d, want exactly one: the newer decision, reviewed once", got)
	}
	if got := rf.reviewLinkCount(t); got != 1 {
		t.Fatalf("review index rows = %d, want 1: one per decision and state", got)
	}
	verdict := rf.lastVerdict(t)
	if verdict.Revision != fixtureRevision {
		t.Fatalf("verdict revision = %q, want %q", verdict.Revision, fixtureRevision)
	}
	if verdict.CaseState != string(workflowcase.Active) {
		t.Fatalf("verdict case state = %q, want ACTIVE: the newer case is the one that was reviewed", verdict.CaseState)
	}
	if verdict.LatestAssessmentID != nil {
		t.Fatalf("verdict latest assessment = %q, want none: the newer case has none", *verdict.LatestAssessmentID)
	}
}

// The accepted-index fall-through, on the case that makes it necessary. A case
// whose newest assessment cites a newer revision's decision has, by that fact, no
// decision of its own *on the assessment path* - but it may well have one: a
// revision whose triage completed was accepted on its own decision before
// anything superseded it, and the driver's accepted index is the record of that.
// So the walk past the foreign decision has to end at the case's own index
// rather than at "nothing to review", and the older case has to come back clean
// through the superseded branch rather than skipped or flagged.
func TestReviewerReviewsASupersededCaseThatHadItsOwnDecision(t *testing.T) {
	const older = "2026-09-28T09:00:00Z"
	f := newDriverFixture(t)
	// Both revisions triaged, so both cases carry a decision of their own, and
	// the older one stays ACTIVE (ready-to-plan) until the supersession closes
	// it - Assess refuses a case that is not active.
	f.completeTaskWithDecision(t, older, readyToPlanDecision(42, older))
	f.completeTaskWithDecision(t, fixtureRevision, readyToPlanDecision(42, fixtureRevision))
	if _, err := f.driver.Tick(context.Background(), f.missionID); err != nil {
		t.Fatal(err)
	}
	if got := f.caseState(t, older); got != string(workflowcase.Blocked) {
		t.Fatalf("older case state = %q, want BLOCKED: the driver superseded it", got)
	}
	if got := f.latestAssessmentReason(t, older); got != "superseded-by:"+fixtureRevision {
		t.Fatalf("older assessment reason = %q, want superseded-by:%s", got, fixtureRevision)
	}
	// Its own decision is still on the store and still named by its own accepted
	// index, so there is something of its own to review.
	if got := f.decisionFor(t, older).Stage3.Rule; got != "actionable-without-repro-small-or-medium" {
		t.Fatalf("older decision stage 3 rule = %q, want the one the fixture stored", got)
	}
	model := fakemodel.New()
	review := ghtriage.NewReviewer(f.cases, f.evidenceStore,
		ghtriage.NewReviewIndex(f.store, f.clock), model, f.clock)
	model.ScriptedReview = []bool{true, true}
	rf := &reviewerFixture{driver: f, model: model, review: review}

	result, err := rf.review.Tick(context.Background(), rf.driver.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reviewed != 2 || result.Skipped != 0 || result.Failed != 0 {
		t.Fatalf("result = %+v, want both revisions reviewed: the older one has a decision of its own", result)
	}
	if got := len(model.ReviewInputs); got != 2 {
		t.Fatalf("model calls = %d, want one per revision", got)
	}
	if got := rf.reviewLinkCount(t); got != 2 {
		t.Fatalf("review index rows = %d, want one per revision", got)
	}

	byRevision := rf.verdicts(t)
	if len(byRevision) != 2 {
		t.Fatalf("linked verdicts = %d, want one per revision", len(byRevision))
	}
	older2, found := byRevision[older]
	if !found {
		t.Fatalf("no verdict for the superseded revision %s, only %v", older, keysOf(byRevision))
	}
	// The older verdict is about the older decision, under the older case's state.
	if older2.Revision != older || older2.Issue != 42 {
		t.Fatalf("older verdict = revision %q issue %d, want %s/42", older2.Revision, older2.Issue, older)
	}
	if older2.DecisionEvidenceID != string(f.decisionEvidenceIDFor(t, older)) {
		t.Fatalf("older verdict names decision %s, want its own %s",
			older2.DecisionEvidenceID, f.decisionEvidenceIDFor(t, older))
	}
	if older2.CaseState != string(workflowcase.Blocked) {
		t.Fatalf("older verdict case state = %q, want BLOCKED", older2.CaseState)
	}
	// Clean through the superseded branch: the reason prefix is what allows it,
	// and the disposition itself (ready-to-plan) never closed this case.
	if !older2.Structural.StateMatchesDisposition {
		t.Fatalf("a superseded case with its own decision was reported as a violation: %+v", older2.Structural)
	}
	if older2.LatestAssessmentID == nil {
		t.Fatal("older verdict names no assessment: it was assessed as superseded")
	}
	newer, found := byRevision[fixtureRevision]
	if !found {
		t.Fatalf("no verdict for %s, only %v", fixtureRevision, keysOf(byRevision))
	}
	if newer.CaseState != string(workflowcase.Active) || newer.LatestAssessmentID != nil {
		t.Fatalf("newer verdict = state %q assessment %v, want ACTIVE and no assessment",
			newer.CaseState, newer.LatestAssessmentID)
	}
}

func keysOf(byRevision map[string]ghtriage.ReviewVerdict) []string {
	out := make([]string, 0, len(byRevision))
	for revision := range byRevision {
		out = append(out, revision)
	}
	return out
}

func TestReviewerWritesNoVerdictAndNoIndexRowWhenTheModelFails(t *testing.T) {
	rf := newReviewerFixture(t)
	rf.model.Err = errors.New("model unavailable")

	result, err := rf.review.Tick(context.Background(), rf.driver.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Failed != 1 {
		t.Fatalf("result = %+v, want one failure", result)
	}
	if got := rf.verdictCount(t); got != 0 {
		t.Fatalf("verdict documents = %d, want 0", got)
	}
	if got := rf.reviewLinkCount(t); got != 0 {
		t.Fatalf("review index rows = %d, want 0 so the next tick retries", got)
	}
}

// The consequence of writing no index row, asserted rather than inferred: a
// reviewer that had recorded the failure as done would skip the case for ever,
// and the retry is the whole reason the model call is not allowed to leave a
// verdict behind. The second call is the one that was suppressed by the first
// tick's silence, so a count of two says the case came back.
func TestReviewerRetriesTheCaseAFailedModelCallLeftUnreviewed(t *testing.T) {
	rf := newReviewerFixture(t)
	rf.model.Err = errors.New("model unavailable")
	if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	rf.model.Err = nil
	rf.model.ScriptedReview = []bool{true}

	result, err := rf.review.Tick(context.Background(), rf.driver.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reviewed != 1 || result.Skipped != 0 || result.Failed != 0 {
		t.Fatalf("result = %+v, want the case reviewed on the retry", result)
	}
	if got := len(rf.model.ReviewInputs); got != 2 {
		t.Fatalf("model calls = %d, want the failed one and the retry", got)
	}
	if got := rf.verdictCount(t); got != 1 {
		t.Fatalf("verdict documents = %d, want 1: only the retry wrote one", got)
	}
	if got := rf.reviewLinkCount(t); got != 1 {
		t.Fatalf("review index rows = %d, want 1: only the retry linked one", got)
	}
}

// One case that cannot be read is not a reason to stop reviewing the others. The
// loop reports the failure and carries on, so a mission whose snapshot for one
// issue went missing still gets a verdict for every other issue rather than a
// single failure that hides them.
func TestReviewerReviewsTheOtherCasesAfterOneFails(t *testing.T) {
	rf := newReviewerFixture(t)
	broken := "2026-09-29T10:00:00Z"
	rf.driver.registerIssueRevision(t, "o/r#43", broken)
	rf.driver.completeTaskWithDecision(t, broken, readyToPlanDecision(43, broken))
	if _, err := rf.driver.driver.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	// The snapshot of one revision is what its decision is re-derived from, so
	// taking it away is a case the reviewer cannot read rather than one it judges
	// wrong.
	if _, err := rf.driver.store.DB().ExecContext(rf.driver.ctx,
		`DELETE FROM evidence_objects WHERE evidence_id = ?`,
		rf.driver.snapshotEvidenceID(t, broken)); err != nil {
		t.Fatal(err)
	}
	rf.model.ScriptedReview = []bool{true, true}

	result, err := rf.review.Tick(context.Background(), rf.driver.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reviewed != 1 || result.Failed != 1 || result.Skipped != 0 {
		t.Fatalf("result = %+v, want one review and one failure over two cases", result)
	}
	if got := rf.verdictCount(t); got != 1 {
		t.Fatalf("verdict documents = %d, want one, for the case that could be read", got)
	}
}

// A decision may cite any snapshot it likes: the field is a string, and nothing
// rewrites it after the fact. So the reviewer has to derive from the case's own
// observation, not from the citation, and a citation pointing at another issue's
// snapshot is itself the violation - because every check that read the citation
// would be comparing the decision with whatever the record points at and agreeing
// with it by construction, and the model would be asked the other issue's
// question.
func TestReviewerReportsADecisionThatCitesAnotherIssuesSnapshot(t *testing.T) {
	const other = "2026-09-28T11:00:00Z"
	rf := newReviewerFixture(t)
	rf.model.ScriptedReview = []bool{true, true}
	// A second issue of the same mission, so the store holds a real snapshot
	// that is not this case's. Its own triage is left unfinished, so it
	// contributes nothing but the evidence this case wrongly cites.
	rf.driver.registerSnapshotRevision(t, "o/r#43", other, ghtriage.Snapshot{
		Title: "Crash on export", Body: "it crashes on export", Triage: "bug", Labels: []string{"bug"},
	})
	foreign := rf.driver.snapshotEvidenceID(t, other)
	rf.rewriteSnapshotCitation(t, foreign)

	if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	verdict := rf.lastVerdict(t)
	// The question was asked about this case's issue, which is the whole point
	// of deriving from the case's own observation: the two snapshots have
	// different titles, so the one the model was handed names the other issue.
	if len(rf.model.ReviewInputs) != 1 {
		t.Fatalf("model calls = %d, want one", len(rf.model.ReviewInputs))
	}
	if own := rf.driver.casesByRev[fixtureRevision].ObservationEvidenceID; own == foreign {
		t.Fatal("the fixture pointed the decision at its own observation")
	}
	if got := rf.model.ReviewInputs[0].Title; got != "Crash on save" {
		t.Fatalf("question title = %q, want this case's own title", got)
	}
	// The citation is what disagrees, and it is the classification check that
	// says so: nothing else in the block reads it.
	if verdict.Structural.ClassificationAgreesWithIntake {
		t.Fatalf("a decision citing another issue's snapshot was reported as agreeing with intake: %+v",
			verdict.Structural)
	}
	// Every other field still re-derives against this case's snapshot, so the
	// verdict is not simply all-false: it says which part of the record is wrong.
	if verdict.Structural.RuleMatchesRecomputation != "match" {
		t.Fatalf("recomputation = %q, want match: the decision agrees with this case's snapshot on the rule",
			verdict.Structural.RuleMatchesRecomputation)
	}
	if verdict.Issue != 42 {
		t.Fatalf("verdict issue = %d, want 42: the decision is still reviewed under its own case", verdict.Issue)
	}
}

// The branch of the re-derivation that has no outcome of its own to accept: a
// stage 1 that did not resolve the issue has to carry both stage 2 and stage 3,
// so a record missing either one cannot be re-derived from anything and is
// reported as a mismatch rather than compared. It is reachable only for a
// decision the schema already rejects, which is why it is worth pinning in the
// same place as the schema check: a re-derivation that read the missing stage
// instead of refusing it would not return a verdict at all, and the block would
// say nothing about a record it cannot read.
func TestReviewerReportsAMismatchForAnUnresolvedDecisionMissingStage2(t *testing.T) {
	rf := newReviewerFixture(t)
	rf.model.ScriptedReview = []bool{true}
	rf.rewriteDecision(t, func(d *ghtriage.Decision) { d.Stage2 = nil })

	if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	// The precondition the branch depends on: the decision's own stage 1 left the
	// issue unresolved, so the disposition rules are the ones in force and the
	// stage 3 on the record is the one nothing can be re-derived from.
	verdict := rf.lastVerdict(t)
	decision := rf.decisionOfVerdict(t, verdict)
	if decision.Stage1.Resolved() || decision.Stage2 != nil || decision.Stage3 == nil {
		t.Fatalf("reviewed decision = %+v, want an unresolved stage 1 and no stage 2", decision)
	}
	structural := verdict.Structural
	if structural.RuleMatchesRecomputation != "mismatch" {
		t.Fatalf("recomputation = %q, want mismatch: there is nothing to re-derive the stage 3 from",
			structural.RuleMatchesRecomputation)
	}
	if structural.SchemaConformant {
		t.Fatal("an unresolved decision with no stage 2 was reported as schema conformant")
	}
	if got := len(rf.model.ReviewInputs); got != 1 {
		t.Fatalf("model calls = %d, want exactly the reviewer's own question", got)
	}
}

// The other half of "this record is about this case": the citation says which
// snapshot a decision was derived from, and nothing rewrites the repository and
// issue it claims, so a record about #99 filed against this case's snapshot
// re-derives cleanly through every other check and is published as this case's
// verdict. Without the comparison the artefact makes a claim nobody verified,
// and the identity check is where it is verified.
func TestReviewerReportsADecisionThatIsAboutAnotherIssue(t *testing.T) {
	cases := []struct {
		name           string
		mutate         func(*ghtriage.Decision)
		wantRepository string
		wantIssue      int64
	}{
		{
			name:           "another issue number",
			mutate:         func(d *ghtriage.Decision) { d.Issue = 99 },
			wantRepository: "o/r", wantIssue: 99,
		},
		{
			name:           "another repository",
			mutate:         func(d *ghtriage.Decision) { d.Repository = "other/repo" },
			wantRepository: "other/repo", wantIssue: 42,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rf := newReviewerFixture(t)
			rf.model.ScriptedReview = []bool{true}
			rf.rewriteDecision(t, tc.mutate)

			if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
				t.Fatal(err)
			}
			verdict := rf.lastVerdict(t)
			if verdict.Structural.ClassificationAgreesWithIntake {
				t.Fatalf("a decision claiming %s#%d was reported as agreeing with intake to a case about %s: %+v",
					tc.wantRepository, tc.wantIssue, fixtureIssue, verdict.Structural)
			}
			// Not simply all-false: the record agrees with this case's snapshot on
			// everything it says about the issue, and the identity is the only thing
			// that is wrong. A block reporting a disagreement has to say which one.
			if !verdict.Structural.SchemaConformant || !verdict.Structural.Stage2OnlyIfUnresolved ||
				!verdict.Structural.StateMatchesDisposition ||
				verdict.Structural.RuleMatchesRecomputation != "match" {
				t.Fatalf("only the identity disagrees, but the block reports %+v", verdict.Structural)
			}
			// The verdict reports the decision as written, identity included: the
			// disagreement is in the block, and the document says what was reviewed.
			if verdict.Repository != tc.wantRepository || verdict.Issue != tc.wantIssue {
				t.Fatalf("verdict identity = %s#%d, want the decision's own %s#%d",
					verdict.Repository, verdict.Issue, tc.wantRepository, tc.wantIssue)
			}
			// The model is asked about this case's issue and handed the record as it
			// was written, foreign identity included. Sending it is the decision:
			// the document under review is the question's subject, and editing it
			// before the model sees it would answer a question the verdict does not
			// answer.
			if got := len(rf.model.ReviewInputs); got != 1 {
				t.Fatalf("model calls = %d, want 1", got)
			}
			asked := rf.model.ReviewInputs[0]
			if asked.Title != "Crash on save" {
				t.Fatalf("question title = %q, want this case's own title", asked.Title)
			}
			if asked.Decision.Issue != tc.wantIssue || asked.Decision.Repository != tc.wantRepository {
				t.Fatalf("decision handed to the model = %s#%d, want the record's own %s#%d",
					asked.Decision.Repository, asked.Decision.Issue, tc.wantRepository, tc.wantIssue)
			}
		})
	}
}

// The case's observation is the one document every check that needs it reads, and
// the field naming it is written once by intake and never re-checked - the
// executor proves it on its own copy of the id when it stores a decision, but
// that check does not travel with the case row. So the reader re-establishes it:
// an observation that is not a canonical snapshot of this case's issue fails the
// review, which is counted as a failure and retried, rather than decoding to an
// empty snapshot and reporting a correct decision as a mismatch. False
// accusation is the one direction of this error worse than silence.
func TestReviewerFailsWhenTheObservationIsNotACanonicalSnapshot(t *testing.T) {
	cases := []struct {
		name        string
		observation func(*testing.T, *reviewerFixture) string
		wantErr     string
	}{
		{
			// The case's own decision document: a real object the store holds,
			// decoding cleanly and naming no issue.
			name: "evidence of another kind",
			observation: func(t *testing.T, rf *reviewerFixture) string {
				return string(rf.driver.decisionEvidenceID(t))
			},
			wantErr: `is a "github.issue.triage.decision", not an issue snapshot`,
		},
		{
			name: "a snapshot naming no issue",
			observation: func(t *testing.T, rf *reviewerFixture) string {
				return string(putSnapshot(t, rf.driver.evidenceStore, ghtriage.Snapshot{}))
			},
			wantErr: "not a canonical issue snapshot",
		},
		{
			name: "a snapshot of another issue",
			observation: func(t *testing.T, rf *reviewerFixture) string {
				return string(putSnapshot(t, rf.driver.evidenceStore, ghtriage.Snapshot{
					Repo: "o/r", Issue: 43, Title: "Crash on export", Triage: "bug",
				}))
			},
			wantErr: "is a snapshot of o/r#43, but the case is o/r#42",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rf := newReviewerFixture(t)
			rf.model.ScriptedReview = []bool{true}
			rf.repointObservation(t, tc.observation(t, rf))
			readStderr := captureReviewerStderr(t)

			result, err := rf.review.Tick(context.Background(), rf.driver.missionID)
			if err != nil {
				t.Fatal(err)
			}
			if result.Failed != 1 || result.Reviewed != 0 || result.Skipped != 0 {
				t.Fatalf("result = %+v, want the one case failed and nothing else", result)
			}
			// The failure is the case's and it says why on stderr, because an
			// operator reading the counters cannot tell an unreadable observation
			// from anything else.
			if printed := readStderr(); !strings.Contains(printed, tc.wantErr) {
				t.Fatalf("stderr %q does not report %q", printed, tc.wantErr)
			}
			// No verdict, and nothing linked: a false accusation is the failure
			// shape this check exists to prevent, so the store is left clean and
			// the next tick asks again.
			if got := rf.verdictCount(t); got != 0 {
				t.Fatalf("verdict documents = %d, want 0: the decision was correct, the observation was not readable", got)
			}
			if got := rf.reviewLinkCount(t); got != 0 {
				t.Fatalf("review index rows = %d, want 0 so the next tick retries", got)
			}
			// The question is not asked of a model about an observation the
			// reviewer could not prove it read.
			if got := len(rf.model.ReviewInputs); got != 0 {
				t.Fatalf("model calls = %d, want none: the case was never read", got)
			}
		})
	}
}

func TestReviewerReportsAnInjectedStage3Mismatch(t *testing.T) {
	rf := newReviewerFixture(t)
	rf.model.ScriptedReview = []bool{true}
	rf.corruptStage3Rule(t, "a-rule-that-does-not-match")

	if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	if got := rf.lastVerdict(t).Structural.RuleMatchesRecomputation; got != "mismatch" {
		t.Fatalf("recomputation = %q, want mismatch", got)
	}
	// The one scripted response is consumed by the reviewer's own question, so a
	// recomputation that asked the model anything - even and especially to redo
	// stage 2 - would run out of script and be reported as a failed review rather
	// than a mismatch. The verdict value alone cannot tell those apart, because a
	// model call that fails and a rule that does not match both read "mismatch";
	// the call count is what says the check was re-derived, not re-asked.
	if got := len(rf.model.ReviewInputs); got != 1 {
		t.Fatalf("model calls = %d, want exactly the reviewer's own question", got)
	}
}

func TestReviewerReportsNotApplicableForAFutureRulesVersion(t *testing.T) {
	rf := newReviewerFixture(t)
	rf.model.ScriptedReview = []bool{true}
	rf.rewriteDecision(t, func(d *ghtriage.Decision) {
		d.DispositionRulesVersion = "ghtriage.dispositions.v99"
		d.Stage3 = &ghtriage.Stage3Result{Disposition: ghtriage.DispositionReadyToPlan, Rule: "a-future-rule"}
	})

	if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	if got := rf.lastVerdict(t).Structural.RuleMatchesRecomputation; got != "not-applicable" {
		t.Fatalf("recomputation = %q, want not-applicable", got)
	}
}

// The same question asked of the other half of the pipeline. A stage 1 the newer
// triage rules resolved is the one record the reviewer would otherwise re-derive
// with rules it does not have, and it is the branch the disposition-rules test
// above cannot reach.
//
// The future rule resolves this issue as not-actionable, and not-actionable
// closes its case, so the driver is given a tick to close it: a case the decision
// has not been reconciled against is one the reviewer declines (see reconciled),
// and what this test is about is the rules version, not the state.
func TestReviewerReportsNotApplicableForAFutureTriageRulesVersion(t *testing.T) {
	rf := newReviewerFixture(t)
	rf.model.ScriptedReview = []bool{true}
	rf.rewriteDecision(t, func(d *ghtriage.Decision) {
		d.TriageRulesVersion = "ghtriage.rules.v99"
		d.Stage1.Disposition = ghtriage.DispositionNotActionable
		d.Stage1.Rule = "a-future-rule"
		d.Stage2 = nil
		d.Stage3 = nil
	})
	if _, err := rf.driver.driver.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	if got := rf.driver.caseState(t, fixtureRevision); got != string(workflowcase.Blocked) {
		t.Fatalf("case state = %q, want BLOCKED: the driver closed the case as the record now says", got)
	}

	if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	verdict := rf.lastVerdict(t)
	if got := verdict.Structural.RuleMatchesRecomputation; got != "not-applicable" {
		t.Fatalf("recomputation = %q, want not-applicable", got)
	}
	// The snapshot carries no duplicate label, so a reviewer implementing the
	// current rules would derive no disposition at all here. Reporting
	// not-applicable rather than a mismatch is what keeps the record honest
	// about why it did not check.
	if got := len(rf.model.ReviewInputs); got != 1 {
		t.Fatalf("model calls = %d, want only the reviewer's own question", got)
	}
}

func TestReviewerAcceptsASupersededCaseAsAnAllowedTerminalState(t *testing.T) {
	rf := newReviewerFixture(t)
	rf.model.ScriptedReview = []bool{true}
	rf.supersedeCase(t)

	if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	verdict := rf.lastVerdict(t)
	if !verdict.Structural.StateMatchesDisposition {
		t.Fatalf("a correctly superseded case was reported as a violation: %+v", verdict.Structural)
	}
}

// The other two branches of stateMatches, which the superseded case above never
// reaches: a review of a case that is behaving exactly as its disposition says
// has to come back clean, and a review of one that was closed for some other
// reason has to come back flagged. A rule that only ever reports false is not
// a rule - it makes the whole field meaningless - so all three outcomes are
// pinned here, not only the one a violation would exercise.
func TestReviewerAcceptsARecentReadyToPlanCaseAsMatchingItsDisposition(t *testing.T) {
	rf := newReviewerFixture(t)
	rf.model.ScriptedReview = []bool{true}

	if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	verdict := rf.lastVerdict(t)
	// The precondition the branch reads: a ready-to-plan decision leaves the
	// case ACTIVE and assesses nothing, so there is no assessment to disagree
	// with and the case is not waiting on anybody.
	if verdict.LatestAssessmentID != nil {
		t.Fatalf("latest assessment = %q, want none: ready-to-plan closes nothing", *verdict.LatestAssessmentID)
	}
	if verdict.CaseState != string(workflowcase.Active) {
		t.Fatalf("case state = %q, want ACTIVE", verdict.CaseState)
	}
	if !verdict.Structural.StateMatchesDisposition {
		t.Fatalf("an active case with a ready-to-plan decision was reported as a violation: %+v", verdict.Structural)
	}
}

func TestReviewerAcceptsReadyToPlanCaseAfterPlanningHandoff(t *testing.T) {
	rf := newReviewerFixture(t)
	model := &plannerModel{output: validPlannerOutput()}
	planner := ghtriage.NewPlanner(rf.driver.cases, rf.driver.execSvc, rf.driver.verifSvc, rf.driver.evidenceStore, model)
	planned, err := planner.Tick(rf.driver.ctx, rf.driver.missionID)
	if err != nil || planned.Planned != 1 || len(planned.Failures) != 0 {
		t.Fatalf("planning: %+v, %v", planned, err)
	}
	rf.model.ScriptedReview = []bool{true}
	if _, err := rf.review.Tick(rf.driver.ctx, rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	if verdict := rf.lastVerdict(t); !verdict.Structural.StateMatchesDisposition {
		t.Fatalf("planned case reported as violation: %+v", verdict)
	}
}

// A not-actionable decision is closed by the driver as an assessment whose
// reason is the disposition itself, so the matching case is BLOCKED and its
// latest assessment says not-actionable. That is the pair the default branch
// compares, and it has to be accepted for the check to mean anything.
func TestReviewerAcceptsANotActionableCaseClosedUnderItsOwnDisposition(t *testing.T) {
	rf := newNotActionableReviewerFixture(t)
	rf.model.ScriptedReview = []bool{true}

	if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	verdict := rf.lastVerdict(t)
	if verdict.CaseState != string(workflowcase.Blocked) {
		t.Fatalf("case state = %q, want BLOCKED: not-actionable closes the case", verdict.CaseState)
	}
	if got := rf.driver.latestAssessmentReason(t, fixtureRevision); got != string(ghtriage.DispositionNotActionable) {
		t.Fatalf("assessment reason = %q, want %q", got, ghtriage.DispositionNotActionable)
	}
	if !verdict.Structural.StateMatchesDisposition {
		t.Fatalf("a case closed under its own disposition was reported as a violation: %+v", verdict.Structural)
	}
}

// The same case, the same decision, the same assessment, one field apart: a
// case closed for a reason the decision does not name. Nothing else in the
// verdict moves, so this is the only thing that says the default branch reads
// the reason at all rather than only the case state - and a not-actionable case
// closed under some other reason is exactly the state a disagreement between
// the decision and the driver's disposition would produce.
func TestReviewerReportsANotActionableCaseClosedUnderAnotherReason(t *testing.T) {
	rf := newNotActionableReviewerFixture(t)
	rf.model.ScriptedReview = []bool{true}
	rf.rewriteLatestAssessmentReason(t, "ready-to-plan")

	if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	verdict := rf.lastVerdict(t)
	if verdict.Structural.StateMatchesDisposition {
		t.Fatalf("a case closed as ready-to-plan against a not-actionable decision was accepted: %+v", verdict.Structural)
	}
}

// A case whose triage exhausted its retries was never triaged, so it has no
// decision and the driver has already reported it. It is skipped rather than
// reviewed, and no question is asked of the model on its behalf - which is what
// a case with nothing to review means, so the behaviour is pinned here rather
// than a triage-failed test of its own in the loop.
func TestReviewerSkipsACaseWhoseTriageFailed(t *testing.T) {
	rf := newReviewerFixture(t)
	rf.model.ScriptedReview = []bool{true}
	rf.supersedeCase(t)
	if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	// A second case, triaged by the executor rather than completed by the
	// fixture, whose retries run out.
	rf.driver.failTaskTwice(t, "2026-09-28T10:30:00Z")
	if _, err := rf.driver.driver.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	if got := rf.driver.latestAssessmentReason(t, "2026-09-28T10:30:00Z"); got != ghtriage.ReasonTriageFailed {
		t.Fatalf("assessment reason = %q, want %q", got, ghtriage.ReasonTriageFailed)
	}
	before := len(rf.model.ReviewInputs)

	result, err := rf.review.Tick(context.Background(), rf.driver.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Failed != 0 {
		t.Fatalf("result = %+v, want no failure: nothing about this case is unreadable", result)
	}
	if result.Reviewed != 0 || result.Skipped != 2 {
		t.Fatalf("result = %+v, want both cases skipped", result)
	}
	if got := len(rf.model.ReviewInputs); got != before {
		t.Fatalf("model calls = %d, want %d: a case with no decision is not worth a question", got, before)
	}
	if got := rf.reviewLinkCount(t); got != 1 {
		t.Fatalf("review index rows = %d, want 1: only the first tick linked one", got)
	}
}

// Two ticks that reach the same decision at the same time both pay for the model
// call, and the primary key on the index decides which verdict is the one a
// reader finds. The loser has already written its document by then, so the only
// thing the bool it gets back buys is that it does not report or count a verdict
// no reader can reach.
//
// Two real goroutines are not run here: the reviewer prints to the process's
// stderr and writes to one store, so a concurrent test would be testing the
// driver's serialisation at least as much as the reviewer's handling, and the
// property under test is the store's answers, which the index test proves
// directly. What this pins is that a losing link is counted as a lost race and
// not folded into either of the other two skip reasons.
func TestReviewerSaysNothingWhenTheIndexKeptTheOtherTicksVerdict(t *testing.T) {
	rf := newReviewerFixture(t)
	rf.model.ScriptedReview = []bool{true}
	rf.reviewWithIndex(losingLinkIndex{ReviewIndexStore: rf.index})

	readStderr := captureReviewerStderr(t)
	result, err := rf.review.Tick(context.Background(), rf.driver.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reviewed != 0 || result.Skipped != 1 || result.Failed != 0 {
		t.Fatalf("result = %+v, want the case skipped: this tick stored no verdict", result)
	}
	if result.LostRace != 1 || result.AlreadyReviewed != 0 || result.NoDecision != 0 {
		t.Fatalf("result = %+v, want the skip attributed to the lost race alone", result)
	}
	if printed := readStderr(); strings.TrimSpace(printed) != "" {
		t.Fatalf("stderr = %q, want nothing: the verdict this tick did not store was reported", printed)
	}
	if got := rf.reviewLinkCount(t); got != 0 {
		t.Fatalf("review index rows = %d, want 0: the other tick's row is not this fixture's", got)
	}
}

// losingLinkIndex is the index as a concurrent tick would have it by the time
// this one reaches the link: the decision has already been reviewed for this
// state, and this tick's row is the one that loses.
type losingLinkIndex struct {
	ghtriage.ReviewIndexStore
}

func (losingLinkIndex) LinkReview(context.Context, domain.ID, string, string, domain.ID) (bool, error) {
	return false, nil
}

func TestReviewerReportsAClassificationThatDisagreesWithIntake(t *testing.T) {
	rf := newReviewerFixture(t)
	rf.model.ScriptedReview = []bool{true}
	rf.rewriteDecision(t, func(d *ghtriage.Decision) { d.Stage1.Triage = ghtriage.TriageFeature })

	if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	if rf.lastVerdict(t).Structural.ClassificationAgreesWithIntake {
		t.Fatal("a stage 1 triage that disagrees with the snapshot was accepted")
	}
}

// A decision that says stage 1 settled the issue and also carries the stage 2
// and stage 3 documents is the one fabrication two structural checks exist for:
// the schema says a stage 1 decision carries neither, and the model must not have
// been consulted at all. Neither field is reached by the tests above, so both are
// pinned here against a decision that is wrong in both ways at once.
//
// The fixture's decision says duplicate, and a duplicate closes its case, so the
// driver is given a tick to close it: a case the decision has not been reconciled
// against is one the reviewer declines (see reconciled), and what this test is
// about is a decision that disagrees with itself, not a case caught between two
// steps. The state check is not the subject here and is not asserted.
func TestReviewerReportsAStage1DecisionThatStillCarriesStage2(t *testing.T) {
	rf := newReviewerFixture(t)
	rf.model.ScriptedReview = []bool{true}
	rf.rewriteDecision(t, func(d *ghtriage.Decision) {
		d.Stage1.Disposition = ghtriage.DispositionDuplicate
		d.Stage1.Rule = "explicit-duplicate-label"
	})
	if _, err := rf.driver.driver.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	if got := rf.driver.caseState(t, fixtureRevision); got != string(workflowcase.Blocked) {
		t.Fatalf("case state = %q, want BLOCKED: the driver closed the case as the record now says", got)
	}

	if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	structural := rf.lastVerdict(t).Structural
	if structural.SchemaConformant {
		t.Fatal("a stage 1 decision carrying stage 2 and stage 3 was reported as schema conformant")
	}
	if structural.Stage2OnlyIfUnresolved {
		t.Fatal("a resolved stage 1 that also carries stage 2 was reported as stage 2 only if unresolved")
	}
}

// The verdict names what was reviewed and what it was read against. The
// observation is not decoration: every check that needs the snapshot derives from
// it and the decision's own citation is never trusted, so a verdict that named
// only the citation would name the one document the review is not about.
func TestReviewerRecordsTheDecisionTheObservationAndTheStateItObserved(t *testing.T) {
	rf := newReviewerFixture(t)
	rf.model.ScriptedReview = []bool{true}

	if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	verdict := rf.lastVerdict(t)
	if verdict.DecisionEvidenceID == "" {
		t.Fatal("the verdict does not name the decision it reviewed")
	}
	if want := rf.driver.observationEvidenceID(t, fixtureRevision); verdict.ObservationEvidenceID != want {
		t.Fatalf("verdict observation = %q, want the case's own %q", verdict.ObservationEvidenceID, want)
	}
	// The observation it names is the one the decision cites in this fixture, and
	// it is named in its own right: the reviewer does not read the citation.
	if want := rf.driver.snapshotEvidenceID(t, fixtureRevision); verdict.ObservationEvidenceID != want {
		t.Fatalf("verdict observation = %q, want the snapshot the case was observed from %q",
			verdict.ObservationEvidenceID, want)
	}
	if verdict.ReviewerVersion != ghtriage.ReviewerVersion {
		t.Fatalf("reviewer version = %q", verdict.ReviewerVersion)
	}
	if verdict.CaseState == "" {
		t.Fatal("the verdict does not record the case state it observed")
	}
}

// A review that changes nothing is only useful to somebody who reads it, and the
// stored document is the one thing a later tick has already decided to skip
// past. The line on stderr is what an operator watching a loop sees, so it has
// to carry the same verdict ID the index points at rather than a summary of one.
func TestReviewerPrintsTheVerdictItLinked(t *testing.T) {
	rf := newReviewerFixture(t)
	rf.model.ScriptedReview = []bool{true}

	readStderr := captureReviewerStderr(t)
	if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	printed := readStderr()
	var linked string
	if err := rf.driver.store.DB().QueryRowContext(rf.driver.ctx,
		`SELECT verdict_evidence_id FROM github_issue_triage_reviews LIMIT 1`).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{linked, "o/r#42", "plausible=true"} {
		if !strings.Contains(printed, want) {
			t.Fatalf("stderr %q does not mention %q", printed, want)
		}
	}
}

// The reviewer is designed to read a decision before the case has moved:
// findDecision falls back to the driver's accepted index precisely to cover the
// windows between the worker completing a triage and the driver closing the case
// on it. In both of those windows the case is still ACTIVE with no assessment,
// which is the state stateMatches calls a violation for every disposition that
// closes a case - so a reviewer running in that window linked a durable verdict
// reporting state_matches_disposition: false against a decision that was correct.
// The verdict is the reviewer's only product and exists to be scored against
// human labels, so a false violation there is a mislabel in a corpus meant to be
// ground truth.
//
// Both windows are pinned here, on the disposition that closes a case, and both
// assert the same three things the false violation needed: no verdict, no index
// row and no question asked. The follow-up is asserted in each, because a case
// that is never reviewed would be the other half of the same wrong answer.

// crashBeforeAccept is the window before AcceptTask: the driver's accepted index
// is written, the task is still AWAITING_VERIFICATION and the case is ACTIVE
// with nothing assessed.
func crashBeforeAccept(t *testing.T) *driverFixture {
	t.Helper()
	f := newDriverFixture(t)
	f.completeTaskWithDecision(t, fixtureRevision, notActionableDecision(42, fixtureRevision))
	f.repointAccepted(t, fixtureRevision, f.decisionEvidenceIDFor(t, fixtureRevision))
	return f
}

func TestReviewerDoesNotReviewACaseTheDriverHasDecidedButNotAccepted(t *testing.T) {
	f := crashBeforeAccept(t)
	// The precondition, stated rather than assumed: the window is reachable only
	// because the decision is found through the accepted index, because the case
	// has not been assessed, and because the disposition closes a case.
	if got := f.taskState(t, fixtureRevision); got != string(domain.TaskAwaitingVerification) {
		t.Fatalf("task state = %q, want AWAITING_VERIFICATION: the accept has not happened", got)
	}
	if got := f.caseState(t, fixtureRevision); got != string(workflowcase.Active) {
		t.Fatalf("case state = %q, want ACTIVE", got)
	}
	if got := f.latestAssessmentReason(t, fixtureRevision); got != "" {
		t.Fatalf("assessment reason = %q, want none", got)
	}
	rf := reviewWith(t, f)

	result, err := rf.review.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Unreconciled != 1 || result.Reviewed != 0 || result.NoDecision != 0 || result.Failed != 0 {
		t.Fatalf("result = %+v, want the case skipped as unreconciled and nothing else", result)
	}
	if result.Skipped != 1 {
		t.Fatalf("result = %+v, want the skip counted", result)
	}
	if got := rf.verdictCount(t); got != 0 {
		t.Fatalf("verdict documents = %d, want 0: there is no case state to compare the disposition against", got)
	}
	if got := rf.reviewLinkCount(t); got != 0 {
		t.Fatalf("review index rows = %d, want 0, so the case is reviewed once it is closed", got)
	}
	if got := len(rf.model.ReviewInputs); got != 0 {
		t.Fatalf("model calls = %d, want none", got)
	}

	// Once the driver has moved the case, the next tick reviews it, and the state
	// it was in all along is reported as what it is.
	if tickResult, err := f.driver.Tick(context.Background(), f.missionID); err != nil || len(tickResult.Failures) != 0 {
		t.Fatalf("driver tick: result=%+v err=%v", tickResult, err)
	}
	if got := f.caseState(t, fixtureRevision); got != string(workflowcase.Blocked) {
		t.Fatalf("case state after the driver tick = %q, want BLOCKED", got)
	}
	after, err := rf.review.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Reviewed != 1 || after.Skipped != 0 {
		t.Fatalf("result = %+v, want the case reviewed once the driver closed it", after)
	}
	if !rf.lastVerdict(t).Structural.StateMatchesDisposition {
		t.Fatalf("a correctly closed case was reported as %+v", rf.lastVerdict(t).Structural)
	}
}

func TestReviewerDoesNotReviewACaseTheDriverHasAcceptedButNotClosed(t *testing.T) {
	f := newDriverFixture(t)
	// The window after AcceptTask and before Assess: the task is accepted, the
	// index it was replayed from is written, and the case is still ACTIVE.
	f.simulateRestartAfterAcceptance(t, fixtureIssue, fixtureRevision)
	if got := f.taskState(t, fixtureRevision); got != string(domain.TaskSucceeded) {
		t.Fatalf("task state = %q, want SUCCEEDED: the accept has happened", got)
	}
	if got := f.caseState(t, fixtureRevision); got != string(workflowcase.Active) {
		t.Fatalf("case state = %q, want ACTIVE", got)
	}
	if got := f.latestAssessmentReason(t, fixtureRevision); got != "" {
		t.Fatalf("assessment reason = %q, want none: the assess has not happened", got)
	}
	rf := reviewWith(t, f)

	result, err := rf.review.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Unreconciled != 1 || result.Reviewed != 0 || result.NoDecision != 0 || result.Failed != 0 {
		t.Fatalf("result = %+v, want the case skipped as unreconciled and nothing else", result)
	}
	if got := rf.verdictCount(t); got != 0 {
		t.Fatalf("verdict documents = %d, want 0: there is no case state to compare the disposition against", got)
	}
	if got := rf.reviewLinkCount(t); got != 0 {
		t.Fatalf("review index rows = %d, want 0, so the case is reviewed once it is closed", got)
	}
	if got := len(rf.model.ReviewInputs); got != 0 {
		t.Fatalf("model calls = %d, want none", got)
	}

	if _, err := f.driver.Tick(context.Background(), f.missionID); err != nil {
		t.Fatal(err)
	}
	if got := f.caseState(t, fixtureRevision); got != string(workflowcase.Blocked) {
		t.Fatalf("case state after the driver tick = %q, want BLOCKED", got)
	}
	after, err := rf.review.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Reviewed != 1 || after.Skipped != 0 {
		t.Fatalf("result = %+v, want the case reviewed once the driver closed it", after)
	}
	if !rf.lastVerdict(t).Structural.StateMatchesDisposition {
		t.Fatalf("a correctly closed case was reported as %+v", rf.lastVerdict(t).Structural)
	}
}

// A ready-to-plan case is ACTIVE with nothing assessed too, and that is its
// settled state rather than a window: the disposition closes nothing, so the
// case waits for the planner. Declining that one as unreconciled would leave a
// triaged issue unreviewed for as long as the planner takes, and every
// ready-to-plan case on the branch would stop being reviewed at all.
func TestReviewerStillReviewsAnActiveCaseWhoseDecisionClosesNothing(t *testing.T) {
	rf := newReviewerFixture(t)
	rf.model.ScriptedReview = []bool{true}
	if got := rf.driver.caseState(t, fixtureRevision); got != string(workflowcase.Active) {
		t.Fatalf("case state = %q, want ACTIVE", got)
	}
	if got := rf.driver.latestAssessmentReason(t, fixtureRevision); got != "" {
		t.Fatalf("assessment reason = %q, want none: ready-to-plan closes nothing", got)
	}

	result, err := rf.review.Tick(context.Background(), rf.driver.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reviewed != 1 || result.Unreconciled != 0 || result.Skipped != 0 {
		t.Fatalf("result = %+v, want the case reviewed: its state is settled, not a window", result)
	}
}

// reviewWith builds the reviewer over a driver fixture the way the review tests
// do, for a fixture that had to be shaped by hand.
func reviewWith(t *testing.T, f *driverFixture) *reviewerFixture {
	t.Helper()
	model := fakemodel.New()
	model.ScriptedReview = []bool{true}
	return &reviewerFixture{
		driver: f, model: model,
		review: ghtriage.NewReviewer(f.cases, f.evidenceStore,
			ghtriage.NewReviewIndex(f.store, f.clock), model, f.clock),
	}
}

// captureReviewerStderr points os.Stderr at a pipe for one tick. The reviewer
// prints to the process's stderr rather than an injected writer, so this swaps
// the process-wide stream the way the subcommand tests do; no test in this
// package calls t.Parallel, and the original is restored through t.Cleanup.
func captureReviewerStderr(t *testing.T) func() string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = writer
	t.Cleanup(func() {
		os.Stderr = original
		_ = reader.Close()
		_ = writer.Close()
	})
	var captured bytes.Buffer
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(&captured, reader)
	}()
	return func() string {
		_ = writer.Close()
		<-drained
		return captured.String()
	}
}
