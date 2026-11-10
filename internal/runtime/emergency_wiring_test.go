package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/emergency"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

const wiringOwner = domain.ID("owner-wiring")

// openWiringBox opens a Box on root with one ELIGIBLE Task that may use the
// lostack provider, and returns that Task's ID.
func openWiringBox(t *testing.T, root string, clk *testutil.Clock, lease time.Duration, seed bool) (*Box, domain.ID) {
	t.Helper()
	ctx := context.Background()
	box, err := Open(ctx, Config{
		StatePath: filepath.Join(root, "state.db"), EvidencePath: filepath.Join(root, "evidence"),
		CollectiveID: "collective-1", OwnerPrincipalID: wiringOwner, PolicyEngine: compositionPolicy{},
		CubeID: "cube-wiring", CubePrincipalID: "princ-cube-wiring", Clock: clk, LeaseDuration: lease,
		OperationProviders: []operations.Provider{lostAckProvider{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = box.Close() })
	if !seed {
		return box, ""
	}
	now := clk.Now().UTC().Format(time.RFC3339Nano)
	envelopeID, taskID := domain.NewID("envelope"), domain.NewID("task")
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO principals(principal_id, principal_kind, public_key, custody_profile, created_at)
		  VALUES (?, 'HUMAN', X'00', 'LOCAL', ?) ON CONFLICT DO NOTHING`, []any{wiringOwner, now}},
		{`INSERT INTO policy_sets(policy_set_id, version, module_name, module, policy_hash, capabilities_hash, active, created_at)
		  VALUES ('policy-test', 1, 'test.rego', 'package test', 'hash', 'caps', 1, ?)`, []any{now}},
		{`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, 1000, ?)`, []any{envelopeID, now}},
		{`INSERT INTO tasks(task_id, purpose_kind, purpose_id, state, current_fence, acceptance_criteria_json,
		  required_capabilities_json, required_enforcement, authority_ceiling_json, resource_envelope_id, priority, created_at, updated_at)
		  VALUES (?, 'OWNER_DIRECTIVE', 'owner-test', 'ELIGIBLE', 0, '[]', '[]', 'ENFORCED', '["external:lostack"]', ?, 0, ?, ?)`,
			[]any{taskID, envelopeID, now, now}},
	} {
		if _, err := box.Store.DB().ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	return box, taskID
}

func prepareLostAck(ctx context.Context, box *Box, attemptID domain.ID, slot string) error {
	_, err := box.Operations.Prepare(ctx, operations.PrepareRequest{
		AttemptID: attemptID, Provider: "lostack", TrustedSlotKey: slot, Intent: testutil.PurchaseIntent{SKU: "s", Quantity: 1}, Risk: "LOW",
	})
	return err
}

// The Owner's horizon is seven days whatever lease a process runs with, and a
// later process with another lease does not move it.
func TestBoxOwnerHorizonDoesNotFollowLeaseDuration(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	clk := testutil.NewClock(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	box, taskID := openWiringBox(t, root, clk, 5*time.Minute, true)

	// Well past ten leases: the old derived timeout would have tripped here.
	clk.Advance(3 * time.Hour)
	attempt, err := box.Execution.StartAttempt(ctx, taskID, "test-executor", time.Hour)
	if err != nil {
		t.Fatalf("lease three hours after start: %v", err)
	}
	if err := prepareLostAck(ctx, box, attempt.ID, "slot-1"); err != nil {
		t.Fatalf("external operation three hours after start: %v", err)
	}
	if err := box.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, _ := openWiringBox(t, root, clk, 35*time.Minute, false)
	_, deadMan, err := reopened.Emergency.GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := int(emergency.DefaultHeartbeatTimeout.Seconds()); deadMan.HeartbeatTimeoutSec != want {
		t.Fatalf("horizon after reopening with another lease = %ds, want %ds", deadMan.HeartbeatTimeoutSec, want)
	}

	// Past the horizon the Collective goes dormant, and the Owner's heartbeat wakes it.
	clk.Advance(8 * 24 * time.Hour)
	if err := prepareLostAck(ctx, reopened, attempt.ID, "slot-2"); !errors.Is(err, emergency.ErrSafeModeRestricted) {
		t.Fatalf("external operation past the horizon = %v, want ErrSafeModeRestricted", err)
	}
	if err := reopened.Emergency.RecordOwnerHeartbeat(ctx, wiringOwner); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	fresh, err := reopened.Execution.StartAttempt(ctx, taskID, "test-executor", time.Hour)
	if err != nil {
		t.Fatalf("lease after the Owner's heartbeat: %v", err)
	}
	if err := prepareLostAck(ctx, reopened, fresh.ID, "slot-2"); err != nil {
		t.Fatalf("external operation after the Owner's heartbeat: %v", err)
	}
}

// Break glass stops new leases, not only external operations, and every lease
// it revokes is on the execution trail.
func TestBoxBreakGlassStopsLeasingUntilResume(t *testing.T) {
	ctx := context.Background()
	clk := testutil.NewClock(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	box, taskID := openWiringBox(t, t.TempDir(), clk, 5*time.Minute, true)

	running, err := box.Execution.StartAttempt(ctx, taskID, "test-executor", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := box.Emergency.TriggerBreakGlass(ctx, wiringOwner, "stop everything"); err != nil {
		t.Fatal(err)
	}
	if _, err := box.Execution.StartAttempt(ctx, taskID, "test-executor", time.Hour); !errors.Is(err, emergency.ErrBreakGlassFrozen) {
		t.Fatalf("StartAttempt while FROZEN = %v, want ErrBreakGlassFrozen", err)
	}
	var events string
	if err := box.Store.DB().QueryRowContext(ctx,
		`SELECT group_concat(event_type) FROM (SELECT event_type FROM execution_events WHERE attempt_id = ? ORDER BY rowid)`, running.ID,
	).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != "ATTEMPT_LEASED,LEASE_REVOKED" {
		t.Fatalf("execution trail of the frozen Attempt = %s, want ATTEMPT_LEASED,LEASE_REVOKED", events)
	}

	if err := box.Emergency.ResumeSystem(ctx, wiringOwner, "verified"); err != nil {
		t.Fatal(err)
	}
	if _, err := box.Execution.StartAttempt(ctx, taskID, "test-executor", time.Hour); err != nil {
		t.Fatalf("StartAttempt after resume: %v", err)
	}
}
