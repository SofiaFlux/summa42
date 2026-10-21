package verification

import (
	"errors"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
)

// insertOperation writes an external operation for the fixture attempt directly
// in the given state, with the rows its foreign keys need.
func (f *fixture) insertOperation(t *testing.T, state domain.OperationState) {
	t.Helper()
	db := f.store.DB()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(f.ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	now := domain.FormatTime(f.clock.Now())
	exec(`INSERT OR IGNORE INTO policy_sets(policy_set_id, version, module_name, module, policy_hash, capabilities_hash, active, created_at)
	      VALUES ('policy_k13', 1, 'm', x'00', 'ph', 'ch', 1, ?)`, now)
	exec(`INSERT OR IGNORE INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES ('envelope_k13', 100, ?)`, now)
	exec(`INSERT INTO resource_reservations(reservation_id, envelope_id, state, reserved_amount, cost_control, cost_source, require_hard_cap, created_at, updated_at)
	      VALUES ('res_k13', 'envelope_k13', 'HELD', 1, 'TECHNICALLY_CAPPED', 'test', 1, ?, ?)`, now, now)
	exec(`INSERT INTO effect_slots(effect_slot_id, collective_id, task_id, trusted_slot_key, provider, descriptor_type, intent_fingerprint, intent_revision, canonical_intent_json, adapter_version, adapter_version_semantic, created_at, updated_at)
	      VALUES ('slot_k13', 'c', ?, 'k', 'p', 'd', 'fp', 1, '{}', 'v1', 0, ?, ?)`, f.task.ID, now, now)
	exec(`INSERT INTO external_operations(operation_id, task_id, attempt_id, effect_slot_id, operation_sequence, state, intent_fingerprint, intent_revision, provider, adapter_version, reservation_id, risk,
	        prepare_policy_decision_id, prepare_policy_set_id, prepare_policy_set_hash, prepare_policy_capabilities_hash, created_at)
	      SELECT 'op_k13', ?, ?, 'slot_k13', 1, ?, 'fp', 1, 'p', 'v1', 'res_k13', 'LOW', 'decision', policy_set_id, policy_hash, capabilities_hash, ?
	      FROM policy_sets WHERE active = 1`, f.task.ID, f.attempt.ID, state, now)
}

func TestCompleteRefusesUnsettledOperations(t *testing.T) {
	for _, state := range []domain.OperationState{domain.OperationDispatched, domain.OperationOutcomeUnknown} {
		t.Run(string(state), func(t *testing.T) {
			f := newFixture(t)
			ev := f.put(t, "result")
			f.insertOperation(t, state)
			_, err := f.verify.CompleteAttempt(f.ctx, f.attempt.ID, CompletionManifest{EvidenceIDs: []domain.ID{ev.ID}})
			if !errors.Is(err, ErrUnsettledOperations) {
				t.Fatalf("err = %v, want ErrUnsettledOperations", err)
			}
		})
	}
	t.Run("settled operation does not block", func(t *testing.T) {
		f := newFixture(t)
		ev := f.put(t, "result")
		f.insertOperation(t, domain.OperationConfirmedEffect)
		if _, err := f.verify.CompleteAttempt(f.ctx, f.attempt.ID, CompletionManifest{EvidenceIDs: []domain.ID{ev.ID}}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestVerifierMustDifferFromExecutor(t *testing.T) {
	f := newFixture(t) // the fixture attempt's executor kind is "test"
	ev := f.put(t, "result")
	if _, err := f.verify.CompleteAttempt(f.ctx, f.attempt.ID, CompletionManifest{EvidenceIDs: []domain.ID{ev.ID}}); err != nil {
		t.Fatal(err)
	}
	_, err := f.verify.AcceptTask(f.ctx, f.task.ID, AcceptanceRequest{
		VerifierID: "test", VerifierType: "INDEPENDENT_TEST", CriteriaMet: true, EvidenceIDs: []domain.ID{ev.ID},
	})
	if !errors.Is(err, ErrVerifierIsExecutor) {
		t.Fatalf("err = %v, want ErrVerifierIsExecutor", err)
	}
	if _, err := f.verify.AcceptTask(f.ctx, f.task.ID, AcceptanceRequest{
		VerifierID: "verifier_independent", VerifierType: "INDEPENDENT_TEST", CriteriaMet: true, EvidenceIDs: []domain.ID{ev.ID},
	}); err != nil {
		t.Fatalf("a distinct verifier must still be accepted: %v", err)
	}
}
