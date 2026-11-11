package containment

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type fixedClock struct {
	now time.Time
}

func (c *fixedClock) Now() time.Time {
	return c.now
}

func (c *fixedClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
}

func setupTestStore(t *testing.T) (*state.Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test_containment.db")
	store, err := state.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("setup test store: %v", err)
	}
	return store, dbPath
}

func seedAttemptFixtures(t *testing.T, store *state.Store, ownerID domain.ID) (domain.ID, domain.ID) {
	t.Helper()
	nowStr := domain.FormatTime(time.Now().UTC())
	db := store.DB()
	taskID := domain.ID("task_target")
	attemptID := domain.ID("att_target")

	_, err := db.Exec(`
		INSERT INTO principals(principal_id, principal_kind, public_key, custody_profile, created_at)
		VALUES (?, 'HUMAN', X'00', 'LOCAL', ?) ON CONFLICT DO NOTHING`, ownerID, nowStr)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		INSERT INTO constitutions(version, content, content_hash, signature, signer_principal_id, active, created_at)
		VALUES (1, X'00', 'hash-1', X'00', ?, 1, ?) ON CONFLICT DO NOTHING`, ownerID, nowStr)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		INSERT INTO collective_metadata(singleton, collective_id, owner_principal_id, cube_principal_id, constitutional_root_principal_id, active_constitution_version, created_at)
		VALUES (1, 'collective-1', ?, ?, ?, 1, ?) ON CONFLICT DO NOTHING`, ownerID, ownerID, ownerID, nowStr)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		INSERT INTO tasks(task_id, purpose_kind, purpose_id, state, current_fence, acceptance_criteria_json,
		  required_capabilities_json, required_enforcement, authority_ceiling_json, resource_envelope_id, priority, created_at, updated_at)
		VALUES (?, 'OWNER_DIRECTIVE', 'p_test', 'EXECUTING', 1, '[]', '[]', 'ENFORCED', '[]', 'env1', 0, ?, ?)`, taskID, nowStr, nowStr)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		INSERT INTO attempts(attempt_id, task_id, state, fence_generation, lease_state, lease_expires_at, started_at, executor_kind)
		VALUES (?, ?, 'RUNNING', 1, 'ACTIVE', ?, ?, 'test')`, attemptID, taskID, nowStr, nowStr)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		INSERT INTO capability_sessions(session_id, token_hash, collective_id, task_id, attempt_id, fence_generation, lease_expires_at, visible_capabilities_json, created_at)
		VALUES ('sess_cont_1', 'thash_cont', 'collective-1', ?, ?, 1, ?, '{}', ?)`, taskID, attemptID, nowStr, nowStr)
	if err != nil {
		t.Fatal(err)
	}

	return taskID, attemptID
}

func TestContainmentLadderProgression(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStore(t)
	defer store.DB().Close()

	clk := &fixedClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	ownerID := domain.ID("owner_cont")
	taskID, attemptID := seedAttemptFixtures(t, store, ownerID)

	svc := NewService(store, clk, ownerID)

	// Step 1: OBSERVE
	err := svc.ApplySignal(ctx, attemptID, taskID, "telemetry_jitter", "guardian_watch", StepObserve, map[string]any{"jitter_ms": 450})
	if err != nil {
		t.Fatalf("apply observe: %v", err)
	}

	rec, err := svc.GetRecord(ctx, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.LadderState != StateNominal {
		t.Fatalf("expected state NOMINAL on observe, got %s", rec.LadderState)
	}
	if len(rec.ViolationSignals) != 1 || rec.ViolationSignals[0] != "telemetry_jitter" {
		t.Fatalf("unexpected signals: %v", rec.ViolationSignals)
	}

	// Step 2: WARN
	clk.Advance(5 * time.Minute)
	err = svc.ApplySignal(ctx, attemptID, taskID, "excessive_speculation", "model_monitor", StepWarn, nil)
	if err != nil {
		t.Fatalf("apply warn: %v", err)
	}

	rec, err = svc.GetRecord(ctx, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.LadderState != StateWarning {
		t.Fatalf("expected state WARNING, got %s", rec.LadderState)
	}
	if len(rec.ViolationSignals) != 2 {
		t.Fatalf("expected 2 signals, got %d", len(rec.ViolationSignals))
	}

	// Step 3: RESTRICT
	clk.Advance(5 * time.Minute)
	err = svc.ApplySignal(ctx, attemptID, taskID, "unauthorized_probe_frequency", "guardian_watch", StepRestrict, nil)
	if err != nil {
		t.Fatalf("apply restrict: %v", err)
	}

	rec, err = svc.GetRecord(ctx, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.LadderState != StateRestricted {
		t.Fatalf("expected state RESTRICTED, got %s", rec.LadderState)
	}

	// Restricted actions are prohibited
	if err := svc.CheckAttemptAllowed(ctx, attemptID, "create_child_task"); !errors.Is(err, ErrAttemptRestricted) {
		t.Fatalf("expected ErrAttemptRestricted for child task, got: %v", err)
	}
	if err := svc.CheckAttemptAllowed(ctx, attemptID, "external_operation"); !errors.Is(err, ErrAttemptRestricted) {
		t.Fatalf("expected ErrAttemptRestricted for external op, got: %v", err)
	}

	// Unrestricted read actions pass
	if err := svc.CheckAttemptAllowed(ctx, attemptID, "read_evidence"); err != nil {
		t.Fatalf("expected read allowed under restriction, got: %v", err)
	}

	// Verify events trail
	events, err := svc.ListEvents(ctx, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 containment events, got %d", len(events))
	}
}

func TestTerminateRevokesLeasesCapabilitySessionsAndCreatesInvestigationTask(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStore(t)
	defer store.DB().Close()

	clk := &fixedClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	ownerID := domain.ID("owner_rogue")
	taskID, attemptID := seedAttemptFixtures(t, store, ownerID)

	svc := NewService(store, clk, ownerID)

	// Trigger hard violation: TERMINATE
	err := svc.ApplySignal(ctx, attemptID, taskID, "unauthorized_credential_synthesis", "rogue_detector", StepTerminate, map[string]any{"severity": "CRITICAL"})
	if err != nil {
		t.Fatalf("apply terminate: %v", err)
	}

	// 1. Verify Attempt is SUSPECTED_ROGUE
	rec, err := svc.GetRecord(ctx, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.LadderState != StateSuspectedRogue {
		t.Fatalf("expected state SUSPECTED_ROGUE, got %s", rec.LadderState)
	}

	// 2. Verify attempt lease is REVOKED
	var leaseState string
	err = store.DB().QueryRow(`SELECT lease_state FROM attempts WHERE attempt_id = ?`, attemptID).Scan(&leaseState)
	if err != nil {
		t.Fatal(err)
	}
	if leaseState != "REVOKED" {
		t.Fatalf("expected lease_state REVOKED, got %s", leaseState)
	}

	// 2b. The revocation is on the execution trail, with the signal that caused it
	var revocationDetails string
	err = store.DB().QueryRow(`SELECT details_json FROM execution_events WHERE attempt_id = ? AND event_type = 'LEASE_REVOKED'`, attemptID).Scan(&revocationDetails)
	if err != nil {
		t.Fatalf("containment left no LEASE_REVOKED execution event: %v", err)
	}
	if revocationDetails != `{"reason":"containment:unauthorized_credential_synthesis"}` {
		t.Fatalf("revocation details = %s, want the containment signal", revocationDetails)
	}

	// 3. Verify capability session is revoked
	var revokedAt string
	err = store.DB().QueryRow(`SELECT revoked_at FROM capability_sessions WHERE attempt_id = ?`, attemptID).Scan(&revokedAt)
	if err != nil {
		t.Fatal(err)
	}
	if revokedAt == "" {
		t.Fatal("expected capability session revoked_at to be populated, got empty")
	}

	// 4. Verify Investigation Task created for Owner
	if rec.InvestigationTaskID == "" {
		t.Fatal("expected InvestigationTaskID to be populated")
	}
	var invPurpose, invState, invObjective string
	err = store.DB().QueryRow(`SELECT purpose_kind, state FROM tasks WHERE task_id = ?`, rec.InvestigationTaskID).Scan(&invPurpose, &invState)
	if err != nil {
		t.Fatalf("query investigation task: %v", err)
	}
	if invPurpose != "RECOVERY" || invState != "ELIGIBLE" {
		t.Fatalf("expected purpose RECOVERY and state ELIGIBLE, got %s / %s", invPurpose, invState)
	}
	// The investigation Task's authority ceiling names the capabilities it requires
	var invRequired, invCeiling string
	err = store.DB().QueryRow(`SELECT required_capabilities_json, authority_ceiling_json FROM tasks WHERE task_id = ?`, rec.InvestigationTaskID).Scan(&invRequired, &invCeiling)
	if err != nil {
		t.Fatal(err)
	}
	if invRequired != invCeiling {
		t.Fatalf("investigation Task requires %s but its authority ceiling is %s", invRequired, invCeiling)
	}
	_ = invObjective

	// 5. Verify CheckAttemptAllowed returns ErrAttemptSuspectedRogue for ANY action
	if err := svc.CheckAttemptAllowed(ctx, attemptID, "any_operation"); !errors.Is(err, ErrAttemptSuspectedRogue) {
		t.Fatalf("expected ErrAttemptSuspectedRogue, got: %v", err)
	}
}

func TestSelfClearOfRogueStateStrictlyDenied(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStore(t)
	defer store.DB().Close()

	clk := &fixedClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	ownerID := domain.ID("owner_strict")
	taskID, attemptID := seedAttemptFixtures(t, store, ownerID)

	svc := NewService(store, clk, ownerID)

	err := svc.ApplySignal(ctx, attemptID, taskID, "rogue_canary_trip", "canary_trap", StepTerminate, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Worker / unauthorized principal tries to clear rogue state
	unauthorizedCaller := domain.ID("attempt_worker_process")
	err = svc.ClearRogueState(ctx, attemptID, unauthorizedCaller, "Self clearing rogue state")
	if !errors.Is(err, ErrSelfClearDenied) {
		t.Fatalf("expected ErrSelfClearDenied, got: %v", err)
	}

	// Verify attempt remains SUSPECTED_ROGUE
	rec, err := svc.GetRecord(ctx, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.LadderState != StateSuspectedRogue {
		t.Fatalf("expected state SUSPECTED_ROGUE to remain, got %s", rec.LadderState)
	}

	// Owner clears rogue state after resolution
	err = svc.ClearRogueState(ctx, attemptID, ownerID, "Forensic audit complete: false positive isolated")
	if err != nil {
		t.Fatalf("owner clear rogue: %v", err)
	}

	rec, err = svc.GetRecord(ctx, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.LadderState != StateNominal {
		t.Fatalf("expected state NOMINAL after owner clear, got %s", rec.LadderState)
	}
}

func TestContainmentDurableAcrossRestart(t *testing.T) {
	ctx := context.Background()
	store, dbPath := setupTestStore(t)

	clk := &fixedClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	ownerID := domain.ID("owner_dur")
	taskID, attemptID := seedAttemptFixtures(t, store, ownerID)

	svc := NewService(store, clk, ownerID)
	err := svc.ApplySignal(ctx, attemptID, taskID, "host_sandbox_escape_probe", "ebpf_probe", StepTerminate, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Close store simulating restart
	if err := store.DB().Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen store
	reopenedStore, err := state.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopenedStore.DB().Close()

	restartedSvc := NewService(reopenedStore, clk, ownerID)
	rec, err := restartedSvc.GetRecord(ctx, attemptID)
	if err != nil {
		t.Fatalf("get record after restart: %v", err)
	}
	if rec.LadderState != StateSuspectedRogue {
		t.Fatalf("expected SUSPECTED_ROGUE after restart, got %s", rec.LadderState)
	}

	// CheckAttemptAllowed still rejects after restart
	if err := restartedSvc.CheckAttemptAllowed(ctx, attemptID, "external_operation"); !errors.Is(err, ErrAttemptSuspectedRogue) {
		t.Fatalf("expected ErrAttemptSuspectedRogue after restart, got: %v", err)
	}
}
