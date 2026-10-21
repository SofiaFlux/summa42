package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	state "github.com/SofiaFlux/summa42/internal/state/sqlite"

	"github.com/SofiaFlux/summa42/internal/testutil"
)

func TestOpenAppliesSafetyPragmas(t *testing.T) {
	store := testutil.OpenStore(t)
	assertPragma(t, store.DB(), "journal_mode", "wal")
	assertPragma(t, store.DB(), "synchronous", "2")
	assertPragma(t, store.DB(), "foreign_keys", "1")
	assertPragma(t, store.DB(), "busy_timeout", "5000")
}

func TestOpenAppliesMigrations(t *testing.T) {
	store := testutil.OpenStore(t)
	var count int
	if err := store.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("schema_migrations table count = %d, want 1", count)
	}
}

func assertPragma(t *testing.T, db *sql.DB, name, want string) {
	t.Helper()
	var got string
	if err := db.QueryRow(fmt.Sprintf("PRAGMA %s", name)).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("PRAGMA %s = %q, want %q", name, got, want)
	}
}

func TestFieldFeedbackMigrationCreatesDurableSchema(t *testing.T) {
	store := testutil.OpenStore(t)
	ctx := context.Background()

	for _, table := range []string{
		"field_observations", "field_observation_evidence",
		"feedback_candidates", "feedback_candidate_observations",
		"sanitization_results", "sanitized_feedback", "feedback_emissions",
		"approval_requests", "approval_decisions", "adaptation_grant_requests", "adaptation_grants",
		"experience_proposals", "experience_rules", "experience_rule_evidence",
		"experience_outcomes",
	} {
		var name string
		if err := store.DB().QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name); err != nil {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}

	for _, trigger := range []string{
		"sanitization_results_no_update", "sanitization_results_no_delete",
		"sanitized_feedback_no_update", "sanitized_feedback_no_delete",
		"adaptation_grants_no_update", "adaptation_grants_no_delete",
		"approval_requests_no_insert_replace", "approval_requests_binding_immutable", "approval_requests_no_delete",
		"approval_decisions_no_insert_replace", "approval_decisions_no_update", "approval_decisions_no_delete",
		"experience_proposals_no_insert_replace", "experience_proposals_binding_immutable",
		"experience_rules_no_insert_replace", "experience_rules_no_update", "experience_rules_no_delete",
		"experience_outcomes_no_insert_replace", "experience_outcomes_no_update", "experience_outcomes_no_delete",
	} {
		var name string
		if err := store.DB().QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='trigger' AND name=?`, trigger,
		).Scan(&name); err != nil {
			t.Fatalf("trigger %s missing: %v", trigger, err)
		}
	}

	var taskClassColumn, approvalColumn int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('tasks') WHERE name='task_class'`,
	).Scan(&taskClassColumn); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('external_operations') WHERE name='approval_id'`,
	).Scan(&approvalColumn); err != nil {
		t.Fatal(err)
	}
	if taskClassColumn != 1 || approvalColumn != 1 {
		t.Fatalf("migration columns task_class=%d approval_id=%d, want 1/1", taskClassColumn, approvalColumn)
	}
}

func TestAttemptRunManifestMigrationCreatesImmutableSchema(t *testing.T) {
	store := testutil.OpenStore(t)
	ctx := context.Background()
	var table string
	if err := store.DB().QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type='table' AND name='attempt_run_manifests'",
	).Scan(&table); err != nil {
		t.Fatal(err)
	}
	for _, trigger := range []string{"attempt_run_manifests_no_insert_replace", "attempt_run_manifests_no_update", "attempt_run_manifests_no_delete"} {
		var name string
		if err := store.DB().QueryRowContext(ctx,
			"SELECT name FROM sqlite_master WHERE type='trigger' AND name=?", trigger,
		).Scan(&name); err != nil {
			t.Fatalf("trigger %s missing: %v", trigger, err)
		}
	}
}

func TestAttemptRunManifestRejectsInsertOrReplace(t *testing.T) {
	store := testutil.OpenStore(t)
	ctx := context.Background()
	db := store.DB()

	for _, statement := range []string{
		`INSERT INTO resource_envelopes(envelope_id,hard_limit,created_at) VALUES ('manifest-envelope',1,'2026-09-21T00:00:00Z')`,
		`INSERT INTO tasks(task_id,purpose_kind,purpose_id,task_class,state,current_fence,acceptance_criteria_json,required_capabilities_json,required_enforcement,authority_ceiling_json,resource_envelope_id,created_at,updated_at) VALUES ('manifest-task','OWNER_DIRECTIVE','owner','test','EXECUTING',1,'[]','[]','ENFORCED','[]','manifest-envelope','2026-09-21T00:00:00Z','2026-09-21T00:00:00Z')`,
		`INSERT INTO attempts(attempt_id,task_id,state,fence_generation,lease_state,lease_expires_at,started_at,executor_kind) VALUES ('manifest-attempt','manifest-task','RUNNING',1,'ACTIVE','2026-09-22T00:00:00Z','2026-09-21T00:00:00Z','test')`,
		`INSERT INTO attempt_run_manifests(attempt_id,task_id,manifest_hash,manifest_json,created_at) VALUES ('manifest-attempt','manifest-task','original','{}','2026-09-21T00:00:00Z')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := db.ExecContext(ctx, `INSERT OR REPLACE INTO attempt_run_manifests(attempt_id,task_id,manifest_hash,manifest_json,created_at) VALUES ('manifest-attempt','manifest-task','replaced','{"changed":true}','2026-09-21T00:01:00Z')`); err == nil {
		t.Fatal("INSERT OR REPLACE replaced immutable attempt run manifest")
	}
}

func TestGovernanceBindingTriggersAllowLifecycleButRejectHistoryRewrite(t *testing.T) {
	store := testutil.OpenStore(t)
	ctx := context.Background()
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO approval_requests(
			approval_id,subject_kind,subject_id,request_digest,policy_decision_id,
			required_approvers_json,requested_by,state,expires_at,created_at
		) VALUES ('approval-hardening','TEST','subject-1','digest-1','policy-1','["owner"]','requester','PENDING','2026-09-22T00:00:00Z','2026-09-21T00:00:00Z')`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx,
		"UPDATE approval_requests SET state='APPROVED', decided_at='2026-09-21T01:00:00Z', approver_id='owner', decision_action='APPROVE' WHERE approval_id='approval-hardening'",
	); err != nil {
		t.Fatalf("valid lifecycle transition blocked: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		"UPDATE approval_requests SET request_digest='rewritten' WHERE approval_id='approval-hardening'",
	); err == nil {
		t.Fatal("approval request digest rewrite succeeded")
	}
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO approval_decisions(approval_id,approver_id,request_digest,decision_action,decided_at)
		VALUES ('approval-hardening','owner','digest-1','APPROVE','2026-09-21T01:00:00Z')`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx,
		"UPDATE approval_decisions SET decision_action='REJECT' WHERE approval_id='approval-hardening' AND approver_id='owner'",
	); err == nil {
		t.Fatal("approval decision rewrite succeeded")
	}
	for _, statement := range []string{
		`INSERT INTO adaptation_grant_requests(grant_request_id,grant_digest,definition_json,approval_id,requested_by,created_at) VALUES ('grant-request-hardening','grant-digest-hardening','{}','approval-hardening','owner','2026-09-21T00:00:00Z')`,
		`INSERT INTO adaptation_grants(grant_id,grant_request_id,adaptation_kind,scope_key,allowed_executors_json,min_verified_samples,max_acceptance_regression_bps,max_cost_regression_bps,owner_principal_id,expires_at,activated_at) VALUES ('grant-hardening','grant-request-hardening','EXECUTOR_PREFERENCE','repo.review','["codex"]',1,0,0,'owner','2026-09-22T00:00:00Z','2026-09-21T00:00:00Z')`,
	} {
		if _, err := store.DB().ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`INSERT OR REPLACE INTO adaptation_grants(grant_id,grant_request_id,adaptation_kind,scope_key,allowed_executors_json,min_verified_samples,max_acceptance_regression_bps,max_cost_regression_bps,owner_principal_id,expires_at,activated_at) VALUES ('grant-replaced','grant-request-hardening','EXECUTOR_PREFERENCE','repo.review','["other"]',1,0,0,'attacker','2026-09-23T00:00:00Z','2026-09-21T00:01:00Z')`,
	); err == nil {
		t.Fatal("INSERT OR REPLACE rewrote immutable adaptation grant by grant request")
	}
}

func TestGovernanceStateSurvivesCloseAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "restart.db")
	store, err := state.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	db := store.DB()
	now := "2026-09-21T18:00:00Z"

	statements := []string{
		`INSERT INTO policy_sets(policy_set_id,version,module_name,module,policy_hash,capabilities_hash,active,created_at)
		  VALUES ('policy-1',1,'test.rego','package test','ph','ch',1,'2026-09-21T18:00:00Z')`,
		`INSERT INTO resource_envelopes(envelope_id,hard_limit,created_at)
		  VALUES ('env-1',100,'2026-09-21T18:00:00Z')`,
		`INSERT INTO tasks(task_id,purpose_kind,purpose_id,task_class,state,current_attempt_id,current_fence,
		  acceptance_criteria_json,required_capabilities_json,required_enforcement,authority_ceiling_json,
		  resource_envelope_id,priority,created_at,updated_at)
		  VALUES ('task-1','OWNER_DIRECTIVE','owner-1','repo.review','EXECUTING','attempt-1',1,
		  '["ok"]','[]','ENFORCED','[]','env-1',0,'2026-09-21T18:00:00Z','2026-09-21T18:00:00Z')`,
		`INSERT INTO attempts(attempt_id,task_id,state,fence_generation,lease_state,lease_expires_at,started_at,executor_kind)
		  VALUES ('attempt-1','task-1','RUNNING',1,'ACTIVE','2026-09-21T19:00:00Z','2026-09-21T18:00:00Z','codex')`,
		`INSERT INTO attempt_run_manifests(attempt_id,task_id,manifest_hash,manifest_json,created_at)
		  VALUES ('attempt-1','task-1','manifest-hash','{"schema_version":1}','2026-09-21T18:00:00Z')`,
		`INSERT INTO approval_requests(approval_id,subject_kind,subject_id,request_digest,policy_decision_id,
		  required_approvers_json,requested_by,state,expires_at,created_at)
		  VALUES ('approval-1','EXTERNAL_OPERATION','operation-1','digest-1','decision-1','["owner"]','cube','PENDING',
		  '2026-09-22T18:00:00Z','2026-09-21T18:00:00Z')`,
		`INSERT INTO approval_requests(approval_id,subject_kind,subject_id,request_digest,policy_decision_id,
		  required_approvers_json,requested_by,state,expires_at,decided_at,approver_id,decision_action,created_at)
		  VALUES ('approval-grant','ADAPTATION_GRANT','grant-request-1','digest-g','decision-g','["owner"]','owner','CONSUMED',
		  '2026-09-22T18:00:00Z','2026-09-21T18:01:00Z','owner','APPROVE','2026-09-21T18:00:00Z')`,
		`INSERT INTO adaptation_grant_requests(grant_request_id,grant_digest,definition_json,approval_id,requested_by,created_at)
		  VALUES ('grant-request-1','digest-g','{}','approval-grant','owner','2026-09-21T18:00:00Z')`,
		`INSERT INTO adaptation_grants(grant_id,grant_request_id,adaptation_kind,scope_key,allowed_executors_json,
		  min_verified_samples,max_acceptance_regression_bps,max_cost_regression_bps,owner_principal_id,expires_at,activated_at)
		  VALUES ('grant-1','grant-request-1','EXECUTOR_PREFERENCE','repo.review','["codex"]',3,0,0,'owner',
		  '2026-09-28T18:00:00Z','2026-09-21T18:01:00Z')`,
		`INSERT INTO experience_proposals(proposal_id,grant_id,generic_task_class,scope_key,preferred_executor,state,created_at,updated_at)
		  VALUES ('proposal-1','grant-1','REVIEW','repo.review','codex','ACTIVE','2026-09-21T18:02:00Z','2026-09-21T18:02:00Z')`,
		`INSERT INTO experience_rules(rule_id,proposal_id,grant_id,version,adaptation_kind,scope_key,generic_task_class,
		  preferred_executor,state,verified_samples,activated_at,created_at,updated_at)
		  VALUES ('rule-1','proposal-1','grant-1',1,'EXECUTOR_PREFERENCE','repo.review','REVIEW','codex','ACTIVE',3,
		  '2026-09-21T18:03:00Z','2026-09-21T18:03:00Z','2026-09-21T18:03:00Z')`,
		`INSERT INTO experience_outcomes(outcome_id,task_id,generic_task_class,scope_key,executor_kind,accepted,
		  human_intervention,retry_count,recorded_at)
		  VALUES ('outcome-1','task-1','REVIEW','repo.review','codex',1,0,0,'2026-09-21T18:03:00Z')`,
		`INSERT INTO resource_reservations(reservation_id,envelope_id,state,reserved_amount,cost_control,cost_source,
		  require_hard_cap,created_at,updated_at)
		  VALUES ('reservation-1','env-1','UNRESOLVED',1,'TECHNICALLY_CAPPED','test',0,
		  '2026-09-21T18:00:00Z','2026-09-21T18:00:00Z')`,
		`INSERT INTO effect_slots(effect_slot_id,collective_id,task_id,trusted_slot_key,provider,descriptor_type,
		  intent_fingerprint,intent_revision,canonical_intent_json,adapter_version,adapter_version_semantic,created_at,updated_at)
		  VALUES ('slot-1','collective-1','task-1','feedback:1','github','feedback.issue.emit.v1',
		  'fp',1,'{}','v1',0,'2026-09-21T18:00:00Z','2026-09-21T18:00:00Z')`,
		`INSERT INTO external_operations(operation_id,task_id,attempt_id,effect_slot_id,operation_sequence,state,
		  intent_fingerprint,intent_revision,provider,adapter_version,reservation_id,risk,prepare_policy_decision_id,
		  prepare_policy_set_id,prepare_policy_set_hash,prepare_policy_capabilities_hash,dispatcher_claim,
		  reconciliation_required,created_at,dispatched_at,approval_id)
		  VALUES ('operation-1','task-1','attempt-1','slot-1',1,'OUTCOME_UNKNOWN','fp',1,'github','v1',
		  'reservation-1','LOW','decision-1','policy-1','ph','ch','claim-1',1,
		  '2026-09-21T18:00:00Z','2026-09-21T18:01:00Z','approval-1')`,
	}
	_ = now
	for i, stmt := range statements {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed statement %d: %v", i, err)
		}
	}
	for name, statement := range map[string]string{
		"approval request subject/digest":  `INSERT OR REPLACE INTO approval_requests(approval_id,subject_kind,subject_id,request_digest,policy_decision_id,required_approvers_json,requested_by,state,expires_at,created_at) VALUES ('approval-replaced','EXTERNAL_OPERATION','operation-1','digest-1','decision-2','[]','attacker','PENDING','2026-09-22T18:00:00Z','2026-09-21T18:01:00Z')`,
		"experience rule proposal/version": `INSERT OR REPLACE INTO experience_rules(rule_id,proposal_id,grant_id,version,adaptation_kind,scope_key,generic_task_class,preferred_executor,state,verified_samples,created_at,updated_at) VALUES ('rule-replaced','proposal-1','grant-1',1,'EXECUTOR_PREFERENCE','repo.review','REVIEW','other','ACTIVE',3,'2026-09-21T18:03:00Z','2026-09-21T18:04:00Z')`,
		"experience outcome dedupe":        `INSERT OR REPLACE INTO experience_outcomes(outcome_id,task_id,generic_task_class,scope_key,executor_kind,accepted,human_intervention,retry_count,recorded_at) VALUES ('outcome-replaced','task-1','REVIEW','repo.review','codex',1,1,1,'2026-09-21T18:04:00Z')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("INSERT OR REPLACE rewrote immutable %s", name)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := state.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.DB().Close()
	var manifestHash, approvalState, ruleState, operationState string
	var reconciliation int
	if err := reopened.DB().QueryRowContext(ctx, "SELECT manifest_hash FROM attempt_run_manifests WHERE attempt_id='attempt-1'").Scan(&manifestHash); err != nil {
		t.Fatal(err)
	}
	if err := reopened.DB().QueryRowContext(ctx, "SELECT state FROM approval_requests WHERE approval_id='approval-1'").Scan(&approvalState); err != nil {
		t.Fatal(err)
	}
	if err := reopened.DB().QueryRowContext(ctx, "SELECT state FROM experience_rules WHERE rule_id='rule-1'").Scan(&ruleState); err != nil {
		t.Fatal(err)
	}
	if err := reopened.DB().QueryRowContext(ctx, "SELECT state,reconciliation_required FROM external_operations WHERE operation_id='operation-1'").Scan(&operationState, &reconciliation); err != nil {
		t.Fatal(err)
	}
	if manifestHash != "manifest-hash" || approvalState != "PENDING" || ruleState != "ACTIVE" || operationState != "OUTCOME_UNKNOWN" || reconciliation != 1 {
		t.Fatalf("reopened state manifest=%q approval=%q rule=%q operation=%q reconcile=%d",
			manifestHash, approvalState, ruleState, operationState, reconciliation)
	}
}

func TestConcurrentProcessesWriteWithoutBusyCrash(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	var stores [2]*state.Store
	for i := range stores {
		s, err := state.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.DB().Close() })
		stores[i] = s
	}
	if _, err := stores[0].DB().ExecContext(ctx, `CREATE TABLE IF NOT EXISTS k19_counter (id INTEGER PRIMARY KEY, n INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := stores[0].DB().ExecContext(ctx, `INSERT INTO k19_counter (id, n) VALUES (1, 0)`); err != nil {
		t.Fatal(err)
	}
	const writes = 200
	errs := make(chan error, len(stores))
	for _, s := range stores {
		go func() {
			for i := 0; i < writes; i++ {
				// Guarded read-then-write: a deferred BEGIN fails here with a stale snapshot.
				err := s.WithTx(ctx, func(tx *sql.Tx) error {
					var n int
					if err := tx.QueryRowContext(ctx, `SELECT n FROM k19_counter WHERE id = 1`).Scan(&n); err != nil {
						return err
					}
					_, err := tx.ExecContext(ctx, `UPDATE k19_counter SET n = ? WHERE id = 1 AND n = ?`, n+1, n)
					return err
				})
				if err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		}()
	}
	for range stores {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent write: %v", err)
		}
	}
	var n int
	if err := stores[0].DB().QueryRowContext(ctx, `SELECT n FROM k19_counter WHERE id = 1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2*writes {
		t.Fatalf("counter = %d, want %d (lost update)", n, 2*writes)
	}
}

func TestIsBusyClassifiesBusyAndLockedCodes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	holder, err := state.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.DB().Close() })
	other, err := state.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.DB().Close() })
	if _, err := other.DB().ExecContext(ctx, `PRAGMA busy_timeout = 0`); err != nil {
		t.Fatal(err)
	}
	_ = holder.WithTx(ctx, func(*sql.Tx) error {
		err := other.WithTx(ctx, func(*sql.Tx) error { return nil })
		if err == nil || !state.IsBusy(err) {
			t.Fatalf("contended BEGIN IMMEDIATE err = %v, want busy", err)
		}
		return nil
	})
	if state.IsBusy(nil) || state.IsBusy(errors.New("database is locked")) {
		t.Fatal("IsBusy must not match nil or plain strings")
	}
}
