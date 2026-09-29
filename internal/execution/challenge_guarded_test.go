package execution_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

type storeDB interface {
	DB() *sql.DB
}

func newTriageTask(t *testing.T, store storeDB, svc *execution.Service, ctx context.Context, key string) domain.Task {
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

func newTriageService(t *testing.T) (storeDB, *execution.Service, context.Context) {
	t.Helper()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC))
	return store, execution.New(store, clk, purpose.New(store, clk)), context.Background()
}

// leaseTriageTask leaves the task EXECUTING with a live lease, which is the
// only state in which the lease-revocation branch of the guarded challenge can
// run.
func leaseTriageTask(t *testing.T, store storeDB, svc *execution.Service, ctx context.Context, key string) (domain.Task, domain.Attempt) {
	t.Helper()
	task := newTriageTask(t, store, svc, ctx, key)
	attempt, err := svc.StartAttempt(ctx, task.ID, "github.issue.triage", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return task, attempt
}

type attemptRow struct {
	leaseState  domain.LeaseState
	state       domain.AttemptState
	completedAt sql.NullString
}

func readAttempt(t *testing.T, store storeDB, ctx context.Context, attemptID domain.ID) attemptRow {
	t.Helper()
	var row attemptRow
	if err := store.DB().QueryRowContext(ctx,
		`SELECT lease_state, state, completed_at FROM attempts WHERE attempt_id = ?`, attemptID,
	).Scan(&row.leaseState, &row.state, &row.completedAt); err != nil {
		t.Fatal(err)
	}
	return row
}

func countRows(t *testing.T, store storeDB, ctx context.Context, query string, args ...any) int {
	t.Helper()
	var count int
	if err := store.DB().QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestChallengeTaskIfInStatesRefusesAnAcceptedTask(t *testing.T) {
	store, svc, ctx := newTriageService(t)
	task, attempt := leaseTriageTask(t, store, svc, ctx, "work-accepted")

	// The shape is built by hand on purpose: leaseTriageTask is the only way to
	// give a task a current_attempt_id, and it leaves the task EXECUTING, so a
	// SUCCEEDED task still holding a LEASED/ACTIVE attempt with a NULL
	// completed_at cannot be produced by any production path.
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
	if challenges := countRows(t, store, ctx,
		`SELECT count(*) FROM task_challenges WHERE task_id = ?`, task.ID); challenges != 0 {
		t.Fatalf("challenge rows = %d, want 0", challenges)
	}
	if events := countRows(t, store, ctx,
		`SELECT count(*) FROM execution_events WHERE task_id = ? AND event_type = 'TASK_CHALLENGED'`, task.ID); events != 0 {
		t.Fatalf("TASK_CHALLENGED events = %d, want 0", events)
	}
	got := readAttempt(t, store, ctx, attempt.ID)
	if got.leaseState != domain.LeaseActive {
		t.Fatalf("lease state = %q, want ACTIVE untouched by the refusal", got.leaseState)
	}
	if got.state != domain.AttemptLeased {
		t.Fatalf("attempt state = %q, want LEASED untouched by the refusal", got.state)
	}
	if got.completedAt.Valid {
		t.Fatalf("completed_at = %q, want NULL after the refusal", got.completedAt.String)
	}
}

func TestChallengeTaskIfInStatesChallengesAnEligibleTask(t *testing.T) {
	store, svc, ctx := newTriageService(t)
	task := newTriageTask(t, store, svc, ctx, "work-eligible")
	evidenceID := domain.NewID("evidence")

	changed, err := svc.ChallengeTaskIfInStates(ctx, task.ID,
		[]domain.TaskState{domain.TaskEligible, domain.TaskExecuting},
		domain.ChallengeTask, "superseded-by:2026-09-28T11:00:00Z",
		[]domain.ID{evidenceID})
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
	wantEvidence, err := json.Marshal([]domain.ID{evidenceID})
	if err != nil {
		t.Fatal(err)
	}
	if evidence != string(wantEvidence) {
		t.Fatalf("evidence_ids_json = %q, want %s", evidence, wantEvidence)
	}
	if events := countRows(t, store, ctx,
		`SELECT count(*) FROM execution_events WHERE task_id = ? AND event_type = 'TASK_CHALLENGED'`, task.ID); events != 1 {
		t.Fatalf("TASK_CHALLENGED events = %d, want 1", events)
	}
}

// A task in EXECUTING holds a live lease. The guarded challenge must stop that
// execution, or the superseded revision keeps working after the challenge.
func TestChallengeTaskIfInStatesRevokesTheLeaseOfAnExecutingTask(t *testing.T) {
	store, svc, ctx := newTriageService(t)
	task, attempt := leaseTriageTask(t, store, svc, ctx, "work-executing")

	changed, err := svc.ChallengeTaskIfInStates(ctx, task.ID,
		[]domain.TaskState{domain.TaskEligible, domain.TaskExecuting},
		domain.ChallengeTask, "superseded-by:2026-09-28T11:00:00Z", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("an EXECUTING task was not challenged")
	}
	stored, err := svc.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != domain.TaskChallenged {
		t.Fatalf("state = %q, want CHALLENGED", stored.State)
	}
	got := readAttempt(t, store, ctx, attempt.ID)
	if got.leaseState != domain.LeaseRevoked {
		t.Fatalf("lease state = %q, want REVOKED", got.leaseState)
	}
	if got.state != domain.AttemptCancelled {
		t.Fatalf("attempt state = %q, want CANCELLED", got.state)
	}
	if !got.completedAt.Valid {
		t.Fatal("completed_at is NULL, want the revocation timestamp")
	}
	if challenges := countRows(t, store, ctx,
		`SELECT count(*) FROM task_challenges WHERE task_id = ?`, task.ID); challenges != 1 {
		t.Fatalf("challenge rows = %d, want 1", challenges)
	}
	if events := countRows(t, store, ctx,
		`SELECT count(*) FROM execution_events WHERE task_id = ? AND event_type = 'TASK_CHALLENGED'`, task.ID); events != 1 {
		t.Fatalf("TASK_CHALLENGED events = %d, want 1", events)
	}
}

// Two superseded-revision callers racing for the same task: the state predicate
// is evaluated inside the transaction, so exactly one of them may write.
func TestChallengeTaskIfInStatesAdmitsExactlyOneConcurrentCaller(t *testing.T) {
	store, svc, ctx := newTriageService(t)
	task, attempt := leaseTriageTask(t, store, svc, ctx, "work-concurrent")

	start := make(chan struct{})
	results := make([]bool, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = svc.ChallengeTaskIfInStates(ctx, task.ID,
				[]domain.TaskState{domain.TaskEligible, domain.TaskExecuting},
				domain.ChallengeTask, "superseded-by:2026-09-28T11:00:00Z", nil)
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if results[i] {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("changed = %v, want exactly one true", results)
	}
	if challenges := countRows(t, store, ctx,
		`SELECT count(*) FROM task_challenges WHERE task_id = ?`, task.ID); challenges != 1 {
		t.Fatalf("challenge rows = %d, want 1", challenges)
	}
	if events := countRows(t, store, ctx,
		`SELECT count(*) FROM execution_events WHERE task_id = ? AND event_type = 'TASK_CHALLENGED'`, task.ID); events != 1 {
		t.Fatalf("TASK_CHALLENGED events = %d, want 1", events)
	}
	got := readAttempt(t, store, ctx, attempt.ID)
	if got.leaseState != domain.LeaseRevoked || got.state != domain.AttemptCancelled {
		t.Fatalf("attempt = %q/%q, want CANCELLED/REVOKED", got.state, got.leaseState)
	}
}

// FailAttempt leaves current_attempt_id pointing at the attempt it just made
// terminal and returns the task to ELIGIBLE, so a transient model failure leaves
// a task that is ELIGIBLE with a terminal current attempt. That is a routine
// state, and a superseded revision must still be able to stop it.
func TestChallengeTaskIfInStatesChallengesATaskAwaitingRetryAfterAFailedAttempt(t *testing.T) {
	store, svc, ctx := newTriageService(t)
	task, attempt := leaseTriageTask(t, store, svc, ctx, "work-awaiting-retry")

	if err := svc.FailAttempt(ctx, attempt.ID, domain.FailureTransient,
		"transient-model-error", nil); err != nil {
		t.Fatal(err)
	}
	awaiting, err := svc.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if awaiting.State != domain.TaskEligible {
		t.Fatalf("state after FailAttempt = %q, want ELIGIBLE", awaiting.State)
	}
	failed := readAttempt(t, store, ctx, attempt.ID)
	if failed.state != domain.AttemptFailed || failed.leaseState != domain.LeaseRevoked {
		t.Fatalf("attempt after FailAttempt = %q/%q, want FAILED/REVOKED", failed.state, failed.leaseState)
	}

	changed, err := svc.ChallengeTaskIfInStates(ctx, task.ID,
		[]domain.TaskState{domain.TaskEligible, domain.TaskExecuting},
		domain.ChallengeTask, "superseded-by:2026-09-28T11:00:00Z", nil)
	if err != nil {
		t.Fatalf("an ELIGIBLE task with a terminal current attempt was refused: %v", err)
	}
	if !changed {
		t.Fatal("an ELIGIBLE task awaiting retry was not challenged")
	}
	stored, err := svc.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != domain.TaskChallenged {
		t.Fatalf("state = %q, want CHALLENGED", stored.State)
	}
	if challenges := countRows(t, store, ctx,
		`SELECT count(*) FROM task_challenges WHERE task_id = ?`, task.ID); challenges != 1 {
		t.Fatalf("challenge rows = %d, want 1", challenges)
	}
	if events := countRows(t, store, ctx,
		`SELECT count(*) FROM execution_events WHERE task_id = ? AND event_type = 'TASK_CHALLENGED'`, task.ID); events != 1 {
		t.Fatalf("TASK_CHALLENGED events = %d, want 1", events)
	}
	// The attempt was already terminal, so the challenge must leave it alone
	// rather than rewrite FAILED as CANCELLED.
	after := readAttempt(t, store, ctx, attempt.ID)
	if after.state != domain.AttemptFailed || after.leaseState != domain.LeaseRevoked {
		t.Fatalf("attempt = %q/%q, want the pre-existing FAILED/REVOKED", after.state, after.leaseState)
	}
}

// The current attempt no longer resolves to an attempts row, so revoking its
// lease cannot match and there is nothing to show it is terminal. The challenge
// must fail rather than report changed for an attempt it never touched.
func TestChallengeTaskIfInStatesReportsAStaleCurrentAttempt(t *testing.T) {
	store, svc, ctx := newTriageService(t)
	task, _ := leaseTriageTask(t, store, svc, ctx, "work-stale-attempt")

	if _, err := store.DB().ExecContext(ctx,
		`UPDATE tasks SET current_attempt_id = ? WHERE task_id = ?`,
		domain.NewID("attempt"), task.ID); err != nil {
		t.Fatal(err)
	}

	changed, err := svc.ChallengeTaskIfInStates(ctx, task.ID,
		[]domain.TaskState{domain.TaskEligible, domain.TaskExecuting},
		domain.ChallengeTask, "superseded-by:2026-09-28T11:00:00Z", nil)
	if !errors.Is(err, domain.ErrStaleAttempt) {
		t.Fatalf("err = %v, want ErrStaleAttempt", err)
	}
	if changed {
		t.Fatal("a task with a stale current attempt reported changed")
	}
	stored, err := svc.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != domain.TaskExecuting {
		t.Fatalf("state = %q, want EXECUTING", stored.State)
	}
	if challenges := countRows(t, store, ctx,
		`SELECT count(*) FROM task_challenges WHERE task_id = ?`, task.ID); challenges != 0 {
		t.Fatalf("challenge rows = %d, want 0 after rollback", challenges)
	}
}

func TestChallengeTaskIfInStatesRejectsAnUnknownTaskState(t *testing.T) {
	store, svc, ctx := newTriageService(t)
	task := newTriageTask(t, store, svc, ctx, "work-misspelled-state")

	changed, err := svc.ChallengeTaskIfInStates(ctx, task.ID,
		[]domain.TaskState{domain.TaskState("eligible")},
		domain.ChallengeTask, "superseded-by:2026-09-28T11:00:00Z", nil)
	if err == nil {
		t.Fatal("a misspelled task state was accepted")
	}
	if changed {
		t.Fatal("a misspelled task state reported changed")
	}
	stored, err := svc.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != domain.TaskEligible {
		t.Fatalf("state = %q, want ELIGIBLE", stored.State)
	}
	if challenges := countRows(t, store, ctx,
		`SELECT count(*) FROM task_challenges WHERE task_id = ?`, task.ID); challenges != 0 {
		t.Fatalf("challenge rows = %d, want 0", challenges)
	}
}

func TestChallengeTaskIfInStatesRejectsAnEmptyStateList(t *testing.T) {
	store, svc, ctx := newTriageService(t)
	task := newTriageTask(t, store, svc, ctx, "work-no-states")

	changed, err := svc.ChallengeTaskIfInStates(ctx, task.ID, nil,
		domain.ChallengeTask, "superseded", nil)
	if err == nil {
		t.Fatal("an empty state list was accepted")
	}
	if changed {
		t.Fatal("an empty state list reported changed")
	}
}
