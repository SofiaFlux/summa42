package execution

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newExecutionService(t *testing.T) (*Service, *fakeClock, context.Context, domain.ID) {
	t.Helper()
	store := testutil.OpenStore(t)
	clk := &fakeClock{now: time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)}
	purposes := purpose.New(store, clk)
	missionID, err := purposes.CreateMission(context.Background(), "Exercise durable execution semantics")
	if err != nil {
		t.Fatal(err)
	}
	return New(store, clk, purposes), clk, context.Background(), missionID
}

func baseTaskRequest(missionID domain.ID) TaskRequest {
	return TaskRequest{
		Purpose:              domain.PurposeRef{Kind: domain.PurposeMission, ID: missionID},
		AcceptanceCriteria:   []string{"verified output"},
		RequiredCapabilities: []string{"read", "write"},
		RequiredEnforcement:  domain.EnforcementPartial,
		ResourceEnvelopeID:   domain.ID("envelope_parent"),
		Priority:             10,
	}
}

func seedActiveAttempt(t *testing.T, svc *Service, missionID domain.ID) (domain.Task, domain.Attempt) {
	t.Helper()
	ctx := context.Background()
	task, err := svc.CreateTask(ctx, baseTaskRequest(missionID))
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := svc.StartAttempt(ctx, task.ID, "test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return task, attempt
}

type failingAttemptStartRecorder struct{}

func (failingAttemptStartRecorder) RecordAttemptStartInTx(context.Context, *sql.Tx, domain.Attempt, domain.Task) error {
	return errors.New("manifest write failed")
}

func TestStartAttemptRollsBackWhenRunManifestCannotBePersisted(t *testing.T) {
	svc, _, ctx, missionID := newExecutionService(t)
	svc.startRecorders = []AttemptStartRecorder{failingAttemptStartRecorder{}}
	task, err := svc.CreateTask(ctx, baseTaskRequest(missionID))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.StartAttempt(ctx, task.ID, "test", time.Minute); err == nil {
		t.Fatal("StartAttempt succeeded despite run manifest failure")
	}
	var attempts int
	if err := svc.store.DB().QueryRowContext(ctx,
		"SELECT count(*) FROM attempts WHERE task_id = ?", task.ID,
	).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Fatalf("attempt rows = %d, want 0 after rollback", attempts)
	}
	var state domain.TaskState
	var currentAttempt sql.NullString
	var fence int64
	if err := svc.store.DB().QueryRowContext(ctx,
		"SELECT state, current_attempt_id, current_fence FROM tasks WHERE task_id = ?", task.ID,
	).Scan(&state, &currentAttempt, &fence); err != nil {
		t.Fatal(err)
	}
	if state != domain.TaskEligible || currentAttempt.Valid || fence != 0 {
		t.Fatalf("task mutated despite manifest rollback: state=%s current=%v fence=%d", state, currentAttempt, fence)
	}
}

func TestGuardRejectsExpiredLeaseEvenWhenFenceMatches(t *testing.T) {
	svc, fakeClock, ctx, missionID := newExecutionService(t)
	task, attempt := seedActiveAttempt(t, svc, missionID)
	fakeClock.Advance(2 * time.Minute)
	err := svc.WithGuardedAttempt(ctx, attempt.ID, []domain.TaskState{domain.TaskExecuting}, func(tx *sql.Tx, guarded GuardedAttempt) error {
		return nil
	})
	if !errors.Is(err, domain.ErrLeaseInactive) {
		t.Fatalf("got %v for task %s, want ErrLeaseInactive", err, task.ID)
	}
}

func TestGuardRejectsRevokedLeaseWithoutReplacement(t *testing.T) {
	svc, _, ctx, missionID := newExecutionService(t)
	_, attempt := seedActiveAttempt(t, svc, missionID)
	if err := svc.RevokeLease(ctx, attempt.ID); err != nil {
		t.Fatal(err)
	}
	err := svc.WithGuardedAttempt(ctx, attempt.ID, []domain.TaskState{domain.TaskEligible, domain.TaskExecuting}, func(*sql.Tx, GuardedAttempt) error { return nil })
	if !errors.Is(err, domain.ErrLeaseInactive) {
		t.Fatalf("got %v, want ErrLeaseInactive", err)
	}
}

func TestGuardRejectsNonCurrentAttemptAndFenceMismatch(t *testing.T) {
	svc, _, ctx, missionID := newExecutionService(t)
	task, first := seedActiveAttempt(t, svc, missionID)
	if err := svc.RevokeLease(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := svc.StartAttempt(ctx, task.ID, "replacement", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second.FenceGeneration <= first.FenceGeneration {
		t.Fatalf("replacement fence %d must exceed old fence %d", second.FenceGeneration, first.FenceGeneration)
	}
	if err := svc.WithGuardedAttempt(ctx, first.ID, []domain.TaskState{domain.TaskExecuting}, func(*sql.Tx, GuardedAttempt) error { return nil }); !errors.Is(err, domain.ErrStaleAttempt) {
		t.Fatalf("old Attempt guard = %v, want ErrStaleAttempt", err)
	}

	if _, err := svc.store.DB().ExecContext(ctx, `UPDATE tasks SET current_fence = current_fence + 1 WHERE task_id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.WithGuardedAttempt(ctx, second.ID, []domain.TaskState{domain.TaskExecuting}, func(*sql.Tx, GuardedAttempt) error { return nil }); !errors.Is(err, domain.ErrStaleAttempt) {
		t.Fatalf("mismatched fence guard = %v, want ErrStaleAttempt", err)
	}
}

func TestGuardRejectsWrongTaskStateAndAcceptsValidLease(t *testing.T) {
	svc, _, ctx, missionID := newExecutionService(t)
	_, attempt := seedActiveAttempt(t, svc, missionID)
	called := false
	if err := svc.WithGuardedAttempt(ctx, attempt.ID, []domain.TaskState{domain.TaskExecuting}, func(_ *sql.Tx, guarded GuardedAttempt) error {
		called = true
		if guarded.AttemptID != attempt.ID || guarded.FenceGeneration != attempt.FenceGeneration {
			t.Fatalf("unexpected guard: %#v", guarded)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("guarded mutation callback was not called")
	}
	if err := svc.WithGuardedAttempt(ctx, attempt.ID, []domain.TaskState{domain.TaskAwaitingVerification}, func(*sql.Tx, GuardedAttempt) error { return nil }); !errors.Is(err, domain.ErrStaleAttempt) {
		t.Fatalf("wrong-state guard = %v, want ErrStaleAttempt", err)
	}
}

func TestCreateTaskRejectsInvalidPurpose(t *testing.T) {
	svc, _, ctx, missionID := newExecutionService(t)
	req := baseTaskRequest(missionID)
	req.Purpose.ID = "mission_missing"
	if _, err := svc.CreateTask(ctx, req); !errors.Is(err, domain.ErrInvalidPurpose) {
		t.Fatalf("got %v, want ErrInvalidPurpose", err)
	}
}

func TestChildTaskInheritsAndCannotWidenParentScope(t *testing.T) {
	svc, _, ctx, missionID := newExecutionService(t)
	parent, err := svc.CreateTask(ctx, baseTaskRequest(missionID))
	if err != nil {
		t.Fatal(err)
	}
	child, err := svc.CreateChildTask(ctx, parent.ID, TaskRequest{
		AcceptanceCriteria:   []string{"child verified"},
		RequiredCapabilities: []string{"read"},
		RequiredEnforcement:  domain.EnforcementEnforced,
	})
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentTaskID != parent.ID || child.Purpose != parent.Purpose || child.ResourceEnvelopeID != parent.ResourceEnvelopeID {
		t.Fatalf("child did not inherit lineage/envelope: parent=%#v child=%#v", parent, child)
	}

	if _, err := svc.CreateChildTask(ctx, parent.ID, TaskRequest{RequiredCapabilities: []string{"admin"}}); err == nil {
		t.Fatal("child must not widen parent capability scope")
	}
	if _, err := svc.CreateChildTask(ctx, parent.ID, TaskRequest{ResourceEnvelopeID: "envelope_larger"}); err == nil {
		t.Fatal("child must not replace inherited resource envelope")
	}
	if _, err := svc.CreateChildTask(ctx, parent.ID, TaskRequest{RequiredEnforcement: domain.EnforcementUnenforced}); err == nil {
		t.Fatal("child must not weaken parent enforcement requirement")
	}
}

func TestFailAttemptBlocksRepeatedFailureSignature(t *testing.T) {
	svc, _, ctx, missionID := newExecutionService(t)
	task, first := seedActiveAttempt(t, svc, missionID)
	if err := svc.FailAttempt(ctx, first.ID, domain.FailureTransient, "provider-timeout", nil); err != nil {
		t.Fatal(err)
	}
	second, err := svc.StartAttempt(ctx, task.ID, "retry", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.FailAttempt(ctx, second.ID, domain.FailureTransient, "provider-timeout", nil); err != nil {
		t.Fatal(err)
	}
	var state domain.TaskState
	if err := svc.store.DB().QueryRowContext(ctx, `SELECT state FROM tasks WHERE task_id = ?`, task.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != domain.TaskBlocked {
		t.Fatalf("repeated failure left task %q, want BLOCKED replan signal", state)
	}
}

func TestChallengeTaskPersistsChallengeAndStopsExecution(t *testing.T) {
	svc, _, ctx, missionID := newExecutionService(t)
	task, _ := seedActiveAttempt(t, svc, missionID)
	if _, err := svc.ChallengeTaskIfInStates(ctx, task.ID, testutil.AnyTaskState, domain.ChallengeMissionAssumption, "mission assumption contradicted", []domain.ID{"evidence_1"}); err != nil {
		t.Fatal(err)
	}
	var state domain.TaskState
	var count int
	if err := svc.store.DB().QueryRowContext(ctx, `SELECT state FROM tasks WHERE task_id = ?`, task.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM task_challenges WHERE task_id = ?`, task.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if state != domain.TaskChallenged || count != 1 {
		t.Fatalf("challenge state=%q count=%d", state, count)
	}
}

func TestFindByIdempotencyKeyHitAndMiss(t *testing.T) {
	svc, _, ctx, missionID := newExecutionService(t)
	request := baseTaskRequest(missionID)
	request.IdempotencyKey = "driver-work-2"
	task, err := svc.CreateTask(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := svc.FindByIdempotencyKey(ctx, request.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if !found || got.ID != task.ID || got.IdempotencyKey != request.IdempotencyKey {
		t.Fatalf("FindByIdempotencyKey hit = %+v, %v; want task %s", got, found, task.ID)
	}
	got, found, err = svc.FindByIdempotencyKey(ctx, "missing")
	if err != nil {
		t.Fatal(err)
	}
	if found || got.ID != "" {
		t.Fatalf("FindByIdempotencyKey miss = %+v, %v; want zero task, false", got, found)
	}
}
