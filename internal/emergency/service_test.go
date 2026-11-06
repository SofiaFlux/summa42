package emergency

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
	dbPath := filepath.Join(t.TempDir(), "test_emergency.db")
	store, err := state.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("setup test store: %v", err)
	}
	return store, dbPath
}

func seedEmergencyFixtures(t *testing.T, store *state.Store, ownerID domain.ID) {
	t.Helper()
	nowStr := domain.FormatTime(time.Now().UTC())
	db := store.DB()
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
}

func TestBreakGlassFreezesExecutionAndRevokesLeases(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStore(t)
	defer store.DB().Close()

	clk := &fixedClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	ownerID := domain.ID("owner_root")
	seedEmergencyFixtures(t, store, ownerID)

	svc := NewService(store, clk, ownerID)
	if err := svc.EnsureDefaults(ctx, 3600); err != nil {
		t.Fatal(err)
	}

	// Create dummy tasks, attempts, and capability sessions to verify atomic revocation
	nowStr := domain.FormatTime(clk.Now())
	_, err := store.DB().Exec(`
		INSERT INTO tasks(task_id, purpose_kind, purpose_id, state, current_fence, acceptance_criteria_json,
		  required_capabilities_json, required_enforcement, authority_ceiling_json, resource_envelope_id, priority, created_at, updated_at)
		VALUES ('task_1', 'OWNER_DIRECTIVE', 'p1', 'EXECUTING', 1, '[]', '[]', 'ENFORCED', '[]', 'env1', 0, ?, ?)`, nowStr, nowStr)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.DB().Exec(`
		INSERT INTO attempts(attempt_id, task_id, state, fence_generation, lease_state, lease_expires_at, started_at, executor_kind)
		VALUES ('attempt_1', 'task_1', 'RUNNING', 1, 'ACTIVE', ?, ?, 'test')`, nowStr, nowStr)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.DB().Exec(`
		INSERT INTO capability_sessions(session_id, token_hash, collective_id, task_id, attempt_id, fence_generation, lease_expires_at, visible_capabilities_json, created_at)
		VALUES ('sess_1', 'thash1', 'collective-1', 'task_1', 'attempt_1', 1, ?, '{}', ?)`, nowStr, nowStr)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Trigger Break Glass
	err = svc.TriggerBreakGlass(ctx, ownerID, "Human emergency override: breach detected")
	if err != nil {
		t.Fatalf("trigger break glass: %v", err)
	}

	// 2. Verify state is FROZEN
	bg, _, err := svc.GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bg.State != BreakGlassFrozen {
		t.Fatalf("expected Break Glass state FROZEN, got %s", bg.State)
	}
	if bg.FreezeReason != "Human emergency override: breach detected" {
		t.Fatalf("unexpected freeze reason: %s", bg.FreezeReason)
	}

	// 3. Verify attempt lease was atomically revoked
	var leaseState string
	err = store.DB().QueryRow(`SELECT lease_state FROM attempts WHERE attempt_id = 'attempt_1'`).Scan(&leaseState)
	if err != nil {
		t.Fatal(err)
	}
	if leaseState != "REVOKED" {
		t.Fatalf("expected attempt lease state REVOKED, got %s", leaseState)
	}

	// 4. Verify capability session was atomically revoked
	var revokedAt string
	err = store.DB().QueryRow(`SELECT revoked_at FROM capability_sessions WHERE session_id = 'sess_1'`).Scan(&revokedAt)
	if err != nil {
		t.Fatal(err)
	}
	if revokedAt == "" {
		t.Fatal("expected capability session revoked_at to be set, got empty")
	}

	// 5. Verify CheckExecutionAllowed returns ErrBreakGlassFrozen
	err = svc.CheckExecutionAllowed(ctx, "any_operation")
	if !errors.Is(err, ErrBreakGlassFrozen) {
		t.Fatalf("expected ErrBreakGlassFrozen, got: %v", err)
	}

	// 6. Resume system
	clk.Advance(15 * time.Minute)
	err = svc.ResumeSystem(ctx, ownerID, "Golden Constitution re-verified; resuming operations")
	if err != nil {
		t.Fatalf("resume system: %v", err)
	}

	bg, _, err = svc.GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bg.State != BreakGlassRestored {
		t.Fatalf("expected state RESTORED, got %s", bg.State)
	}

	// 7. Verify audit events
	events, err := svc.ListEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 {
		t.Fatalf("expected at least 2 emergency events, got %d", len(events))
	}
}

func TestBreakGlassOnlyOwnerCanTriggerAndResume(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStore(t)
	defer store.DB().Close()

	clk := &fixedClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	ownerID := domain.ID("owner_authorized")
	rogueActorID := domain.ID("cube_rogue")
	seedEmergencyFixtures(t, store, ownerID)

	svc := NewService(store, clk, ownerID)
	if err := svc.EnsureDefaults(ctx, 3600); err != nil {
		t.Fatal(err)
	}

	// Unauthorized caller tries to trigger Break Glass
	err := svc.TriggerBreakGlass(ctx, rogueActorID, "Unauthorized shutdown")
	if !errors.Is(err, ErrUnauthorizedEmergencyCaller) {
		t.Fatalf("expected ErrUnauthorizedEmergencyCaller, got: %v", err)
	}

	// Authorized owner triggers Break Glass
	err = svc.TriggerBreakGlass(ctx, ownerID, "Owner shutdown")
	if err != nil {
		t.Fatal(err)
	}

	// Unauthorized caller tries to resume system
	err = svc.ResumeSystem(ctx, rogueActorID, "Unauthorized resume")
	if !errors.Is(err, ErrUnauthorizedEmergencyCaller) {
		t.Fatalf("expected ErrUnauthorizedEmergencyCaller on resume, got: %v", err)
	}
}

func TestDeadManSwitchTriggersSafeModeOnTimeout(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStore(t)
	defer store.DB().Close()

	clk := &fixedClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	ownerID := domain.ID("owner_dm")
	seedEmergencyFixtures(t, store, ownerID)

	svc := NewService(store, clk, ownerID)
	// Set heartbeat timeout to 2 hours
	if err := svc.EnsureDefaults(ctx, 7200); err != nil {
		t.Fatal(err)
	}

	// Initial status should be NOMINAL
	_, dm, err := svc.GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dm.State != DeadManNominal {
		t.Fatalf("expected initial state NOMINAL, got %s", dm.State)
	}

	// Allowed operations pass in NOMINAL
	if err := svc.CheckExecutionAllowed(ctx, "strategic_expansion"); err != nil {
		t.Fatalf("expected strategic_expansion allowed in NOMINAL, got %v", err)
	}

	// Advance time past the 2-hour heartbeat threshold (2h 10m)
	clk.Advance(2*time.Hour + 10*time.Minute)

	// Now check execution allowed: Dead Man switch evaluates and triggers SAFE_MODE
	err = svc.CheckExecutionAllowed(ctx, "strategic_expansion")
	if !errors.Is(err, ErrSafeModeRestricted) {
		t.Fatalf("expected ErrSafeModeRestricted for strategic_expansion in SAFE_MODE, got: %v", err)
	}

	err = svc.CheckExecutionAllowed(ctx, "spend")
	if !errors.Is(err, ErrSafeModeRestricted) {
		t.Fatalf("expected ErrSafeModeRestricted for spend in SAFE_MODE, got: %v", err)
	}

	// Pre-authorized critical obligations and diagnostics remain allowed!
	if err := svc.CheckExecutionAllowed(ctx, "critical_obligation"); err != nil {
		t.Fatalf("expected critical_obligation allowed in SAFE_MODE, got: %v", err)
	}
	if err := svc.CheckExecutionAllowed(ctx, "diagnostics:health"); err != nil {
		t.Fatalf("expected diagnostics allowed in SAFE_MODE, got: %v", err)
	}

	// Verify status confirms SAFE_MODE
	_, dm, err = svc.GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dm.State != DeadManSafeMode {
		t.Fatalf("expected state SAFE_MODE, got %s", dm.State)
	}
}

func TestOwnerHeartbeatRestoresNominalFromSafeMode(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStore(t)
	defer store.DB().Close()

	clk := &fixedClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	ownerID := domain.ID("owner_hb")
	seedEmergencyFixtures(t, store, ownerID)

	svc := NewService(store, clk, ownerID)
	if err := svc.EnsureDefaults(ctx, 3600); err != nil {
		t.Fatal(err)
	}

	// Advance past timeout to enter SAFE_MODE
	clk.Advance(2 * time.Hour)
	_, err := svc.EvaluateDeadMan(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Owner sends heartbeat signal
	clk.Advance(5 * time.Minute)
	err = svc.RecordOwnerHeartbeat(ctx, ownerID)
	if err != nil {
		t.Fatalf("record owner heartbeat: %v", err)
	}

	// Verify state restored to NOMINAL
	_, dm, err := svc.GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dm.State != DeadManNominal {
		t.Fatalf("expected state restored to NOMINAL, got %s", dm.State)
	}

	// Strategic expansion now allowed again
	if err := svc.CheckExecutionAllowed(ctx, "strategic_expansion"); err != nil {
		t.Fatalf("expected strategic_expansion allowed after heartbeat, got: %v", err)
	}
}

func TestEmergencyInvariantsDurableAcrossRestart(t *testing.T) {
	ctx := context.Background()
	store, dbPath := setupTestStore(t)

	clk := &fixedClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	ownerID := domain.ID("owner_durable")
	seedEmergencyFixtures(t, store, ownerID)

	svc := NewService(store, clk, ownerID)
	if err := svc.EnsureDefaults(ctx, 3600); err != nil {
		t.Fatal(err)
	}

	if err := svc.TriggerBreakGlass(ctx, ownerID, "Crash containment"); err != nil {
		t.Fatal(err)
	}

	// Close store (crash / restart)
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
	bg, _, err := restartedSvc.GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bg.State != BreakGlassFrozen {
		t.Fatalf("expected Break Glass state FROZEN to persist across restart, got %s", bg.State)
	}
	if bg.FreezeReason != "Crash containment" {
		t.Fatalf("expected freeze reason preserved, got %s", bg.FreezeReason)
	}
}

func TestCheckExecutionFailsClosedOnCorruptedOrMissingTables(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStore(t)
	defer store.DB().Close()

	clk := &fixedClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	ownerID := domain.ID("owner_failclosed")
	seedEmergencyFixtures(t, store, ownerID)

	svc := NewService(store, clk, ownerID)

	// Intentionally corrupt emergency_controls by dropping it to simulate DB failure
	_, err := store.DB().Exec(`DROP TABLE emergency_controls`)
	if err != nil {
		t.Fatal(err)
	}

	// Must FAIL CLOSED (return an error rather than silently allowing execution)
	err = svc.CheckExecutionAllowed(ctx, "any_action")
	if err == nil {
		t.Fatal("expected fail-closed error when emergency_controls table is missing/corrupted, got nil")
	}
}

func TestBreakGlassIdempotency(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStore(t)
	defer store.DB().Close()

	clk := &fixedClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	ownerID := domain.ID("owner_idemp")
	seedEmergencyFixtures(t, store, ownerID)

	svc := NewService(store, clk, ownerID)
	if err := svc.EnsureDefaults(ctx, 3600); err != nil {
		t.Fatal(err)
	}

	// First trigger
	err := svc.TriggerBreakGlass(ctx, ownerID, "First freeze")
	if err != nil {
		t.Fatal(err)
	}

	// Second trigger should succeed idempotently without crashing or appending duplicate freeze rows
	err = svc.TriggerBreakGlass(ctx, ownerID, "Second redundant freeze")
	if err != nil {
		t.Fatalf("expected idempotent second freeze to succeed, got %v", err)
	}

	events, err := svc.ListEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Only 1 BREAK_GLASS_TRIGGERED event should exist
	breakGlassCount := 0
	for _, ev := range events {
		if ev.EventType == EventBreakGlassTriggered {
			breakGlassCount++
		}
	}
	if breakGlassCount != 1 {
		t.Fatalf("expected exactly 1 break glass event due to idempotency, got %d", breakGlassCount)
	}
}
