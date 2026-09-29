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
	review *ghtriage.Reviewer
}

func newReviewerFixture(t *testing.T) *reviewerFixture {
	t.Helper()
	f := newDriverFixture(t)
	f.registerRevision(t, fixtureRevision)
	// A ready-to-plan decision leaves the case ACTIVE, which is the state the
	// reviewer has to be able to read a decision from and the one it can then
	// see superseded. The driver must run once so the task is accepted and the
	// accepted index is written; the reviewer reads state the driver produced.
	f.completeTaskWithDecision(t, fixtureRevision, readyToPlanDecision(42, fixtureRevision))
	if _, err := f.driver.Tick(context.Background(), f.missionID); err != nil {
		t.Fatal(err)
	}
	model := fakemodel.New()
	return &reviewerFixture{
		driver: f,
		model:  model,
		review: ghtriage.NewReviewer(f.cases, f.evidenceStore, ghtriage.NewReviewIndex(f.store, f.clock), model, f.clock),
	}
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

// lastVerdict reads the verdict of the most recently observed case state. The
// clock does not move between the two reviews a supersession earns, so created_at
// alone is not an order: the fingerprint is what tells a review of ACTIVE|none
// from a review of BLOCKED|assessment-...
func (rf *reviewerFixture) lastVerdict(t *testing.T) ghtriage.ReviewVerdict {
	t.Helper()
	var id string
	if err := rf.driver.store.DB().QueryRowContext(rf.driver.ctx,
		`SELECT verdict_evidence_id FROM github_issue_triage_reviews
		 ORDER BY created_at DESC, state_fingerprint DESC, decision_evidence_id DESC,
		          verdict_evidence_id DESC LIMIT 1`).Scan(&id); err != nil {
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
	first := rf.model.ReviewCallCount()
	if first != 1 {
		t.Fatalf("model calls on the first tick = %d, want 1", first)
	}
	if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	if got := rf.model.ReviewCallCount(); got != first {
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
	if _, err := rf.review.Tick(context.Background(), rf.driver.missionID); err != nil {
		t.Fatal(err)
	}
	if got := rf.model.ReviewCallCount(); got != 2 {
		t.Fatalf("model calls = %d, want a second call after the fingerprint changed", got)
	}
	if got := rf.verdictCount(t); got != 2 {
		t.Fatalf("verdict documents = %d, want 2", got)
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
	if rf.model.ReviewCallCount() != 1 {
		t.Fatalf("model calls = %d, want only the reviewer's own question", rf.model.ReviewCallCount())
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
