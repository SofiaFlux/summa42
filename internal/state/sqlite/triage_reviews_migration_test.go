package sqlite

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

// The scheduling key the reviewer relies on, and the index a reader goes through
// to find the verdict a key points at, are both properties of the migration and
// of nothing else: the index store asserts the behaviour, and a table or a key
// that was never created fails the same way - every insert errors and every
// lookup misses. So the table, the key and the index are asserted to exist here,
// and asserted to go away and come back with the migration that owns them.
//
// The index is the half nothing else can reach. No statement in the repository
// reads the table by verdict_evidence_id, so a dropped index is a silent
// regression: every query that would use it still returns the right rows.
func TestTriageReviewTableKeyAndVerdictIndexComeWithTheMigration(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "triage-reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	db := store.DB()

	const table = "github_issue_triage_reviews"
	var key string
	if err := db.QueryRowContext(ctx,
		`SELECT group_concat(name) FROM pragma_table_info(?) WHERE pk > 0 ORDER BY pk`,
		table).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if want := "decision_evidence_id,reviewer_version,state_fingerprint"; key != want {
		t.Fatalf("scheduling key = %q, want %q: the columns a new reviewer version has to differ on", key, want)
	}
	// Existence first, so a missing index says it is missing rather than failing
	// on the NULL a pragma over an absent index returns.
	var exists int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='github_issue_triage_reviews_verdict'`,
	).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists != 1 {
		t.Fatalf("verdict index count = %d, want 1: nothing reads the table by verdict id, so a missing one is silent", exists)
	}
	var indexed string
	if err := db.QueryRowContext(ctx,
		`SELECT group_concat(name) FROM pragma_index_info('github_issue_triage_reviews_verdict')`,
	).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if want := "verdict_evidence_id"; indexed != want {
		t.Fatalf("verdict index columns = %q, want %q", indexed, want)
	}

	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithTableName("schema_migrations"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 16); err != nil {
		t.Fatalf("down to v16: %v", err)
	}
	for _, name := range []string{table, "github_issue_triage_reviews_verdict"} {
		kind := "table"
		if name != table {
			kind = "index"
		}
		var count int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE type=? AND name=?`, kind, name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s %s survived the down migration: %d", kind, name, count)
		}
	}
	if _, err := provider.UpTo(ctx, 17); err != nil {
		t.Fatalf("up to v17: %v", err)
	}
	for _, name := range []string{table, "github_issue_triage_reviews_verdict"} {
		kind := "table"
		if name != table {
			kind = "index"
		}
		var count int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE type=? AND name=?`, kind, name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s %s count after re-up = %d, want 1", kind, name, count)
		}
	}
}
