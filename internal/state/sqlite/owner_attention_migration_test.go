package sqlite

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestOwnerAttentionMigrationRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "attention_roundtrip.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	db := store.DB()

	for _, table := range []string{"owner_attention_budgets", "owner_decision_packets"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("expected table %s to exist, count=%d", table, count)
		}
	}

	var dailyBudget, usedBudget int
	err = db.QueryRowContext(ctx, `
		SELECT daily_budget_units, used_budget_units FROM owner_attention_budgets WHERE singleton = 1`).Scan(&dailyBudget, &usedBudget)
	if err != nil {
		t.Fatalf("query budget singleton: %v", err)
	}
	if dailyBudget != 100 || usedBudget != 0 {
		t.Fatalf("unexpected budget defaults: daily=%d, used=%d", dailyBudget, usedBudget)
	}

	// Verify inserting and querying a decision packet
	_, err = db.ExecContext(ctx, `
		INSERT INTO owner_decision_packets (
			packet_id, title, category, urgency, sovereign_required,
			status, estimated_attention_units, summary, evidence_ids_json,
			sub_decisions_json, proposed_action_json, created_at, updated_at
		) VALUES (
			'pkt_test_1', 'Authorize strategic expansion', 'STRATEGIC_MISSION', 'HIGH', 1,
			'PENDING', 15, 'Expansion proposal', '["ev_1"]',
			'[{"id":"dec_1","type":"MISSION","summary":"Expand","risk_score":3}]', '{"action":"APPROVE_EXPANSION"}', '2026-11-09T08:00:00Z', '2026-11-09T08:00:00Z'
		)`,
	)
	if err != nil {
		t.Fatalf("insert decision packet: %v", err)
	}

	// Verify rollback down to migration 29
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithTableName("schema_migrations"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := provider.DownTo(ctx, 29); err != nil {
		t.Fatalf("down to v29: %v", err)
	}

	for _, table := range []string{"owner_attention_budgets", "owner_decision_packets"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s after down: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("expected table %s to be dropped after rollback, count=%d", table, count)
		}
	}

	// Re-apply migration Up to confirm seed re-creation and idempotence
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up to latest: %v", err)
	}

	for _, table := range []string{"owner_attention_budgets", "owner_decision_packets"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s after re-up: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("expected table %s to exist after re-up, count=%d", table, count)
		}
	}

	err = db.QueryRowContext(ctx, `
		SELECT daily_budget_units, used_budget_units FROM owner_attention_budgets WHERE singleton = 1`).Scan(&dailyBudget, &usedBudget)
	if err != nil {
		t.Fatalf("query budget singleton after re-up: %v", err)
	}
	if dailyBudget != 100 || usedBudget != 0 {
		t.Fatalf("unexpected budget defaults after re-up: daily=%d, used=%d", dailyBudget, usedBudget)
	}
}
