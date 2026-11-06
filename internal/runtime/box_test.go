package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/SofiaFlux/summa42/internal/quarantine"
	"github.com/SofiaFlux/summa42/internal/resources"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

// lostAckProvider performs the effect but never acknowledges it, so only a
// lookup can establish the outcome.
type lostAckProvider struct{}

func (lostAckProvider) Name() string                              { return "lostack" }
func (lostAckProvider) Capability() string                        { return "external:lostack" }
func (lostAckProvider) EnforcementLevel() domain.EnforcementLevel { return domain.EnforcementEnforced }
func (lostAckProvider) AdapterVersion() string                    { return "v1" }
func (lostAckProvider) AdapterVersionSemanticallyRelevant() bool  { return false }
func (lostAckProvider) SettleWindow() time.Duration               { return 0 }
func (lostAckProvider) MinimumRisk() string                       { return "LOW" }
func (lostAckProvider) CanonicalIntent(d operations.IntentDescriptor) ([]byte, error) {
	return json.Marshal(d)
}
func (lostAckProvider) CostProfile(operations.IntentDescriptor) (operations.CostProfile, error) {
	return operations.CostProfile{MaxExposure: 10, Enforceability: resources.Enforceability{
		CostControl: resources.CostTechnicallyCapped, RequireHardCap: true, Source: "test",
	}}, nil
}
func (lostAckProvider) Dispatch(context.Context, operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	return operations.ProviderOutcome{}, errors.New("connection reset")
}
func (lostAckProvider) LookupOutcome(_ context.Context, r operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	return operations.ProviderOutcome{State: domain.OperationConfirmedEffect, ProviderReference: "ref-" + string(r.OperationID), ActualCost: 10}, nil
}

func TestBoxStartupReconcilesDispatchedOperation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	box, err := Open(ctx, Config{
		StatePath: filepath.Join(root, "state.db"), EvidencePath: filepath.Join(root, "evidence"),
		CollectiveID: "collective-1", OwnerPrincipalID: "owner-1", PolicyEngine: compositionPolicy{},
		OperationProviders: []operations.Provider{lostAckProvider{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	now := box.Clock.Now().UTC().Format(time.RFC3339Nano)
	db := box.Store.DB()
	envelopeID, taskID := domain.NewID("envelope"), domain.NewID("task")
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO policy_sets(policy_set_id, version, module_name, module, policy_hash, capabilities_hash, active, created_at)
		  VALUES ('policy-test', 1, 'test.rego', 'package test', 'hash', 'caps', 1, ?)`, []any{now}},
		{`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, 1000, ?)`, []any{envelopeID, now}},
		{`INSERT INTO tasks(task_id, purpose_kind, purpose_id, state, current_fence, acceptance_criteria_json,
		  required_capabilities_json, required_enforcement, authority_ceiling_json, resource_envelope_id, priority, created_at, updated_at)
		  VALUES (?, 'OWNER_DIRECTIVE', 'owner-test', 'ELIGIBLE', 0, '[]', '[]', 'ENFORCED', '["external:lostack"]', ?, 0, ?, ?)`,
			[]any{taskID, envelopeID, now, now}},
	} {
		if _, err := db.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	attempt, err := box.Execution.StartAttempt(ctx, taskID, "test-executor", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	op, err := box.Operations.Prepare(ctx, operations.PrepareRequest{
		AttemptID: attempt.ID, Provider: "lostack", TrustedSlotKey: "slot", Intent: testutil.PurchaseIntent{SKU: "s", Quantity: 1}, Risk: "LOW",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.Operations.Dispatch(ctx, op.ID, attempt.ID); !errors.Is(err, domain.ErrOutcomeUnknown) {
		t.Fatalf("Dispatch = %v, want ErrOutcomeUnknown", err)
	}
	// Simulate a crash between the commit boundary and settlement.
	if _, err := db.ExecContext(ctx, `UPDATE external_operations SET state = 'DISPATCHED', reconciliation_required = 0, dispatched_at = ? WHERE operation_id = ?`, time.Now().Add(-24*time.Hour).UTC().Format(time.RFC3339Nano), op.ID); err != nil {
		t.Fatal(err)
	}

	report, err := box.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.ReconciledOperations != 1 {
		t.Fatalf("report = %+v, want 1 reconciled operation", report)
	}
	var stored domain.OperationState
	if err := db.QueryRowContext(ctx, `SELECT state FROM external_operations WHERE operation_id = ?`, op.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != domain.OperationConfirmedEffect {
		t.Fatalf("state = %s, want CONFIRMED_EFFECT", stored)
	}
}

func TestBoxQuarantineEnforcementRejectsOperations(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	ownerID := domain.ID("owner-quarantine-test")
	cubeID := domain.ID("cube-quarantine-test")
	cubePrincipalID := domain.ID("princ-cube-test")

	box, err := Open(ctx, Config{
		StatePath: filepath.Join(root, "state.db"), EvidencePath: filepath.Join(root, "evidence"),
		CollectiveID: "collective-1", OwnerPrincipalID: ownerID, PolicyEngine: compositionPolicy{},
		CubeID: cubeID, CubePrincipalID: cubePrincipalID,
		OperationProviders: []operations.Provider{lostAckProvider{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()

	now := box.Clock.Now().UTC().Format(time.RFC3339Nano)
	db := box.Store.DB()
	envelopeID, taskID := domain.NewID("envelope"), domain.NewID("task")
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO principals(principal_id, principal_kind, public_key, custody_profile, created_at)
		  VALUES (?, 'HUMAN', X'00', 'LOCAL', ?) ON CONFLICT DO NOTHING`, []any{ownerID, now}},
		{`INSERT INTO principals(principal_id, principal_kind, public_key, custody_profile, created_at)
		  VALUES (?, 'CUBE', X'00', 'LOCAL', ?) ON CONFLICT DO NOTHING`, []any{cubePrincipalID, now}},
		{`INSERT INTO constitutions(version, content, content_hash, signature, signer_principal_id, active, created_at)
		  VALUES (1, X'00', 'hash-const', X'00', ?, 1, ?)`, []any{ownerID, now}},
		{`INSERT INTO collective_metadata(singleton, collective_id, owner_principal_id, cube_principal_id, constitutional_root_principal_id, active_constitution_version, created_at)
		  VALUES (1, 'collective-1', ?, ?, ?, 1, ?)`, []any{ownerID, cubePrincipalID, ownerID, now}},
		{`INSERT INTO policy_sets(policy_set_id, version, module_name, module, policy_hash, capabilities_hash, active, created_at)
		  VALUES ('policy-test', 1, 'test.rego', 'package test', 'hash', 'caps', 1, ?)`, []any{now}},
		{`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, 1000, ?)`, []any{envelopeID, now}},
		{`INSERT INTO tasks(task_id, purpose_kind, purpose_id, state, current_fence, acceptance_criteria_json,
		  required_capabilities_json, required_enforcement, authority_ceiling_json, resource_envelope_id, priority, created_at, updated_at)
		  VALUES (?, 'OWNER_DIRECTIVE', 'owner-test', 'ELIGIBLE', 0, '[]', '[]', 'ENFORCED', '["external:lostack"]', ?, 0, ?, ?)`,
			[]any{taskID, envelopeID, now, now}},
	} {
		if _, err := db.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := box.Quarantine.RegisterCube(ctx, box.CubeID, cubePrincipalID); err != nil {
		t.Fatal(err)
	}

	attempt, err := box.Execution.StartAttempt(ctx, taskID, "test-executor", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// 1. In ACTIVE state, Prepare succeeds
	op, err := box.Operations.Prepare(ctx, operations.PrepareRequest{
		AttemptID: attempt.ID, Provider: "lostack", TrustedSlotKey: "slot-1", Intent: testutil.PurchaseIntent{SKU: "s", Quantity: 1}, Risk: "LOW",
	})
	if err != nil {
		t.Fatalf("prepare in active state: %v", err)
	}
	if op.State != domain.OperationPrepared {
		t.Fatalf("op state = %s, want PREPARED", op.State)
	}

	// 2. Quarantine the Cube
	err = box.Quarantine.QuarantineCube(ctx, box.CubeID, "Systemic containment", ownerID, quarantine.ActiveRecon)
	if err != nil {
		t.Fatalf("quarantine cube: %v", err)
	}

	// 3. In QUARANTINED state, Prepare and Dispatch are deterministically rejected
	_, err = box.Operations.Prepare(ctx, operations.PrepareRequest{
		AttemptID: attempt.ID, Provider: "lostack", TrustedSlotKey: "slot-2", Intent: testutil.PurchaseIntent{SKU: "s2", Quantity: 2}, Risk: "LOW",
	})
	if !errors.Is(err, quarantine.ErrProhibitedInQuarantine) {
		t.Fatalf("expected ErrProhibitedInQuarantine on Prepare under quarantine, got: %v", err)
	}

	_, err = box.Operations.Dispatch(ctx, op.ID, attempt.ID)
	if !errors.Is(err, quarantine.ErrProhibitedInQuarantine) {
		t.Fatalf("expected ErrProhibitedInQuarantine on Dispatch under quarantine, got: %v", err)
	}

	// 4. Cube attempts self-unquarantine - must be rejected
	err = box.Quarantine.UnquarantineCube(ctx, box.CubeID, cubePrincipalID, "Escape attempt")
	if !errors.Is(err, quarantine.ErrSelfUnquarantineDenied) {
		t.Fatalf("expected ErrSelfUnquarantineDenied, got: %v", err)
	}

	// 5. Owner lifts quarantine
	err = box.Quarantine.UnquarantineCube(ctx, box.CubeID, ownerID, "Verification passed")
	if err != nil {
		t.Fatalf("unquarantine by owner: %v", err)
	}

	// 6. After unquarantine, operations resume
	op2, err := box.Operations.Prepare(ctx, operations.PrepareRequest{
		AttemptID: attempt.ID, Provider: "lostack", TrustedSlotKey: "slot-2", Intent: testutil.PurchaseIntent{SKU: "s2", Quantity: 2}, Risk: "LOW",
	})
	if err != nil {
		t.Fatalf("prepare after unquarantine: %v", err)
	}
	if op2.State != domain.OperationPrepared {
		t.Fatalf("op2 state = %s, want PREPARED", op2.State)
	}
}
