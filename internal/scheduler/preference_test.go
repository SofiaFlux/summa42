package scheduler_test

import (
	"context"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/resources"
	"github.com/SofiaFlux/summa42/internal/scheduler"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

type fakePreference struct {
	executor string
	found    bool
	err      error
}

func (f *fakePreference) PreferredExecutor(context.Context, domain.Task, []string) (string, bool, error) {
	return f.executor, f.found, f.err
}

func TestPreferenceCanOnlyReorderEligibleExecutors(t *testing.T) {
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC))
	purposes := purpose.New(store, clk)
	exec := execution.New(store, clk, purposes)
	res := resources.New(store, clk)
	preference := &fakePreference{executor: "codex", found: true}
	svc := scheduler.New(store, clk, purposes, exec, res, time.Minute, preference)
	task := domain.Task{ID: "task-1", TaskClass: "repo.review"}

	chosen, err := svc.ChooseExecutor(context.Background(), task, []string{"claude", "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if chosen != "codex" {
		t.Fatalf("chosen=%q want codex", chosen)
	}

	preference.executor = "untrusted-executor"
	if _, err := svc.ChooseExecutor(context.Background(), task, []string{"claude", "codex"}); err == nil {
		t.Fatal("preference made executor outside eligible set selectable")
	}

	preference.found = false
	chosen, err = svc.ChooseExecutor(context.Background(), task, []string{"codex", "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if chosen != "claude" {
		t.Fatalf("rollback/baseline chosen=%q want lexical claude", chosen)
	}
}

func TestChooseExecutorBaselineIsStableAndDoesNotExpandEligibility(t *testing.T) {
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC))
	purposes := purpose.New(store, clk)
	exec := execution.New(store, clk, purposes)
	res := resources.New(store, clk)
	svc := scheduler.New(store, clk, purposes, exec, res, time.Minute)
	task := domain.Task{
		ID: "task-1", TaskClass: "repo.review",
		RequiredCapabilities: []string{"repo.read"},
		RequiredEnforcement:  domain.EnforcementEnforced,
		AuthorityCeiling:     []string{"repo.read"},
	}
	chosen, err := svc.ChooseExecutor(context.Background(), task, []string{"zeta", "alpha", "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if chosen != "alpha" {
		t.Fatalf("baseline=%q want alpha", chosen)
	}
	if _, err := svc.ChooseExecutor(context.Background(), task, nil); err == nil {
		t.Fatal("empty eligible set was treated as selectable")
	}
}
