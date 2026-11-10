package sqlite

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

// A Collective whose dead man switch was stuck by a lease-derived timeout must
// come out of the upgrade with the Owner's default horizon and working again.
func TestOwnerHorizonMigrationReplacesDerivedTimeoutAndClearsSafeMode(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
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
	if _, err := provider.DownTo(ctx, 34); err != nil {
		t.Fatalf("down to v34: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO dead_man_switch (singleton, state, last_heartbeat_at, heartbeat_timeout_seconds, safe_mode_triggered_at, updated_at)
		VALUES (1, 'SAFE_MODE', '2026-11-06T18:58:15Z', 3000, '2026-11-06T19:48:16Z', '2026-11-06T19:48:16Z')`); err != nil {
		t.Fatal(err)
	}

	before := time.Now().UTC().Add(-time.Minute)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	var state, heartbeat string
	var timeout int
	var triggered *string
	if err := db.QueryRowContext(ctx, `
		SELECT state, last_heartbeat_at, heartbeat_timeout_seconds, safe_mode_triggered_at FROM dead_man_switch WHERE singleton = 1`,
	).Scan(&state, &heartbeat, &timeout, &triggered); err != nil {
		t.Fatal(err)
	}
	if state != "NOMINAL" || timeout != 604800 || triggered != nil {
		t.Fatalf("state=%s timeout=%d triggered=%v; want NOMINAL, 604800 and no trigger time", state, timeout, triggered)
	}
	at, err := time.Parse(time.RFC3339Nano, heartbeat)
	if err != nil {
		t.Fatalf("migrated heartbeat %q is not RFC 3339: %v", heartbeat, err)
	}
	if at.Before(before) {
		t.Fatalf("heartbeat %s was not refreshed by the upgrade", heartbeat)
	}

	// v36: emergency audit events can carry the Owner's signature.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO emergency_audit_events (event_id, event_type, reason, created_at, signature)
		VALUES ('emevent_sig', 'HEARTBEAT_RECORDED', 'r', '2026-11-10T12:00:00Z', X'0102')`); err != nil {
		t.Fatalf("insert an audit event with a signature: %v", err)
	}
}

// The envelope seeded for synthesized obligation Tasks must not carry a budget
// nobody granted: after the upgrade it is unfunded until the Owner funds it.
func TestDefaultEnvelopeIsUnfundedAfterMigration(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	var limit int64
	if err := store.DB().QueryRowContext(ctx, `SELECT hard_limit FROM resource_envelopes WHERE envelope_id = 'envelope_default'`).Scan(&limit); err != nil {
		t.Fatal(err)
	}
	if limit != 0 {
		t.Fatalf("default envelope limit = %d, want 0", limit)
	}
}
