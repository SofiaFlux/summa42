package execution_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

func newTriageTask(t *testing.T, store interface {
	DB() *sql.DB
}, svc *execution.Service, ctx context.Context, key string) domain.Task {
	t.Helper()
	envelope := domain.NewID("envelope")
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, ?, ?)`,
		envelope, 100, "2026-09-28T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	task, err := svc.CreateTask(ctx, execution.TaskRequest{
		Purpose:              domain.PurposeRef{Kind: domain.PurposeOwnerDirective, ID: "owner"},
		TaskClass:            "github.issue.triage",
		Objective:            "triage",
		AcceptanceCriteria:   []string{"issue triaged"},
		RequiredCapabilities: []string{"github.issue.read"},
		RequiredEnforcement:  domain.EnforcementEnforced,
		AuthorityCeiling:     []string{"github.issue.read"},
		ResourceEnvelopeID:   envelope,
		IdempotencyKey:       key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestChallengeTaskIfInStatesRefusesAnAcceptedTask(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC))
	svc := execution.New(store, clk, purpose.New(store, clk))
	task := newTriageTask(t, store, svc, ctx, "work-accepted")

	if _, err := store.DB().ExecContext(ctx,
		`UPDATE tasks SET state = ? WHERE task_id = ?`, domain.TaskSucceeded, task.ID); err != nil {
		t.Fatal(err)
	}

	changed, err := svc.ChallengeTaskIfInStates(ctx, task.ID,
		[]domain.TaskState{domain.TaskEligible, domain.TaskExecuting},
		domain.ChallengeTask, "superseded-by:2026-09-28T11:00:00Z", nil)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("a SUCCEEDED task was challenged")
	}
	stored, err := svc.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != domain.TaskSucceeded {
		t.Fatalf("state = %q, want SUCCEEDED", stored.State)
	}
	var challenges int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM task_challenges WHERE task_id = ?`, task.ID).Scan(&challenges); err != nil {
		t.Fatal(err)
	}
	if challenges != 0 {
		t.Fatalf("challenge rows = %d, want 0", challenges)
	}
}

func TestChallengeTaskIfInStatesChallengesAnEligibleTask(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC))
	svc := execution.New(store, clk, purpose.New(store, clk))
	task := newTriageTask(t, store, svc, ctx, "work-eligible")

	changed, err := svc.ChallengeTaskIfInStates(ctx, task.ID,
		[]domain.TaskState{domain.TaskEligible, domain.TaskExecuting},
		domain.ChallengeTask, "superseded-by:2026-09-28T11:00:00Z",
		[]domain.ID{domain.NewID("evidence")})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("an ELIGIBLE task was not challenged")
	}
	stored, err := svc.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != domain.TaskChallenged {
		t.Fatalf("state = %q, want CHALLENGED", stored.State)
	}
	var scope, reason, evidence string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT scope, reason, evidence_ids_json FROM task_challenges WHERE task_id = ?`, task.ID,
	).Scan(&scope, &reason, &evidence); err != nil {
		t.Fatal(err)
	}
	if scope != string(domain.ChallengeTask) {
		t.Fatalf("scope = %q, want TASK", scope)
	}
	if reason != "superseded-by:2026-09-28T11:00:00Z" {
		t.Fatalf("reason = %q", reason)
	}
	if evidence == "" || evidence == "null" || evidence == "[]" {
		t.Fatalf("evidence_ids_json = %q, want the supplied evidence id", evidence)
	}
}

func TestChallengeTaskIfInStatesRejectsAnEmptyStateList(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC))
	svc := execution.New(store, clk, purpose.New(store, clk))
	task := newTriageTask(t, store, svc, ctx, "work-no-states")

	if _, err := svc.ChallengeTaskIfInStates(ctx, task.ID, nil,
		domain.ChallengeTask, "superseded", nil); err == nil {
		t.Fatal("an empty state list was accepted")
	}
}
