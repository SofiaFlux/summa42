package sqlite

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestEmergencyControlsMigrationRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "emergency_roundtrip.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	db := store.DB()

	for _, table := range []string{"emergency_controls", "dead_man_switch", "emergency_audit_events"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("expected table %s to exist, count=%d", table, count)
		}
	}

	// Verify rollback down to migration 27
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithTableName("schema_migrations"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := provider.DownTo(ctx, 27); err != nil {
		t.Fatalf("down to v27: %v", err)
	}

	for _, table := range []string{"emergency_controls", "dead_man_switch", "emergency_audit_events"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s after down: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("expected table %s to be dropped after rollback, count=%d", table, count)
		}
	}

	// Re-apply migration 28
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("re-applying up: %v", err)
	}
	for _, table := range []string{"emergency_controls", "dead_man_switch", "emergency_audit_events"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s after re-applying up: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("expected table %s to exist after re-up, count=%d", table, count)
		}
	}

	// Verify triggers reject UPDATE and DELETE on emergency_audit_events
	_, err = db.ExecContext(ctx, `
		INSERT INTO emergency_audit_events(event_id, event_type, reason, created_at)
		VALUES ('ev_em_1', 'BREAK_GLASS_TRIGGERED', 'Emergency override', '2026-11-06T12:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.ExecContext(ctx, `UPDATE emergency_audit_events SET reason = 'tampered' WHERE event_id = 'ev_em_1'`)
	if err == nil {
		t.Fatal("expected error on UPDATE emergency_audit_events due to immutability trigger, got nil")
	}

	_, err = db.ExecContext(ctx, `DELETE FROM emergency_audit_events WHERE event_id = 'ev_em_1'`)
	if err == nil {
		t.Fatal("expected error on DELETE emergency_audit_events due to immutability trigger, got nil")
	}
}
