package sqlite

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestCollectiveBoardMigrationRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "board_roundtrip.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	db := store.DB()

	// Verify the board_cards and board_card_moves tables exist
	for _, table := range []string{"board_cards", "board_card_moves"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("expected table %s to exist, count=%d", table, count)
		}
	}

	// Verify rollback down to migration 25
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithTableName("schema_migrations"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := provider.DownTo(ctx, 25); err != nil {
		t.Fatalf("down to v25: %v", err)
	}

	for _, table := range []string{"board_cards", "board_card_moves"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s after down: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("expected table %s to be dropped after rollback, count=%d", table, count)
		}
	}

	// Re-apply migration 26
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("re-applying up: %v", err)
	}
	for _, table := range []string{"board_cards", "board_card_moves"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("check table %s after re-applying up: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("expected table %s to exist after re-up, count=%d", table, count)
		}
	}
}
