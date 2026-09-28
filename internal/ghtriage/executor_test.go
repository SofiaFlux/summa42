package ghtriage_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
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

func putObject(t *testing.T, store *evidence.Store, kind string, snap ghtriage.Snapshot) domain.ID {
	t.Helper()
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.Put(context.Background(), strings.NewReader(string(raw)), evidence.Metadata{
		MediaType: "application/json", Kind: kind,
	})
	if err != nil {
		t.Fatal(err)
	}
	return object.ID
}

func putSnapshot(t *testing.T, store *evidence.Store, snap ghtriage.Snapshot) domain.ID {
	t.Helper()
	return putObject(t, store, "github.issue.snapshot", snap)
}

// intakePayloadKeys is the key set internal/ghissue.taskTemplate marshals into
// a github.issue.triage payload. The worker hands that JSON to the executor
// verbatim, so the executor reads those keys and no others.
var intakePayloadKeys = []string{
	"repo", "issue", "revision", "title", "url", "author", "labels", "triage", "issueSnapshot",
}

// intakePayload builds a payload with exactly intake's key set, so the executor
// is never tested against a payload no real task can carry. Every payload in
// this file is built here rather than from a hand-written map.
func intakePayload(snapshotID domain.ID) map[string]any {
	return map[string]any{
		"repo": "o/r", "issue": 42,
		"revision": "2026-09-28T10:00:00Z", "issueSnapshot": string(snapshotID),
		"title": "Crash", "url": "https://github.com/o/r/issues/42", "author": "octocat",
		"labels": []string{"bug"}, "triage": "unclassified",
	}
}

func mustPayload(t *testing.T, payload map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func envelopeFor(t *testing.T, snapshotID domain.ID) executors.AttemptEnvelope {
	t.Helper()
	return executors.AttemptEnvelope{
		TaskID: domain.NewID("task"), AttemptID: domain.NewID("attempt"),
		PayloadJSON:        mustPayload(t, intakePayload(snapshotID)),
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

// The executor and the intake that writes its payload are two separate writers
// of one contract, and only a test that carries a real intake payload makes
// them meet. The executor once read a repository key that intake never wrote,
// which no test in either package could see: every payload here was built by
// this file, and intake's own test only ever checks the keys it reads itself.
func TestExecutorAcceptsThePayloadKeySetIntakeWrites(t *testing.T) {
	keys := make([]string, 0, len(intakePayloadKeys))
	for key := range intakePayload(domain.NewID("snap")) {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	want := append([]string(nil), intakePayloadKeys...)
	sort.Strings(want)
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("payload keys = %q, want exactly the keys intake writes %q", keys, intakePayloadKeys)
	}

	exec, _, evidenceStore := newExecutorFixture(t)
	snap := ghtriage.Snapshot{
		Repo: "o/r", Issue: 42, Title: "Crash", Body: "boom",
		Triage: "bug", Labels: []string{"duplicate-of:41"}, UpdatedAt: "2026-09-28T10:00:00Z",
	}
	snapshotID := putSnapshot(t, evidenceStore, snap)
	envelope := envelopeFor(t, snapshotID)

	result, err := exec.Start(context.Background(), envelope)
	if err != nil {
		t.Fatalf("a payload with intake's key set was rejected: %v", err)
	}
	if len(result.Evidence) != 1 {
		t.Fatalf("evidence = %d, want exactly one decision", len(result.Evidence))
	}
	var decision ghtriage.Decision
	if err := json.Unmarshal([]byte(result.Evidence[0].Content), &decision); err != nil {
		t.Fatal(err)
	}
	if decision.Repository != "o/r" || decision.Issue != 42 {
		t.Fatalf("decision = %s#%d, want o/r#42", decision.Repository, decision.Issue)
	}
}

func TestExecutorResolvesADuplicateWithoutCallingTheModel(t *testing.T) {
	exec, model, evidenceStore := newExecutorFixture(t)
	snap := ghtriage.Snapshot{
		Repo: "o/r", Issue: 42, Title: "Crash", Body: "boom",
		Triage: "bug", Labels: []string{"duplicate-of:41"}, UpdatedAt: "2026-09-28T10:00:00Z",
	}
	snapshotID := putSnapshot(t, evidenceStore, snap)
	envelope := envelopeFor(t, snapshotID)

	result, err := exec.Start(context.Background(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	if len(model.ClassifyInputs) != 0 {
		t.Fatalf("the model was consulted %d times on a decisive stage 1", len(model.ClassifyInputs))
	}
	if len(result.Evidence) != 1 {
		t.Fatalf("evidence = %d, want exactly one decision", len(result.Evidence))
	}
	// The consumer downstream selects the decision by kind, so a decision
	// emitted under another kind is not found at all.
	if result.Evidence[0].Kind != executors.EvidenceKind(ghtriage.KindDecision) {
		t.Fatalf("evidence kind = %q, want %q", result.Evidence[0].Kind, ghtriage.KindDecision)
	}
	var decision ghtriage.Decision
	if err := json.Unmarshal([]byte(result.Evidence[0].Content), &decision); err != nil {
		t.Fatal(err)
	}
	// The decision cites the snapshot it was derived from, so a placeholder
	// here would break the citation for every issue triaged.
	if decision.SnapshotEvidenceID != string(snapshotID) {
		t.Fatalf("snapshot evidence id = %q, want the cited object %q", decision.SnapshotEvidenceID, snapshotID)
	}
	if decision.FinalDisposition() != ghtriage.DispositionDuplicate {
		t.Fatalf("disposition = %q", decision.FinalDisposition())
	}
	if decision.Stage2 != nil || decision.Stage3 != nil {
		t.Fatal("a decisive stage 1 produced later stages")
	}
}

// The snapshot is deliberately awkward: unsorted labels, a title carrying a
// scope prefix, and a body that opens with a reproduction fence. The prepared
// input is the only context the model ever sees, so asserting its whole shape
// here is what distinguishes "the executor handed over the filtered context"
// from "the executor called the model somehow".
func TestExecutorRecordsAStage3DecisionWithTheExactPreparedInput(t *testing.T) {
	exec, model, evidenceStore := newExecutorFixture(t)
	model.Scripted = []ghtriage.Stage2Output{{
		IsActionable: true, Scope: ghtriage.ScopeSmall,
		SuggestedType: ghtriage.SuggestedTypeBug, Rationale: "enough detail to plan",
	}}
	snap := ghtriage.Snapshot{
		Repo: "o/r", Issue: 42,
		Title:  "[crash] null pointer on save",
		Body:   "```\npanic: nil pointer\n```\nSteps:\n1. open\n2. save",
		Triage: "bug", Labels: []string{"needs-triage", "crash", "bug"},
		UpdatedAt: "2026-09-28T10:00:00Z",
	}
	envelope := envelopeFor(t, putSnapshot(t, evidenceStore, snap))

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
	if len(model.ClassifyInputs) != 1 {
		t.Fatalf("classifier calls = %d, want 1", len(model.ClassifyInputs))
	}
	prepared := model.ClassifyInputs[0]
	if prepared.Question != ghtriage.Stage2Question {
		t.Fatalf("question = %q, want the fixed literal", prepared.Question)
	}
	if prepared.Schema != ghtriage.Stage2InputSchema {
		t.Fatalf("input schema = %q", prepared.Schema)
	}
	if prepared.Title != snap.Title {
		t.Fatalf("title = %q, want the snapshot title %q", prepared.Title, snap.Title)
	}
	if !reflect.DeepEqual(prepared.Labels, []string{"bug", "crash", "needs-triage"}) {
		t.Fatalf("labels = %q, want the snapshot labels sorted", prepared.Labels)
	}
	if !reflect.DeepEqual(prepared.Signals, []string{
		"has-repro", "label:bug", "label:crash", "label:needs-triage", "title:[crash]",
	}) {
		t.Fatalf("signals = %q, want the stage 1 signals sorted", prepared.Signals)
	}
	if prepared.BodyExcerpt != "Steps:\n1. open\n2. save" {
		t.Fatalf("body excerpt = %q, want the body without the reproduction block", prepared.BodyExcerpt)
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
	envelope := envelopeFor(t, putSnapshot(t, evidenceStore, snap))

	result, err := exec.Start(context.Background(), envelope)
	if err == nil {
		t.Fatal("two invalid responses did not fail the attempt")
	}
	if len(result.Evidence) != 0 {
		t.Fatalf("a failed attempt fabricated %d evidence documents", len(result.Evidence))
	}
	if len(model.ClassifyInputs) != 2 {
		t.Fatalf("classifier calls = %d, want exactly one retry", len(model.ClassifyInputs))
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
	envelope := envelopeFor(t, putSnapshot(t, evidenceStore, snap))

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
	absent := domain.NewID("absent")
	result, err := exec.Start(context.Background(), envelopeFor(t, absent))
	if err == nil {
		t.Fatal("a missing snapshot evidence was accepted")
	}
	if !errors.Is(err, evidence.ErrEvidenceNotFound) {
		t.Fatalf("error %q is not the absent-object error", err)
	}
	if !strings.Contains(err.Error(), string(absent)) {
		t.Fatalf("error %q does not name the absent evidence id %q", err, absent)
	}
	if len(result.Evidence) != 0 {
		t.Fatalf("a failed attempt fabricated %d evidence documents", len(result.Evidence))
	}
}

// Every payload field the decision echoes is guarded in the decoder, so the
// decoder's own diagnostics are pinned here, field by field, and not only
// through the decision validator that would catch the damage later.
func TestExecutorFailsOnEachIncompletePayloadField(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantErr string
		// notAbsent requires the rejection to be the decoder's own rather than
		// the snapshot load's. Only the missing-snapshot row needs it, because
		// an absent object reports the snapshot too and the field name alone
		// cannot tell the two failures apart.
		notAbsent bool
	}{
		{
			name:    "repository missing",
			payload: `{"issue":42,"revision":"r","issueSnapshot":"ev_1"}`,
			wantErr: "lacks a repository or issue number",
		},
		{
			name:    "repository empty",
			payload: `{"repo":"","issue":42,"revision":"r","issueSnapshot":"ev_1"}`,
			wantErr: "lacks a repository or issue number",
		},
		{
			name:    "issue missing",
			payload: `{"repo":"o/r","revision":"r","issueSnapshot":"ev_1"}`,
			wantErr: "lacks a repository or issue number",
		},
		{
			name:    "issue empty",
			payload: `{"repo":"o/r","issue":0,"revision":"r","issueSnapshot":"ev_1"}`,
			wantErr: "lacks a repository or issue number",
		},
		{
			name:    "revision missing",
			payload: `{"repo":"o/r","issue":42,"issueSnapshot":"ev_1"}`,
			wantErr: "lacks a revision",
		},
		{
			name:    "revision empty",
			payload: `{"repo":"o/r","issue":42,"revision":"","issueSnapshot":"ev_1"}`,
			wantErr: "lacks a revision",
		},
		{
			name:      "issue snapshot missing",
			payload:   `{"repo":"o/r","issue":42,"revision":"r"}`,
			wantErr:   "lacks the issue snapshot evidence id",
			notAbsent: true,
		},
		{
			name:    "issue snapshot empty",
			payload: `{"repo":"o/r","issue":42,"revision":"r","issueSnapshot":""}`,
			wantErr: "lacks the issue snapshot evidence id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec, _, _ := newExecutorFixture(t)
			envelope := executors.AttemptEnvelope{
				TaskID: domain.NewID("task"), AttemptID: domain.NewID("attempt"),
				PayloadJSON: []byte(tc.payload),
			}
			result, err := exec.Start(context.Background(), envelope)
			if err == nil {
				t.Fatalf("payload %s was accepted", tc.payload)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not report the missing field (%q)", err, tc.wantErr)
			}
			if tc.notAbsent && errors.Is(err, evidence.ErrEvidenceNotFound) {
				t.Fatalf("error %q came from the snapshot load, not the payload decoder", err)
			}
			if len(result.Evidence) != 0 {
				t.Fatalf("a failed attempt fabricated %d evidence documents", len(result.Evidence))
			}
		})
	}
}

func TestExecutorFailsOnANonCanonicalSnapshot(t *testing.T) {
	exec, _, evidenceStore := newExecutorFixture(t)
	// A stored snapshot of the zero value: valid JSON that decodes cleanly and
	// names no issue. The decision's repository and issue come from the payload,
	// so without the guard this records a disposition for an issue that was
	// never triaged, and nothing downstream re-checks it.
	envelope := envelopeFor(t, putSnapshot(t, evidenceStore, ghtriage.Snapshot{}))

	result, err := exec.Start(context.Background(), envelope)
	if err == nil {
		t.Fatal("a snapshot naming no issue was accepted")
	}
	if !strings.Contains(err.Error(), "not a canonical issue snapshot") {
		t.Fatalf("error %q does not report a non-canonical snapshot", err)
	}
	if len(result.Evidence) != 0 {
		t.Fatalf("a failed attempt fabricated %d evidence documents", len(result.Evidence))
	}
}

func TestExecutorRejectsAnEvidenceObjectThatIsNotAnIssueSnapshot(t *testing.T) {
	exec, _, evidenceStore := newExecutorFixture(t)
	snap := ghtriage.Snapshot{
		Repo: "o/r", Issue: 42, Title: "Crash", Body: "boom",
		Triage: "bug", Labels: []string{"bug"}, UpdatedAt: "2026-09-28T10:00:00Z",
	}
	envelope := envelopeFor(t, putObject(t, evidenceStore, ghtriage.KindReview, snap))

	result, err := exec.Start(context.Background(), envelope)
	if err == nil {
		t.Fatal("an evidence object of another kind was read as an issue snapshot")
	}
	if !strings.Contains(err.Error(), "not an issue snapshot") {
		t.Fatalf("error %q does not report the wrong evidence kind", err)
	}
	if len(result.Evidence) != 0 {
		t.Fatalf("a failed attempt fabricated %d evidence documents", len(result.Evidence))
	}
}

func TestExecutorFailsWhenTheSnapshotDoesNotMatchThePayload(t *testing.T) {
	exec, _, evidenceStore := newExecutorFixture(t)
	snap := ghtriage.Snapshot{
		Repo: "o/r", Issue: 42, Title: "Crash", Body: "boom",
		Triage: "bug", Labels: []string{"bug"}, UpdatedAt: "2026-09-28T10:00:00Z",
	}
	id := putSnapshot(t, evidenceStore, snap)

	cases := []struct {
		name       string
		repository string
		issue      int64
	}{
		{"the payload names another repository", "other/repo", 42},
		{"the payload names another issue", "o/r", 41},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := intakePayload(id)
			payload["repo"] = tc.repository
			payload["issue"] = tc.issue
			result, err := exec.Start(context.Background(), executors.AttemptEnvelope{
				TaskID: domain.NewID("task"), AttemptID: domain.NewID("attempt"),
				PayloadJSON: mustPayload(t, payload),
			})
			if err == nil {
				t.Fatal("a payload that contradicts its own snapshot was accepted")
			}
			if !strings.Contains(err.Error(), "but the task payload names") {
				t.Fatalf("error %q does not report the disagreement", err)
			}
			if len(result.Evidence) != 0 {
				t.Fatalf("a failed attempt fabricated %d evidence documents", len(result.Evidence))
			}
		})
	}
}
