package ghtriage_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/ghissue"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/ghtriage/fakemodel"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

// Intake and the triage executor are two packages writing one contract, and
// nothing binds them: intake owns the payload keys, the snapshot kind and the
// object id, and this package decodes four keys, reads that kind and parses that
// id. Every test on either side of the boundary is green when the two disagree -
// a rename in intake and in its own test is a single commit, and it once left
// this package decoding a payload no real task can carry, so every triage
// attempt was rejected at the decoder with both suites passing. The three tests
// below are the fence: each builds the artifact intake really produces and hands
// it to the code that has to read it.

// intakeIssue is the issue the intake fixtures parse, at the revision every
// driver and reviewer fixture in this package uses.
func intakeIssue(t *testing.T) ghissue.Issue {
	t.Helper()
	updated, err := time.Parse(time.RFC3339, fixtureRevision)
	if err != nil {
		t.Fatal(err)
	}
	return ghissue.Issue{
		Repository: "o/r", Number: 42, Title: "Crash on save", Body: "it crashes",
		State: "open", Author: "maintainer", Labels: []string{"duplicate-of:41"},
		URL: "https://github.com/o/r/issues/42", Triage: ghissue.TriageBug, UpdatedAt: updated,
	}
}

// TestExecutorDecodesThePayloadIntakeBuilds is the payload contract. A
// hand-written map of the same keys would agree with the decoder only because
// one person wrote both, which is how a rename on either side of the boundary
// passed unnoticed; the payload here is the one intake's own builder produces.
func TestExecutorDecodesThePayloadIntakeBuilds(t *testing.T) {
	issue := intakeIssue(t)
	exec, _, evidenceStore := newExecutorFixture(t)
	snapshotID := putIntakeSnapshot(t, evidenceStore, issue)
	payload, err := ghissue.TriageTaskPayload(issue, string(snapshotID))
	if err != nil {
		t.Fatal(err)
	}

	result, err := exec.Start(context.Background(), intakeEnvelope(payload))
	if err != nil {
		t.Fatalf("the payload intake writes was rejected: %v", err)
	}
	if len(result.Evidence) != 1 {
		t.Fatalf("evidence = %d, want exactly one decision", len(result.Evidence))
	}
	var decision ghtriage.Decision
	if err := json.Unmarshal([]byte(result.Evidence[0].Content), &decision); err != nil {
		t.Fatal(err)
	}
	// Every field the payload carries and the decision echoes, read off the
	// document the executor stored rather than off the payload: a key the
	// executor stopped reading leaves one of them at its zero value, which is
	// what the decoder's own diagnostics do not reach.
	if decision.Repository != issue.Repository || decision.Issue != issue.Number ||
		decision.Revision != issue.RevisionID() || decision.SnapshotEvidenceID != string(snapshotID) {
		t.Fatalf("decision = %s#%d at %s citing %s, want the issue and snapshot the payload named",
			decision.Repository, decision.Issue, decision.Revision, decision.SnapshotEvidenceID)
	}
}

// TestExecutorReadsTheSnapshotKindIntakeStores is the same contract on the other
// side of the payload: the payload cites an object, and the executor reads it
// only if it is the kind intake stored it under. A kind renamed on the intake
// side alone failed every attempt loudly, which is the better of the two
// failures and still no reason to accept it.
func TestExecutorReadsTheSnapshotKindIntakeStores(t *testing.T) {
	issue := intakeIssue(t)
	exec, _, evidenceStore := newExecutorFixture(t)
	canonical, err := ghissue.CanonicalSnapshot(issue)
	if err != nil {
		t.Fatal(err)
	}
	put := func(kind string) domain.ID {
		t.Helper()
		object, err := evidenceStore.Put(context.Background(), strings.NewReader(string(canonical)),
			evidence.Metadata{MediaType: "application/json", Kind: kind})
		if err != nil {
			t.Fatal(err)
		}
		return object.ID
	}
	payload, err := ghissue.TriageTaskPayload(issue, string(put(ghissue.SnapshotKind)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Start(context.Background(), intakeEnvelope(payload)); err != nil {
		t.Fatalf("a snapshot stored under the kind intake stores it under was rejected: %v", err)
	}
	// And the other direction, so a reader that accepted any object of any kind
	// could not pass: the same bytes stored under another kind are refused.
	renamed, err := ghissue.TriageTaskPayload(issue, string(put("github.issue.other")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Start(context.Background(), intakeEnvelope(renamed)); err == nil {
		t.Fatal("a snapshot stored under another kind was read as an issue snapshot")
	}
}

// TestReviewerResolvesTheObjectIDIntakeWrites is the third spelling. The
// reviewer reads a case's issue out of its object id to decide whether a
// decision is about the issue the case was registered for, and a parser written
// on this side of the boundary is a second copy of a format intake owns. A case
// registered under the object id intake really writes has to come back clean
// through the one check that reads it.
func TestReviewerResolvesTheObjectIDIntakeWrites(t *testing.T) {
	issue := intakeIssue(t)
	object := issue.ObjectID()
	// The precondition, stated rather than assumed: this is the qualified form
	// intake writes, which is not the bare repo#number the rest of this package's
	// fixtures use.
	if !strings.HasPrefix(object, "github:") {
		t.Fatalf("fixture object id = %q, want the source-qualified form intake writes", object)
	}
	f := newDriverFixture(t)
	f.registerIssueRevision(t, object, fixtureRevision)
	f.completeTaskWithDecision(t, fixtureRevision, notActionableDecision(42, fixtureRevision))
	if _, err := f.driver.Tick(context.Background(), f.missionID); err != nil {
		t.Fatal(err)
	}
	if got := f.caseState(t, fixtureRevision); got != string(workflowcase.Blocked) {
		t.Fatalf("case state = %q, want BLOCKED", got)
	}
	model := fakemodel.New()
	model.ScriptedReview = []bool{true}
	rf := &reviewerFixture{
		driver: f, model: model,
		review: ghtriage.NewReviewer(f.cases, f.evidenceStore,
			ghtriage.NewReviewIndex(f.store, f.clock), model, f.clock),
	}

	result, err := rf.review.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	// A reader that could not resolve the object id does not review the case at
	// all: the observation is proved to be a snapshot of the issue the case names
	// before anything is derived from it, so an unreadable id is a failed case.
	if result.Reviewed != 1 || result.Failed != 0 {
		t.Fatalf("result = %+v, want the case reviewed: an object id it could not read fails the case", result)
	}
	if !rf.lastVerdict(t).Structural.ClassificationAgreesWithIntake {
		// The check that reads the object id, and the only one that does. A
		// parser disagreeing with the writer reports a correct decision as
		// disagreeing with intake, which is a false accusation in the corpus
		// meant to be ground truth.
		t.Fatalf("a case registered under the object id intake writes was reported as %+v",
			rf.lastVerdict(t).Structural)
	}
}

// putIntakeSnapshot stores the canonical snapshot intake writes for an issue,
// under intake's own kind.
func putIntakeSnapshot(t *testing.T, store *evidence.Store, issue ghissue.Issue) domain.ID {
	t.Helper()
	canonical, err := ghissue.CanonicalSnapshot(issue)
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.Put(context.Background(), strings.NewReader(string(canonical)),
		evidence.Metadata{MediaType: "application/json", Kind: ghissue.SnapshotKind})
	if err != nil {
		t.Fatal(err)
	}
	return object.ID
}

func intakeEnvelope(payload json.RawMessage) executors.AttemptEnvelope {
	return executors.AttemptEnvelope{
		TaskID: domain.NewID("task"), AttemptID: domain.NewID("attempt"),
		PayloadJSON:        payload,
		AcceptanceCriteria: []string{"triage decision recorded for " + fixtureRevision},
	}
}
