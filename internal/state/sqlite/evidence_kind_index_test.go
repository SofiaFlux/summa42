package sqlite

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// The triage driver walks the evidence objects of one kind newest first, because
// the store has no case or revision column. The index is what keeps that lookup
// from being a full scan and sort of the whole table on every case, on every
// revision, on every tick.
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

	// The plan has to name the index: an assertion that the index exists says
	// nothing about whether the lookup uses it.
	rows, err := db.QueryContext(ctx,
		`EXPLAIN QUERY PLAN
		 SELECT evidence_id, content_hash, media_type, kind, size_bytes, created_at
		 FROM evidence_objects WHERE kind = ?
		 ORDER BY created_at DESC, evidence_id DESC LIMIT 10`, "doc")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	plan := make([]string, 0, 4)
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(plan) == 0 {
		t.Fatal("the lookup produced no query plan")
	}
	if !strings.Contains(plan[len(plan)-1], "evidence_objects_kind_created_at") {
		t.Fatalf("lookup plan = %v, want it served by evidence_objects_kind_created_at", plan)
	}
	for _, step := range plan {
		if strings.Contains(step, "SCAN") && !strings.Contains(step, "USING") {
			t.Fatalf("lookup plan = %v, want no bare scan", plan)
		}
	}

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
