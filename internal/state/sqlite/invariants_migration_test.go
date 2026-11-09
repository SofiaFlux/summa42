package sqlite

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestBusinessInvariantsMigrationRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "invariants_roundtrip.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	db := store.DB()

	for _, table := range []string{"business_invariants", "invariant_evaluations"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("expected table %s to exist, count=%d", table, count)
		}
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO business_invariants (
			invariant_id, name, description, target_kind, rule_type,
			rule_config_json, severity, action_type, action_config_json, enabled, created_at, updated_at
		) VALUES (
			'inv_sla_1', 'SLA Expiration Guard', 'Ensures contracts do not expire unrenewed',
			'CONTRACT', 'EXPIRATION', '{"lead_window_seconds": 86400}', 'BREACH',
			'REMEDIATION_TASK', '{"task_title": "Renew Contract"}', 1, '2026-11-09T08:00:00Z', '2026-11-09T08:00:00Z'
		)`,
	)
	if err != nil {
		t.Fatalf("insert invariant: %v", err)
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO invariant_evaluations (
			evaluation_id, invariant_id, entity_id, status, observed_value_json,
			evidence_id, triggered_task_id, triggered_packet_id, containment_signal, evaluated_at
		) VALUES (
			'eval_1', 'inv_sla_1', 'ent_contract_1', 'BREACHED', '{"time_to_expiry_seconds": 3600}',
			'ev_123', 'task_renew_1', 'pkt_1', 'WARN', '2026-11-09T09:00:00Z'
		)`,
	)
	if err != nil {
		t.Fatalf("insert invariant evaluation: %v", err)
	}

	// Rollback down to migration 32
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithTableName("schema_migrations"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := provider.DownTo(ctx, 32); err != nil {
		t.Fatalf("down to 32: %v", err)
	}

	for _, table := range []string{"business_invariants", "invariant_evaluations"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s dropped: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("expected table %s to be dropped, got %d", table, count)
		}
	}

	// Re-apply migration Up to confirm idempotent re-creation
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up to latest: %v", err)
	}

	for _, table := range []string{"business_invariants", "invariant_evaluations"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s after re-up: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("expected table %s to exist after re-up, count=%d", table, count)
		}
	}
}
