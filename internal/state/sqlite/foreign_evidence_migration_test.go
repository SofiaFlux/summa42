package sqlite

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestForeignEvidenceLineageMigrationRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "foreign_evidence_roundtrip.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	db := store.DB()

	for _, table := range []string{"foreign_evidence_lineage", "external_task_reconciliations"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("expected table %s to exist, count=%d", table, count)
		}
	}

	// Insert parent evidence object and task/attempt
	_, err = db.ExecContext(ctx, `
		INSERT INTO evidence_objects(evidence_id, content_hash, media_type, kind, size_bytes, created_at)
		VALUES ('ev_foreign_1', 'abc123hash', 'application/json', 'FOREIGN_ARTIFACT', 128, '2026-11-09T12:00:00Z');

		INSERT INTO tasks(
			task_id, purpose_kind, purpose_id, state,
			acceptance_criteria_json, required_capabilities_json, required_enforcement,
			authority_ceiling_json, resource_envelope_id, created_at, updated_at
		) VALUES (
			'task_ext_1', 'MISSION', 'purpose_1', 'CREATED',
			'{"criteria": "test"}', '[]', 'ENFORCED',
			'{}', 'env_1', '2026-11-09T12:00:00Z', '2026-11-09T12:00:00Z'
		);

		INSERT INTO attempts(attempt_id, task_id, state, fence_generation, lease_state, lease_expires_at, started_at, executor_kind)
		VALUES ('att_ext_1', 'task_ext_1', 'RUNNING', 1, 'ACTIVE', '2026-11-09T13:00:00Z', '2026-11-09T12:00:00Z', 'foreign_agent');
	`)
	if err != nil {
		t.Fatalf("seed prerequisites: %v", err)
	}

	// Insert foreign evidence lineage
	_, err = db.ExecContext(ctx, `
		INSERT INTO foreign_evidence_lineage (
			evidence_id, content_hash, origin_collective_id, producer_principal_id,
			delegation_id, signature_hex, trust_tier, corroboration_level,
			witnesses_json, upstream_lineage_json, created_at
		) VALUES (
			'ev_foreign_1', 'abc123hash', 'collective_alpha', 'agent_smith',
			'del_alpha_1', 'deadbeef', 'VERIFIED_PARTNER', 'INDEPENDENT_WITNESS',
			'["witness_1"]', '["upstream_hash_1"]', '2026-11-09T12:00:00Z'
		)`,
	)
	if err != nil {
		t.Fatalf("insert foreign_evidence_lineage: %v", err)
	}

	// Insert external task reconciliation
	_, err = db.ExecContext(ctx, `
		INSERT INTO external_task_reconciliations (
			reconciliation_id, task_id, attempt_id, origin_collective_id, producer_principal_id,
			external_signal_state, reconciliation_state, reconciliation_reason,
			evidence_ids_json, blind_retry_prevented, evaluated_at
		) VALUES (
			'recon_1', 'task_ext_1', 'att_ext_1', 'collective_alpha', 'agent_smith',
			'CLAIMED_SUCCESS', 'VERIFIED_ACCEPTANCE', 'Verified by independent witness',
			'["ev_foreign_1"]', 1, '2026-11-09T12:01:00Z'
		)`,
	)
	if err != nil {
		t.Fatalf("insert external_task_reconciliations: %v", err)
	}

	// Roll back migration 00034
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithTableName("schema_migrations"))
	if err != nil {
		t.Fatalf("new goose provider: %v", err)
	}
	if _, err := provider.DownTo(ctx, 33); err != nil {
		t.Fatalf("down to 33: %v", err)
	}

	for _, table := range []string{"foreign_evidence_lineage", "external_task_reconciliations"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s after rollback: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("expected table %s to be dropped, count=%d", table, count)
		}
	}

	// Re-apply migration Up to confirm idempotent re-creation
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up to latest: %v", err)
	}

	for _, table := range []string{"foreign_evidence_lineage", "external_task_reconciliations"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s after re-up: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("expected table %s to exist after re-up, count=%d", table, count)
		}
	}
}
