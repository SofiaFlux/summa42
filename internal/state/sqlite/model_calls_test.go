package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/capacity"
)

func TestModelCallsPoolStateSurvivesRestartAndClearsOnSuccess(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "capacity.db")
	observed := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	reset := observed.Add(49 * time.Hour)

	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var ledger capacity.Ledger = store
	if err := ledger.RecordCall(ctx, capacity.Call{
		Pool: "commandcode:plan", Harness: "commandcode", Model: "deepseek/deepseek-v4-pro",
		Stage: "plan-review", StartedAt: observed, Outcome: capacity.OutcomeRefused,
		ResetAt: reset, ResetSource: "parsed", Message: "weekly usage limit",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	ledger = store

	state, err := ledger.PoolState(ctx, "commandcode:plan", observed.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !state.Exhausted || !state.ResetAt.Equal(reset) {
		t.Fatalf("before reset: got %+v, want exhausted until %v", state, reset)
	}

	state, err = ledger.PoolState(ctx, "commandcode:plan", reset.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if state.Exhausted {
		t.Fatalf("after reset: pool still exhausted: %+v", state)
	}

	if err := ledger.RecordCall(ctx, capacity.Call{
		Pool: "commandcode:plan", Harness: "commandcode", Model: "deepseek/deepseek-v4-pro",
		Stage: "plan-review", StartedAt: observed.Add(2 * time.Hour), Outcome: capacity.OutcomeOK,
	}); err != nil {
		t.Fatal(err)
	}
	state, err = ledger.PoolState(ctx, "commandcode:plan", observed.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if state.Exhausted {
		t.Fatalf("after a successful call: pool still exhausted: %+v", state)
	}
}
