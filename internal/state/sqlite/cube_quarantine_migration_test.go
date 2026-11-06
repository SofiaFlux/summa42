package sqlite

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestCubeQuarantineMigrationRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cube_quarantine_roundtrip.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	db := store.DB()

	// Verify tables cubes and cube_quarantine_events exist
	for _, table := range []string{"cubes", "cube_quarantine_events"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("expected table %s to exist, count=%d", table, count)
		}
	}

	// Verify rollback down to migration 26
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithTableName("schema_migrations"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := provider.DownTo(ctx, 26); err != nil {
		t.Fatalf("down to v26: %v", err)
	}

	for _, table := range []string{"cubes", "cube_quarantine_events"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s after down: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("expected table %s to be dropped after rollback, count=%d", table, count)
		}
	}

	// Re-apply migration 27
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("re-applying up: %v", err)
	}
	for _, table := range []string{"cubes", "cube_quarantine_events"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s after re-applying up: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("expected table %s to exist after re-up, count=%d", table, count)
		}
	}

	// Verify triggers reject UPDATE and DELETE on cube_quarantine_events
	_, err = db.ExecContext(ctx, `
		INSERT INTO principals(principal_id, principal_kind, public_key, custody_profile, created_at)
		VALUES ('p_imm', 'HUMAN', X'00', 'LOCAL', '2026-11-06T12:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO cubes(cube_id, principal_id, state, created_at, updated_at)
		VALUES ('c_imm', 'p_imm', 'ACTIVE', '2026-11-06T12:00:00Z', '2026-11-06T12:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO cube_quarantine_events(event_id, cube_id, action, reason, enforced_by_principal_id, created_at)
		VALUES ('ev_imm', 'c_imm', 'QUARANTINE', 'Initial test', 'p_imm', '2026-11-06T12:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.ExecContext(ctx, `UPDATE cube_quarantine_events SET reason = 'tampered' WHERE event_id = 'ev_imm'`)
	if err == nil {
		t.Fatal("expected error on UPDATE cube_quarantine_events due to immutability trigger, got nil")
	}

	_, err = db.ExecContext(ctx, `DELETE FROM cube_quarantine_events WHERE event_id = 'ev_imm'`)
	if err == nil {
		t.Fatal("expected error on DELETE cube_quarantine_events due to immutability trigger, got nil")
	}
}
