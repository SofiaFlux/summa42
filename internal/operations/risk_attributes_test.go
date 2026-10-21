package operations_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/SofiaFlux/summa42/internal/policy"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

// useDefaultPolicy swaps the harness onto the shipped default OPA profile, with
// the policy row and Collective owner that a bootstrapped Collective carries.
func useDefaultPolicy(t *testing.T, h *harness) domain.ID {
	t.Helper()
	profile, err := policy.DefaultProfile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB().ExecContext(h.ctx,
		`UPDATE policy_sets SET policy_set_id = ?, module_name = ?, module = ?, policy_hash = ?, capabilities_hash = ? WHERE active = 1`,
		profile.PolicySetID, profile.ModuleName, []byte(profile.Module), profile.PolicyHash, profile.CapabilitiesHash,
	); err != nil {
		t.Fatal(err)
	}
	owner := domain.ID("principal_owner_test")
	now := h.clock.Now().UTC().Format(time.RFC3339Nano)
	for _, principal := range []struct{ id, kind string }{{string(owner), "OWNER"}, {"principal_cube_test", "CUBE"}, {"principal_root_test", "CONSTITUTIONAL_ROOT"}} {
		if _, err := h.store.DB().ExecContext(h.ctx,
			`INSERT INTO principals(principal_id, principal_kind, public_key, custody_profile, created_at) VALUES (?, ?, x'00', 'test', ?)`,
			principal.id, principal.kind, now,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.store.DB().ExecContext(h.ctx,
		`INSERT INTO constitutions(version, content, content_hash, signature, signer_principal_id, active, created_at) VALUES (1, x'00', 'hash', x'00', 'principal_root_test', 1, ?)`, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB().ExecContext(h.ctx,
		`INSERT INTO collective_metadata(singleton, collective_id, owner_principal_id, cube_principal_id, constitutional_root_principal_id, active_constitution_version, created_at) VALUES (1, 'collective_test', ?, 'principal_cube_test', 'principal_root_test', 1, ?)`,
		owner, now,
	); err != nil {
		t.Fatal(err)
	}
	engine := policy.NewOPAEngine(policy.OPAConfig{
		ModuleName: profile.ModuleName, Module: profile.Module,
		PolicySetID: profile.PolicySetID, PolicySetHash: profile.PolicyHash,
	})
	h.svc = operations.New(h.store, h.clock, h.exec, engine, h.ledger, h.approvals, "collective_test", h.provider)
	return owner
}

func TestEffectiveRiskIsProviderMinimum(t *testing.T) {
	h := newHarness(t)
	h.provider.minimumRisk = "MEDIUM"
	owner := useDefaultPolicy(t, h)

	op := preparePurchase(t, h, h.attempt.ID, "purchase-primary", 1)
	if op.ApprovalID == "" {
		t.Fatal("caller LOW on a MEDIUM provider prepared without an approval request; want REQUIRE_APPROVAL")
	}
	approval, err := h.approvals.Get(h.ctx, op.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(approval.RequiredApprovers, []domain.ID{owner}) {
		t.Fatalf("required approvers = %v, want [%s]", approval.RequiredApprovers, owner)
	}
	var risk string
	if err := h.store.DB().QueryRowContext(h.ctx, `SELECT risk FROM external_operations WHERE operation_id = ?`, op.ID).Scan(&risk); err != nil {
		t.Fatal(err)
	}
	if risk != "MEDIUM" {
		t.Fatalf("persisted risk = %q, want MEDIUM", risk)
	}
}

func TestPrepareRejectsUnknownCallerRisk(t *testing.T) {
	h := newHarness(t)
	_, err := h.svc.Prepare(h.ctx, operations.PrepareRequest{
		AttemptID: h.attempt.ID, Provider: h.provider.Name(), TrustedSlotKey: "purchase-primary",
		Intent: testutil.PurchaseIntent{SKU: "sku-1", Quantity: 1}, Risk: "TRIVIAL",
	})
	if err == nil {
		t.Fatal("prepare accepted an unknown risk class")
	}
}

func TestCallerAttributesCannotOverrideTrustedKeys(t *testing.T) {
	h := newHarness(t)
	_, err := h.svc.Prepare(h.ctx, operations.PrepareRequest{
		AttemptID: h.attempt.ID, Provider: h.provider.Name(), TrustedSlotKey: "purchase-primary",
		Intent: testutil.PurchaseIntent{SKU: "sku-1", Quantity: 1}, Risk: "LOW",
		Attributes: map[string]any{
			"capability": "evil", "provider": "evil", "phase": "DISPATCH", "task_id": "task_evil",
			"owner_principal_id": "intruder", "caller": "nested",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.policy.inputs) != 1 {
		t.Fatalf("policy inputs = %d, want 1", len(h.policy.inputs))
	}
	attrs := h.policy.inputs[0].Attributes
	if attrs["capability"] != h.provider.Capability() || attrs["provider"] != h.provider.Name() ||
		attrs["phase"] != "PREPARE" || attrs["task_id"] != h.taskID {
		t.Fatalf("trusted attributes overwritten: %#v", attrs)
	}
	caller, ok := attrs["caller"].(map[string]any)
	if !ok || caller["capability"] != "evil" || caller["caller"] != "nested" {
		t.Fatalf("caller attributes not namespaced under caller: %#v", attrs["caller"])
	}
	if h.policy.inputs[0].OwnerPrincipalID == "intruder" {
		t.Fatal("caller attribute set the trusted owner principal")
	}
}

func TestDispatchReplaysPrepareAttributes(t *testing.T) {
	h := newHarness(t)
	op, err := h.svc.Prepare(h.ctx, operations.PrepareRequest{
		AttemptID: h.attempt.ID, Provider: h.provider.Name(), TrustedSlotKey: "purchase-primary",
		Intent: testutil.PurchaseIntent{SKU: "sku-1", Quantity: 1}, Risk: "LOW",
		Attributes: map[string]any{"repository": "owner/repo", "issue": 42},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Dispatch(h.ctx, op.ID, h.attempt.ID); err != nil {
		t.Fatal(err)
	}
	if len(h.policy.inputs) != 2 {
		t.Fatalf("policy inputs = %d, want PREPARE and DISPATCH", len(h.policy.inputs))
	}
	prepare, dispatch := h.policy.inputs[0].Attributes, h.policy.inputs[1].Attributes
	if dispatch["phase"] != "DISPATCH" {
		t.Fatalf("second evaluation phase = %v", dispatch["phase"])
	}
	if dispatch["caller"] == nil || !reflect.DeepEqual(prepare["caller"], dispatch["caller"]) {
		t.Fatalf("dispatch caller attributes %#v, want prepare's %#v", dispatch["caller"], prepare["caller"])
	}
	if h.policy.inputs[1].Risk != h.policy.inputs[0].Risk {
		t.Fatalf("dispatch risk %q != prepare risk %q", h.policy.inputs[1].Risk, h.policy.inputs[0].Risk)
	}
}
