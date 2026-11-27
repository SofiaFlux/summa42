package scheduler_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/resources"
	"github.com/SofiaFlux/summa42/internal/scheduler"
	"github.com/SofiaFlux/summa42/internal/testutil"
	"github.com/SofiaFlux/summa42/internal/verification"
)

type callbackExecutor struct {
	fn func(ctx context.Context, env executors.AttemptEnvelope) (executors.ExecutionResult, error)
}

func (c callbackExecutor) Start(ctx context.Context, env executors.AttemptEnvelope) (executors.ExecutionResult, error) {
	return c.fn(ctx, env)
}

func TestWorkerHeartbeatRenewsLongAttempt(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	purposes := purpose.New(store, clk)
	execSvc := execution.New(store, clk, purposes)
	resSvc := resources.New(store, clk)
	execSvc.SetEnvelopeChecker(resSvc)

	leaseDuration := 10 * time.Minute
	schedSvc := scheduler.New(store, clk, purposes, execSvc, resSvc, leaseDuration)
	evidenceStore, err := evidence.New(store, t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	verifySvc := verification.New(store, clk, execSvc)

	envelopeID := domain.NewID("envelope")
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, ?, ?)`,
		envelopeID, 5, clk.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	task, err := execSvc.CreateTask(ctx, execution.TaskRequest{
		Purpose:              domain.PurposeRef{Kind: domain.PurposeOwnerDirective, ID: "owner-hb"},
		Objective:            "run long task",
		AcceptanceCriteria:   []string{"done"},
		RequiredCapabilities: []string{"shell"},
		RequiredEnforcement:  domain.EnforcementPartial,
		ResourceEnvelopeID:   envelopeID,
	})
	if err != nil {
		t.Fatal(err)
	}

	var observedExpiry time.Time
	exec := callbackExecutor{fn: func(runCtx context.Context, env executors.AttemptEnvelope) (executors.ExecutionResult, error) {
		initial, err := execSvc.Attempt(runCtx, env.AttemptID)
		if err != nil {
			return executors.ExecutionResult{}, err
		}
		clk.Advance(4 * time.Minute)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			cur, err := execSvc.Attempt(runCtx, env.AttemptID)
			if err != nil {
				return executors.ExecutionResult{}, err
			}
			if cur.LeaseExpiresAt.After(initial.LeaseExpiresAt) {
				observedExpiry = cur.LeaseExpiresAt
				return executors.ExecutionResult{
					ExitCode: 0,
					Stdout:   "completed after renewal",
					Evidence: []executors.Evidence{{Kind: executors.EvidenceAgentMessage, Content: "ok"}},
				}, nil
			}
			time.Sleep(5 * time.Millisecond)
		}
		return executors.ExecutionResult{ExitCode: 1}, errors.New("timed out waiting for lease renewal heartbeat")
	}}

	worker, err := scheduler.NewWorker(schedSvc, execSvc, evidenceStore, verifySvc,
		map[string]executors.Executor{"shell": exec}, clk, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worker.WithLeaseHeartbeat(10*time.Millisecond, time.Hour)

	res, err := worker.StepOnce(ctx, scheduler.CapacitySnapshot{Capabilities: map[string]scheduler.CapabilityCapacity{
		"shell": {Accessible: true, Enforcement: domain.EnforcementPartial},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != scheduler.StepCompleted || res.TaskID != task.ID {
		t.Fatalf("outcome = %s task = %s, want COMPLETED and %s", res.Outcome, res.TaskID, task.ID)
	}
	if observedExpiry.IsZero() {
		t.Fatal("lease expiry was never renewed by heartbeat")
	}
}

func TestWorkerHeartbeatCancelsOnEnvelopeExhausted(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	purposes := purpose.New(store, clk)
	execSvc := execution.New(store, clk, purposes)
	resSvc := resources.New(store, clk)
	execSvc.SetEnvelopeChecker(resSvc)

	schedSvc := scheduler.New(store, clk, purposes, execSvc, resSvc, 10*time.Minute)
	evidenceStore, err := evidence.New(store, t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	verifySvc := verification.New(store, clk, execSvc)

	envelopeID := domain.NewID("envelope")
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, ?, ?)`,
		envelopeID, 1, clk.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := execSvc.CreateTask(ctx, execution.TaskRequest{
		Purpose:              domain.PurposeRef{Kind: domain.PurposeOwnerDirective, ID: "owner-hb-stop"},
		Objective:            "stop when envelope spent",
		AcceptanceCriteria:   []string{"done"},
		RequiredCapabilities: []string{"shell"},
		RequiredEnforcement:  domain.EnforcementPartial,
		ResourceEnvelopeID:   envelopeID,
	}); err != nil {
		t.Fatal(err)
	}

	var cancelled atomic.Bool
	exec := callbackExecutor{fn: func(runCtx context.Context, _ executors.AttemptEnvelope) (executors.ExecutionResult, error) {
		if err := store.WithTx(context.Background(), func(tx *sql.Tx) error {
			res, err := resSvc.ReserveInTx(context.Background(), tx, envelopeID, 1, resources.Enforceability{
				CostControl: resources.CostPrepaidQuota, RequireHardCap: true, Source: "profile:claude",
			})
			if err != nil {
				return err
			}
			return resSvc.SettleInTx(context.Background(), tx, res.ID, 1)
		}); err != nil {
			return executors.ExecutionResult{}, err
		}
		select {
		case <-runCtx.Done():
			cancelled.Store(true)
			return executors.ExecutionResult{ExitCode: 1}, runCtx.Err()
		case <-time.After(2 * time.Second):
			return executors.ExecutionResult{ExitCode: 0}, nil
		}
	}}

	worker, err := scheduler.NewWorker(schedSvc, execSvc, evidenceStore, verifySvc,
		map[string]executors.Executor{"shell": exec}, clk, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worker.WithLeaseHeartbeat(10*time.Millisecond, time.Hour)

	res, err := worker.StepOnce(ctx, scheduler.CapacitySnapshot{Capabilities: map[string]scheduler.CapabilityCapacity{
		"shell": {Accessible: true, Enforcement: domain.EnforcementPartial},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != scheduler.StepFailed || !cancelled.Load() {
		t.Fatalf("outcome=%s cancelled=%v, want FAILED and cancelled=true", res.Outcome, cancelled.Load())
	}
	var failureClass, signature string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT failure_class, signature FROM attempt_failures WHERE attempt_id = ?`, res.AttemptID,
	).Scan(&failureClass, &signature); err != nil {
		t.Fatal(err)
	}
	if domain.FailureClass(failureClass) != domain.FailureResource || !strings.HasPrefix(signature, "worker:budget:") {
		t.Fatalf("failure_class=%q signature=%q, want RESOURCE and worker:budget:...", failureClass, signature)
	}
}

func TestWorkerHeartbeatRejectsInvalidConfigAndCapsBelowLease(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	purposes := purpose.New(store, clk)
	execSvc := execution.New(store, clk, purposes)
	resSvc := resources.New(store, clk)
	schedSvc := scheduler.New(store, clk, purposes, execSvc, resSvc, 10*time.Minute)
	evidenceStore, err := evidence.New(store, t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	verifySvc := verification.New(store, clk, execSvc)

	envelopeID := domain.NewID("envelope")
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, 5, ?)`,
		envelopeID, clk.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := execSvc.CreateTask(ctx, execution.TaskRequest{
		Purpose:              domain.PurposeRef{Kind: domain.PurposeOwnerDirective, ID: "owner-hb-cfg"},
		Objective:            "validate heartbeat config",
		AcceptanceCriteria:   []string{"done"},
		RequiredCapabilities: []string{"shell"},
		RequiredEnforcement:  domain.EnforcementPartial,
		ResourceEnvelopeID:   envelopeID,
	}); err != nil {
		t.Fatal(err)
	}

	capSnap := scheduler.CapacitySnapshot{Capabilities: map[string]scheduler.CapabilityCapacity{
		"shell": {Accessible: true, Enforcement: domain.EnforcementPartial},
	}}

	// Missing maxWallTime must error.
	w1, err := scheduler.NewWorker(schedSvc, execSvc, evidenceStore, verifySvc,
		map[string]executors.Executor{"shell": &fakeExecutor{}}, clk, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w1.WithLeaseHeartbeat(10*time.Millisecond, 0)
	if _, err := w1.StepOnce(ctx, capSnap); err == nil {
		t.Fatal("StepOnce succeeded with heartbeatInterval > 0 and maxWallTime == 0")
	}

	// Re-make eligible by testing on a fresh task/worker where interval >= lease.
	task2, err := execSvc.CreateTask(ctx, execution.TaskRequest{
		Purpose:              domain.PurposeRef{Kind: domain.PurposeOwnerDirective, ID: "owner-hb-cfg2"},
		Objective:            "validate interval < lease",
		AcceptanceCriteria:   []string{"done"},
		RequiredCapabilities: []string{"shell"},
		RequiredEnforcement:  domain.EnforcementPartial,
		ResourceEnvelopeID:   envelopeID,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = task2
	w2, err := scheduler.NewWorker(schedSvc, execSvc, evidenceStore, verifySvc,
		map[string]executors.Executor{"shell": &fakeExecutor{}}, clk, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w2.WithLeaseHeartbeat(15*time.Minute, time.Hour)
	if _, err := w2.StepOnce(ctx, capSnap); err == nil {
		t.Fatal("StepOnce succeeded with heartbeatInterval >= leaseDuration")
	}

	// maxWallTime shorter than leaseDuration cancels execution cleanly at maxWallTime.
	task3, err := execSvc.CreateTask(ctx, execution.TaskRequest{
		Purpose:              domain.PurposeRef{Kind: domain.PurposeOwnerDirective, ID: "owner-hb-cap"},
		Objective:            "cap below lease",
		AcceptanceCriteria:   []string{"done"},
		RequiredCapabilities: []string{"shell"},
		RequiredEnforcement:  domain.EnforcementPartial,
		ResourceEnvelopeID:   envelopeID,
	})
	if err != nil {
		t.Fatal(err)
	}
	var timedOut atomic.Bool
	w3, err := scheduler.NewWorker(schedSvc, execSvc, evidenceStore, verifySvc,
		map[string]executors.Executor{"shell": callbackExecutor{fn: func(runCtx context.Context, _ executors.AttemptEnvelope) (executors.ExecutionResult, error) {
			select {
			case <-runCtx.Done():
				timedOut.Store(errors.Is(runCtx.Err(), context.DeadlineExceeded))
				return executors.ExecutionResult{ExitCode: 1}, runCtx.Err()
			case <-time.After(2 * time.Second):
				return executors.ExecutionResult{ExitCode: 0}, nil
			}
		}}}, clk, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w3.WithLeaseHeartbeat(10*time.Millisecond, 40*time.Millisecond)
	// Cancel task1 and task2 (returned/left ELIGIBLE because w1/w2 errored on
	// invalid heartbeat config before execution completed) so task3 runs.
	for _, id := range []domain.ID{task2.ID} {
		_, _ = execSvc.CancelEligibleTask(ctx, id)
	}
	res3, err := w3.StepOnce(ctx, capSnap)
	if err != nil {
		t.Fatal(err)
	}
	// If task1 ran first (priority order), cancel/verify timedOut was reached.
	if res3.TaskID != task3.ID && !timedOut.Load() {
		t.Fatalf("unexpected task %s ran without timing out", res3.TaskID)
	}
	if res3.Outcome != scheduler.StepFailed || !timedOut.Load() {
		t.Fatalf("outcome=%s timedOut=%v, want FAILED and timedOut=true", res3.Outcome, timedOut.Load())
	}
}
