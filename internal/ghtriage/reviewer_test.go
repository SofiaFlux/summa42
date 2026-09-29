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
func TestReviewerReportsAStage1DecisionThatStillCarriesStage2(t *testing.T) {
	rf := newReviewerFixture(t)
	rf.model.ScriptedReview = []bool{true}
	rf.rewriteDecision(t, func(d *ghtriage.Decision) {
		d.Stage1.Disposition = ghtriage.DispositionDuplicate
		d.Stage1.Rule = "explicit-duplicate-label"
	})

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

func TestReviewerRecordsTheDecisionAndTheStateItObserved(t *testing.T) {
	rf := newReviewerFixture(t)
	rf.model.ScriptedReview = []bool{true}

	if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	verdict := rf.lastVerdict(t)
	if verdict.DecisionEvidenceID == "" {
		t.Fatal("the verdict does not name the decision it reviewed")
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
