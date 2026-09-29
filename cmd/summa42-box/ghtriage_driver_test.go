package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/capabilities"
	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/runmanifest"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/teb"
	"github.com/SofiaFlux/summa42/internal/verification"
	"github.com/SofiaFlux/summa42/internal/workflow"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

// An object the driver could not advance has to reach the operator as a non-zero
// exit. A permanently stalled case - a supersession with no evidence to record,
// a decision that cannot be decoded - is otherwise a tick that reports success
// on stdout forever, which is the one failure mode a scheduler cannot notice.
func TestGHTriageDriverTickFailureFailsTheCommandOnAPerObjectFailure(t *testing.T) {
	if err := ghtriageDriverTickFailure(ghtriage.DriverResult{Blocked: 1, Superseded: 1}); err != nil {
		t.Fatalf("a tick that only blocked and superseded returned %v, want nil", err)
	}
	err := ghtriageDriverTickFailure(ghtriage.DriverResult{
		Accepted: 1, Failures: []string{"o/r#42: no evidence to record", "o/r#43: read provenance"},
	})
	if err == nil {
		t.Fatal("a tick with per-object failures returned nil")
	}
	for _, want := range []string{"2", "o/r#42: no evidence to record", "o/r#43: read provenance"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

// The driver takes one identity and refuses to start without it, so a
// mistyped invocation cannot open a Collective and report an empty tick as a
// healthy one.
func TestParseGHTriageDriverFlagsTakesAMissionAndNothingElse(t *testing.T) {
	mission, err := parseGHTriageDriverFlags([]string{"--mission", "  mission-1  "})
	if err != nil {
		t.Fatal(err)
	}
	if mission != "mission-1" {
		t.Fatalf("mission = %q, want the trimmed id", mission)
	}
	for name, args := range map[string][]string{
		"no mission":   {"--envelope", "envelope-1"},
		"blank":        {"--mission", "   "},
		"unknown flag": {"--mission", "mission-1", "--model-binary", "/bin/true"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseGHTriageDriverFlags(args); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

// The driver applies a decision the worker already produced, so it calls no
// executor, no capability provider and no operation provider. The composition
// that says so is not decoration: it is the only reason the subcommand can
// start with no model binary, no ADO MCP command and no feedback sink
// configured, which is every one of the things this Box has to open without.
//
// The control matters as much as the assertion. The same configuration handed
// to runtime.Open is refused outright - a field feedback export with no sink is
// an error - and with a sink it opens a box carrying an executor. So the box
// that opens here opened because those three fields were stripped, not because
// the stripped composition was harmless.
func TestOpenGHTriageDriverBoxStripsWritableComposition(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	cfg := summa42runtime.Config{
		StatePath: filepath.Join(root, "state.db"), EvidencePath: filepath.Join(root, "evidence"),
		CollectiveID: "collective-1", OwnerPrincipalID: "owner-1", PolicyEngine: intakePolicy{},
		FieldFeedback: localconfig.FieldFeedbackConfig{
			Enabled: true, Mode: localconfig.FeedbackModeAutoIfAllowed, Provider: "github",
			Destination: "o/r", MaintenanceEnvelopeID: "envelope-1", RequiredEnforcement: domain.EnforcementEnforced,
		},
		CapabilityProviders: []capabilities.Provider{intakeCapabilityProvider{}},
		Executors:           map[string]executors.Executor{"ado-publish": publishCapacityExecutor{}},
	}
	if _, err := summa42runtime.Open(ctx, cfg); err == nil {
		t.Fatal("runtime.Open accepted a field feedback export with no sink, so this configuration proves nothing")
	}
	withSink := cfg
	withSink.FeedbackSink = intakeSink{}
	control, err := summa42runtime.Open(ctx, withSink)
	if err != nil {
		t.Fatalf("control composition: %v", err)
	}
	if _, exported := control.Executors["feedback-emitter"]; !exported {
		t.Fatalf("control box = %v, want the feedback emitter this composition would have carried",
			control.Executors)
	}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}

	box, err := openGHTriageDriverBox(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	if len(box.Executors) != 0 {
		t.Fatalf("executors = %v, want none: the driver calls no executor", box.Executors)
	}
	// The registry's own wording is what tells "this composition holds no
	// capability provider" from "the provider it holds cannot be assessed":
	// with the CapabilityProviders line dropped, this call fails the second way
	// instead of the first, because the provider handed in is a provider.
	if _, err := box.Capabilities.AssessProvider(ctx, "ado"); err == nil ||
		!strings.Contains(err.Error(), "is not registered") {
		t.Fatalf("AssessProvider(ado) = %v, want this composition to hold no capability provider", err)
	}
	// The services the driver does write through are what the composition is
	// for, so they are asserted rather than assumed.
	if box.Store == nil || box.Execution == nil || box.Verification == nil ||
		box.Evidence == nil || box.RunManifests == nil {
		t.Fatalf("box = %+v, want the writable services the driver needs", box)
	}
	if _, err := openGHTriageDriverBox(nil, cfg); err == nil {
		t.Fatal("a nil context opened a Box")
	}
}

// triageDriverFixture is the writable half of what the subcommand opens for
// itself, over the initialized Collective's own state and evidence directories:
// the same mission, case and task the command will find when it runs. Only the
// fields a test needs afterwards are kept; the run manifest service exists here
// because execution needs an AttemptStartRecorder, exactly as the Box supplies
// one.
type triageDriverFixture struct {
	store    *state.Store
	execSvc  *execution.Service
	verifSvc *verification.Service
	evidence *evidence.Store
	cases    *workflowcase.Service
	mission  domain.ID
	envelope domain.ID
}

func newTriageDriverFixture(t *testing.T, config localconfig.Config) *triageDriverFixture {
	t.Helper()
	ctx := context.Background()
	store, err := state.Open(ctx, config.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.System{}
	purposes := purpose.New(store, clk)
	execSvc := execution.New(store, clk, purposes, runmanifest.New(store,
		runmanifest.StaticContext{TEBProfile: teb.EnforcedOfflineProfile()}))
	evidenceStore, err := evidence.New(store, config.EvidencePath, clk)
	if err != nil {
		t.Fatal(err)
	}
	mission, err := purposes.CreateMission(ctx, "triage incoming issues")
	if err != nil {
		t.Fatal(err)
	}
	envelope := domain.NewID("envelope")
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, ?, ?)`,
		envelope, 100, clk.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	return &triageDriverFixture{
		store: store, execSvc: execSvc, verifSvc: verification.New(store, clk, execSvc),
		evidence: evidenceStore, cases: workflowcase.New(store, clk, purposes),
		mission: mission, envelope: envelope,
	}
}

// Close releases the state store, because the subcommand opens the same
// database itself and a test that left this one open would be testing two
// writers on one file.
func (f *triageDriverFixture) Close() {
	_ = f.store.DB().Close()
}

// addCaseWithAnUnreplayableTriage registers one GitHub case and drives its
// triage task to SUCCEEDED without writing the accepted index the driver keeps,
// which is the one state a tick cannot recover: internal/verification has no
// read accessor for an acceptance record, so the index is the only replay path.
func (f *triageDriverFixture) addCaseWithAnUnreplayableTriage(t *testing.T, object, revision string) domain.ID {
	t.Helper()
	ctx := context.Background()
	raw, err := json.Marshal(ghtriage.Snapshot{
		Repo: "o/r", Issue: 42, Title: "Crash on save", Body: "it crashes",
		Author: "maintainer", Labels: []string{"bug"}, Triage: "bug", UpdatedAt: revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.evidence.Put(ctx, strings.NewReader(string(raw)), evidence.Metadata{
		MediaType: "application/json", Kind: "github.issue.snapshot",
	})
	if err != nil {
		t.Fatal(err)
	}
	created, task, err := f.cases.EnsureAndMaterialize(ctx, f.execSvc, workflowcase.Observation{
		MissionID: f.mission, Source: ghtriage.SourceGitHub, ObjectID: object,
		RevisionID: revision, EvidenceID: string(snapshot.ID),
		FirstWork: workflow.WorkProposal{
			Kind: ghtriage.TaskClass, RequiredCapabilities: []string{ghtriage.RequiredCapability},
			AuthorityCeiling: []string{ghtriage.RequiredCapability}, ProposedActions: []string{"github.issue.read"},
		},
		Grant:           workflow.Grant{Capabilities: []string{ghtriage.RequiredCapability}, Actions: []string{"github.issue.read"}},
		MaxSteps:        3,
		RemainingBudget: 10,
	}, execution.TaskRequest{
		Purpose:              domain.PurposeRef{Kind: domain.PurposeMission, ID: f.mission},
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
	attempt, err := f.execSvc.StartAttempt(ctx, task.ID, ghtriage.ExecutorKind, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := json.Marshal(ghtriage.Decision{
		Schema: ghtriage.DecisionSchema, Repository: "o/r", Issue: 42, Revision: revision,
		TriageRulesVersion: ghtriage.TriageRulesVersion, DispositionRulesVersion: ghtriage.DispositionRulesVersion,
		Stage1:             ghtriage.Stage1Result{Triage: ghtriage.TriageBug, Signals: []string{"has-repro"}},
		Stage2:             &ghtriage.Stage2Output{IsActionable: false, Scope: ghtriage.ScopeSmall, Rationale: "no defect described"},
		Stage3:             &ghtriage.Stage3Result{Disposition: ghtriage.DispositionNotActionable, Rule: "not-actionable"},
		SnapshotEvidenceID: string(snapshot.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	// text/plain, because that is what the worker stores every executor blob as.
	stored, err := f.evidence.Put(ctx, strings.NewReader(string(decision)), evidence.Metadata{
		MediaType: "text/plain", Kind: ghtriage.KindDecision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.verifSvc.CompleteAttempt(ctx, attempt.ID, verification.CompletionManifest{
		EvidenceIDs: []domain.ID{stored.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.verifSvc.AcceptTask(ctx, task.ID, verification.AcceptanceRequest{
		VerifierID: ghtriage.DriverVerifierID, VerifierType: ghtriage.DriverVerifierType,
		CriteriaMet: true, EvidenceIDs: []domain.ID{stored.ID},
	}); err != nil {
		t.Fatal(err)
	}
	created, err = f.cases.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	return created.ID
}

func decodeDriverResult(t *testing.T, out string) ghtriage.DriverResult {
	t.Helper()
	var result ghtriage.DriverResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &result); err != nil {
		t.Fatalf("stdout is not a driver result: %v (%q)", err, out)
	}
	return result
}

// The whole chain, over a real Collective: a case the tick cannot replay has to
// come back as a non-zero exit, not as a JSON document on stdout that says
// everything is fine. The document is still printed, because the work the tick
// did complete is the operator's to see either way.
func TestRunGHTriageDriverFailsTheCommandOverACaseItCannotReplay(t *testing.T) {
	ctx := context.Background()
	config := initializedCollective(t)
	f := newTriageDriverFixture(t, config)
	caseID := f.addCaseWithAnUnreplayableTriage(t, "o/r#42", "2026-09-28T10:00:00Z")
	f.Close()

	readStdout := captureStdout(t)
	err := runGHTriageDriver(ctx, []string{"--mission", string(f.mission)})
	if err == nil {
		t.Fatal("run-gh-triage-driver exited 0 over a case it could not replay")
	}
	for _, want := range []string{"o/r#42", string(caseID), "no accepted index"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	result := decodeDriverResult(t, readStdout())
	if len(result.Failures) != 1 {
		t.Fatalf("result = %+v, want one failure", result)
	}
	if result.Accepted != 0 || result.Assessed != 0 || result.Blocked != 0 || result.Superseded != 0 {
		t.Fatalf("result = %+v, want every counter zero", result)
	}
}

// The other end of the same command: a mission with nothing to advance is a
// healthy tick, and it must not borrow the failure path's exit code.
func TestRunGHTriageDriverExitsZeroForAMissionWithNothingToAdvance(t *testing.T) {
	ctx := context.Background()
	config := initializedCollective(t)
	f := newTriageDriverFixture(t, config)
	f.Close()

	readStdout := captureStdout(t)
	if err := runGHTriageDriver(ctx, []string{"--mission", string(f.mission)}); err != nil {
		t.Fatalf("run-gh-triage-driver over an empty mission: %v", err)
	}
	result := decodeDriverResult(t, readStdout())
	if len(result.Failures) != 0 {
		t.Fatalf("result = %+v, want no failures", result)
	}
	if result.Accepted != 0 || result.Assessed != 0 || result.Blocked != 0 || result.Superseded != 0 {
		t.Fatalf("result = %+v, want every counter zero", result)
	}
}

// A mission that was never created is not a command error, and this says so
// rather than leaving it to a comment. ListActive filters on mission_id and
// never resolves the mission, so a mistyped --mission finds no cases and the
// tick reports exactly what it reports over a mission that exists and had nothing
// to advance: exit 0, every counter zero, no failures. The two are
// indistinguishable, which is the behaviour an operator with a typo is actually
// given. It is asserted rather than left implicit because the obvious fix -
// resolving the mission and failing on a miss - changes what a scheduler sees for
// a typo, and should be a deliberate change with its own test rather than a
// side effect of somebody tidying this one up.
func TestRunGHTriageDriverExitsZeroForAMissionThatWasNeverCreated(t *testing.T) {
	ctx := context.Background()
	config := initializedCollective(t)
	f := newTriageDriverFixture(t, config)
	f.Close()

	readStdout := captureStdout(t)
	if err := runGHTriageDriver(ctx, []string{"--mission", "mission-does-not-exist"}); err != nil {
		t.Fatalf("run-gh-triage-driver over a mission that was never created: %v", err)
	}
	result := decodeDriverResult(t, readStdout())
	// The same assertions, and the same result, as the mission that exists and
	// has nothing in it.
	if len(result.Failures) != 0 {
		t.Fatalf("result = %+v, want no failures", result)
	}
	if result.Accepted != 0 || result.Assessed != 0 || result.Blocked != 0 || result.Superseded != 0 {
		t.Fatalf("result = %+v, want every counter zero", result)
	}
}

// The two invocation errors the subcommand does catch: no --mission at all, and
// no context to run in. It says nothing about a mission that was never created -
// TestRunGHTriageDriverExitsZeroForAMissionThatWasNeverCreated covers that, and
// the answer there is an empty tick.
func TestRunGHTriageDriverRejectsAMissingFlagAndAMissingContext(t *testing.T) {
	ctx := context.Background()
	config := initializedCollective(t)
	f := newTriageDriverFixture(t, config)
	f.Close()

	if err := runGHTriageDriver(ctx, nil); err == nil {
		t.Fatal("run-gh-triage-driver ran without --mission")
	}
	if err := runGHTriageDriver(nil, []string{"--mission", string(f.mission)}); err == nil {
		t.Fatal("run-gh-triage-driver ran with no context")
	}
}
