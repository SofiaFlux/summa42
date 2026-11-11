package execution

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

type closedLeaseGate struct{ err error }

func (g closedLeaseGate) CheckLeaseAllowedTx(context.Context, *sql.Tx) error { return g.err }

func TestStartAttemptRefusedByLeaseGateLeavesNoAttempt(t *testing.T) {
	svc, _, ctx, missionID := newExecutionService(t)
	task, err := svc.CreateTask(ctx, baseTaskRequest(missionID))
	if err != nil {
		t.Fatal(err)
	}
	refused := errors.New("leasing is closed")
	svc.SetLeaseGate(closedLeaseGate{err: refused})

	if _, err := svc.StartAttempt(ctx, task.ID, "test", time.Minute); !errors.Is(err, refused) {
		t.Fatalf("StartAttempt = %v, want the gate's refusal", err)
	}
	var attempts int
	if err := svc.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM attempts WHERE task_id = ?`, task.ID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	stored, err := svc.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || stored.State != domain.TaskEligible {
		t.Fatalf("attempts = %d, task state = %s; a refused lease must change nothing", attempts, stored.State)
	}

	svc.SetLeaseGate(closedLeaseGate{})
	if _, err := svc.StartAttempt(ctx, task.ID, "test", time.Minute); err != nil {
		t.Fatalf("StartAttempt with an open gate: %v", err)
	}
}

func TestRevokeActiveLeasesTxReturnsTasksAndRecordsReason(t *testing.T) {
	svc, clk, ctx, missionID := newExecutionService(t)
	first, firstAttempt := seedActiveAttempt(t, svc, missionID)
	second, secondAttempt := seedActiveAttempt(t, svc, missionID)

	var revoked int
	err := svc.store.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		revoked, err = RevokeActiveLeasesTx(ctx, tx, clk.Now(), "break_glass")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if revoked != 2 {
		t.Fatalf("revoked = %d, want 2", revoked)
	}
	for _, pair := range []struct {
		task    domain.Task
		attempt domain.Attempt
	}{{first, firstAttempt}, {second, secondAttempt}} {
		attempt, err := svc.Attempt(ctx, pair.attempt.ID)
		if err != nil {
			t.Fatal(err)
		}
		task, err := svc.Task(ctx, pair.task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if attempt.LeaseState != domain.LeaseRevoked || attempt.State != domain.AttemptCancelled || task.State != domain.TaskEligible {
			t.Fatalf("attempt lease=%s state=%s, task=%s; want REVOKED/CANCELLED and ELIGIBLE", attempt.LeaseState, attempt.State, task.State)
		}
		var details string
		if err := svc.store.DB().QueryRowContext(ctx,
			`SELECT details_json FROM execution_events WHERE attempt_id = ? AND event_type = 'LEASE_REVOKED'`, pair.attempt.ID,
		).Scan(&details); err != nil {
			t.Fatalf("LEASE_REVOKED event for %s: %v", pair.attempt.ID, err)
		}
		if details != `{"reason":"break_glass"}` {
			t.Fatalf("event details = %s, want the revocation reason", details)
		}
	}
	// A second pass finds nothing left to revoke.
	err = svc.store.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		revoked, err = RevokeActiveLeasesTx(ctx, tx, clk.Now(), "break_glass")
		return err
	})
	if err != nil || revoked != 0 {
		t.Fatalf("second pass revoked = %d, err = %v; want 0, nil", revoked, err)
	}
}

func TestRevokeLeaseTxLeavesOtherAttemptsAlone(t *testing.T) {
	svc, clk, ctx, missionID := newExecutionService(t)
	_, target := seedActiveAttempt(t, svc, missionID)
	_, other := seedActiveAttempt(t, svc, missionID)

	err := svc.store.WithTx(ctx, func(tx *sql.Tx) error {
		return RevokeLeaseTx(ctx, tx, target.ID, clk.Now(), "containment")
	})
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := svc.Attempt(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	untouched, err := svc.Attempt(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if revoked.LeaseState != domain.LeaseRevoked || untouched.LeaseState != domain.LeaseActive {
		t.Fatalf("target lease=%s, other lease=%s; want REVOKED and ACTIVE", revoked.LeaseState, untouched.LeaseState)
	}
}
