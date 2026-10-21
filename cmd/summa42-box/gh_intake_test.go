package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/capabilities"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/fieldfeedback"
	"github.com/SofiaFlux/summa42/internal/ghissue"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/policy"
	"github.com/SofiaFlux/summa42/internal/purpose"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/testutil"
	"github.com/SofiaFlux/summa42/internal/workflow"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

type intakePolicy struct{}

func (intakePolicy) Evaluate(_ context.Context, in policy.PolicyInput) (domain.PolicyDecision, error) {
	return domain.PolicyDecision{
		ID: domain.NewID("policy-decision"), Outcome: domain.PolicyAllow,
		PolicySetID: "policy-test", PolicySetHash: "hash", PolicyCapabilitiesHash: "caps",
		InputDigest: "input", EvaluatedAt: in.Now,
	}, nil
}

type intakeCapabilityProvider struct{}

func (intakeCapabilityProvider) Name() string { return "ado" }
func (intakeCapabilityProvider) Call(context.Context, string, any) (any, error) {
	return nil, nil
}

type intakeSink struct{}

func (intakeSink) Create(context.Context, fieldfeedback.IssuePayload) (string, int64, error) {
	return "issue-1", 0, nil
}
func (intakeSink) FindByMarker(context.Context, string) (string, bool, error) { return "", false, nil }

func validGHIntakeArgs() []string {
	return []string{
		"--mission", "mission-1", "--repo", "o/r", "--maintainer", "maint",
		"--grant-capability", "github.issue.read", "--envelope", "envelope-1",
		"--max-steps", "3", "--remaining-budget", "10", "--poll-interval", "1s",
	}
}

func withoutFlag(args []string, flag, value string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == flag && i+1 < len(args) && args[i+1] == value {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func replaceFlag(args []string, flag, old, value string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == flag && i+1 < len(args) && args[i+1] == old {
			out = append(out, flag, value)
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func TestParseGHIntakeFlagsAcceptsValidInput(t *testing.T) {
	t.Setenv("SUMMA42_GITHUB_REPOSITORY", "env/repo")
	cfg, interval, err := parseGHIntakeFlags(validGHIntakeArgs())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MissionID != "mission-1" || cfg.Repository != "o/r" {
		t.Fatalf("mission=%q repo=%q (--repo must win over env)", cfg.MissionID, cfg.Repository)
	}
	if len(cfg.Maintainers) != 1 || cfg.Maintainers[0] != "maint" {
		t.Fatalf("maintainers = %v", cfg.Maintainers)
	}
	if len(cfg.Grant.Capabilities) != 1 || cfg.Grant.Capabilities[0] != "github.issue.read" {
		t.Fatalf("grant = %v", cfg.Grant.Capabilities)
	}
	if cfg.ResourceEnvelopeID != "envelope-1" || cfg.MaxSteps != 3 || cfg.RemainingBudget != 10 {
		t.Fatalf("cfg = %+v", cfg)
	}
	if interval != time.Second {
		t.Fatalf("interval = %v", interval)
	}
}

func TestParseGHIntakeFlagsFallsBackToRepositoryEnv(t *testing.T) {
	t.Setenv("SUMMA42_GITHUB_REPOSITORY", "env/repo")
	args := withoutFlag(validGHIntakeArgs(), "--repo", "o/r")
	cfg, _, err := parseGHIntakeFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Repository != "env/repo" {
		t.Fatalf("repo = %q, want env fallback", cfg.Repository)
	}
}

func TestParseGHIntakeFlagsRejectsInvalidInput(t *testing.T) {
	valid := validGHIntakeArgs()
	cases := map[string][]string{
		"missing mission":               withoutFlag(valid, "--mission", "mission-1"),
		"missing repository":            withoutFlag(valid, "--repo", "o/r"),
		"missing maintainer":            withoutFlag(valid, "--maintainer", "maint"),
		"missing envelope":              withoutFlag(valid, "--envelope", "envelope-1"),
		"empty grant":                   withoutFlag(valid, "--grant-capability", "github.issue.read"),
		"grant without read capability": replaceFlag(valid, "--grant-capability", "github.issue.read", "other.cap"),
		"non-positive max steps":        replaceFlag(valid, "--max-steps", "3", "0"),
		"non-positive budget":           replaceFlag(valid, "--remaining-budget", "10", "0"),
		"non-positive interval":         replaceFlag(valid, "--poll-interval", "1s", "0s"),
		"work capability outside grant": append(append([]string{}, valid...), "--work-capability", "extra.cap"),
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("SUMMA42_GITHUB_REPOSITORY", "")
			if _, _, err := parseGHIntakeFlags(args); err == nil {
				t.Fatal("expected startup error")
			}
		})
	}
}

func TestParseGHIntakeFlagsStoresTrimmedTokens(t *testing.T) {
	t.Setenv("SUMMA42_GITHUB_REPOSITORY", "env/repo")
	args := append(replaceFlag(validGHIntakeArgs(), "--maintainer", "maint", " maint "),
		"--grant-capability", " github.pull.write ", "--work-capability", " github.pull.write ")
	cfg, _, err := parseGHIntakeFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Maintainers) != 1 || cfg.Maintainers[0] != "maint" {
		t.Fatalf("maintainers = %q, want the trimmed login", cfg.Maintainers)
	}
	want := []string{"github.issue.read", "github.pull.write"}
	if !reflect.DeepEqual(cfg.Grant.Capabilities, want) {
		t.Fatalf("grant = %q, want %q", cfg.Grant.Capabilities, want)
	}
	if !reflect.DeepEqual(cfg.WorkCapabilities, []string{"github.pull.write"}) {
		t.Fatalf("work capabilities = %q, want the trimmed token", cfg.WorkCapabilities)
	}
}

func TestOpenGHIntakeBoxStripsWritableComposition(t *testing.T) {
	root := t.TempDir()
	box, err := openGHIntakeBox(context.Background(), summa42runtime.Config{
		StatePath: filepath.Join(root, "state.db"), EvidencePath: filepath.Join(root, "evidence"),
		CollectiveID: "collective-1", OwnerPrincipalID: "owner-1", PolicyEngine: intakePolicy{},
		FieldFeedback: localconfig.FieldFeedbackConfig{
			Enabled: true, Mode: localconfig.FeedbackModeAutoIfAllowed, Provider: "github",
			Destination: "o/r", MaintenanceEnvelopeID: "envelope-1", RequiredEnforcement: domain.EnforcementEnforced,
		},
		FeedbackSink:        intakeSink{},
		OperationProviders:  nil,
		CapabilityProviders: []capabilities.Provider{intakeCapabilityProvider{}},
		Executors:           map[string]executors.Executor{"ado-publish": publishCapacityExecutor{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	if len(box.Executors) != 0 {
		t.Fatalf("executors = %v, want none", box.Executors)
	}
	if _, err := box.Capabilities.AssessProvider(context.Background(), "ado"); err == nil {
		t.Fatal("capability provider registered in read-only intake Box")
	}
}

func TestGHIntakeClientConfigReadsTokenFileAndKeepsGitHubBase(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("sekrit"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUMMA42_GITHUB_TOKEN_FILE", "  "+token+"  ")
	t.Setenv("SUMMA42_GITHUB_BASE_URL", "http://127.0.0.1:8080")
	cfg := ghIntakeClientConfig("o/r", "")
	if cfg.Repository != "o/r" || cfg.TokenFile != token {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.BaseURL != "" {
		t.Fatalf("base URL = %q, want empty so ghissue applies its api.github.com default", cfg.BaseURL)
	}
	client, err := ghissue.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if client.Name() != "o/r" {
		t.Fatalf("client repository = %q", client.Name())
	}
	if _, err := ghissue.New(ghissue.Config{Repository: "o/r", TokenFile: token, BaseURL: "http://127.0.0.1:8080"}); err != nil {
		t.Fatalf("loopback base URL must stay a client-level seam the CLI cannot reach: %v", err)
	}
	// A credential path that is not usable must be a startup error, so the Box
	// is never opened and no evidence directory created for a run that cannot
	// make a single request.
	if _, err := ghissue.New(ghissue.Config{Repository: "o/r", TokenFile: "/secrets/typo"}); err == nil {
		t.Fatal("expected a startup error for an unusable token path")
	}
}

// Trigger: bare reason tokens cannot tell an operator whether issues are
// legitimately held or every durable write in this Box is failing, and a
// per-issue failure was printed twice on the same line.
func TestFormatGHIntakeTickRendersExclusionIdentityAndCause(t *testing.T) {
	cause := "ensure and materialize workflow case: mission is not active"
	result := ghissue.ObserveResult{
		Excluded: []ghissue.ExcludedIssue{
			{Reason: ghissue.ReasonUnparseable},
			{Issue: ghissue.Issue{Repository: "o/r", Number: 11}, Reason: ghissue.ReasonNotMaintainer},
			{Issue: ghissue.Issue{Repository: "o/r", Number: 13}, Reason: ghissue.ReasonEnsureFailed, Detail: cause},
		},
		Failed:              []ghissue.FailedIssue{{Issue: ghissue.Issue{Repository: "o/r", Number: 7}, Err: "write failed"}},
		PullRequestsSkipped: 1,
	}
	want := "gh-intake tick ensured=0 materialized=0 excluded=3 " +
		"[unparseable-issue, not-maintainer github:o/r#11, ensure-failed github:o/r#13: " + cause + "] " +
		"failed=1 [github:o/r#7: write failed] pull_requests_skipped=1"
	if line := formatGHIntakeTick(result, errors.New("write failed")); line != want {
		t.Fatalf("line = %q\nwant   %q", line, want)
	}
	tick := formatGHIntakeTick(ghissue.ObserveResult{}, errors.New("list GitHub issues: unexpected status 403"))
	if !strings.HasSuffix(tick, " error=list GitHub issues: unexpected status 403") {
		t.Fatalf("tick = %q, want the tick error with no per-issue bucket to duplicate it", tick)
	}
}

type intakeTickFixture struct {
	store         *state.Store
	cases         *workflowcase.Service
	execSvc       *execution.Service
	evidenceStore *evidence.Store
	cfg           ghissue.ObserveConfig
}

func setupIntakeTick(t *testing.T) intakeTickFixture {
	t.Helper()
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	purposes := purpose.New(store, clk)
	execSvc := execution.New(store, clk, purposes)
	evidenceStore, err := evidence.New(store, t.TempDir(), clk)
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
	return intakeTickFixture{
		store: store, cases: workflowcase.New(store, clk, purposes), execSvc: execSvc, evidenceStore: evidenceStore,
		cfg: ghissue.ObserveConfig{
			MissionID: mission, Repository: "o/r", Maintainers: []string{"maint"},
			Grant:              workflow.Grant{Capabilities: []string{"github.issue.read"}},
			ResourceEnvelopeID: envelope, MaxSteps: 3, RemainingBudget: 10,
		},
	}
}

type intakeLister struct {
	pages []intakePage
}

type intakePage struct {
	items []any
	bad   []ghissue.Unparseable
	err   error
}

func (l *intakeLister) ListIssues(context.Context, string) ([]any, []ghissue.Unparseable, string, error) {
	if len(l.pages) == 0 {
		return nil, nil, "", errors.New("unexpected page request")
	}
	page := l.pages[0]
	l.pages = l.pages[1:]
	return page.items, page.bad, "", page.err
}

func (l *intakeLister) Name() string { return "o/r" }

func intakeWireIssue(mutate ...func(map[string]any)) map[string]any {
	item := map[string]any{
		"number":     float64(7),
		"title":      "App crashes on save",
		"body":       "steps",
		"state":      "open",
		"user":       map[string]any{"login": "maint"},
		"labels":     []any{},
		"updated_at": "2026-09-24T10:00:00+00:00",
		"html_url":   "https://github.com/o/r/issues/7",
	}
	for _, apply := range mutate {
		apply(item)
	}
	return item
}

// The operator must be able to tell an idle tick from a tick whose durable
// writes keep failing: the summary carries both buckets plus the reason tokens
// and the error text, and never the credential.
func TestGHIntakeTickReporterAccountsForMixedBatch(t *testing.T) {
	t.Setenv("SUMMA42_GITHUB_TOKEN_FILE", "/secrets/gh-token")
	f := setupIntakeTick(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	lister := &intakeLister{pages: []intakePage{
		{items: []any{intakeWireIssue()}},
		{
			items: []any{
				intakeWireIssue(),
				intakeWireIssue(func(m map[string]any) { m["number"] = float64(10) }),
				intakeWireIssue(func(m map[string]any) { m["pull_request"] = map[string]any{"url": "x"} }),
				intakeWireIssue(func(m map[string]any) { m["user"] = map[string]any{"login": "stranger"} }),
				intakeWireIssue(func(m map[string]any) {
					m["number"] = float64(12)
					m["labels"] = []any{map[string]any{"name": "wontfix"}}
				}),
			},
			bad: []ghissue.Unparseable{{Raw: map[string]any{"number": float64(99)}}},
		},
	}}
	var results []ghissue.ObserveResult
	var lines []string
	var out bytes.Buffer
	reporter := ghIntakeTickReporter(&out)
	ticks := 0
	if err := ghissue.RunReporting(ctx, lister, f.cases, f.execSvc, f.evidenceStore, f.cfg, time.Millisecond,
		func(result ghissue.ObserveResult, err error) {
			results = append(results, result)
			reporter(result, err)
			ticks++
			if ticks == 1 {
				// The registered case becomes unwritable between ticks, so the
				// second tick mixes one durable-write failure with healthy work.
				if _, err := f.store.DB().ExecContext(ctx,
					`UPDATE workflow_cases SET next_work_json = '{oops' WHERE object_id = 'github:o/r#7'`); err != nil {
					t.Fatal(err)
				}
				return
			}
			cancel()
		}); err != nil {
		t.Fatalf("RunReporting = %v, want nil on cancellation", err)
	}
	if ticks != 2 {
		t.Fatalf("ticks = %d, want 2", ticks)
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		lines = append(lines, line)
	}
	if len(lines) != 2 {
		t.Fatalf("emitted %d lines, want one per tick: %q", len(lines), out.String())
	}
	if strings.Contains(out.String(), "gh-token") {
		t.Fatalf("summary leaked the token file: %q", out.String())
	}
	tick := results[1]
	if len(tick.Ensured) != 1 || len(tick.Materialized) != 1 {
		t.Fatalf("tick = %+v, want issue #10 materialized alongside the failure", tick)
	}
	if len(tick.Failed) != 1 || tick.Failed[0].Issue.ObjectID() != "github:o/r#7" {
		t.Fatalf("failed = %+v, want only issue #7", tick.Failed)
	}
	if tick.PullRequestsSkipped != 1 {
		t.Fatalf("skipped = %d, want 1", tick.PullRequestsSkipped)
	}
	for _, want := range []string{
		"ensured=1", "materialized=1", "excluded=3",
		ghissue.ReasonUnparseable + ",", "not-maintainer github:o/r#7", "held-by-label github:o/r#12",
		"failed=1", tick.Failed[0].Issue.ObjectID(), tick.Failed[0].Err,
		"pull_requests_skipped=1",
	} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("summary %q does not account for %q", lines[1], want)
		}
	}
	if strings.Contains(lines[1], "error=") {
		t.Errorf("summary %q repeats the per-issue failure as a tick error", lines[1])
	}
	if count := strings.Count(lines[1], tick.Failed[0].Err); count != 1 {
		t.Errorf("summary %q prints the per-issue failure %d times, want 1", lines[1], count)
	}
	if strings.Contains(lines[0], "failed=1") || strings.Contains(lines[0], "error=") {
		t.Fatalf("clean first tick reported a failure: %q", lines[0])
	}
}

func TestIntakeGrantActionSubsetOfCapabilities(t *testing.T) {
	t.Setenv("SUMMA42_GITHUB_REPOSITORY", "")
	bad := append(append([]string{}, validGHIntakeArgs()...), "--grant-action", "workspace.test")
	_, _, err := parseGHIntakeFlags(bad)
	if err == nil || err.Error() != `grant-action "workspace.test" must also be a granted capability` {
		t.Fatalf("err = %v, want grant-action subset error", err)
	}
	good := append(append([]string{}, validGHIntakeArgs()...),
		"--grant-capability", "workspace.repo.write", "--grant-capability", "workspace.test",
		"--grant-action", "workspace.repo.write", "--grant-action", "workspace.test")
	cfg, _, err := parseGHIntakeFlags(good)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"workspace.repo.write", "workspace.test"}; !reflect.DeepEqual(cfg.Grant.Actions, want) {
		t.Fatalf("actions = %q, want %q", cfg.Grant.Actions, want)
	}
}

func TestIntakeWithoutGrantActionHasNoActions(t *testing.T) {
	t.Setenv("SUMMA42_GITHUB_REPOSITORY", "")
	args := append(append([]string{}, validGHIntakeArgs()...), "--grant-capability", "workspace.repo.write")
	cfg, _, err := parseGHIntakeFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Grant.Actions) != 0 {
		t.Fatalf("actions = %q, want none (never inferred from capabilities)", cfg.Grant.Actions)
	}
}

type countingLister struct{ calls int }

func (l *countingLister) ListIssues(context.Context, string) ([]any, []ghissue.Unparseable, string, error) {
	l.calls++
	return []any{intakeWireIssue()}, nil, "", nil
}
func (l *countingLister) Name() string { return "o/r" }

func TestIntakeOnceRunsSinglePass(t *testing.T) {
	t.Setenv("SUMMA42_GITHUB_REPOSITORY", "")
	cfg, _, err := parseGHIntakeFlags(append(append([]string{}, validGHIntakeArgs()...), "--once"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Once {
		t.Fatal("--once did not set cfg.Once")
	}
	f := setupIntakeTick(t)
	f.cfg.Once = true
	lister := &countingLister{}
	ticks := 0
	if err := ghissue.RunReporting(context.Background(), lister, f.cases, f.execSvc, f.evidenceStore, f.cfg, time.Millisecond,
		func(ghissue.ObserveResult, error) { ticks++ }); err != nil {
		t.Fatalf("RunReporting = %v, want nil", err)
	}
	if lister.calls != 1 || ticks != 1 {
		t.Fatalf("list calls = %d, ticks = %d, want 1 and 1", lister.calls, ticks)
	}
}
