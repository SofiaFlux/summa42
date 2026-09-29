package ghtriage_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/runmanifest"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/teb"
	"github.com/SofiaFlux/summa42/internal/testutil"
	"github.com/SofiaFlux/summa42/internal/verification"
	"github.com/SofiaFlux/summa42/internal/workflow"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

const (
	fixtureIssue    = "o/r#42"
	fixtureRevision = "2026-09-28T10:00:00Z"
)

type driverFixture struct {
	ctx           context.Context
	store         *state.Store
	clock         *testutil.Clock
	cases         *workflowcase.Service
	execSvc       *execution.Service
	verifSvc      *verification.Service
	manifests     *runmanifest.Service
	evidenceStore *evidence.Store
	driver        *ghtriage.Driver
	missionID     domain.ID
	envelope      domain.ID
	casesByRev    map[string]workflowcase.Case
	tasksByRev    map[string]domain.Task
}

func newDriverFixture(t *testing.T) *driverFixture {
	t.Helper()
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC))
	purposes := purpose.New(store, clk)
	manifests := runmanifest.New(store, runmanifest.StaticContext{TEBProfile: teb.EnforcedOfflineProfile()})
	execSvc := execution.New(store, clk, purposes, manifests)
	verifSvc := verification.New(store, clk, execSvc)
	evidenceStore, err := evidence.New(store, t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	cases := workflowcase.New(store, clk, purposes)
	mission, err := purposes.CreateMission(ctx, "triage issues")
	if err != nil {
		t.Fatal(err)
	}
	envelope := domain.NewID("envelope")
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, ?, ?)`,
		envelope, 100, clk.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	return &driverFixture{
		ctx: ctx, store: store, clock: clk, cases: cases, execSvc: execSvc, verifSvc: verifSvc,
		manifests: manifests, evidenceStore: evidenceStore,
		driver:    ghtriage.NewDriver(cases, execSvc, verifSvc, manifests, evidenceStore, clk),
		missionID: mission, envelope: envelope,
		casesByRev: map[string]workflowcase.Case{},
		tasksByRev: map[string]domain.Task{},
	}
}

// issueOf reads the issue number out of a GitHub object ID, the shape the
// intake writes: one object per issue, named repo#number.
func issueOf(t *testing.T, object string) int64 {
	t.Helper()
	issue, err := strconv.ParseInt(strings.TrimPrefix(object, "o/r#"), 10, 64)
	if err != nil {
		t.Fatalf("object %q carries no issue number: %v", object, err)
	}
	return issue
}

// registerRevision materializes the case and its triage Task for one revision of
// the fixture issue.
func (f *driverFixture) registerRevision(t *testing.T, revision string) {
	t.Helper()
	f.registerIssueRevision(t, fixtureIssue, revision)
}

func (f *driverFixture) registerIssueRevision(t *testing.T, object, revision string) {
	t.Helper()
	issue := issueOf(t, object)
	snapshot := ghtriage.Snapshot{
		Repo: "o/r", Issue: issue, Title: "Crash on save", Body: "it crashes",
		Author: "maintainer", Labels: []string{"bug"}, Triage: "bug", UpdatedAt: revision,
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshotObject, err := f.evidenceStore.Put(f.ctx, strings.NewReader(string(raw)), evidence.Metadata{
		MediaType: "application/json", Kind: "github.issue.snapshot",
	})
	if err != nil {
		t.Fatal(err)
	}
	createdCase, task, err := f.cases.EnsureAndMaterialize(f.ctx, f.execSvc,
		workflowcase.Observation{
			MissionID: f.missionID, Source: ghtriage.SourceGitHub, ObjectID: object,
			RevisionID: revision, EvidenceID: string(snapshotObject.ID),
			FirstWork: workflow.WorkProposal{
				Kind:                 ghtriage.TaskClass,
				RequiredCapabilities: []string{ghtriage.RequiredCapability},
				AuthorityCeiling:     []string{ghtriage.RequiredCapability},
				ProposedActions:      []string{"github.issue.read"},
			},
			Grant:           workflow.Grant{Capabilities: []string{ghtriage.RequiredCapability}, Actions: []string{"github.issue.read"}},
			MaxSteps:        3,
			RemainingBudget: 10,
		},
		execution.TaskRequest{
			Purpose:              domain.PurposeRef{Kind: domain.PurposeMission, ID: f.missionID},
			TaskClass:            ghtriage.TaskClass,
			Objective:            "Triage " + object,
			AcceptanceCriteria:   []string{"triage decision recorded for " + revision},
			RequiredCapabilities: []string{ghtriage.RequiredCapability},
			RequiredEnforcement:  domain.EnforcementEnforced,
			AuthorityCeiling:     []string{ghtriage.RequiredCapability},
			ResourceEnvelopeID:   f.envelope,
			IdempotencyKey:       "triage-" + object + "-" + revision,
		})
	if err != nil {
		t.Fatal(err)
	}
	f.casesByRev[revision] = createdCase
	f.tasksByRev[revision] = task
}

// completeTaskWithDecision drives the task to AWAITING_VERIFICATION exactly as
// the worker does: one attempt, the decision written as output evidence, then
// CompleteAttempt.
func (f *driverFixture) completeTaskWithDecision(t *testing.T, revision string, decision ghtriage.Decision) {
	t.Helper()
	if _, ok := f.casesByRev[revision]; !ok {
		f.registerRevision(t, revision)
	}
	task := f.tasksByRev[revision]
	attempt, err := f.execSvc.StartAttempt(f.ctx, task.ID, ghtriage.ExecutorKind, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	decision.SnapshotEvidenceID = f.snapshotEvidenceID(t, revision)
	raw, err := decision.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	// The worker stores every executor blob as text/plain whatever the executor
	// meant by it, so a decision reaches the store as text/plain and is found by
	// its kind. A fixture that wrote the decision's own media type let a driver
	// selecting on media type pass against a shape production never produces.
	object, err := f.evidenceStore.Put(f.ctx, strings.NewReader(string(raw)), evidence.Metadata{
		MediaType: "text/plain", Kind: ghtriage.KindDecision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.verifSvc.CompleteAttempt(f.ctx, attempt.ID, verification.CompletionManifest{
		EvidenceIDs: []domain.ID{object.ID},
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *driverFixture) snapshotEvidenceID(t *testing.T, revision string) string {
	t.Helper()
	object, _, found, err := f.evidenceStore.FindByKind(f.ctx, "github.issue.snapshot", 10,
		func(_ evidence.EvidenceObject, raw []byte) bool {
			var snapshot struct {
				UpdatedAt string `json:"updatedAt"`
			}
			if err := json.Unmarshal(raw, &snapshot); err != nil {
				return false
			}
			return snapshot.UpdatedAt == revision
		})
	if err != nil || !found {
		t.Fatalf("snapshot for revision %s: found=%v err=%v", revision, found, err)
	}
	return string(object.ID)
}

// failTaskTwice drives the task to TaskBlocked, which FailAttempt does on the
// second failure carrying the same signature.
func (f *driverFixture) failTaskTwice(t *testing.T, revision string) {
	t.Helper()
	if _, ok := f.casesByRev[revision]; !ok {
		f.registerRevision(t, revision)
	}
	task := f.tasksByRev[revision]
	for i := 0; i < 2; i++ {
		attempt, err := f.execSvc.StartAttempt(f.ctx, task.ID, ghtriage.ExecutorKind, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.execSvc.FailAttempt(f.ctx, attempt.ID, domain.FailureExecution, "ghtriage:stage2", nil); err != nil {
			t.Fatal(err)
		}
	}
}

// simulateRestartAfterAcceptance accepts the task itself and stops, leaving the
// case ACTIVE. That is the crash window the driver must recover from, and it is
// why the driver keeps its own accepted index.
func (f *driverFixture) simulateRestartAfterAcceptance(t *testing.T, object, revision string) {
	t.Helper()
	f.crashAfterAcceptance(t, object, revision, true)
}

// crashAfterAcceptance accepts the task and stops, leaving the case ACTIVE.
// writeIndex=false models the state the index exists to prevent: a task already
// accepted with nothing written that a later tick could replay.
func (f *driverFixture) crashAfterAcceptance(t *testing.T, object, revision string, writeIndex bool) {
	t.Helper()
	if _, ok := f.casesByRev[revision]; !ok {
		f.registerIssueRevision(t, object, revision)
	}
	task := f.tasksByRev[revision]
	createdCase := f.casesByRev[revision]
	attempt, err := f.execSvc.StartAttempt(f.ctx, task.ID, ghtriage.ExecutorKind, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	decision := notActionableDecision(issueOf(t, object), revision)
	decision.SnapshotEvidenceID = f.snapshotEvidenceID(t, revision)
	raw, err := decision.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	// The worker stores every executor blob as text/plain whatever the executor
	// meant by it, so a decision reaches the store as text/plain and is found by
	// its kind. A fixture that wrote the decision's own media type let a driver
	// selecting on media type pass against a shape production never produces.
	decisionObject, err := f.evidenceStore.Put(f.ctx, strings.NewReader(string(raw)), evidence.Metadata{
		MediaType: "text/plain", Kind: ghtriage.KindDecision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if writeIndex {
		acceptedRaw, err := json.Marshal(map[string]any{
			"schema": "github.issue.triage.accepted.v1", "case_id": createdCase.ID,
			"task_id": createdCase.CurrentWorkID, "revision": revision, "decision_evidence_id": decisionObject.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.evidenceStore.Put(f.ctx, strings.NewReader(string(acceptedRaw)), evidence.Metadata{
			MediaType: ghtriage.DecisionMediaType, Kind: ghtriage.KindAccepted,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.verifSvc.CompleteAttempt(f.ctx, attempt.ID, verification.CompletionManifest{
		EvidenceIDs: []domain.ID{decisionObject.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.verifSvc.AcceptTask(f.ctx, task.ID, verification.AcceptanceRequest{
		VerifierID: ghtriage.DriverVerifierID, VerifierType: ghtriage.DriverVerifierType,
		CriteriaMet: true, EvidenceIDs: []domain.ID{decisionObject.ID},
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *driverFixture) caseState(t *testing.T, revision string) string {
	t.Helper()
	createdCase, err := f.cases.Get(f.ctx, f.casesByRev[revision].ID)
	if err != nil {
		t.Fatal(err)
	}
	return string(createdCase.State)
}

func (f *driverFixture) taskState(t *testing.T, revision string) string {
	t.Helper()
	task, err := f.execSvc.Task(f.ctx, f.tasksByRev[revision].ID)
	if err != nil {
		t.Fatal(err)
	}
	return string(task.State)
}

func (f *driverFixture) latestAssessmentReason(t *testing.T, revision string) string {
	t.Helper()
	records, err := f.cases.ListAssessments(f.ctx, f.casesByRev[revision].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		return ""
	}
	var result workflowcase.AssessmentResult
	if err := json.Unmarshal([]byte(records[len(records)-1].ResultJSON), &result); err != nil {
		t.Fatal(err)
	}
	return result.Decision.Reason
}

func (f *driverFixture) observationEvidenceID(t *testing.T, revision string) string {
	t.Helper()
	return f.casesByRev[revision].ObservationEvidenceID
}

// challengeEvidenceIDs reads the evidence the challenge was recorded against,
// which is the column that must never hold a blank ID.
func (f *driverFixture) challengeEvidenceIDs(t *testing.T, revision string) []string {
	t.Helper()
	var raw string
	if err := f.store.DB().QueryRowContext(f.ctx,
		`SELECT evidence_ids_json FROM task_challenges WHERE task_id = ? ORDER BY created_at DESC, challenge_id DESC LIMIT 1`,
		f.tasksByRev[revision].ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		t.Fatal(err)
	}
	return ids
}

func (f *driverFixture) hasEvidenceKind(t *testing.T, kind string) bool {
	t.Helper()
	var n int
	if err := f.store.DB().QueryRowContext(f.ctx,
		`SELECT count(*) FROM evidence_objects WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// putAcceptedIndex stores a record of the accepted index kind naming a case that
// is not registered here, which is what every other accepted triage in a store
// leaves behind. The clock is advanced first so the record sorts ahead of
// anything written before it, whatever the evidence ID tiebreak does.
func (f *driverFixture) putAcceptedIndexForAnotherCase(t *testing.T, seq int) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"schema": "github.issue.triage.accepted.v1", "case_id": domain.ID("case_another_" + strconv.Itoa(seq)),
		"task_id":              domain.ID("work_another_" + strconv.Itoa(seq)),
		"revision":             "2026-09-28T10:00:00Z",
		"decision_evidence_id": domain.ID("evidence_another_" + strconv.Itoa(seq)),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Second)
	if _, err := f.evidenceStore.Put(f.ctx, strings.NewReader(string(raw)), evidence.Metadata{
		MediaType: ghtriage.DecisionMediaType, Kind: ghtriage.KindAccepted,
	}); err != nil {
		t.Fatal(err)
	}
}

func notActionableDecision(issue int64, revision string) ghtriage.Decision {
	return ghtriage.Decision{
		Schema: ghtriage.DecisionSchema, Repository: "o/r", Issue: issue, Revision: revision,
		TriageRulesVersion: ghtriage.TriageRulesVersion, DispositionRulesVersion: ghtriage.DispositionRulesVersion,
		Stage1: ghtriage.Stage1Result{Triage: ghtriage.TriageBug, Signals: []string{"has-repro"}},
		Stage2: &ghtriage.Stage2Output{IsActionable: false, Scope: ghtriage.ScopeSmall, Rationale: "no defect described"},
		Stage3: &ghtriage.Stage3Result{Disposition: ghtriage.DispositionNotActionable, Rule: "not-actionable"},
	}
}

func readyToPlanDecision(issue int64, revision string) ghtriage.Decision {
	return ghtriage.Decision{
		Schema: ghtriage.DecisionSchema, Repository: "o/r", Issue: issue, Revision: revision,
		TriageRulesVersion: ghtriage.TriageRulesVersion, DispositionRulesVersion: ghtriage.DispositionRulesVersion,
		Stage1: ghtriage.Stage1Result{Triage: ghtriage.TriageBug, Signals: []string{"has-repro"}},
		Stage2: &ghtriage.Stage2Output{IsActionable: true, Scope: ghtriage.ScopeSmall, Rationale: "enough detail"},
		Stage3: &ghtriage.Stage3Result{Disposition: ghtriage.DispositionReadyToPlan, Rule: "actionable-without-repro-small-or-medium"},
	}
}

func TestDriverAcceptsThenBlocksNotActionable(t *testing.T) {
	f := newDriverFixture(t)
	f.completeTaskWithDecision(t, fixtureRevision, notActionableDecision(42, fixtureRevision))

	result, err := f.driver.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted != 1 || result.Assessed != 1 {
		t.Fatalf("result = %+v, want one acceptance and one assessment", result)
	}
	if got := f.caseState(t, fixtureRevision); got != "BLOCKED" {
		t.Fatalf("case state = %q, want BLOCKED", got)
	}
	if got := f.taskState(t, fixtureRevision); got != "SUCCEEDED" {
		t.Fatalf("task state = %q, want SUCCEEDED", got)
	}
	if got := f.latestAssessmentReason(t, fixtureRevision); got != "not-actionable" {
		t.Fatalf("assessment reason = %q, want not-actionable", got)
	}
}

func TestDriverAcceptsAndLeavesReadyToPlanActive(t *testing.T) {
	f := newDriverFixture(t)
	f.completeTaskWithDecision(t, fixtureRevision, readyToPlanDecision(42, fixtureRevision))

	if _, err := f.driver.Tick(context.Background(), f.missionID); err != nil {
		t.Fatal(err)
	}
	if got := f.caseState(t, fixtureRevision); got != "ACTIVE" {
		t.Fatalf("case state = %q, want ACTIVE", got)
	}
	if got := f.taskState(t, fixtureRevision); got != "SUCCEEDED" {
		t.Fatalf("task state = %q, want SUCCEEDED", got)
	}
}

func TestDriverIsIdempotentAcrossTicks(t *testing.T) {
	f := newDriverFixture(t)
	f.completeTaskWithDecision(t, fixtureRevision, notActionableDecision(42, fixtureRevision))

	first, err := f.driver.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.driver.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Accepted != 1 || first.Assessed != 1 {
		t.Fatalf("first tick = %+v", first)
	}
	if second.Accepted != 0 || second.Assessed != 0 {
		t.Fatalf("second tick = %+v, want no repeated work", second)
	}
}

func TestDriverBlocksAnExhaustedTaskWithFailureEvidence(t *testing.T) {
	f := newDriverFixture(t)
	f.failTaskTwice(t, fixtureRevision)

	result, err := f.driver.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Blocked != 1 {
		t.Fatalf("result = %+v, want one blocked case", result)
	}
	// A blocked case is an outcome the tick performed, not something that
	// failed: Failures is what the command turns into a non-zero exit, and a
	// mission with one exhausted triage task is a mission that worked.
	if len(result.Failures) != 0 {
		t.Fatalf("failures = %v, want none", result.Failures)
	}
	if got := f.caseState(t, fixtureRevision); got != "BLOCKED" {
		t.Fatalf("case state = %q, want BLOCKED", got)
	}
	if got := f.latestAssessmentReason(t, fixtureRevision); got != ghtriage.ReasonTriageFailed {
		t.Fatalf("assessment reason = %q, want %q", got, ghtriage.ReasonTriageFailed)
	}
	if !f.hasEvidenceKind(t, ghtriage.KindFailure) {
		t.Fatal("no failure evidence was written")
	}
}

func TestDriverSupersedesAnOlderRevisionAndChallengesItsPendingTask(t *testing.T) {
	older := "2026-09-28T09:00:00Z"
	f := newDriverFixture(t)
	f.registerRevision(t, older)
	f.completeTaskWithDecision(t, fixtureRevision, readyToPlanDecision(42, fixtureRevision))

	result, err := f.driver.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Superseded != 1 {
		t.Fatalf("result = %+v, want one superseded revision", result)
	}
	if got := f.taskState(t, older); got != "CHALLENGED" {
		t.Fatalf("older task state = %q, want CHALLENGED", got)
	}
	if got := f.caseState(t, older); got != "BLOCKED" {
		t.Fatalf("older case state = %q, want BLOCKED", got)
	}
	if got := f.caseState(t, fixtureRevision); got != "ACTIVE" {
		t.Fatalf("newer case state = %q, want ACTIVE", got)
	}
	if got := f.latestAssessmentReason(t, older); got != "superseded-by:"+fixtureRevision {
		t.Fatalf("assessment reason = %q", got)
	}
}

// The driver has not run for a revision when the next one is registered, so the
// newer revision has no decision and nothing of its own to cite. The snapshot
// intake stored for it is the evidence that exists, and it is what establishes
// that the revision was seen at all. Superseding with a blank evidence ID
// instead challenges the older task and then fails to assess the older case, on
// every tick, forever.
func TestDriverSupersedesWithTheNewerRevisionsSnapshotWhenItHasNoDecision(t *testing.T) {
	older, newer := "2026-09-28T09:00:00Z", "2026-09-28T10:30:00Z"
	f := newDriverFixture(t)
	f.registerRevision(t, older)
	f.registerRevision(t, newer)

	result, err := f.driver.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Failures) != 0 {
		t.Fatalf("failures = %v, want none", result.Failures)
	}
	if result.Superseded != 1 {
		t.Fatalf("result = %+v, want one superseded revision", result)
	}
	if got := f.taskState(t, older); got != "CHALLENGED" {
		t.Fatalf("older task state = %q, want CHALLENGED", got)
	}
	if got := f.caseState(t, older); got != "BLOCKED" {
		t.Fatalf("older case state = %q, want BLOCKED", got)
	}
	if got := f.latestAssessmentReason(t, older); got != "superseded-by:"+newer {
		t.Fatalf("assessment reason = %q, want superseded-by:%s", got, newer)
	}
	wantEvidence := f.observationEvidenceID(t, newer)
	gotEvidence := f.challengeEvidenceIDs(t, older)
	if len(gotEvidence) != 1 || gotEvidence[0] != wantEvidence {
		t.Fatalf("challenge evidence = %v, want the newer revision's snapshot %s",
			gotEvidence, wantEvidence)
	}

	// The older case is closed and the newer one is still waiting for its own
	// triage, so a second tick has nothing to do. It must say so, not repeat the
	// older revision.
	second, err := f.driver.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Failures) != 0 {
		t.Fatalf("second tick failures = %v, want none", second.Failures)
	}
	if second.Superseded != 0 || second.Assessed != 0 || second.Accepted != 0 {
		t.Fatalf("second tick = %+v, want no repeated work", second)
	}
	if got := f.caseState(t, newer); got != "ACTIVE" {
		t.Fatalf("newer case state = %q, want ACTIVE", got)
	}
}

// A revision is a timestamp, and a string comparison of two timestamps is not
// the same order. The pair here is 09:00+05:00, which is 04:00Z, against 04:30Z:
// the older revision sorts LATER as a string and EARLIER as a time, so a driver
// that compared revision_id with < would supersede the wrong case. Two
// identically formatted UTC timestamps could never catch that - see the note on
// parseRevision - so the fixture has to be shaped to disagree.
func TestDriverComparesRevisionsAsTimesNotStrings(t *testing.T) {
	older, newer := "2026-09-28T09:00:00+05:00", "2026-09-28T04:30:00Z"
	if older <= newer {
		t.Fatalf("fixture ranks the older revision last as a string too: %q <= %q", older, newer)
	}
	f := newDriverFixture(t)
	f.registerRevision(t, older)
	f.registerRevision(t, newer)
	f.completeTaskWithDecision(t, newer, readyToPlanDecision(42, newer))

	if _, err := f.driver.Tick(context.Background(), f.missionID); err != nil {
		t.Fatal(err)
	}
	if got := f.caseState(t, newer); got != "ACTIVE" {
		t.Fatalf("newest case state = %q, want ACTIVE", got)
	}
	if got := f.caseState(t, older); got != "BLOCKED" {
		t.Fatalf("older case state = %q, want BLOCKED", got)
	}
}

func TestDriverRecoversWhenTheTaskIsAcceptedButTheCaseIsNot(t *testing.T) {
	f := newDriverFixture(t)
	f.simulateRestartAfterAcceptance(t, fixtureIssue, fixtureRevision)

	result, err := f.driver.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Assessed != 1 {
		t.Fatalf("result = %+v, want the assessment completed on the later tick", result)
	}
	if got := f.caseState(t, fixtureRevision); got != "BLOCKED" {
		t.Fatalf("case state = %q, want BLOCKED", got)
	}
}

// The accepted index has no case column, so recovering it means walking the
// records of its kind and picking the one carrying this case's identity - and
// the record that sorts first is not necessarily this case's. Two issues of one
// mission both crash between accepting their task and assessing their case, and
// the second one's record is written last, so a lookup that stops at the first
// record it can decode returns the other issue's record, matches nothing, and
// reports success: that case keeps a SUCCEEDED task and stays ACTIVE forever.
func TestDriverRecoversEveryCaseWhoseAcceptedIndexIsNotTheNewest(t *testing.T) {
	const (
		otherIssue    = "o/r#43"
		otherRevision = "2026-09-28T11:00:00Z"
	)
	f := newDriverFixture(t)
	f.simulateRestartAfterAcceptance(t, fixtureIssue, fixtureRevision)
	// A later created_at, so the other case's record sorts ahead of this one's
	// no matter which way the tiebreak goes.
	f.clock.Advance(time.Second)
	f.simulateRestartAfterAcceptance(t, otherIssue, otherRevision)

	result, err := f.driver.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Failures) != 0 {
		t.Fatalf("failures = %v, want none", result.Failures)
	}
	if result.Assessed != 2 {
		t.Fatalf("result = %+v, want both cases recovered on the same tick", result)
	}
	if got := f.caseState(t, fixtureRevision); got != "BLOCKED" {
		t.Fatalf("%s case state = %q, want BLOCKED", fixtureIssue, got)
	}
	if got := f.caseState(t, otherRevision); got != "BLOCKED" {
		t.Fatalf("%s case state = %q, want BLOCKED", otherIssue, got)
	}
}

// The accepted index is per case and the store has no case column, so finding
// one is a walk over the records of its kind. The ceiling that walk used to
// carry bounded nothing real: the kind is never pruned, and every acceptance
// writes a record carrying a fresh decision_evidence_id, so no two of them hash
// alike and the store grows by one row per accepted triage for the life of the
// mission. Once it passed the ceiling, a case whose record was no longer among
// the newest N was not found, its SUCCEEDED task was skipped, and the tick
// reported an empty result with no failure at all - a case that can never move
// again, announced as a healthy tick. The record here is the oldest of 121, so
// any ceiling under the count of records of the kind loses it.
func TestDriverRecoversAnAcceptedIndexOlderThanTheScanCeiling(t *testing.T) {
	const newerRecords = 120
	f := newDriverFixture(t)
	f.simulateRestartAfterAcceptance(t, fixtureIssue, fixtureRevision)
	for i := 0; i < newerRecords; i++ {
		f.putAcceptedIndexForAnotherCase(t, i)
	}

	result, err := f.driver.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Failures) != 0 {
		t.Fatalf("failures = %v, want none", result.Failures)
	}
	if result.Assessed != 1 {
		t.Fatalf("result = %+v, want the assessment the crash window left undone", result)
	}
	if got := f.caseState(t, fixtureRevision); got != "BLOCKED" {
		t.Fatalf("case state = %q, want BLOCKED", got)
	}
}

// A task the driver accepted with nothing written that a later tick could replay
// is the one state it cannot recover from: internal/verification has no read
// accessor for an acceptance record, so the index the driver writes is the only
// replay path there is. Skipping the case leaves a SUCCEEDED task on an ACTIVE
// case that no later tick can move, and reports a tick with nothing in it. The
// case has to be named in the tick's failures instead, because that is what
// reaches the operator and the command's exit code.
func TestDriverReportsACaseItCannotReplayBecauseItsIndexIsMissing(t *testing.T) {
	f := newDriverFixture(t)
	f.crashAfterAcceptance(t, fixtureIssue, fixtureRevision, false)

	result, err := f.driver.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Failures) != 1 {
		t.Fatalf("failures = %v, want exactly one naming the case that could not be replayed", result.Failures)
	}
	createdCase := f.casesByRev[fixtureRevision]
	for _, want := range []string{fixtureIssue, string(createdCase.ID), fixtureRevision} {
		if !strings.Contains(result.Failures[0], want) {
			t.Fatalf("failure %q does not name %q", result.Failures[0], want)
		}
	}
	if result.Accepted != 0 || result.Assessed != 0 || result.Blocked != 0 || result.Superseded != 0 {
		t.Fatalf("result = %+v, want every counter zero: the tick moved nothing", result)
	}
	// The case is left exactly as it was: the driver has nothing to replay, so
	// it must not invent a transition. What changed is that the stall is said
	// out loud, and it is said again on every later tick.
	if got := f.caseState(t, fixtureRevision); got != "ACTIVE" {
		t.Fatalf("case state = %q, want ACTIVE", got)
	}
	if got := f.taskState(t, fixtureRevision); got != "SUCCEEDED" {
		t.Fatalf("task state = %q, want SUCCEEDED", got)
	}
	second, err := f.driver.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Failures) != 1 {
		t.Fatalf("second tick failures = %v, want the same stall reported again", second.Failures)
	}
}

// Three revisions of one issue are the case the two-revision tests cannot
// reach: the supersession loop compares every revision against the newest, so
// with R1 < R2 < R3 it is R2 and R1 that are both superseded, in that order, by
// R3. Here R2's triage has completed and R3's has not, which is the state that
// makes the two older revisions take different paths in the same tick: R2's task
// is accepted on its own decision before the supersession reaches it, so it is
// SUCCEEDED and never challenged, while R1's is still pending and is. Both
// cases close as superseded-by R3, and each closure is counted once.
func TestDriverSupersedesTwoOlderRevisionsAgainstOneNewest(t *testing.T) {
	oldest, middle, newest := "2026-09-28T08:00:00Z", "2026-09-28T09:00:00Z", "2026-09-28T11:00:00Z"
	f := newDriverFixture(t)
	f.registerRevision(t, oldest)
	f.registerRevision(t, middle)
	f.registerRevision(t, newest)
	// A ready-to-plan decision, so R2's case is still ACTIVE when the
	// supersession runs. A not-actionable one would have closed it as
	// not-actionable in applyDisposition first, and the supersession would find
	// nothing to do.
	f.completeTaskWithDecision(t, middle, readyToPlanDecision(42, middle))

	result, err := f.driver.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Failures) != 0 {
		t.Fatalf("failures = %v, want none", result.Failures)
	}
	// One acceptance, for R2's own completed triage, and two supersessions for
	// the two cases that closed. A case closed as superseded is not also an
	// assessment and not also a block, so both counters stay zero.
	if result.Accepted != 1 || result.Superseded != 2 || result.Assessed != 0 || result.Blocked != 0 {
		t.Fatalf("result = %+v, want one acceptance and two supersessions", result)
	}

	if got := f.taskState(t, middle); got != "SUCCEEDED" {
		t.Fatalf("R2 task state = %q, want SUCCEEDED: its triage completed, so it was accepted rather than challenged", got)
	}
	if got := f.caseState(t, middle); got != "BLOCKED" {
		t.Fatalf("R2 case state = %q, want BLOCKED", got)
	}
	if got := f.latestAssessmentReason(t, middle); got != "superseded-by:"+newest {
		t.Fatalf("R2 assessment reason = %q, want superseded-by:%s", got, newest)
	}
	if got := f.taskState(t, oldest); got != "CHALLENGED" {
		t.Fatalf("R1 task state = %q, want CHALLENGED", got)
	}
	if got := f.caseState(t, oldest); got != "BLOCKED" {
		t.Fatalf("R1 case state = %q, want BLOCKED", got)
	}
	if got := f.latestAssessmentReason(t, oldest); got != "superseded-by:"+newest {
		t.Fatalf("R1 assessment reason = %q, want superseded-by:%s", got, newest)
	}
	// The newest revision is the one everything was superseded for: untouched.
	if got := f.caseState(t, newest); got != "ACTIVE" {
		t.Fatalf("R3 case state = %q, want ACTIVE", got)
	}
	if got := f.latestAssessmentReason(t, newest); got != "" {
		t.Fatalf("R3 assessment reason = %q, want none", got)
	}

	second, err := f.driver.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Failures) != 0 {
		t.Fatalf("second tick failures = %v, want none", second.Failures)
	}
	if second.Accepted != 0 || second.Assessed != 0 || second.Blocked != 0 || second.Superseded != 0 {
		t.Fatalf("second tick = %+v, want no repeated work", second)
	}
}

// The restart path reads back the very record recordAccepted wrote, so the
// recorded task ID has to be the identity findAccepted matches on: the case's
// work ID, which is the task's idempotency key. A driver that recorded the
// task row's own ID instead would satisfy a fixture that writes the same value
// and still never recover, so this pins the writer to the reader's identity.
func TestDriverWritesTheAcceptedIndexUnderTheIdentityItReadsItBy(t *testing.T) {
	f := newDriverFixture(t)
	f.completeTaskWithDecision(t, fixtureRevision, readyToPlanDecision(42, fixtureRevision))

	if _, err := f.driver.Tick(context.Background(), f.missionID); err != nil {
		t.Fatal(err)
	}
	createdCase := f.casesByRev[fixtureRevision]
	var indexID domain.ID
	if err := f.store.DB().QueryRowContext(f.ctx,
		`SELECT evidence_id FROM evidence_objects WHERE kind = ?`, ghtriage.KindAccepted).Scan(&indexID); err != nil {
		t.Fatal(err)
	}
	_, raw, err := f.evidenceStore.Get(f.ctx, indexID)
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Schema   string    `json:"schema"`
		CaseID   domain.ID `json:"case_id"`
		TaskID   domain.ID `json:"task_id"`
		Revision string    `json:"revision"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record.Schema != "github.issue.triage.accepted.v1" || record.CaseID != createdCase.ID ||
		record.TaskID != createdCase.CurrentWorkID || record.Revision != fixtureRevision {
		t.Fatalf("accepted index = %+v, want case %s work %s revision %s",
			record, createdCase.ID, createdCase.CurrentWorkID, fixtureRevision)
	}

	// The task is SUCCEEDED and the case still ACTIVE, so this tick is the one
	// that has to find the record the previous tick wrote.
	result, err := f.driver.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Failures) != 0 {
		t.Fatalf("second tick failures = %v, want none", result.Failures)
	}
	if result.Accepted != 0 {
		t.Fatalf("second tick accepted %d tasks, want the task already accepted", result.Accepted)
	}
}
