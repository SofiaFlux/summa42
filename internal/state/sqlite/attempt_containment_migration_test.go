package sqlite

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestAttemptContainmentMigrationRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "attempt_containment_roundtrip.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	db := store.DB()

	for _, table := range []string{"attempt_containments", "attempt_containment_events"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("expected table %s to exist, count=%d", table, count)
		}
	}

	// Verify rollback down to migration 28
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithTableName("schema_migrations"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := provider.DownTo(ctx, 28); err != nil {
		t.Fatalf("down to v28: %v", err)
	}

	for _, table := range []string{"attempt_containments", "attempt_containment_events"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s after down: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("expected table %s to be dropped after rollback, count=%d", table, count)
		}
	}

	// Re-apply migration 29
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("re-applying up: %v", err)
	}
	for _, table := range []string{"attempt_containments", "attempt_containment_events"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s after re-applying up: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("expected table %s to exist after re-up, count=%d", table, count)
		}
	}

	// Verify triggers reject UPDATE and DELETE on attempt_containment_events
	// Seed prerequisites
	_, err = db.ExecContext(ctx, `
		INSERT INTO tasks(task_id, purpose_kind, purpose_id, state, current_fence, acceptance_criteria_json,
		  required_capabilities_json, required_enforcement, authority_ceiling_json, resource_envelope_id, priority, created_at, updated_at)
		VALUES ('t_imm', 'OWNER_DIRECTIVE', 'p_imm', 'ELIGIBLE', 0, '[]', '[]', 'ENFORCED', '[]', 'env_imm', 0, '2026-11-06T12:00:00Z', '2026-11-06T12:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO attempts(attempt_id, task_id, state, fence_generation, lease_state, lease_expires_at, started_at, executor_kind)
		VALUES ('att_imm', 't_imm', 'RUNNING', 1, 'ACTIVE', '2026-11-06T13:00:00Z', '2026-11-06T12:00:00Z', 'test')`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO attempt_containment_events(event_id, attempt_id, task_id, ladder_step, signal_name, enforced_by, created_at)
		VALUES ('ev_att_1', 'att_imm', 't_imm', 'WARN', 'high_memory_churn', 'guardian', '2026-11-06T12:05:00Z')`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.ExecContext(ctx, `UPDATE attempt_containment_events SET signal_name = 'tampered' WHERE event_id = 'ev_att_1'`)
	if err == nil {
		t.Fatal("expected error on UPDATE attempt_containment_events due to immutability trigger, got nil")
	}

	_, err = db.ExecContext(ctx, `DELETE FROM attempt_containment_events WHERE event_id = 'ev_att_1'`)
	if err == nil {
		t.Fatal("expected error on DELETE attempt_containment_events due to immutability trigger, got nil")
	}
}
