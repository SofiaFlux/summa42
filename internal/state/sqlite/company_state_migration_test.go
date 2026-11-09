package sqlite

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestCompanyStateMigrationRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "company_state_roundtrip.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	db := store.DB()

	for _, table := range []string{"company_entities", "company_state_transitions", "company_projections"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("expected table %s to exist, count=%d", table, count)
		}
	}

	// Verify insertion into company_entities and foreign key relations
	_, err = db.ExecContext(ctx, `
		INSERT INTO company_entities (
			entity_id, kind, name, status, attributes_json, exposure_amount, created_at, updated_at
		) VALUES (
			'ent_cust_1', 'CUSTOMER', 'Acme Corp', 'ACTIVE', '{"tier":"ENTERPRISE"}', 50000,
			'2026-11-09T08:00:00Z', '2026-11-09T08:00:00Z'
		)`,
	)
	if err != nil {
		t.Fatalf("insert company entity: %v", err)
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO company_state_transitions (
			transition_id, entity_id, task_id, evidence_id, transition_kind,
			previous_state_json, new_state_json, recorded_at
		) VALUES (
			'trans_1', 'ent_cust_1', 'task_1', 'ev_1', 'INITIAL_ONBOARDING',
			NULL, '{"status":"ACTIVE"}', '2026-11-09T08:00:00Z'
		)`,
	)
	if err != nil {
		t.Fatalf("insert state transition: %v", err)
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO company_projections (
			projection_id, source_system, external_id, entity_id, payload_json, synced_at
		) VALUES (
			'proj_1', 'CRM', 'crm_acme_1', 'ent_cust_1', '{"crm_id":"crm_acme_1"}', '2026-11-09T08:00:00Z'
		)`,
	)
	if err != nil {
		t.Fatalf("insert projection: %v", err)
	}

	// Rollback down to migration 30
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithTableName("schema_migrations"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := provider.DownTo(ctx, 30); err != nil {
		t.Fatalf("down to v30: %v", err)
	}

	for _, table := range []string{"company_entities", "company_state_transitions", "company_projections"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s after down: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("expected table %s to be dropped after rollback, count=%d", table, count)
		}
	}

	// Re-apply migration Up to confirm idempotent re-creation
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up to latest: %v", err)
	}

	for _, table := range []string{"company_entities", "company_state_transitions", "company_projections"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s after re-up: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("expected table %s to exist after re-up, count=%d", table, count)
		}
	}
}
