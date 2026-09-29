package sqlite_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SofiaFlux/summa42/internal/evidence"
	sqlite "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

// The triage driver walks the evidence objects of one kind newest first, and the
// kind index is what keeps that walk from scanning and sorting the whole table
// on every case, on every revision, on every tick.
//
// The plan has to be taken over evidence.FindByKindQuery, the statement
// FindByKind runs, and not over a copy of it typed here: a plan assertion over
// a re-typed statement proves only that the copy is served. FindByKind could
// order by content_hash, which this index cannot serve, and such a test would
// stay green while every lookup degraded to a scan and a sort.
func TestTheKindIndexServesTheQueryFindByKindRuns(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "evidence-kind-plan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	db := store.DB()

	for _, tc := range []struct{ kind, id, hash, at string }{
		{"doc", "evidence-plan-1", "hash-1", "2026-09-28T10:00:00.000000000Z"},
		{"doc", "evidence-plan-2", "hash-2", "2026-09-28T10:00:01.000000000Z"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO evidence_objects(evidence_id,content_hash,media_type,kind,size_bytes,created_at)
			 VALUES (?,?,'text/plain',?,1,?)`, tc.id, tc.hash, tc.kind, tc.at,
		); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+evidence.FindByKindQuery, "doc", 10)
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
}

// The statement is the one that has to be served, so both halves of it that
// decide how it is served are named in one place: an order the index cannot
// deliver is a silent full scan and sort on every case of every tick, and the
// only thing standing between that regression and production is this file.
func TestTheKindLookupStatementIsTheOneTheIndexWasCreatedFor(t *testing.T) {
	if !strings.Contains(evidence.FindByKindQuery, "WHERE "+evidence.FindByKindPredicate+" ") {
		t.Fatalf("query = %q, want the exported predicate in it", evidence.FindByKindQuery)
	}
	if !strings.Contains(evidence.FindByKindQuery, "ORDER BY "+evidence.FindByKindOrderBy+" ") {
		t.Fatalf("query = %q, want the exported order in it", evidence.FindByKindQuery)
	}
}
