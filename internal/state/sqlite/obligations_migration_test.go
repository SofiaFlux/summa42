package sqlite

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestObligationsMigrationRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "obligations_roundtrip.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	db := store.DB()

	// Verify insertion into obligations with new columns
	_, err = db.ExecContext(ctx, `
		INSERT INTO obligations (
			obligation_id, source, statement, active, due_at,
			grace_period_seconds, consequences_json, cost_of_delay_rate,
			lead_time_seconds, lifecycle_state, created_at, updated_at
		) VALUES (
			'ob_test_1', 'CONTRACT_SLA', 'Maintain 99.99% uptime', 1, '2026-11-15T00:00:00Z',
			3600, '{"penalty":5000}', 100, 86400, 'PENDING',
			'2026-11-09T08:00:00Z', '2026-11-09T08:00:00Z'
		)`,
	)
	if err != nil {
		t.Fatalf("insert obligation with new columns: %v", err)
	}

	var dueAt, lifecycleState string
	var gracePeriod, costOfDelay int
	err = db.QueryRowContext(ctx, `
		SELECT due_at, grace_period_seconds, cost_of_delay_rate, lifecycle_state
		FROM obligations WHERE obligation_id = 'ob_test_1'`,
	).Scan(&dueAt, &gracePeriod, &costOfDelay, &lifecycleState)
	if err != nil {
		t.Fatalf("query obligation: %v", err)
	}
	if dueAt != "2026-11-15T00:00:00Z" || gracePeriod != 3600 || costOfDelay != 100 || lifecycleState != "PENDING" {
		t.Fatalf("unexpected obligation values: due=%s, grace=%d, cost=%d, state=%s", dueAt, gracePeriod, costOfDelay, lifecycleState)
	}

	// Rollback down to migration 31
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithTableName("schema_migrations"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := provider.DownTo(ctx, 31); err != nil {
		t.Fatalf("down to v31: %v", err)
	}

	// Re-apply migration Up to confirm idempotent re-creation
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up to latest: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM obligations`).Scan(&count); err != nil {
		t.Fatalf("check obligations table after re-up: %v", err)
	}
}
