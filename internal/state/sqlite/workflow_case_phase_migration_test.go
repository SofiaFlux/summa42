package sqlite

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestWorkflowCasePhaseMigrationRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "phase_roundtrip.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	db := store.DB()

	// Verify the phase column exists
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('workflow_cases') WHERE name='phase'`).Scan(&count); err != nil {
		t.Fatalf("check phase column: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected phase column to exist, count=%d", count)
	}

	// Verify rollback down to migration 24
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithTableName("schema_migrations"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := provider.DownTo(ctx, 24); err != nil {
		t.Fatalf("down to v24: %v", err)
	}

	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('workflow_cases') WHERE name='phase'`).Scan(&count); err != nil {
		t.Fatalf("check phase column after down: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected phase column to be dropped after rollback, count=%d", count)
	}

	// Re-apply migration 25
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("re-applying up: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('workflow_cases') WHERE name='phase'`).Scan(&count); err != nil {
		t.Fatalf("check phase column after re-applying up: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected phase column to exist after re-up, count=%d", count)
	}
}
