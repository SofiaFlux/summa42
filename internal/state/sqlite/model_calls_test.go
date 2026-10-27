package sqlite

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/capacity"
	"github.com/pressly/goose/v3"
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

func TestModelCallsMigrationRoundTrips(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "roundtrip.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	db := store.DB()

	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithTableName("schema_migrations"))
	if err != nil {
		t.Fatal(err)
	}
	tableCount := func() int {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='model_calls'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if tableCount() != 1 {
		t.Fatal("model_calls missing after Open")
	}
	if _, err := provider.DownTo(ctx, 23); err != nil {
		t.Fatalf("down to v23: %v", err)
	}
	if tableCount() != 0 {
		t.Fatal("model_calls survived the down migration")
	}
	if _, err := provider.UpTo(ctx, 24); err != nil {
		t.Fatalf("up to v24: %v", err)
	}
	if tableCount() != 1 {
		t.Fatal("model_calls not recreated by the up migration")
	}
}
