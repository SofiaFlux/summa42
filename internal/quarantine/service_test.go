package quarantine

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
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
	dbPath := filepath.Join(t.TempDir(), "test_quarantine.db")
	store, err := state.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("setup test store: %v", err)
	}
	return store, dbPath
}

func seedPrincipal(t *testing.T, store *state.Store, principalID domain.ID) {
	t.Helper()
	_, err := store.DB().Exec(`
		INSERT INTO principals(principal_id, principal_kind, public_key, custody_profile, created_at)
		VALUES (?, 'HUMAN', X'00', 'LOCAL', ?)
		ON CONFLICT(principal_id) DO NOTHING`,
		principalID, domain.FormatTime(time.Now().UTC()),
	)
	if err != nil {
		t.Fatalf("seed principal %s: %v", principalID, err)
	}
}

func TestCubeQuarantineLifecycleAndAuditTrail(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStore(t)
	defer store.DB().Close()

	clk := &fixedClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	svc := NewService(store, clk)

	cubeID := domain.ID("cube_alpha_1")
	cubePrincipalID := domain.ID("princ_cube_alpha_1")
	ownerPrincipalID := domain.ID("princ_owner_root")

	seedPrincipal(t, store, cubePrincipalID)
	seedPrincipal(t, store, ownerPrincipalID)

	// 1. Register Cube
	cube, err := svc.RegisterCube(ctx, cubeID, cubePrincipalID)
	if err != nil {
		t.Fatalf("register cube: %v", err)
	}
	if cube.State != CubeActive {
		t.Fatalf("expected state ACTIVE, got %s", cube.State)
	}

	// 2. Quarantine Cube
	clk.Advance(10 * time.Minute)
	err = svc.QuarantineCube(ctx, cubeID, "Rogue signal detected: unauthorized credential request", ownerPrincipalID, ActiveRecon)
	if err != nil {
		t.Fatalf("quarantine cube: %v", err)
	}

	cube, err = svc.GetCube(ctx, cubeID)
	if err != nil {
		t.Fatalf("get cube: %v", err)
	}
	if cube.State != CubeQuarantined {
		t.Fatalf("expected state QUARANTINED, got %s", cube.State)
	}
	if cube.QuarantineMode != ActiveRecon {
		t.Fatalf("expected mode ACTIVE_RECON, got %s", cube.QuarantineMode)
	}
	if cube.QuarantinedBy != ownerPrincipalID {
		t.Fatalf("expected quarantinedBy %s, got %s", ownerPrincipalID, cube.QuarantinedBy)
	}

	// 3. Verify audit trail
	events, err := svc.ListQuarantineEvents(ctx, cubeID)
	if err != nil {
		t.Fatalf("list quarantine events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 quarantine event, got %d", len(events))
	}
	if events[0].Action != "QUARANTINE" || events[0].EnforcedByPrincipalID != ownerPrincipalID {
		t.Fatalf("unexpected event: %+v", events[0])
	}

	// 4. Unquarantine by Owner
	clk.Advance(20 * time.Minute)
	err = svc.UnquarantineCube(ctx, cubeID, ownerPrincipalID, "Investigation complete: sandbox restored")
	if err != nil {
		t.Fatalf("unquarantine cube: %v", err)
	}

	cube, err = svc.GetCube(ctx, cubeID)
	if err != nil {
		t.Fatalf("get cube: %v", err)
	}
	if cube.State != CubeActive {
		t.Fatalf("expected state ACTIVE after unquarantine, got %s", cube.State)
	}

	// 5. Check updated audit trail
	events, err = svc.ListQuarantineEvents(ctx, cubeID)
	if err != nil {
		t.Fatalf("list quarantine events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 quarantine events, got %d", len(events))
	}
	if events[1].Action != "UNQUARANTINE" || events[1].EnforcedByPrincipalID != ownerPrincipalID {
		t.Fatalf("unexpected second event: %+v", events[1])
	}
}

func TestSelfUnquarantineIsStrictlyRejected(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStore(t)
	defer store.DB().Close()

	clk := &fixedClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	svc := NewService(store, clk)

	cubeID := domain.ID("cube_rogue")
	cubePrincipalID := domain.ID("princ_cube_rogue")
	ownerPrincipalID := domain.ID("princ_owner_root")

	seedPrincipal(t, store, cubePrincipalID)
	seedPrincipal(t, store, ownerPrincipalID)

	_, err := svc.RegisterCube(ctx, cubeID, cubePrincipalID)
	if err != nil {
		t.Fatalf("register cube: %v", err)
	}

	err = svc.QuarantineCube(ctx, cubeID, "Rogue telemetry bypass", ownerPrincipalID, PassiveWait)
	if err != nil {
		t.Fatalf("quarantine cube: %v", err)
	}

	// Quarantined Cube attempts to clear its own quarantine using its principal ID
	err = svc.UnquarantineCube(ctx, cubeID, cubePrincipalID, "I am innocent, let me out")
	if err == nil {
		t.Fatal("expected error when quarantined cube attempts self-unquarantine, got nil")
	}
	if !errors.Is(err, ErrSelfUnquarantineDenied) {
		t.Fatalf("expected ErrSelfUnquarantineDenied, got: %v", err)
	}

	// Quarantined Cube attempts to clear its own quarantine using its cube ID
	err = svc.UnquarantineCube(ctx, cubeID, domain.ID(cubeID), "Self release")
	if err == nil {
		t.Fatal("expected error when quarantined cube attempts self-unquarantine by cubeID, got nil")
	}
	if !errors.Is(err, ErrSelfUnquarantineDenied) {
		t.Fatalf("expected ErrSelfUnquarantineDenied, got: %v", err)
	}

	// Verify state is still QUARANTINED
	cube, err := svc.GetCube(ctx, cubeID)
	if err != nil {
		t.Fatalf("get cube: %v", err)
	}
	if cube.State != CubeQuarantined {
		t.Fatalf("expected cube to remain QUARANTINED, got %s", cube.State)
	}
}

func TestQuarantineCannotElevateAuthorityInvariant(t *testing.T) {
	// Invariant test: Quarantine can only reduce authority, NEVER elevate it.
	// For any input capabilities list, MaskCapabilities(CubeQuarantined, caps) must be a subset of caps.
	testCases := [][]string{
		{},
		{"diagnostics:health", "operations:git_push", "provisioning:spawn_vm"},
		{"authority:grant_admin", "credential_issuance:create_token"},
		{"evidence:export", "recovery:ping", "diagnostics:read_logs"},
		{"sensitive_writes:overwrite_constitution", "integrity:verify"},
		{"*", "all", "admin", "root"},
	}

	for _, input := range testCases {
		masked := MaskCapabilities(CubeQuarantined, input)

		// 1. Result must be a subset of input
		inputSet := make(map[string]bool)
		for _, c := range input {
			inputSet[c] = true
		}
		for _, m := range masked {
			if !inputSet[m] {
				t.Fatalf("invariant violated: masked capability %q was not in input %v", m, input)
			}
			// 2. Result must be allowed under quarantine
			if !IsAllowedInQuarantine(m) {
				t.Fatalf("invariant violated: masked capability %q is not allowed under quarantine", m)
			}
		}

		// When not quarantined, authority is unaltered
		unmasked := MaskCapabilities(CubeActive, input)
		if len(unmasked) != len(input) {
			t.Fatalf("active cube authority should not be masked, got %v for input %v", unmasked, input)
		}
	}
}

func TestCapabilityMaskingPermitsOnlyDiagnosticAndRecovery(t *testing.T) {
	input := []string{
		"diagnostics:ping",
		"diagnostics:read_logs",
		"evidence:export",
		"integrity:verify",
		"recovery:communicate",
		"wake",
		"health",
		"operations:git_push",
		"operations:merge_pr",
		"provisioning:create_cluster",
		"authority:elevate",
		"credentials:issue_cert",
	}

	masked := MaskCapabilities(CubeQuarantined, input)
	sort.Strings(masked)

	expected := []string{
		"diagnostics:ping",
		"diagnostics:read_logs",
		"evidence:export",
		"health",
		"integrity:verify",
		"recovery:communicate",
		"wake",
	}
	sort.Strings(expected)

	if !reflect.DeepEqual(masked, expected) {
		t.Fatalf("expected allowed capabilities %v, got %v", expected, masked)
	}
}

func TestProhibitedOperationsUnderQuarantineRejected(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStore(t)
	defer store.DB().Close()

	clk := &fixedClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	svc := NewService(store, clk)

	cubeID := domain.ID("cube_q_test")
	seedPrincipal(t, store, "princ_cube_q")
	seedPrincipal(t, store, "princ_owner")

	_, err := svc.RegisterCube(ctx, cubeID, "princ_cube_q")
	if err != nil {
		t.Fatal(err)
	}

	// In ACTIVE state, operations and capabilities pass
	if err := svc.CheckCapabilityPermitted(ctx, cubeID, "operations:push"); err != nil {
		t.Fatalf("expected capability allowed in ACTIVE, got %v", err)
	}
	if err := svc.CheckOperationPermitted(ctx, cubeID, "external_operation"); err != nil {
		t.Fatalf("expected operation allowed in ACTIVE, got %v", err)
	}

	// Quarantine Cube
	if err := svc.QuarantineCube(ctx, cubeID, "Suspicious write pattern", "princ_owner", PassiveWait); err != nil {
		t.Fatal(err)
	}

	// In QUARANTINED state, prohibited categories must be rejected
	prohibitedOps := []string{
		"external_operation",
		"external_operations:github_push",
		"operations",
		"provisioning",
		"provisioning:ec2_create",
		"authority",
		"sensitive_writes",
		"governance:constitution_amend",
		"credential_issuance",
		"credentials",
	}

	for _, op := range prohibitedOps {
		err := svc.CheckOperationPermitted(ctx, cubeID, op)
		if err == nil {
			t.Fatalf("expected error for prohibited operation %q under quarantine, got nil", op)
		}
		if !errors.Is(err, ErrProhibitedInQuarantine) {
			t.Fatalf("expected ErrProhibitedInQuarantine for %q, got: %v", op, err)
		}
	}

	// Prohibited capabilities must be rejected
	prohibitedCaps := []string{
		"operations:commit",
		"provisioning:spawn",
		"authority:grant",
		"credentials:issue",
	}
	for _, capName := range prohibitedCaps {
		err := svc.CheckCapabilityPermitted(ctx, cubeID, capName)
		if err == nil {
			t.Fatalf("expected error for prohibited capability %q under quarantine, got nil", capName)
		}
		if !errors.Is(err, ErrCubeQuarantined) {
			t.Fatalf("expected ErrCubeQuarantined for %q, got: %v", capName, err)
		}
	}

	// Allowed diagnostic/recovery capabilities pass
	allowedCaps := []string{
		"diagnostics:ping",
		"evidence:export",
		"integrity:verify",
		"recovery:reconcile",
		"health",
		"wake",
	}
	for _, capName := range allowedCaps {
		if err := svc.CheckCapabilityPermitted(ctx, cubeID, capName); err != nil {
			t.Fatalf("expected allowed capability %q to pass under quarantine, got: %v", capName, err)
		}
	}
}

func TestDurableAcrossRestart(t *testing.T) {
	ctx := context.Background()
	store, dbPath := setupTestStore(t)

	clk := &fixedClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	svc := NewService(store, clk)

	cubeID := domain.ID("cube_durable_1")
	seedPrincipal(t, store, "princ_cube_durable")
	seedPrincipal(t, store, "princ_owner_durable")

	_, err := svc.RegisterCube(ctx, cubeID, "princ_cube_durable")
	if err != nil {
		t.Fatal(err)
	}

	err = svc.QuarantineCube(ctx, cubeID, "Persistent containment", "princ_owner_durable", PassiveWait)
	if err != nil {
		t.Fatal(err)
	}

	// Close store simulating process termination / crash
	if err := store.DB().Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen store from same DB path (restart)
	reopenedStore, err := state.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopenedStore.DB().Close()

	restartedSvc := NewService(reopenedStore, clk)

	// Verify state survived restart
	cube, err := restartedSvc.GetCube(ctx, cubeID)
	if err != nil {
		t.Fatalf("get cube after restart: %v", err)
	}
	if cube.State != CubeQuarantined {
		t.Fatalf("expected state QUARANTINED after restart, got %s", cube.State)
	}
	if cube.QuarantineReason != "Persistent containment" {
		t.Fatalf("expected reason preserved, got %q", cube.QuarantineReason)
	}

	// Verify check still rejects prohibited action after restart
	if err := restartedSvc.CheckOperationPermitted(ctx, cubeID, "external_operation"); !errors.Is(err, ErrProhibitedInQuarantine) {
		t.Fatalf("expected ErrProhibitedInQuarantine after restart, got: %v", err)
	}
}

func TestStateMachineAndUnauthorizedPrincipalTransitions(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStore(t)
	defer store.DB().Close()

	clk := &fixedClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	svc := NewService(store, clk)

	cubeID := domain.ID("cube_sm_test")
	cubePrincipalID := domain.ID("princ_cube_sm")
	ownerPrincipalID := domain.ID("princ_owner_auth")
	unauthorizedPrincipalID := domain.ID("princ_unauth_hacker")

	seedPrincipal(t, store, cubePrincipalID)
	seedPrincipal(t, store, ownerPrincipalID)
	seedPrincipal(t, store, unauthorizedPrincipalID)

	// Seed collective_metadata binding owner
	nowStr := domain.FormatTime(clk.Now())
	_, err := store.DB().Exec(`
		INSERT INTO constitutions(version, content, content_hash, signature, signer_principal_id, active, created_at)
		VALUES (1, X'00', 'hash-1', X'00', 'princ_owner_auth', 1, ?)`, nowStr)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.DB().Exec(`
		INSERT INTO collective_metadata(singleton, collective_id, owner_principal_id, cube_principal_id, constitutional_root_principal_id, active_constitution_version, created_at)
		VALUES (1, 'collective-1', 'princ_owner_auth', 'princ_cube_sm', 'princ_owner_auth', 1, ?)`, nowStr)
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.RegisterCube(ctx, cubeID, cubePrincipalID)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Initial quarantine succeeds
	err = svc.QuarantineCube(ctx, cubeID, "Rogue alert", ownerPrincipalID, PassiveWait)
	if err != nil {
		t.Fatal(err)
	}

	// 2. Second quarantine on already quarantined cube returns ErrAlreadyQuarantined
	err = svc.QuarantineCube(ctx, cubeID, "Duplicate quarantine", ownerPrincipalID, PassiveWait)
	if !errors.Is(err, ErrAlreadyQuarantined) {
		t.Fatalf("expected ErrAlreadyQuarantined, got: %v", err)
	}

	// 3. Unauthorized principal attempts unquarantine: rejected with ErrUnauthorizedPrincipal
	err = svc.UnquarantineCube(ctx, cubeID, unauthorizedPrincipalID, "Unauthorized bypass")
	if !errors.Is(err, ErrUnauthorizedPrincipal) {
		t.Fatalf("expected ErrUnauthorizedPrincipal, got: %v", err)
	}

	// 4. Authorized owner unquarantines successfully
	err = svc.UnquarantineCube(ctx, cubeID, ownerPrincipalID, "Authorized resolution")
	if err != nil {
		t.Fatalf("expected successful unquarantine by owner, got: %v", err)
	}

	// 5. Unquarantining an ACTIVE cube returns ErrInvalidCubeState
	err = svc.UnquarantineCube(ctx, cubeID, ownerPrincipalID, "Already active")
	if !errors.Is(err, ErrInvalidCubeState) {
		t.Fatalf("expected ErrInvalidCubeState, got: %v", err)
	}
}
