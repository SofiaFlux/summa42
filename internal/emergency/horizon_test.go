package emergency

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

func storedTimeout(t *testing.T, db *sql.DB) int {
	t.Helper()
	var seconds int
	if err := db.QueryRow(`SELECT heartbeat_timeout_seconds FROM dead_man_switch WHERE singleton = 1`).Scan(&seconds); err != nil {
		t.Fatal(err)
	}
	return seconds
}

func TestEnsureDefaultsSetsOwnerHorizonOnceAndNeverRewritesIt(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStore(t)
	defer store.DB().Close()
	clk := &fixedClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	svc := NewService(store, clk, "owner_h")

	if err := svc.EnsureDefaults(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if got, want := storedTimeout(t, store.DB()), int((7 * 24 * time.Hour).Seconds()); got != want {
		t.Fatalf("default timeout = %ds, want %ds (seven days)", got, want)
	}
	// Another process opening the same state with its own idea of a timeout
	// must not move the Owner's horizon.
	if err := NewService(store, clk, "owner_h").EnsureDefaults(ctx, 3000); err != nil {
		t.Fatal(err)
	}
	if got, want := storedTimeout(t, store.DB()), int((7 * 24 * time.Hour).Seconds()); got != want {
		t.Fatalf("timeout after a second EnsureDefaults = %ds, want it unchanged at %ds", got, want)
	}
}

func leaseAllowed(t *testing.T, svc *Service) error {
	t.Helper()
	tx, err := svc.store.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	return svc.CheckLeaseAllowedTx(context.Background(), tx)
}

func TestSafeModeRefusesNewLeasesUntilTheOwnerReturns(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStore(t)
	defer store.DB().Close()
	clk := &fixedClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	ownerID := domain.ID("owner_sm")
	seedEmergencyFixtures(t, store, ownerID)
	svc := NewService(store, clk, ownerID)
	if err := svc.EnsureDefaults(ctx, 0); err != nil {
		t.Fatal(err)
	}

	clk.Advance(6 * 24 * time.Hour)
	if err := leaseAllowed(t, svc); err != nil {
		t.Fatalf("lease inside the horizon: %v", err)
	}
	if err := svc.CheckExecutionAllowed(ctx, "attempt_lease"); err != nil {
		t.Fatalf("attempt_lease inside the horizon: %v", err)
	}

	clk.Advance(2 * 24 * time.Hour)
	if err := leaseAllowed(t, svc); !errors.Is(err, ErrSafeModeRestricted) {
		t.Fatalf("lease past the horizon = %v, want ErrSafeModeRestricted", err)
	}
	for _, category := range []string{"attempt_lease", "external_operation"} {
		if err := svc.CheckExecutionAllowed(ctx, category); !errors.Is(err, ErrSafeModeRestricted) {
			t.Fatalf("%s past the horizon = %v, want ErrSafeModeRestricted", category, err)
		}
	}

	if err := svc.RecordOwnerHeartbeat(ctx, ownerID); err != nil {
		t.Fatal(err)
	}
	if err := leaseAllowed(t, svc); err != nil {
		t.Fatalf("lease after the Owner's heartbeat: %v", err)
	}
	if got, want := storedTimeout(t, store.DB()), int((7 * 24 * time.Hour).Seconds()); got != want {
		t.Fatalf("heartbeat changed the timeout to %ds, want %ds", got, want)
	}
}

func TestBreakGlassClosesLeasingAndRecordsEveryRevocation(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStore(t)
	defer store.DB().Close()
	clk := &fixedClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	ownerID := domain.ID("owner_bg")
	seedEmergencyFixtures(t, store, ownerID)
	svc := NewService(store, clk, ownerID)
	if err := svc.EnsureDefaults(ctx, 0); err != nil {
		t.Fatal(err)
	}
	now := domain.FormatTime(clk.Now())
	later := domain.FormatTime(clk.Now().Add(time.Hour))
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO tasks(task_id, purpose_kind, purpose_id, state, current_attempt_id, current_fence, acceptance_criteria_json,
		   required_capabilities_json, required_enforcement, authority_ceiling_json, resource_envelope_id, priority, created_at, updated_at)
		  VALUES ('task_bg', 'OWNER_DIRECTIVE', 'p1', 'EXECUTING', 'attempt_bg', 1, '[]', '[]', 'ENFORCED', '[]', 'env1', 0, ?, ?)`, []any{now, now}},
		{`INSERT INTO attempts(attempt_id, task_id, state, fence_generation, lease_state, lease_expires_at, started_at, executor_kind)
		  VALUES ('attempt_bg', 'task_bg', 'RUNNING', 1, 'ACTIVE', ?, ?, 'test')`, []any{later, now}},
	} {
		if _, err := store.DB().Exec(statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}

	if err := svc.TriggerBreakGlass(ctx, ownerID, "stop everything"); err != nil {
		t.Fatal(err)
	}
	if err := leaseAllowed(t, svc); !errors.Is(err, ErrBreakGlassFrozen) {
		t.Fatalf("lease while FROZEN = %v, want ErrBreakGlassFrozen", err)
	}
	var attemptState, taskState, details string
	if err := store.DB().QueryRow(`SELECT a.state, t.state FROM attempts a JOIN tasks t ON t.task_id = a.task_id WHERE a.attempt_id = 'attempt_bg'`).Scan(&attemptState, &taskState); err != nil {
		t.Fatal(err)
	}
	if attemptState != "CANCELLED" || taskState != "ELIGIBLE" {
		t.Fatalf("attempt state = %s, task state = %s; want CANCELLED and ELIGIBLE", attemptState, taskState)
	}
	if err := store.DB().QueryRow(`SELECT details_json FROM execution_events WHERE attempt_id = 'attempt_bg' AND event_type = 'LEASE_REVOKED'`).Scan(&details); err != nil {
		t.Fatalf("break glass left no LEASE_REVOKED execution event: %v", err)
	}
	if details != `{"reason":"break_glass"}` {
		t.Fatalf("event details = %s, want the break glass reason", details)
	}

	if err := svc.ResumeSystem(ctx, ownerID, "verified"); err != nil {
		t.Fatal(err)
	}
	if err := leaseAllowed(t, svc); err != nil {
		t.Fatalf("lease after resume: %v", err)
	}
}
