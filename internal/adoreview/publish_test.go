package adoreview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/adoeffects"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

type fakePublishOps struct {
	prepared      []operations.PrepareRequest
	dispatched    []domain.ID
	ops           map[domain.ID]domain.ExternalOperation
	nextID        int
	dispatchState domain.OperationState
	prepareErrAt  int
	prepareErr    error
	prepareState  domain.OperationState
	dispatchErrAt int
	dispatchErr   error
}

func (f *fakePublishOps) Prepare(_ context.Context, request operations.PrepareRequest) (domain.ExternalOperation, error) {
	f.prepared = append(f.prepared, request)
	if f.prepareErrAt == len(f.prepared) {
		return domain.ExternalOperation{}, f.prepareErr
	}
	f.nextID++
	op := domain.ExternalOperation{ID: domain.ID(fmt.Sprintf("op-%d", f.nextID)), State: f.prepareState}
	if f.ops == nil {
		f.ops = make(map[domain.ID]domain.ExternalOperation)
	}
	f.ops[op.ID] = op
	return op, nil
}

func (f *fakePublishOps) Dispatch(_ context.Context, operationID, _ domain.ID) (domain.ExternalOperation, error) {
	f.dispatched = append(f.dispatched, operationID)
	op := f.ops[operationID]
	if f.dispatchState != "" {
		op.State = f.dispatchState
	} else {
		op.State = domain.OperationConfirmedEffect
	}
	f.ops[operationID] = op
	if f.dispatchErrAt == len(f.dispatched) {
		return op, f.dispatchErr
	}
	return op, nil
}

func newPublishTestEvidenceStore(t *testing.T) *evidence.Store {
	t.Helper()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC))
	evidenceStore, err := evidence.New(store, t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	return evidenceStore
}

func TestPublishContractHandlesDecisionsInEveryMode(t *testing.T) {
	for _, mode := range []PublishMode{PublishNone, PublishComments, PublishAll} {
		contract := (&Publisher{config: PublishConfig{Mode: mode}}).ExecutionContract()
		for _, cap := range []string{"ado.pr.comment", "ado.pr.approve"} {
			if !contains(contract.Capabilities, cap) {
				t.Fatalf("mode %s cannot handle %s decision", mode, cap)
			}
		}
	}
}

func TestPublishConfigDoesNotHoldProviderObjects(t *testing.T) {
	configType := reflect.TypeOf(PublishConfig{})
	for _, name := range []string{"Comment", "Vote"} {
		if _, exists := configType.FieldByName(name); exists {
			t.Errorf("PublishConfig.%s must not be defined", name)
		}
	}
}

func TestNewPublisherRequiresOperationsAndEvidence(t *testing.T) {
	evidenceStore := newPublishTestEvidenceStore(t)
	tests := []struct {
		name   string
		config PublishConfig
	}{
		{name: "operations", config: PublishConfig{Mode: PublishNone, Evidence: evidenceStore}},
		{name: "evidence", config: PublishConfig{Mode: PublishNone, Operations: &fakePublishOps{}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewPublisher(test.config); err == nil {
				t.Fatal("NewPublisher succeeded without required service")
			}
		})
	}
}

func TestNewPublisherAllRequiresApproveRisk(t *testing.T) {
	for _, risk := range []string{"", " \t"} {
		_, err := NewPublisher(PublishConfig{
			Mode:        PublishAll,
			Operations:  &fakePublishOps{},
			Evidence:    newPublishTestEvidenceStore(t),
			RiskApprove: risk,
		})
		if err == nil {
			t.Fatalf("NewPublisher accepted blank approve risk %q", risk)
		}
	}
}

func putDecision(t *testing.T, store *evidence.Store, ctx context.Context, decision ReviewDecision) domain.ID {
	t.Helper()
	raw, err := json.Marshal(decision)
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.Put(ctx, bytes.NewReader(raw), evidence.Metadata{MediaType: "application/json", Kind: "ado.review.decision"})
	if err != nil {
		t.Fatal(err)
	}
	return object.ID
}

func publishEnvelope(task, attempt string, workspace string) executors.AttemptEnvelope {
	return executors.AttemptEnvelope{TaskID: domain.ID(task), AttemptID: domain.ID(attempt), Workspace: workspace, Objective: "publish"}
}

func publishPayload(t *testing.T, decision domain.ID) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(PublishPayload{Decision: string(decision), CaseID: "case-1", WorkID: "work-1", Project: "proj", Repo: "shop", PR: 1, Revision: "a:b"})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertPublishEvidence(t *testing.T, result executors.ExecutionResult, want string) {
	t.Helper()
	if len(result.Evidence) != 1 {
		t.Fatalf("evidence entries = %d, want 1", len(result.Evidence))
	}
	if result.Evidence[0].Kind != executors.EvidenceAgentMessage {
		t.Fatalf("evidence kind = %q, want %q", result.Evidence[0].Kind, executors.EvidenceAgentMessage)
	}
	if result.Evidence[0].Content != want {
		t.Fatalf("evidence content = %q, want %q", result.Evidence[0].Content, want)
	}
}

func TestPublishNoneRecordsWithoutPrepare(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC))
	evidenceStore, err := evidence.New(store, t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	id := putDecision(t, evidenceStore, ctx, ReviewDecision{Action: DecisionApproveAction, Vote: "approve", Reason: "clean"})
	publisher, err := NewPublisher(PublishConfig{Mode: PublishNone, Operations: &fakePublishOps{ops: map[domain.ID]domain.ExternalOperation{}}, Evidence: evidenceStore, OwnerApprovals: []domain.ID{"owner-1"}, RiskComment: "LOW"})
	if err != nil {
		t.Fatal(err)
	}
	envelope := publishEnvelope("task-1", "attempt-1", t.TempDir())
	envelope.PayloadJSON = publishPayload(t, id)
	result, err := publisher.Start(ctx, envelope)
	if err != nil {
		t.Fatal(err)
	}
	assertPublishEvidence(t, result, `{"slot":"ado.pr.approve:proj/shop#1:a:b","recorded-only":true}`)
}

func TestPublishCommentsDispatchesComments(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC))
	evidenceStore, err := evidence.New(store, t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	id := putDecision(t, evidenceStore, ctx, ReviewDecision{Action: DecisionCommentAction, Reason: "mixed",
		Comments: []DecisionComment{{Path: "a.go", Line: 1, Body: "a.go:1: nit"}, {Path: "b.go", Line: 0, Body: "b.go: nit"}}})
	ops := &fakePublishOps{ops: map[domain.ID]domain.ExternalOperation{}}
	publisher, err := NewPublisher(PublishConfig{Mode: PublishComments, Operations: ops, Evidence: evidenceStore, OwnerApprovals: []domain.ID{"owner-1"}, RiskComment: "LOW"})
	if err != nil {
		t.Fatal(err)
	}
	envelope := publishEnvelope("task-1", "attempt-1", t.TempDir())
	envelope.PayloadJSON = publishPayload(t, id)
	result, err := publisher.Start(ctx, envelope)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops.prepared) != 2 {
		t.Fatalf("prepared = %d, want 2", len(ops.prepared))
	}
	for i, want := range []string{"ado.pr.comment:proj/shop#1:a:b:0", "ado.pr.comment:proj/shop#1:a:b:1"} {
		if ops.prepared[i].TrustedSlotKey != want {
			t.Fatalf("slot[%d] = %q, want %q", i, ops.prepared[i].TrustedSlotKey, want)
		}
		if ops.prepared[i].Provider != "ado-pr-comment" || ops.prepared[i].Risk != "LOW" {
			t.Fatalf("prepared[%d] = %+v", i, ops.prepared[i])
		}
		if len(ops.prepared[i].RequiredApprovals) != 1 || ops.prepared[i].RequiredApprovals[0] != "owner-1" {
			t.Fatalf("approvals = %v", ops.prepared[i].RequiredApprovals)
		}
	}
	assertPublishEvidence(t, result, "{\"slot\":\"ado.pr.comment:proj/shop#1:a:b:0\",\"operation\":\"op-1\",\"state\":\"CONFIRMED_EFFECT\"}\n{\"slot\":\"ado.pr.comment:proj/shop#1:a:b:1\",\"operation\":\"op-2\",\"state\":\"CONFIRMED_EFFECT\"}")
}

func TestPublishCommentLookupUsesIndexedMarker(t *testing.T) {
	payload := PublishPayload{CaseID: "case-1", WorkID: "work-1", Project: "proj", Repo: "shop", PR: 1, Revision: "a:b"}
	decision := ReviewDecision{Action: DecisionCommentAction, Comments: []DecisionComment{
		{Path: "a.go", Line: 1, Body: "first"},
		{Path: "b.go", Line: 2, Body: "second"},
	}}
	intents, err := buildPublishIntents(payload, decision)
	if err != nil {
		t.Fatal(err)
	}
	firstMarker := "[summa42:case-1:work-1:0]"
	provider, err := adoeffects.NewCommentProvider(adoeffects.Config{Command: "/bin/true", Organization: "Contoso"}, func(context.Context, string, any) (any, error) {
		return map[string]any{"threads": []any{map[string]any{
			"threadId": "thread-1",
			"comments": []any{map[string]any{"content": "first\n" + firstMarker}},
		}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for index, intent := range intents {
		canonical, err := provider.CanonicalIntent(intent.intent)
		if err != nil {
			t.Fatal(err)
		}
		var comment adoeffects.CommentIntent
		if err := json.Unmarshal(canonical, &comment); err != nil {
			t.Fatal(err)
		}
		wantMarker := firstMarker
		if index == 1 {
			wantMarker = "[summa42:case-1:work-1:1]"
		}
		if comment.Marker != wantMarker {
			t.Fatalf("comment[%d] marker = %q, want %q", index, comment.Marker, wantMarker)
		}
		outcome, err := provider.LookupOutcome(context.Background(), operations.ProviderDispatchRequest{CanonicalIntent: canonical})
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 && outcome.State != domain.OperationConfirmedEffect {
			t.Fatalf("first lookup = %q, want CONFIRMED_EFFECT", outcome.State)
		}
		if index == 1 && outcome.State != domain.OperationOutcomeUnknown {
			t.Fatalf("second lookup = %q, want OUTCOME_UNKNOWN", outcome.State)
		}
	}
}

func TestPublishCommentsSkipsApprove(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC))
	evidenceStore, err := evidence.New(store, t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	id := putDecision(t, evidenceStore, ctx, ReviewDecision{Action: DecisionApproveAction, Vote: "approve", Reason: "clean"})
	ops := &fakePublishOps{ops: map[domain.ID]domain.ExternalOperation{}}
	publisher, err := NewPublisher(PublishConfig{Mode: PublishComments, Operations: ops, Evidence: evidenceStore, RiskComment: "LOW"})
	if err != nil {
		t.Fatal(err)
	}
	envelope := publishEnvelope("task-1", "attempt-1", t.TempDir())
	envelope.PayloadJSON = publishPayload(t, id)
	result, err := publisher.Start(ctx, envelope)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops.prepared) != 0 {
		t.Fatalf("prepared = %d, want 0", len(ops.prepared))
	}
	assertPublishEvidence(t, result, `{"slot":"ado.pr.approve:proj/shop#1:a:b","skipped":true}`)
}

func TestPublishAllDispatchesVote(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC))
	evidenceStore, err := evidence.New(store, t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	id := putDecision(t, evidenceStore, ctx, ReviewDecision{Action: DecisionApproveAction, Vote: "approve", Reason: "clean"})
	ops := &fakePublishOps{ops: map[domain.ID]domain.ExternalOperation{}}
	publisher, err := NewPublisher(PublishConfig{Mode: PublishAll, Operations: ops, Evidence: evidenceStore, RiskComment: "LOW", RiskApprove: "OWNER"})
	if err != nil {
		t.Fatal(err)
	}
	envelope := publishEnvelope("task-1", "attempt-1", t.TempDir())
	envelope.PayloadJSON = publishPayload(t, id)
	result, err := publisher.Start(ctx, envelope)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops.prepared) != 1 {
		t.Fatalf("prepared = %d, want 1", len(ops.prepared))
	}
	got := ops.prepared[0]
	if got.Provider != "ado-pr-vote" || got.TrustedSlotKey != "ado.pr.approve:proj/shop#1:a:b" || got.Risk != "OWNER" {
		t.Fatalf("prepared = %+v", got)
	}
	assertPublishEvidence(t, result, `{"slot":"ado.pr.approve:proj/shop#1:a:b","operation":"op-1","state":"CONFIRMED_EFFECT"}`)
}

func TestPublishDispatchUnknownRecordedAndStops(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC))
	evidenceStore, err := evidence.New(store, t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	id := putDecision(t, evidenceStore, ctx, ReviewDecision{Action: DecisionCommentAction, Reason: "mixed",
		Comments: []DecisionComment{{Path: "a.go", Line: 1, Body: "a.go:1: nit"}, {Path: "b.go", Body: "b.go: nit"}}})
	ops := &fakePublishOps{ops: map[domain.ID]domain.ExternalOperation{}, dispatchState: domain.OperationOutcomeUnknown}
	publisher, err := NewPublisher(PublishConfig{Mode: PublishComments, Operations: ops, Evidence: evidenceStore, RiskComment: "LOW"})
	if err != nil {
		t.Fatal(err)
	}
	envelope := publishEnvelope("task-1", "attempt-1", t.TempDir())
	envelope.PayloadJSON = publishPayload(t, id)
	result, err := publisher.Start(ctx, envelope)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops.prepared) != 1 {
		t.Fatalf("prepared = %d, want 1 (stop after UNKNOWN)", len(ops.prepared))
	}
	if len(ops.dispatched) != 1 {
		t.Fatalf("dispatched = %d, want 1", len(ops.dispatched))
	}
	assertPublishEvidence(t, result, `{"slot":"ado.pr.comment:proj/shop#1:a:b:0","operation":"op-1","state":"OUTCOME_UNKNOWN"}`)
}

func TestPublishRejectsContradictoryDecisionBeforePrepare(t *testing.T) {
	tests := []struct {
		name     string
		decision ReviewDecision
	}{
		{
			name: "comment with approve vote",
			decision: ReviewDecision{
				Action: DecisionCommentAction, Vote: "approve",
				Comments: []DecisionComment{{Path: "a.go", Line: 1, Body: "nit"}},
			},
		},
		{
			name: "comment with reject vote",
			decision: ReviewDecision{
				Action: DecisionCommentAction, Vote: "reject",
				Comments: []DecisionComment{{Path: "a.go", Line: 1, Body: "nit"}},
			},
		},
		{
			name: "approve with comments",
			decision: ReviewDecision{
				Action: DecisionApproveAction, Vote: "approve",
				Comments: []DecisionComment{{Path: "a.go", Line: 1, Body: "nit"}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			evidenceStore := newPublishTestEvidenceStore(t)
			id := putDecision(t, evidenceStore, ctx, test.decision)
			ops := &fakePublishOps{ops: map[domain.ID]domain.ExternalOperation{}}
			publisher, err := NewPublisher(PublishConfig{Mode: PublishAll, Operations: ops, Evidence: evidenceStore, RiskApprove: "OWNER"})
			if err != nil {
				t.Fatal(err)
			}
			envelope := publishEnvelope("task-1", "attempt-1", t.TempDir())
			envelope.PayloadJSON = publishPayload(t, id)
			if _, err := publisher.Start(ctx, envelope); err == nil {
				t.Fatal("accepted contradictory decision")
			} else if !strings.Contains(err.Error(), "review decision") {
				t.Fatalf("error = %q, want decision validation error", err)
			}
			if len(ops.prepared) != 0 {
				t.Fatalf("prepared = %d, want 0", len(ops.prepared))
			}
		})
	}
}

func TestPublishReturnsAccumulatedEvidenceOnPrepareFailure(t *testing.T) {
	ctx := context.Background()
	evidenceStore := newPublishTestEvidenceStore(t)
	id := putDecision(t, evidenceStore, ctx, ReviewDecision{Action: DecisionCommentAction, Comments: []DecisionComment{
		{Path: "a.go", Line: 1, Body: "first"},
		{Path: "b.go", Line: 2, Body: "second"},
	}})
	ops := &fakePublishOps{
		ops:          map[domain.ID]domain.ExternalOperation{},
		prepareErrAt: 2,
		prepareErr:   errors.New("prepare failed"),
	}
	publisher, err := NewPublisher(PublishConfig{Mode: PublishComments, Operations: ops, Evidence: evidenceStore})
	if err != nil {
		t.Fatal(err)
	}
	envelope := publishEnvelope("task-1", "attempt-1", t.TempDir())
	envelope.PayloadJSON = publishPayload(t, id)
	result, err := publisher.Start(ctx, envelope)
	if err == nil || !strings.Contains(err.Error(), "prepare") {
		t.Fatalf("error = %v, want prepare failure", err)
	}
	if len(ops.dispatched) != 1 {
		t.Fatalf("dispatched = %d, want 1", len(ops.dispatched))
	}
	assertPublishEvidence(t, result, `{"slot":"ado.pr.comment:proj/shop#1:a:b:0","operation":"op-1","state":"CONFIRMED_EFFECT"}`)
}

func TestPublishReturnsAccumulatedEvidenceOnDispatchFailure(t *testing.T) {
	ctx := context.Background()
	evidenceStore := newPublishTestEvidenceStore(t)
	id := putDecision(t, evidenceStore, ctx, ReviewDecision{Action: DecisionCommentAction, Comments: []DecisionComment{
		{Path: "a.go", Line: 1, Body: "first"},
		{Path: "b.go", Line: 2, Body: "second"},
	}})
	ops := &fakePublishOps{
		ops:           map[domain.ID]domain.ExternalOperation{},
		dispatchErrAt: 2,
		dispatchErr:   errors.New("dispatch failed"),
	}
	publisher, err := NewPublisher(PublishConfig{Mode: PublishComments, Operations: ops, Evidence: evidenceStore})
	if err != nil {
		t.Fatal(err)
	}
	envelope := publishEnvelope("task-1", "attempt-1", t.TempDir())
	envelope.PayloadJSON = publishPayload(t, id)
	result, err := publisher.Start(ctx, envelope)
	if err == nil || !strings.Contains(err.Error(), "dispatch") {
		t.Fatalf("error = %v, want dispatch failure", err)
	}
	if len(ops.dispatched) != 2 {
		t.Fatalf("dispatched = %d, want 2", len(ops.dispatched))
	}
	assertPublishEvidence(t, result, `{"slot":"ado.pr.comment:proj/shop#1:a:b:0","operation":"op-1","state":"CONFIRMED_EFFECT"}`)
}

func TestPublishPrepareUnknownRecordedAndStops(t *testing.T) {
	ctx := context.Background()
	evidenceStore := newPublishTestEvidenceStore(t)
	id := putDecision(t, evidenceStore, ctx, ReviewDecision{Action: DecisionCommentAction, Comments: []DecisionComment{
		{Path: "a.go", Line: 1, Body: "first"},
		{Path: "b.go", Line: 2, Body: "second"},
	}})
	ops := &fakePublishOps{ops: map[domain.ID]domain.ExternalOperation{}, prepareState: domain.OperationOutcomeUnknown}
	publisher, err := NewPublisher(PublishConfig{Mode: PublishComments, Operations: ops, Evidence: evidenceStore})
	if err != nil {
		t.Fatal(err)
	}
	envelope := publishEnvelope("task-1", "attempt-1", t.TempDir())
	envelope.PayloadJSON = publishPayload(t, id)
	result, err := publisher.Start(ctx, envelope)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops.prepared) != 1 {
		t.Fatalf("prepared = %d, want 1", len(ops.prepared))
	}
	if len(ops.dispatched) != 0 {
		t.Fatalf("dispatched = %d, want 0", len(ops.dispatched))
	}
	assertPublishEvidence(t, result, `{"slot":"ado.pr.comment:proj/shop#1:a:b:0","operation":"op-1","state":"OUTCOME_UNKNOWN"}`)
}

func TestDecodePublishPayloadRejectsUnknownAndTrailingValues(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
		reason  string
	}{
		{
			name:    "unknown field",
			payload: `{"decision":"decision","caseID":"case","workID":"work","project":"proj","repo":"shop","pr":1,"revision":"a:b","unexpected":true}`,
			reason:  "unknown field",
		},
		{
			name:    "trailing value",
			payload: `{"decision":"decision","caseID":"case","workID":"work","project":"proj","repo":"shop","pr":1,"revision":"a:b"} {}`,
			reason:  "trailer",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodePublishPayload(json.RawMessage(test.payload)); err == nil {
				t.Fatal("accepted non-strict publish payload")
			} else if !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("error = %q, want reason containing %q", err, test.reason)
			}
		})
	}
}

func TestPublishRejectsBadPayload(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC))
	evidenceStore, err := evidence.New(store, t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	ops := &fakePublishOps{ops: map[domain.ID]domain.ExternalOperation{}}
	publisher, err := NewPublisher(PublishConfig{Mode: PublishAll, Operations: ops, Evidence: evidenceStore, RiskComment: "LOW", RiskApprove: "OWNER"})
	if err != nil {
		t.Fatal(err)
	}
	envelope := publishEnvelope("task-1", "attempt-1", t.TempDir())
	raw, _ := json.Marshal(PublishPayload{CaseID: "case-1", WorkID: "work-1", Project: "proj", Repo: "shop", PR: 1, Revision: "a:b"})
	envelope.PayloadJSON = raw
	if _, err := publisher.Start(ctx, envelope); err == nil {
		t.Fatal("accepted blank decision")
	}
	holdID := putDecision(t, evidenceStore, ctx, ReviewDecision{Action: DecisionHoldAction, Reason: "uncertain"})
	envelope.PayloadJSON = publishPayload(t, holdID)
	result, err := publisher.Start(ctx, envelope)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops.prepared) != 0 {
		t.Fatalf("prepared = %d, want 0", len(ops.prepared))
	}
	assertPublishEvidence(t, result, `{"slot":"hold","recorded-only":true}`)
}
