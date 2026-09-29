package sqlite

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

// The index behind the triage driver's lookup has to exist, carry the columns
// that lookup is equality-first on, and come and go with the migration that
// created it. Whether the lookup is served by it is the other half of the same
// claim, and it is asserted in evidence_kind_query_plan_test.go, which can reach
// the statement the lookup actually runs and this file cannot.
func TestEvidenceKindIndexServesTheTriageDriversLookup(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "evidence-kind-index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	db := store.DB()

	var columns string
	if err := db.QueryRowContext(ctx,
		`SELECT group_concat(name) FROM pragma_index_info('evidence_objects_kind_created_at')`,
	).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if got, want := columns, "kind,created_at,evidence_id"; got != want {
		t.Fatalf("index columns = %q, want %q, the query's own equality then order", got, want)
	}

	for _, tc := range []struct{ kind, id, hash, at string }{
		{"doc", "evidence-index-1", "hash-1", "2026-09-28T10:00:00.000000000Z"},
		{"doc", "evidence-index-2", "hash-2", "2026-09-28T10:00:01.000000000Z"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO evidence_objects(evidence_id,content_hash,media_type,kind,size_bytes,created_at)
			 VALUES (?,?,'text/plain',?,1,?)`, tc.id, tc.hash, tc.kind, tc.at,
		); err != nil {
			t.Fatal(err)
		}
	}

	// That the index exists says nothing about whether the lookup is served by
	// it, and the plan assertion can only be made over the statement the lookup
	// runs - which lives in the evidence package, so it cannot be reached from
	// here. It is in evidence_kind_query_plan_test.go, in the external test
	// package, over evidence.FindByKindQuery itself.
	//
	// What is left here is the half only this package can check: the index
	// carries the columns the query is equality-first on, and it goes away and
	// comes back with the migration that created it.
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithTableName("schema_migrations"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 15); err != nil {
		t.Fatalf("down to v15: %v", err)
	}
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='evidence_objects_kind_created_at'`,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("index survived the down migration: %d", count)
	}
	if _, err := provider.UpTo(ctx, 16); err != nil {
		t.Fatalf("up to v16: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='evidence_objects_kind_created_at'`,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("index count after re-up=%d, want 1", count)
	}
}
