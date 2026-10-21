package adoeffects

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/approvals"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/SofiaFlux/summa42/internal/policy"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/resources"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

type fakeDial struct {
	calls []mcpCall
	pages []map[string]any
	err   error
}

type mcpCall struct {
	tool string
	args map[string]any
}

func (f *fakeDial) dial(context.Context) (callToolFunc, error) {
	return func(_ context.Context, tool string, args map[string]any) (map[string]any, error) {
		f.calls = append(f.calls, mcpCall{tool, args})
		if f.err != nil {
			return nil, f.err
		}
		if len(f.pages) == 0 {
			return map[string]any{"ok": true}, nil
		}
		page := f.pages[0]
		f.pages = f.pages[1:]
		return page, nil
	}, nil
}

func TestCommentIntentCanonicalStable(t *testing.T) {
	p := &CommentProvider{}
	a, err := p.CanonicalIntent(CommentIntent{Project: "proj", Repository: "shop", PR: 7, Path: "main.go", Line: 3, Body: "nit", Marker: "[summa42:c:w]", SourceCommit: "src1", TargetCommit: "tgt1"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.CanonicalIntent(CommentIntent{Project: "proj", Repository: "shop", PR: 7, Path: "main.go", Line: 3, Body: "nit", Marker: "[summa42:c:w]", SourceCommit: "src1", TargetCommit: "tgt1"})
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("canonical unstable:\n%s\n%s", a, b)
	}
	var decoded map[string]any
	if err := json.Unmarshal(a, &decoded); err != nil {
		t.Fatal(err)
	}
}

func TestProvidersRejectForeignIntents(t *testing.T) {
	comment := &CommentProvider{}
	if _, err := comment.CanonicalIntent(VoteIntent{PR: 1, Vote: 10, SourceCommit: "s", TargetCommit: "t", ReviewerID: "me"}); err == nil {
		t.Fatal("comment provider accepted vote intent")
	}
	vote := &VoteProvider{}
	if _, err := vote.CanonicalIntent(CommentIntent{Body: "x"}); err == nil {
		t.Fatal("vote provider accepted comment intent")
	}
	if _, err := vote.CanonicalIntent(VoteIntent{Project: "p", Repository: "r", PR: 1, Vote: -10, SourceCommit: "s", TargetCommit: "t", ReviewerID: "me"}); err == nil {
		t.Fatal("vote provider accepted rejection vote")
	}
	if vote.Capability() != "ado.pr.approve" || comment.Capability() != "ado.pr.comment" {
		t.Fatalf("capabilities = %q %q", vote.Capability(), comment.Capability())
	}
}

func stubRead(context.Context, string, any) (any, error) {
	return map[string]any{"sourceCommit": "src1"}, nil
}

func TestDispatchCallsWriteTools(t *testing.T) {
	commentDial := &fakeDial{pages: []map[string]any{{"threadId": 9}}}
	comment := &CommentProvider{
		config: Config{Command: "/bin/true", Organization: "Contoso", Timeout: time.Minute},
		dial:   commentDial.dial,
		read:   stubRead,
	}
	canonical, err := comment.CanonicalIntent(CommentIntent{Project: "proj", Repository: "shop", PR: 7, Path: "main.go", Line: 3, Body: "nit", Marker: "[summa42:c:w]", SourceCommit: "src1", TargetCommit: "tgt1"})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := comment.Dispatch(context.Background(), operations.ProviderDispatchRequest{
		OperationID: "op-1", TaskID: "task-1", EffectSlotID: "slot-1",
		IntentFingerprint: "fp", IntentRevision: 1, CanonicalIntent: canonical,
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.State != domain.OperationConfirmedEffect && outcome.State != domain.OperationOutcomeUnknown {
		t.Fatalf("state = %q", outcome.State)
	}
	if len(commentDial.calls) != 1 {
		t.Fatalf("calls = %v, want exactly 1", commentDial.calls)
	}
	call := commentDial.calls[0]
	if call.tool != "repo_pull_request_thread_write" || call.args["action"] != "create" {
		t.Fatalf("call = %+v", call)
	}
	voteDial := &fakeDial{pages: []map[string]any{{"vote": 10}}}
	vote := &VoteProvider{
		config: Config{Command: "/bin/true", Organization: "Contoso", Timeout: time.Minute},
		dial:   voteDial.dial,
		read:   stubRead,
	}
	voteCanonical, err := vote.CanonicalIntent(VoteIntent{Project: "proj", Repository: "shop", PR: 7, Vote: 10, SourceCommit: "src1", TargetCommit: "tgt1", ReviewerID: "me"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vote.Dispatch(context.Background(), operations.ProviderDispatchRequest{
		OperationID: "op-2", TaskID: "task-1", EffectSlotID: "slot-2",
		IntentFingerprint: "fp2", IntentRevision: 1, CanonicalIntent: voteCanonical,
	}); err != nil {
		t.Fatal(err)
	}
	if len(voteDial.calls) != 1 || voteDial.calls[0].tool != "repo_pull_request_write" || voteDial.calls[0].args["action"] != "vote" {
		t.Fatalf("calls = %+v", voteDial.calls)
	}
}

func threadsPayload(contents ...string) map[string]any {
	comments := make([]any, 0, len(contents))
	for _, content := range contents {
		comments = append(comments, map[string]any{"content": content})
	}
	return map[string]any{"threads": []any{map[string]any{"comments": comments}}}
}

func mustCommentProvider(t *testing.T, read ReadFunc) *CommentProvider {
	t.Helper()
	provider, err := NewCommentProvider(Config{Command: "/bin/true", Organization: "Contoso"}, read)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func mustVoteProvider(t *testing.T, read ReadFunc) *VoteProvider {
	t.Helper()
	provider, err := NewVoteProvider(Config{Command: "/bin/true", Organization: "Contoso"}, read)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func TestCommentLookupFindsMarker(t *testing.T) {
	marker := "[summa42:c:w]"
	intent := CommentIntent{Project: "proj", Repository: "shop", PR: 7, Path: "main.go", Line: 3, Body: "nit", Marker: marker, SourceCommit: "src1", TargetCommit: "tgt1"}

	found := mustCommentProvider(t, func(context.Context, string, any) (any, error) {
		return threadsPayload("nit\n" + marker), nil
	})
	canonical, err := found.CanonicalIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	request := operations.ProviderDispatchRequest{
		OperationID: "op-1", TaskID: "task-1", EffectSlotID: "slot-1",
		IntentFingerprint: "fp", IntentRevision: 1, CanonicalIntent: canonical,
	}
	outcome, err := found.LookupOutcome(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.State != domain.OperationConfirmedEffect {
		t.Fatalf("state = %q, want CONFIRMED_EFFECT", outcome.State)
	}

	absent := mustCommentProvider(t, func(context.Context, string, any) (any, error) {
		return threadsPayload("unrelated comment"), nil
	})
	outcome, err = absent.LookupOutcome(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.State != domain.OperationOutcomeUnknown {
		t.Fatalf("state = %q, want OUTCOME_UNKNOWN", outcome.State)
	}

	failing := mustCommentProvider(t, func(context.Context, string, any) (any, error) {
		return nil, errors.New("transport boom")
	})
	outcome, err = failing.LookupOutcome(context.Background(), request)
	if err != nil {
		t.Fatalf("read error must surface as UNKNOWN with nil error, got %v", err)
	}
	if outcome.State != domain.OperationOutcomeUnknown {
		t.Fatalf("state = %q, want OUTCOME_UNKNOWN", outcome.State)
	}
}

func TestVoteLookupConfirmsMatchingVote(t *testing.T) {
	intent := VoteIntent{Project: "proj", Repository: "shop", PR: 7, Vote: 10, SourceCommit: "src1", TargetCommit: "tgt1", ReviewerID: "me"}
	assertVoteRead := func(t *testing.T, capability string, request any) {
		t.Helper()
		if capability != "ado.pr.get" {
			t.Fatalf("capability = %q, want ado.pr.get", capability)
		}
		args, ok := request.(map[string]any)
		if !ok {
			t.Fatalf("request type = %T, want map[string]any", request)
		}
		if args["action"] != "get" {
			t.Fatalf("action = %v, want get", args["action"])
		}
	}
	matching := mustVoteProvider(t, func(_ context.Context, capability string, request any) (any, error) {
		assertVoteRead(t, capability, request)
		return map[string]any{"sourceCommit": "src1", "reviewers": []any{map[string]any{"id": "me", "vote": float64(10)}}}, nil
	})
	canonical, err := matching.CanonicalIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	request := operations.ProviderDispatchRequest{
		OperationID: "op-1", TaskID: "task-1", EffectSlotID: "slot-1",
		IntentFingerprint: "fp", IntentRevision: 1, CanonicalIntent: canonical,
	}
	outcome, err := matching.LookupOutcome(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.State != domain.OperationConfirmedEffect {
		t.Fatalf("state = %q, want CONFIRMED_EFFECT", outcome.State)
	}

	zero := mustVoteProvider(t, func(_ context.Context, capability string, request any) (any, error) {
		assertVoteRead(t, capability, request)
		return map[string]any{"reviewers": []any{map[string]any{"id": "me", "vote": float64(0)}}}, nil
	})
	outcome, err = zero.LookupOutcome(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.State != domain.OperationOutcomeUnknown {
		t.Fatalf("state = %q, want OUTCOME_UNKNOWN", outcome.State)
	}

	failing := mustVoteProvider(t, func(_ context.Context, capability string, request any) (any, error) {
		assertVoteRead(t, capability, request)
		return nil, errors.New("transport boom")
	})
	outcome, err = failing.LookupOutcome(context.Background(), request)
	if err != nil {
		t.Fatalf("read error must surface as UNKNOWN with nil error, got %v", err)
	}
	if outcome.State != domain.OperationOutcomeUnknown {
		t.Fatalf("state = %q, want OUTCOME_UNKNOWN", outcome.State)
	}
}

// allowPolicy mirrors the fake-policy allow path in
// internal/operations/service_test.go: a fixed PolicyAllow decision whose
// provenance matches the policy_test profile seeded below.
type allowPolicy struct{}

func (allowPolicy) Evaluate(_ context.Context, in policy.PolicyInput) (domain.PolicyDecision, error) {
	return domain.PolicyDecision{
		ID:                     domain.NewID("decision"),
		Outcome:                domain.PolicyAllow,
		PolicySetID:            domain.ID("policy_test"),
		PolicySetHash:          "policy-v1",
		PolicyCapabilitiesHash: "caps_test",
		InputDigest:            "input_test",
		EvaluatedAt:            in.Now,
	}, nil
}

func TestCommentHarnessRoundTripWithSlotDedup(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 15, 16, 0, 0, 0, time.UTC))
	purposes := purpose.New(store, clk)
	execSvc := execution.New(store, clk, purposes)
	ledger := resources.New(store, clk)
	approvalSvc := approvals.New(store, clk)

	marker := "[summa42:c:w]"
	commentDial := &fakeDial{pages: []map[string]any{{"threadId": 42}}}
	provider, err := NewCommentProvider(Config{Command: "/bin/true", Organization: "Contoso"}, func(context.Context, string, any) (any, error) {
		payload := threadsPayload("nit\n" + marker)
		payload["sourceCommit"] = "src1"
		return payload, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	provider.dial = commentDial.dial

	collectiveID := domain.ID("collective_test")
	svc := operations.New(store, clk, execSvc, allowPolicy{}, ledger, approvalSvc, collectiveID, provider)

	now := clk.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO policy_sets(
			policy_set_id, version, module_name, module, policy_hash, capabilities_hash, active, created_at
		) VALUES ('policy_test', 1, 'test.rego', 'package test', 'policy-v1', 'caps_test', 1, ?)`, now,
	); err != nil {
		t.Fatal(err)
	}
	envelopeID := domain.NewID("envelope")
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, 1000, ?)`,
		envelopeID, now,
	); err != nil {
		t.Fatal(err)
	}
	taskID := domain.NewID("task")
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO tasks(
			task_id, purpose_kind, purpose_id, state, current_fence,
			acceptance_criteria_json, required_capabilities_json, required_enforcement,
			authority_ceiling_json, resource_envelope_id, priority, created_at, updated_at
		) VALUES (?, 'OWNER_DIRECTIVE', 'owner-test', 'ELIGIBLE', 0, '[]', '[]', 'ENFORCED', ?, ?, 0, ?, ?)`,
		taskID, `["`+provider.Capability()+`"]`, envelopeID, now, now,
	); err != nil {
		t.Fatal(err)
	}
	attempt, err := execSvc.StartAttempt(ctx, taskID, "test-executor", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	intent := CommentIntent{Project: "proj", Repository: "shop", PR: 7, Path: "main.go", Line: 3, Body: "nit", Marker: marker, SourceCommit: "src1", TargetCommit: "tgt1"}
	op, err := svc.Prepare(ctx, operations.PrepareRequest{
		AttemptID:      attempt.ID,
		Provider:       provider.Name(),
		TrustedSlotKey: "comment-primary",
		Intent:         intent,
		Risk:           "LOW",
	})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := svc.Dispatch(ctx, op.ID, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != domain.OperationConfirmedEffect {
		t.Fatalf("state = %s, want CONFIRMED_EFFECT", settled.State)
	}

	again, err := svc.Prepare(ctx, operations.PrepareRequest{
		AttemptID:      attempt.ID,
		Provider:       provider.Name(),
		TrustedSlotKey: "comment-primary",
		Intent:         intent,
		Risk:           "LOW",
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != op.ID {
		t.Fatalf("second prepare minted %s, want slot-deduped %s", again.ID, op.ID)
	}
	resettled, err := svc.Dispatch(ctx, again.ID, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resettled.State != domain.OperationConfirmedEffect {
		t.Fatalf("state = %s, want CONFIRMED_EFFECT", resettled.State)
	}
	if len(commentDial.calls) != 1 {
		t.Fatalf("dial calls = %d, want exactly 1 (slot dedup)", len(commentDial.calls))
	}
}

func TestVoteHarnessRoundTripWithSlotDedup(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 15, 16, 0, 0, 0, time.UTC))
	purposes := purpose.New(store, clk)
	execSvc := execution.New(store, clk, purposes)
	ledger := resources.New(store, clk)
	approvalSvc := approvals.New(store, clk)

	voteDial := &fakeDial{pages: []map[string]any{{"id": "vote-1"}}}
	provider, err := NewVoteProvider(Config{Command: "/bin/true", Organization: "Contoso"}, func(context.Context, string, any) (any, error) {
		return map[string]any{"sourceCommit": "src1", "reviewers": []any{map[string]any{"id": "me", "vote": float64(10)}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	provider.dial = voteDial.dial

	collectiveID := domain.ID("collective_test")
	svc := operations.New(store, clk, execSvc, allowPolicy{}, ledger, approvalSvc, collectiveID, provider)

	now := clk.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO policy_sets(
			policy_set_id, version, module_name, module, policy_hash, capabilities_hash, active, created_at
		) VALUES ('policy_test', 1, 'test.rego', 'package test', 'policy-v1', 'caps_test', 1, ?)`, now,
	); err != nil {
		t.Fatal(err)
	}
	envelopeID := domain.NewID("envelope")
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, 1000, ?)`,
		envelopeID, now,
	); err != nil {
		t.Fatal(err)
	}
	taskID := domain.NewID("task")
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO tasks(
			task_id, purpose_kind, purpose_id, state, current_fence,
			acceptance_criteria_json, required_capabilities_json, required_enforcement,
			authority_ceiling_json, resource_envelope_id, priority, created_at, updated_at
		) VALUES (?, 'OWNER_DIRECTIVE', 'owner-test', 'ELIGIBLE', 0, '[]', '[]', 'ENFORCED', ?, ?, 0, ?, ?)`,
		taskID, `["`+provider.Capability()+`"]`, envelopeID, now, now,
	); err != nil {
		t.Fatal(err)
	}
	attempt, err := execSvc.StartAttempt(ctx, taskID, "test-executor", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	intent := VoteIntent{Project: "proj", Repository: "shop", PR: 7, Vote: 10, SourceCommit: "src1", TargetCommit: "tgt1", ReviewerID: "me"}
	op, err := svc.Prepare(ctx, operations.PrepareRequest{
		AttemptID:      attempt.ID,
		Provider:       provider.Name(),
		TrustedSlotKey: "vote-primary",
		Intent:         intent,
		Risk:           "LOW",
	})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := svc.Dispatch(ctx, op.ID, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != domain.OperationConfirmedEffect {
		t.Fatalf("state = %s, want CONFIRMED_EFFECT", settled.State)
	}

	again, err := svc.Prepare(ctx, operations.PrepareRequest{
		AttemptID:      attempt.ID,
		Provider:       provider.Name(),
		TrustedSlotKey: "vote-primary",
		Intent:         intent,
		Risk:           "LOW",
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != op.ID {
		t.Fatalf("second prepare minted %s, want slot-deduped %s", again.ID, op.ID)
	}
	resettled, err := svc.Dispatch(ctx, again.ID, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resettled.State != domain.OperationConfirmedEffect {
		t.Fatalf("state = %s, want CONFIRMED_EFFECT", resettled.State)
	}
	if len(voteDial.calls) != 1 {
		t.Fatalf("dial calls = %d, want exactly 1 (slot dedup)", len(voteDial.calls))
	}
}

func voteTestRequest(t *testing.T, p *VoteProvider, intent VoteIntent) operations.ProviderDispatchRequest {
	t.Helper()
	canonical, err := p.CanonicalIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	return operations.ProviderDispatchRequest{OperationID: "op-1", TaskID: "task-1", EffectSlotID: "slot-1",
		IntentFingerprint: "fp", IntentRevision: 1, CanonicalIntent: canonical}
}

func TestVoteLookupIgnoresOtherReviewersApproval(t *testing.T) {
	intent := VoteIntent{Project: "proj", Repository: "shop", PR: 7, Vote: 10, SourceCommit: "src1", TargetCommit: "tgt1", ReviewerID: "me"}
	other := mustVoteProvider(t, func(context.Context, string, any) (any, error) {
		return map[string]any{"reviewers": []any{
			map[string]any{"id": "someone-else", "vote": float64(10)},
			map[string]any{"id": "me", "vote": float64(0)},
		}}, nil
	})
	outcome, err := other.LookupOutcome(context.Background(), voteTestRequest(t, other, intent))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.State != domain.OperationOutcomeUnknown {
		t.Fatalf("state = %q, want OUTCOME_UNKNOWN when only another reviewer approved", outcome.State)
	}
}

func TestVoteDispatchRefusesMovedSourceCommit(t *testing.T) {
	dial := &fakeDial{}
	vote := mustVoteProvider(t, func(context.Context, string, any) (any, error) {
		return map[string]any{"sourceCommit": "moved"}, nil
	})
	vote.dial = dial.dial
	request := voteTestRequest(t, vote, VoteIntent{Project: "proj", Repository: "shop", PR: 7, Vote: 10, SourceCommit: "src1", TargetCommit: "tgt1", ReviewerID: "me"})
	outcome, err := vote.Dispatch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.State != domain.OperationConfirmedNoEffect || len(dial.calls) != 0 {
		t.Fatalf("state = %q calls = %d, want CONFIRMED_NO_EFFECT with no write", outcome.State, len(dial.calls))
	}
}

func TestVoteIntentRejectsOldShape(t *testing.T) {
	vote := mustVoteProvider(t, stubRead)
	old := []byte(`{"project":"proj","repository":"shop","pr":7,"vote":10}`)
	request := operations.ProviderDispatchRequest{CanonicalIntent: old}
	if _, err := vote.Dispatch(context.Background(), request); err == nil {
		t.Fatal("dispatch accepted an old-shape vote intent")
	}
	if _, err := vote.LookupOutcome(context.Background(), request); err == nil {
		t.Fatal("lookup accepted an old-shape vote intent")
	}
}

func TestCommentSendsThreadContext(t *testing.T) {
	dial := &fakeDial{pages: []map[string]any{{"threadId": 9}}}
	comment := mustCommentProvider(t, stubRead)
	comment.dial = dial.dial
	canonical, err := comment.CanonicalIntent(CommentIntent{Project: "proj", Repository: "shop", PR: 7, Path: "main.go", Line: 3, Body: "nit", Marker: "[m]", SourceCommit: "src1", TargetCommit: "tgt1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := comment.Dispatch(context.Background(), operations.ProviderDispatchRequest{CanonicalIntent: canonical}); err != nil {
		t.Fatal(err)
	}
	if len(dial.calls) != 1 {
		t.Fatalf("calls = %d", len(dial.calls))
	}
	ctxArg, ok := dial.calls[0].args["threadContext"].(map[string]any)
	if !ok || ctxArg["filePath"] != "/main.go" {
		t.Fatalf("threadContext = %#v", dial.calls[0].args["threadContext"])
	}
	start, _ := ctxArg["rightFileStart"].(map[string]any)
	if start["line"] != int64(3) {
		t.Fatalf("rightFileStart = %#v", ctxArg["rightFileStart"])
	}
}

func TestVoteDispatchWrongReviewerReadbackIsUnknown(t *testing.T) {
	dial := &fakeDial{pages: []map[string]any{{"id": "v1"}}}
	vote := mustVoteProvider(t, func(context.Context, string, any) (any, error) {
		return map[string]any{"sourceCommit": "src1", "reviewers": []any{map[string]any{"id": "other", "vote": float64(10)}}}, nil
	})
	vote.dial = dial.dial
	request := voteTestRequest(t, vote, VoteIntent{Project: "proj", Repository: "shop", PR: 7, Vote: 10, SourceCommit: "src1", TargetCommit: "tgt1", ReviewerID: "me"})
	outcome, err := vote.Dispatch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(dial.calls) != 1 || outcome.State != domain.OperationOutcomeUnknown {
		t.Fatalf("state = %q calls = %d, want OUTCOME_UNKNOWN after one write", outcome.State, len(dial.calls))
	}
}

func TestDispatchUnreadablePRErrorsWithoutWrite(t *testing.T) {
	failing := func(context.Context, string, any) (any, error) { return nil, errors.New("boom") }
	dial := &fakeDial{}
	vote := mustVoteProvider(t, failing)
	vote.dial = dial.dial
	if _, err := vote.Dispatch(context.Background(), voteTestRequest(t, vote, VoteIntent{Project: "p", Repository: "r", PR: 7, Vote: 10, SourceCommit: "s", TargetCommit: "t", ReviewerID: "me"})); err == nil {
		t.Fatal("vote dispatch succeeded with unreadable PR")
	}
	comment := mustCommentProvider(t, failing)
	comment.dial = dial.dial
	canonical, err := comment.CanonicalIntent(CommentIntent{Project: "p", Repository: "r", PR: 7, Body: "x", Marker: "[m]", SourceCommit: "s", TargetCommit: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := comment.Dispatch(context.Background(), operations.ProviderDispatchRequest{CanonicalIntent: canonical}); err == nil {
		t.Fatal("comment dispatch succeeded with unreadable PR")
	}
	if len(dial.calls) != 0 {
		t.Fatalf("writes = %d, want 0", len(dial.calls))
	}
}

func TestCommentDispatchRefusesMovedSourceCommit(t *testing.T) {
	dial := &fakeDial{}
	comment := mustCommentProvider(t, func(context.Context, string, any) (any, error) {
		return map[string]any{"sourceCommit": "moved"}, nil
	})
	comment.dial = dial.dial
	canonical, err := comment.CanonicalIntent(CommentIntent{Project: "p", Repository: "r", PR: 7, Body: "x", Marker: "[m]", SourceCommit: "src1", TargetCommit: "t"})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := comment.Dispatch(context.Background(), operations.ProviderDispatchRequest{CanonicalIntent: canonical})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.State != domain.OperationConfirmedNoEffect || len(dial.calls) != 0 {
		t.Fatalf("state = %q calls = %d", outcome.State, len(dial.calls))
	}
}
