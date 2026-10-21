package adoreview

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/purpose"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/testutil"
	"github.com/SofiaFlux/summa42/internal/workflow"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

func setupObserve(t *testing.T) (context.Context, *state.Store, *workflowcase.Service, *execution.Service, *evidence.Store, Config) {
	t.Helper()
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC))
	purposes := purpose.New(store, clk)
	execSvc := execution.New(store, clk, purposes)
	evidenceStore, err := evidence.New(store, t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	cases := workflowcase.New(store, clk, purposes)
	mission, err := purposes.CreateMission(ctx, "ado review")
	if err != nil {
		t.Fatal(err)
	}
	envelope := domain.NewID("envelope")
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, ?, ?)`,
		envelope, 100, clk.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	cfg := Config{MissionID: mission, ReviewerID: "me",
		Grant:              workflow.Grant{Capabilities: []string{"read"}},
		ResourceEnvelopeID: envelope, MaxSteps: 3, RemainingBudget: 10}
	return ctx, store, cases, execSvc, evidenceStore, cfg
}

func goodPRItem() map[string]any {
	return map[string]any{
		"repository": "shop", "number": float64(1),
		"sourceCommit": "a", "targetCommit": "b",
		"author":    "u9",
		"reviewers": []any{map[string]any{"id": "me"}},
	}
}

func draftPRItem() map[string]any {
	return map[string]any{
		"repository": "shop", "number": float64(2),
		"sourceCommit": "c", "targetCommit": "d",
		"author":    "u9",
		"reviewers": []any{map[string]any{"id": "me"}},
		"isDraft":   true,
	}
}

func snapshotCount(t *testing.T, store *state.Store, ctx context.Context) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM evidence_objects WHERE kind = 'ado.pr.snapshot'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestObserveOnceMaterializesNewRevision(t *testing.T) {
	ctx, _, cases, execSvc, evidenceStore, cfg := setupObserve(t)
	caller := &fakeCaller{pages: []any{map[string]any{"prs": []any{goodPRItem()}}}}
	result, err := ObserveOnce(ctx, caller, cases, execSvc, evidenceStore, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Ensured) != 1 || len(result.Materialized) != 1 {
		t.Fatalf("result = %+v, want 1 ensured 1 materialized", result)
	}
	if len(result.Excluded) != 0 || len(result.Failed) != 0 {
		t.Fatalf("result = %+v, want 0 excluded 0 failed", result)
	}
	task, err := execSvc.Task(ctx, result.Materialized[0])
	if err != nil {
		t.Fatal(err)
	}
	if task.Objective != "Review ADO PR shop#1" {
		t.Fatalf("objective = %q, want %q", task.Objective, "Review ADO PR shop#1")
	}
	if task.State != domain.TaskEligible {
		t.Fatalf("task state = %q, want ELIGIBLE (not leased)", task.State)
	}
	if task.ResourceEnvelopeID != cfg.ResourceEnvelopeID {
		t.Fatalf("envelope = %q, want %q", task.ResourceEnvelopeID, cfg.ResourceEnvelopeID)
	}
}

func TestObserveOnceReusesCaseWithoutNewEvidence(t *testing.T) {
	ctx, store, cases, execSvc, evidenceStore, cfg := setupObserve(t)
	caller := &fakeCaller{pages: []any{map[string]any{"prs": []any{goodPRItem()}}}}
	first, err := ObserveOnce(ctx, caller, cases, execSvc, evidenceStore, cfg)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotCount(t, store, ctx)
	caller.SetPages([]any{map[string]any{"prs": []any{goodPRItem()}}})
	second, err := ObserveOnce(ctx, caller, cases, execSvc, evidenceStore, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Ensured) != 1 || second.Ensured[0] != first.Ensured[0] {
		t.Fatalf("second ensured = %v, want [%s]", second.Ensured, first.Ensured[0])
	}
	if len(second.Materialized) != 1 || second.Materialized[0] != first.Materialized[0] {
		t.Fatalf("second materialized = %v, want same task %v", second.Materialized, first.Materialized)
	}
	if after := snapshotCount(t, store, ctx); after != before {
		t.Fatalf("evidence snapshots grew %d -> %d, want unchanged", before, after)
	}
}

func TestObserveOnceExcludesAndRepairsPartialCase(t *testing.T) {
	ctx, _, cases, execSvc, evidenceStore, cfg := setupObserve(t)
	// Partial case: Ensure-without-Task for the good PR revision (a:b).
	partial, err := cases.Ensure(ctx, workflowcase.Observation{
		MissionID: cfg.MissionID, Source: "ado", ObjectID: "shop#1", RevisionID: "a:b",
		EvidenceID: "ev-partial",
		FirstWork:  workflow.WorkProposal{Kind: "ado.pr.review", RequiredCapabilities: []string{"read"}, AuthorityCeiling: []string{"read"}},
		Grant:      cfg.Grant, MaxSteps: cfg.MaxSteps, RemainingBudget: cfg.RemainingBudget,
	})
	if err != nil {
		t.Fatal(err)
	}
	caller := &fakeCaller{pages: []any{map[string]any{"prs": []any{draftPRItem(), goodPRItem()}}}}
	result, err := ObserveOnce(ctx, caller, cases, execSvc, evidenceStore, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Excluded) != 1 || result.Excluded[0].Reason != ReasonDraft {
		t.Fatalf("excluded = %+v, want one draft", result.Excluded)
	}
	if len(result.Ensured) != 1 || result.Ensured[0] != partial.ID {
		t.Fatalf("ensured = %v, want [%s] (no new case)", result.Ensured, partial.ID)
	}
	if len(result.Materialized) != 1 {
		t.Fatalf("materialized = %v, want 1 repaired task", result.Materialized)
	}
	if len(result.Failed) != 0 {
		t.Fatalf("failed = %+v, want empty", result.Failed)
	}
}

func TestRunStopsOnCancelWithoutNewPoll(t *testing.T) {
	ctx, _, cases, execSvc, evidenceStore, cfg := setupObserve(t)
	_ = ctx
	caller := &fakeCaller{pages: []any{map[string]any{"prs": []any{}}}}
	runCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(runCtx, caller, cases, execSvc, evidenceStore, cfg, time.Minute); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if len(caller.Calls()) != 0 {
		t.Fatalf("caller calls = %d, want 0 (cancelled before first poll)", len(caller.Calls()))
	}
}

func TestRunPollsThenStops(t *testing.T) {
	ctx, store, cases, execSvc, evidenceStore, cfg := setupObserve(t)
	_ = ctx
	page := map[string]any{"prs": []any{goodPRItem()}}
	caller := &fakeCaller{pages: []any{page, page}}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(runCtx, caller, cases, execSvc, evidenceStore, cfg, 5*time.Millisecond)
	}()
	deadline := time.Now().Add(10 * time.Second)
	sawSecondTick := false
	for {
		if caller.CallCount() >= 2 {
			sawSecondTick = true
		}
		foundCase, found, err := cases.Find(context.Background(), cfg.MissionID, "ado", "shop#1", "a:b")
		if err != nil {
			t.Fatal(err)
		}
		if found && sawSecondTick {
			var taskID domain.ID
			var state string
			err := store.DB().QueryRowContext(context.Background(),
				`SELECT task_id, state FROM tasks WHERE idempotency_key = ?`, string(foundCase.CurrentWorkID)).Scan(&taskID, &state)
			if err == nil {
				task, err := execSvc.Task(context.Background(), taskID)
				if err != nil {
					t.Fatal(err)
				}
				if task.State == domain.TaskEligible {
					break
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for ELIGIBLE task (calls=%d found=%v secondTick=%v)", caller.CallCount(), found, sawSecondTick)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for Run to stop")
	}
	if caller.CallCount() == 0 {
		t.Fatal("caller was never called, want at least one poll")
	}
	if after := snapshotCount(t, store, context.Background()); after != 1 {
		t.Fatalf("evidence snapshots = %d, want 1 (second tick must reuse the case)", after)
	}
}

func TestRunAbortsOnObserveError(t *testing.T) {
	ctx, _, cases, execSvc, evidenceStore, cfg := setupObserve(t)
	_ = ctx
	caller := &fakeCaller{err: errors.New("ado unavailable")}
	if err := Run(context.Background(), caller, cases, execSvc, evidenceStore, cfg, time.Minute); err == nil {
		t.Fatal("expected Run to return the ObserveOnce error")
	} else if !errors.Is(err, caller.Err()) && err.Error() != "ado unavailable" {
		t.Fatalf("Run = %v, want the caller error", err)
	}
	if len(caller.Calls()) != 1 {
		t.Fatalf("caller calls = %d, want 1 (abort after first failed poll)", len(caller.Calls()))
	}
}

func TestRunRejectsNonPositiveInterval(t *testing.T) {
	ctx, _, cases, execSvc, evidenceStore, cfg := setupObserve(t)
	_ = ctx
	caller := &fakeCaller{pages: []any{map[string]any{"prs": []any{}}}}
	if err := Run(context.Background(), caller, cases, execSvc, evidenceStore, cfg, 0); err == nil {
		t.Fatal("expected error for non-positive poll interval")
	}
	if len(caller.Calls()) != 0 {
		t.Fatalf("caller calls = %d, want 0 (interval guard before first poll)", len(caller.Calls()))
	}
}

func TestCanonicalEvidenceUsesSpecKeys(t *testing.T) {
	pr := PullRequest{Repository: "shop", Number: 42, SourceCommit: "abc", TargetCommit: "def",
		AuthorID: "u1", Reviewers: []Reviewer{{ID: "u2"}, {ID: "g-eng", IsGroup: true}}}
	raw, err := json.Marshal(canonicalEvidence(pr))
	if err != nil {
		t.Fatal(err)
	}
	var blob map[string]any
	if err := json.Unmarshal(raw, &blob); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range blob {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	wantKeys := []string{"author", "draft", "pr", "repo", "reviewers", "sourceCommit", "targetCommit"}
	if len(keys) != len(wantKeys) {
		t.Fatalf("blob keys = %v, want %v (raw %s)", keys, wantKeys, raw)
	}
	for i := range wantKeys {
		if keys[i] != wantKeys[i] {
			t.Fatalf("blob keys = %v, want %v (raw %s)", keys, wantKeys, raw)
		}
	}
	if blob["repo"] != "shop" || blob["pr"] != float64(42) || blob["sourceCommit"] != "abc" ||
		blob["targetCommit"] != "def" || blob["draft"] != false || blob["author"] != "u1" {
		t.Fatalf("blob scalar fields = %s, want spec values", raw)
	}
	reviewers, ok := blob["reviewers"].([]any)
	if !ok || len(reviewers) != 2 || reviewers[0] != "u2" || reviewers[1] != "g-eng" {
		t.Fatalf("blob reviewers = %v, want [u2 g-eng] string ID list", blob["reviewers"])
	}
}

// Trigger: partial case advanced to READY via Assess, so the Find-first repair
// path calls MaterializeTask on a non-active case and deterministically fails.
func TestObserveOnceMaterializeFailureAfterAssessmentSurfacesFailedAndReusesCase(t *testing.T) {
	ctx, store, cases, execSvc, evidenceStore, cfg := setupObserve(t)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO evidence_objects(evidence_id, content_hash, media_type, kind, size_bytes, created_at) VALUES ('reviewed', 'h', 'application/json', 'TEST', 0, '2026-09-23T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	partial, err := cases.Ensure(ctx, workflowcase.Observation{
		MissionID: cfg.MissionID, Source: "ado", ObjectID: "shop#1", RevisionID: "a:b",
		EvidenceID: "ev-partial",
		FirstWork:  workflow.WorkProposal{Kind: "ado.pr.review", RequiredCapabilities: []string{"read"}, AuthorityCeiling: []string{"read"}},
		Grant:      cfg.Grant, MaxSteps: cfg.MaxSteps, RemainingBudget: cfg.RemainingBudget,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cases.Assess(ctx, workflowcase.AssessmentRequest{CaseID: partial.ID, WorkID: partial.CurrentWorkID,
		Assessment:      workflow.Assessment{Verdict: workflow.Ready, EvidenceIDs: []string{"reviewed"}},
		RemainingBudget: cfg.RemainingBudget - 1, ProgressSignature: "reviewed"}); err != nil {
		t.Fatal(err)
	}
	caller := &fakeCaller{pages: []any{map[string]any{"prs": []any{goodPRItem()}}}}
	result, err := ObserveOnce(ctx, caller, cases, execSvc, evidenceStore, cfg)
	if err != nil {
		t.Fatalf("ObserveOnce = %v, want nil tick error (materialize failure is per-PR Failed)", err)
	}
	if len(result.Failed) != 1 {
		t.Fatalf("failed = %+v, want 1 entry for the advanced case", result.Failed)
	}
	if len(result.Ensured) != 0 || len(result.Materialized) != 0 {
		t.Fatalf("result = %+v, want 0 ensured 0 materialized on materialize failure", result)
	}
	reused, found, err := cases.Find(ctx, cfg.MissionID, "ado", "shop#1", "a:b")
	if err != nil {
		t.Fatal(err)
	}
	if !found || reused.ID != partial.ID {
		t.Fatalf("Find = %+v %v, want reuse of case %s", reused, found, partial.ID)
	}
}

func TestObserveOnceRevisionChangeOpensNewCase(t *testing.T) {
	ctx, _, cases, execSvc, evidenceStore, cfg := setupObserve(t)
	firstCaller := &fakeCaller{pages: []any{map[string]any{"prs": []any{goodPRItem()}}}}
	first, err := ObserveOnce(ctx, firstCaller, cases, execSvc, evidenceStore, cfg)
	if err != nil {
		t.Fatal(err)
	}
	moved := goodPRItem()
	moved["sourceCommit"] = "a"
	moved["targetCommit"] = "c"
	secondCaller := &fakeCaller{pages: []any{map[string]any{"prs": []any{moved}}}}
	second, err := ObserveOnce(ctx, secondCaller, cases, execSvc, evidenceStore, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Ensured) != 1 || len(second.Ensured) != 1 {
		t.Fatalf("ensured = %v %v, want 1 case per revision", first.Ensured, second.Ensured)
	}
	if first.Ensured[0] == second.Ensured[0] {
		t.Fatalf("revision change reused case %s, want two distinct cases for a:b and a:c", first.Ensured[0])
	}
	for _, rev := range []string{"a:b", "a:c"} {
		if _, found, err := cases.Find(ctx, cfg.MissionID, "ado", "shop#1", rev); err != nil || !found {
			t.Fatalf("Find shop#1 %s = %v %v, want found", rev, found, err)
		}
	}
}

func TestObserveOnceUnparseableItemYieldsExclusion(t *testing.T) {
	ctx, _, cases, execSvc, evidenceStore, cfg := setupObserve(t)
	caller := &fakeCaller{pages: []any{map[string]any{"prs": []any{
		goodPRItem(),
		map[string]any{"repository": "shop"},
	}}}}
	result, err := ObserveOnce(ctx, caller, cases, execSvc, evidenceStore, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Ensured) != 1 || len(result.Materialized) != 1 {
		t.Fatalf("result = %+v, want the parseable PR materialized", result)
	}
	found := false
	for _, e := range result.Excluded {
		if e.Reason == ReasonUnparseable {
			found = true
		}
	}
	if !found {
		t.Fatalf("excluded = %+v, want one unparseable-pr entry", result.Excluded)
	}
	if len(result.Failed) != 0 {
		t.Fatalf("failed = %+v, want empty (unparseable is an exclusion, not a failure)", result.Failed)
	}
}

// Trigger: work capabilities outside the grant ceiling fail Decide, so Ensure
// fails while the PR identity is known.
func TestObserveOnceEnsureFailureYieldsExclusion(t *testing.T) {
	ctx, _, cases, execSvc, evidenceStore, cfg := setupObserve(t)
	cfg.WorkCapabilities = []string{"write"}
	caller := &fakeCaller{pages: []any{map[string]any{"prs": []any{goodPRItem()}}}}
	result, err := ObserveOnce(ctx, caller, cases, execSvc, evidenceStore, cfg)
	if err != nil {
		t.Fatalf("ObserveOnce = %v, want nil tick error (ensure failure is an exclusion)", err)
	}
	if len(result.Excluded) != 1 || result.Excluded[0].Reason != ReasonEnsureFailed {
		t.Fatalf("excluded = %+v, want one ensure-failed entry", result.Excluded)
	}
	if result.Excluded[0].Detail == "" {
		t.Fatalf("excluded = %+v, want the ensure failure cause in the detail", result.Excluded)
	}
	if len(result.Failed) != 0 || len(result.Ensured) != 0 || len(result.Materialized) != 0 {
		t.Fatalf("result = %+v, want only the ensure-failed exclusion", result)
	}
}

type blockingCaller struct {
	entered chan struct{}
}

func (c *blockingCaller) Call(ctx context.Context, _ string, _ any) (any, error) {
	select {
	case c.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestRunSuppressesContextErrorMidPoll(t *testing.T) {
	_, _, cases, execSvc, evidenceStore, cfg := setupObserve(t)
	caller := &blockingCaller{entered: make(chan struct{}, 1)}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(runCtx, caller, cases, execSvc, evidenceStore, cfg, time.Millisecond) }()
	select {
	case <-caller.entered:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("caller was not entered")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil on cancellation mid-poll", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}
