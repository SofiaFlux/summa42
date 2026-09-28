package ghtriage_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/ghtriage/fakemodel"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

func putSnapshot(t *testing.T, store *evidence.Store, snap ghtriage.Snapshot) domain.ID {
	t.Helper()
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.Put(context.Background(), strings.NewReader(string(raw)), evidence.Metadata{
		MediaType: "application/json", Kind: "github.issue.snapshot",
	})
	if err != nil {
		t.Fatal(err)
	}
	return object.ID
}

func envelopeFor(snapshotID domain.ID) executors.AttemptEnvelope {
	payload, _ := json.Marshal(map[string]any{
		"repository": "o/r", "issue": 42,
		"revision": "2026-09-28T10:00:00Z", "issueSnapshot": snapshotID,
	})
	return executors.AttemptEnvelope{
		TaskID: domain.NewID("task"), AttemptID: domain.NewID("attempt"),
		PayloadJSON:        payload,
		AcceptanceCriteria: []string{"triage decision recorded for 2026-09-28T10:00:00Z"},
	}
}

func newExecutorFixture(t *testing.T) (*ghtriage.Executor, *fakemodel.Fake, *evidence.Store) {
	t.Helper()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC))
	evidenceStore, err := evidence.New(store, t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	model := fakemodel.New()
	return ghtriage.NewExecutor(evidenceStore, model), model, evidenceStore
}

func TestExecutorResolvesADuplicateWithoutCallingTheModel(t *testing.T) {
	exec, model, evidenceStore := newExecutorFixture(t)
	snap := ghtriage.Snapshot{
		Repo: "o/r", Issue: 42, Title: "Crash", Body: "boom",
		Triage: "bug", Labels: []string{"duplicate-of:41"}, UpdatedAt: "2026-09-28T10:00:00Z",
	}
	envelope := envelopeFor(putSnapshot(t, evidenceStore, snap))

	result, err := exec.Start(context.Background(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	if model.ClassifyCallCount() != 0 {
		t.Fatalf("the model was consulted %d times on a decisive stage 1", model.ClassifyCallCount())
	}
	if len(result.Evidence) != 1 {
		t.Fatalf("evidence = %d, want exactly one decision", len(result.Evidence))
	}
	var decision ghtriage.Decision
	if err := json.Unmarshal([]byte(result.Evidence[0].Content), &decision); err != nil {
		t.Fatal(err)
	}
	if decision.FinalDisposition() != ghtriage.DispositionDuplicate {
		t.Fatalf("disposition = %q", decision.FinalDisposition())
	}
	if decision.Stage2 != nil || decision.Stage3 != nil {
		t.Fatal("a decisive stage 1 produced later stages")
	}
}

func TestExecutorRecordsAStage3DecisionWithTheExactPreparedInput(t *testing.T) {
	exec, model, evidenceStore := newExecutorFixture(t)
	model.Scripted = []ghtriage.Stage2Output{{
		IsActionable: true, Scope: ghtriage.ScopeSmall,
		SuggestedType: ghtriage.SuggestedTypeBug, Rationale: "enough detail to plan",
	}}
	snap := ghtriage.Snapshot{
		Repo: "o/r", Issue: 42, Title: "Crash", Body: "boom",
		Triage: "bug", Labels: []string{"bug"}, UpdatedAt: "2026-09-28T10:00:00Z",
	}
	envelope := envelopeFor(putSnapshot(t, evidenceStore, snap))

	result, err := exec.Start(context.Background(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	var decision ghtriage.Decision
	if err := json.Unmarshal([]byte(result.Evidence[0].Content), &decision); err != nil {
		t.Fatal(err)
	}
	if decision.Stage3 == nil || decision.Stage3.Disposition != ghtriage.DispositionReadyToPlan {
		t.Fatalf("stage 3 = %+v", decision.Stage3)
	}
	if model.ClassifyCallCount() != 1 {
		t.Fatalf("classifier calls = %d, want 1", model.ClassifyCallCount())
	}
	if got := model.ClassifyInputs[0].Question; got != ghtriage.Stage2Question {
		t.Fatalf("question = %q, want the fixed literal", got)
	}
	if got := model.ClassifyInputs[0].Schema; got != ghtriage.Stage2InputSchema {
		t.Fatalf("input schema = %q", got)
	}
}

func TestExecutorFailsTheAttemptWhenBothResponsesAreInvalid(t *testing.T) {
	exec, model, evidenceStore := newExecutorFixture(t)
	model.Scripted = []ghtriage.Stage2Output{
		{Scope: "enormous", Rationale: "x"},
		{Scope: "enormous", Rationale: "x"},
	}
	snap := ghtriage.Snapshot{
		Repo: "o/r", Issue: 42, Title: "Crash", Body: "boom",
		Triage: "bug", Labels: []string{"bug"}, UpdatedAt: "2026-09-28T10:00:00Z",
	}
	envelope := envelopeFor(putSnapshot(t, evidenceStore, snap))

	result, err := exec.Start(context.Background(), envelope)
	if err == nil {
		t.Fatal("two invalid responses did not fail the attempt")
	}
	if len(result.Evidence) != 0 {
		t.Fatalf("a failed attempt fabricated %d evidence documents", len(result.Evidence))
	}
	if model.ClassifyCallCount() != 2 {
		t.Fatalf("classifier calls = %d, want exactly one retry", model.ClassifyCallCount())
	}
}

func TestExecutorSucceedsOnTheRetryWhenTheSecondResponseConforms(t *testing.T) {
	exec, model, evidenceStore := newExecutorFixture(t)
	model.Scripted = []ghtriage.Stage2Output{
		{Scope: "enormous", Rationale: "x"},
		{IsActionable: true, Scope: ghtriage.ScopeSmall, Rationale: "enough detail",
			SuggestedType: ghtriage.SuggestedTypeBug},
	}
	snap := ghtriage.Snapshot{
		Repo: "o/r", Issue: 42, Title: "Crash", Body: "boom",
		Triage: "bug", Labels: []string{"bug"}, UpdatedAt: "2026-09-28T10:00:00Z",
	}
	envelope := envelopeFor(putSnapshot(t, evidenceStore, snap))

	result, err := exec.Start(context.Background(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Evidence) != 1 {
		t.Fatalf("evidence = %d, want one decision", len(result.Evidence))
	}
	if len(model.ClassifyInputs) != 2 {
		t.Fatalf("classifier calls = %d, want 2", len(model.ClassifyInputs))
	}
	// Stage2Input carries slice fields, so == does not compile on it and the
	// identical-input property is asserted structurally instead.
	if !reflect.DeepEqual(model.ClassifyInputs[0], model.ClassifyInputs[1]) {
		t.Fatal("the retry changed the prepared input; it must be identical")
	}
}

func TestExecutorFailsWhenTheSnapshotEvidenceIsMissing(t *testing.T) {
	exec, _, _ := newExecutorFixture(t)
	envelope := envelopeFor(domain.NewID("absent"))
	if _, err := exec.Start(context.Background(), envelope); err == nil {
		t.Fatal("a missing snapshot evidence was accepted")
	}
}

func TestExecutorFailsOnAnIncompletePayload(t *testing.T) {
	exec, _, _ := newExecutorFixture(t)
	envelope := executors.AttemptEnvelope{
		TaskID: domain.NewID("task"), AttemptID: domain.NewID("attempt"),
		PayloadJSON: []byte(`{"repository":"o/r","issue":42,"revision":"r"}`),
	}
	_, err := exec.Start(context.Background(), envelope)
	if err == nil {
		t.Fatal("a payload without a snapshot evidence id was accepted")
	}
	if !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("error %q does not name the missing snapshot", err)
	}
}
