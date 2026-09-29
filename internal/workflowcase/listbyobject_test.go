package workflowcase_test

import (
	"context"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/purpose"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/testutil"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

func insertMissionFixture(t *testing.T, store *state.Store, mission string, now string) {
	t.Helper()
	if _, err := store.DB().ExecContext(context.Background(),
		`INSERT INTO missions(mission_id, statement, active, created_at) VALUES (?, 'triage issues', 1, ?)`,
		mission, now,
	); err != nil {
		t.Fatal(err)
	}
}

func TestListByObjectReturnsEveryStateOfOneIssue(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC))
	svc := workflowcase.New(store, clk, purpose.New(store, clk))

	now := clk.Now().UTC().Format(time.RFC3339Nano)
	insertMissionFixture(t, store, "mission-1", now)
	insert := func(mission, source, object, revision string) {
		t.Helper()
		if _, err := store.DB().ExecContext(ctx,
			`INSERT INTO workflow_cases(case_id, mission_id, source, object_id, revision_id,
				observation_evidence_id, initial_request_json, grant_json, state, current_work_id,
				next_work_json, completed_steps, max_steps, remaining_budget, progress_signature,
				created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, '{}', '{}', 'ACTIVE', ?, '{}', 0, 3, 10, '', ?, ?)`,
			domain.NewID("case"), mission, source, object, revision, domain.NewID("evidence"),
			domain.NewID("work"), now, now,
		); err != nil {
			t.Fatal(err)
		}
	}
	insert("mission-1", "github", "o/r#42", "2026-09-28T09:00:00Z")
	insert("mission-1", "github", "o/r#42", "2026-09-28T10:00:00Z")
	insert("mission-1", "github", "o/r#99", "2026-09-28T10:00:00Z")

	cases, err := svc.ListByObject(ctx, "mission-1", "github", "o/r#42")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 {
		t.Fatalf("cases = %d, want both revisions of one object", len(cases))
	}
}

func TestListByObjectRejectsIncompleteArguments(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC))
	svc := workflowcase.New(store, clk, purpose.New(store, clk))

	if _, err := svc.ListByObject(ctx, "", "github", "o/r#42"); err == nil {
		t.Fatal("an empty mission was accepted")
	}
	if _, err := svc.ListByObject(ctx, "mission-1", "", "o/r#42"); err == nil {
		t.Fatal("an empty source was accepted")
	}
	if _, err := svc.ListByObject(ctx, "mission-1", "github", ""); err == nil {
		t.Fatal("an empty object id was accepted")
	}
}

func TestListBySourceSpansEveryState(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC))
	svc := workflowcase.New(store, clk, purpose.New(store, clk))

	now := clk.Now().UTC().Format(time.RFC3339Nano)
	insertMissionFixture(t, store, "mission-1", now)
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO workflow_cases(case_id, mission_id, source, object_id, revision_id,
			observation_evidence_id, initial_request_json, grant_json, state, current_work_id,
			next_work_json, completed_steps, max_steps, remaining_budget, progress_signature,
			created_at, updated_at)
		 VALUES (?, 'mission-1', 'github', 'o/r#42', 'r1', ?, '{}', '{}', 'BLOCKED', '', '{}', 0, 3, 10, '', ?, ?)`,
		domain.NewID("case"), domain.NewID("evidence"), now, now,
	); err != nil {
		t.Fatal(err)
	}

	cases, err := svc.ListBySource(ctx, "mission-1", "github")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 {
		t.Fatalf("cases = %d, want the BLOCKED case too", len(cases))
	}
}
