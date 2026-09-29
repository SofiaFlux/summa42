package ghtriage_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
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
	clock         clock.Clock
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

// registerRevision materializes the case and its triage Task for one revision.
func (f *driverFixture) registerRevision(t *testing.T, revision string) {
	t.Helper()
	snapshot := ghtriage.Snapshot{
		Repo: "o/r", Issue: 42, Title: "Crash on save", Body: "it crashes",
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
			MissionID: f.missionID, Source: ghtriage.SourceGitHub, ObjectID: fixtureIssue,
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
			Objective:            "Triage " + fixtureIssue,
			AcceptanceCriteria:   []string{"triage decision recorded for " + revision},
			RequiredCapabilities: []string{ghtriage.RequiredCapability},
			RequiredEnforcement:  domain.EnforcementEnforced,
			AuthorityCeiling:     []string{ghtriage.RequiredCapability},
			ResourceEnvelopeID:   f.envelope,
			IdempotencyKey:       "triage-" + revision,
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
	object, err := f.evidenceStore.Put(f.ctx, strings.NewReader(string(raw)), evidence.Metadata{
		MediaType: ghtriage.DecisionMediaType, Kind: ghtriage.KindDecision,
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
	object, _, found, err := f.evidenceStore.FindLatestByKind(f.ctx, "github.issue.snapshot", &struct {
		Repo      string `json:"repo"`
		UpdatedAt string `json:"updatedAt"`
	}{UpdatedAt: revision}, 10)
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
func (f *driverFixture) simulateRestartAfterAcceptance(t *testing.T, revision string) {
	t.Helper()
	if _, ok := f.casesByRev[revision]; !ok {
		f.registerRevision(t, revision)
	}
	task := f.tasksByRev[revision]
	createdCase := f.casesByRev[revision]
	attempt, err := f.execSvc.StartAttempt(f.ctx, task.ID, ghtriage.ExecutorKind, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	decision := notActionableDecision(revision)
	decision.SnapshotEvidenceID = f.snapshotEvidenceID(t, revision)
	raw, err := decision.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	object, err := f.evidenceStore.Put(f.ctx, strings.NewReader(string(raw)), evidence.Metadata{
		MediaType: ghtriage.DecisionMediaType, Kind: ghtriage.KindDecision,
	})
	if err != nil {
		t.Fatal(err)
	}
	acceptedRaw, err := json.Marshal(map[string]any{
		"schema": "github.issue.triage.accepted.v1", "case_id": createdCase.ID,
		"task_id": createdCase.CurrentWorkID, "revision": revision, "decision_evidence_id": object.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.evidenceStore.Put(f.ctx, strings.NewReader(string(acceptedRaw)), evidence.Metadata{
		MediaType: ghtriage.DecisionMediaType, Kind: ghtriage.KindAccepted,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.verifSvc.CompleteAttempt(f.ctx, attempt.ID, verification.CompletionManifest{
		EvidenceIDs: []domain.ID{object.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.verifSvc.AcceptTask(f.ctx, task.ID, verification.AcceptanceRequest{
		VerifierID: ghtriage.DriverVerifierID, VerifierType: ghtriage.DriverVerifierType,
		CriteriaMet: true, EvidenceIDs: []domain.ID{object.ID},
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

func (f *driverFixture) hasEvidenceKind(t *testing.T, kind string) bool {
	t.Helper()
	var n int
	if err := f.store.DB().QueryRowContext(f.ctx,
		`SELECT count(*) FROM evidence_objects WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func notActionableDecision(revision string) ghtriage.Decision {
	return ghtriage.Decision{
		Schema: ghtriage.DecisionSchema, Repository: "o/r", Issue: 42, Revision: revision,
		TriageRulesVersion: ghtriage.TriageRulesVersion, DispositionRulesVersion: ghtriage.DispositionRulesVersion,
		Stage1: ghtriage.Stage1Result{Triage: ghtriage.TriageBug, Signals: []string{"has-repro"}},
		Stage2: &ghtriage.Stage2Output{IsActionable: false, Scope: ghtriage.ScopeSmall, Rationale: "no defect described"},
		Stage3: &ghtriage.Stage3Result{Disposition: ghtriage.DispositionNotActionable, Rule: "not-actionable"},
	}
}

func readyToPlanDecision(revision string) ghtriage.Decision {
	return ghtriage.Decision{
		Schema: ghtriage.DecisionSchema, Repository: "o/r", Issue: 42, Revision: revision,
		TriageRulesVersion: ghtriage.TriageRulesVersion, DispositionRulesVersion: ghtriage.DispositionRulesVersion,
		Stage1: ghtriage.Stage1Result{Triage: ghtriage.TriageBug, Signals: []string{"has-repro"}},
		Stage2: &ghtriage.Stage2Output{IsActionable: true, Scope: ghtriage.ScopeSmall, Rationale: "enough detail"},
		Stage3: &ghtriage.Stage3Result{Disposition: ghtriage.DispositionReadyToPlan, Rule: "actionable-without-repro-small-or-medium"},
	}
}

func TestDriverAcceptsThenBlocksNotActionable(t *testing.T) {
	f := newDriverFixture(t)
	f.completeTaskWithDecision(t, fixtureRevision, notActionableDecision(fixtureRevision))

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
	f.completeTaskWithDecision(t, fixtureRevision, readyToPlanDecision(fixtureRevision))

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
	f.completeTaskWithDecision(t, fixtureRevision, notActionableDecision(fixtureRevision))

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
	f.completeTaskWithDecision(t, fixtureRevision, readyToPlanDecision(fixtureRevision))

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

func TestDriverComparesRevisionsAsTimesNotStrings(t *testing.T) {
	older, newer := "2026-09-29T09:00:00Z", "2026-10-01T09:00:00Z"
	f := newDriverFixture(t)
	f.registerRevision(t, older)
	f.registerRevision(t, newer)
	f.completeTaskWithDecision(t, newer, readyToPlanDecision(newer))

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
	f.simulateRestartAfterAcceptance(t, fixtureRevision)

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

// The restart path reads back the very record recordAccepted wrote, so the
// recorded task ID has to be the identity findAccepted matches on: the case's
// work ID, which is the task's idempotency key. A driver that recorded the
// task row's own ID instead would satisfy a fixture that writes the same value
// and still never recover, so this pins the writer to the reader's identity.
func TestDriverWritesTheAcceptedIndexUnderTheIdentityItReadsItBy(t *testing.T) {
	f := newDriverFixture(t)
	f.completeTaskWithDecision(t, fixtureRevision, readyToPlanDecision(fixtureRevision))

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
