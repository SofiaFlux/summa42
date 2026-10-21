package sqlite

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestFieldFeedbackAndRunManifestMigrationsRoundTripWithExistingTask(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "roundtrip.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	db := store.DB()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO resource_envelopes(envelope_id,hard_limit,created_at)
		VALUES ('env-roundtrip',100,'2026-09-21T18:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tasks(task_id,purpose_kind,purpose_id,task_class,state,current_attempt_id,current_fence,
			acceptance_criteria_json,required_capabilities_json,required_enforcement,authority_ceiling_json,
			resource_envelope_id,priority,created_at,updated_at)
		VALUES ('task-roundtrip','OWNER_DIRECTIVE','owner-roundtrip','repo.review','EXECUTING','attempt-roundtrip',1,
			'["ok"]','[]','ENFORCED','[]','env-roundtrip',0,'2026-09-21T18:00:00Z','2026-09-21T18:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO attempts(attempt_id,task_id,state,fence_generation,lease_state,lease_expires_at,started_at,executor_kind)
		VALUES ('attempt-roundtrip','task-roundtrip','RUNNING',1,'ACTIVE','2026-09-21T19:00:00Z','2026-09-21T18:00:00Z','codex')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO attempt_run_manifests(attempt_id,task_id,manifest_hash,manifest_json,created_at)
		VALUES ('attempt-roundtrip','task-roundtrip','hash-roundtrip','{"schema_version":1}','2026-09-21T18:00:00Z')`); err != nil {
		t.Fatal(err)
	}

	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithTableName("schema_migrations"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := provider.DownTo(ctx, 9); err != nil {
		t.Fatalf("down to v9: %v", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('tasks') WHERE name='task_class'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("task_class survived down migration: %d", count)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='attempt_run_manifests'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("attempt_run_manifests survived down migration: %d", count)
	}
	var taskState string
	if err := db.QueryRowContext(ctx, "SELECT state FROM tasks WHERE task_id='task-roundtrip'").Scan(&taskState); err != nil {
		t.Fatal(err)
	}
	if taskState != "EXECUTING" {
		t.Fatalf("base Task lost during down migration: state=%s", taskState)
	}

	if _, err := provider.UpTo(ctx, 12); err != nil {
		t.Fatalf("up to v12: %v", err)
	}
	var taskClass string
	if err := db.QueryRowContext(ctx, "SELECT task_class FROM tasks WHERE task_id='task-roundtrip'").Scan(&taskClass); err != nil {
		t.Fatal(err)
	}
	if taskClass != "" {
		t.Fatalf("re-added task_class=%q, want safe empty default", taskClass)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='attempt_run_manifests'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("attempt_run_manifests table count after re-up=%d", count)
	}
	for _, trigger := range []string{"approval_requests_binding_immutable", "experience_rules_no_update", "attempt_run_manifests_no_update"} {
		var name string
		if err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='trigger' AND name=?`, trigger).Scan(&name); err != nil {
			t.Fatalf("trigger %s missing after re-up: %v", trigger, err)
		}
	}
}

func TestMigration00015RoundTripsWorkflowCaseAndAssessments(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow-verifications-roundtrip.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	db := store.DB()

	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithTableName("schema_migrations"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 14); err != nil {
		t.Fatalf("down to v14: %v", err)
	}

	now := "2026-09-24T10:00:00Z"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO missions(mission_id,statement,active,created_at,deactivated_at)
		VALUES ('mission-workflow-15','Verify workflow migration',1,?,NULL)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_cases(case_id,mission_id,source,object_id,revision_id,observation_evidence_id,
			initial_request_json,grant_json,state,current_work_id,next_work_json,completed_steps,max_steps,
			remaining_budget,progress_signature,created_at,updated_at)
		VALUES ('case-workflow-15','mission-workflow-15','ado','item-15','r15','observation-15',
			'{}','{"Capabilities":["read"],"Actions":null}','ACTIVE','work-15','{}',1,3,2,'ready',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_assessments(assessment_id,case_id,work_id,request_json,result_json,created_at)
		VALUES ('assessment-workflow-15','case-workflow-15','work-15','{}','{}',?)`, now); err != nil {
		t.Fatal(err)
	}

	if _, err := provider.UpTo(ctx, 15); err != nil {
		t.Fatalf("up to v15: %v", err)
	}
	var state string
	if err := db.QueryRowContext(ctx, "SELECT state FROM workflow_cases WHERE case_id='case-workflow-15'").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "ACTIVE" {
		t.Fatalf("state after up=%q,want ACTIVE", state)
	}

	if _, err := provider.DownTo(ctx, 14); err != nil {
		t.Fatalf("down to v14: %v", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT state FROM workflow_cases WHERE case_id='case-workflow-15'").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "ACTIVE" {
		t.Fatalf("state after down=%q,want ACTIVE", state)
	}
	var assessmentCount int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM workflow_assessments WHERE case_id='case-workflow-15'").Scan(&assessmentCount); err != nil {
		t.Fatal(err)
	}
	if assessmentCount != 1 {
		t.Fatalf("assessments after round trip=%d,want 1", assessmentCount)
	}
	var foreignKeyCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_list('workflow_assessments') WHERE "table"='workflow_cases'`).Scan(&foreignKeyCount); err != nil {
		t.Fatal(err)
	}
	if foreignKeyCount != 1 {
		t.Fatalf("workflow_assessments workflow_cases foreign keys=%d,want 1", foreignKeyCount)
	}
	var indexCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name='workflow_cases_state_updated_at'`).Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if indexCount != 1 {
		t.Fatalf("workflow case state index count=%d,want 1", indexCount)
	}
	var verificationTableCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='workflow_verifications'`).Scan(&verificationTableCount); err != nil {
		t.Fatal(err)
	}
	if verificationTableCount != 0 {
		t.Fatalf("workflow_verifications survived down migration")
	}
}

func TestMigration00015DownWithClosedCase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow-verifications-closed-down.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	db := store.DB()

	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithTableName("schema_migrations"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 14); err != nil {
		t.Fatalf("down to v14: %v", err)
	}

	now := "2026-09-24T10:00:00Z"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO missions(mission_id,statement,active,created_at,deactivated_at)
		VALUES ('mission-workflow-15-closed','Verify workflow migration',1,?,NULL)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_cases(case_id,mission_id,source,object_id,revision_id,observation_evidence_id,
			initial_request_json,grant_json,state,current_work_id,next_work_json,completed_steps,max_steps,
			remaining_budget,progress_signature,created_at,updated_at)
		VALUES ('case-workflow-15-closed','mission-workflow-15-closed','ado','item-15','r15','observation-15',
			'{}','{"Capabilities":["read"],"Actions":null}','READY_FOR_VERIFICATION','','{}',1,3,2,'ready',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_assessments(assessment_id,case_id,work_id,request_json,result_json,created_at)
		VALUES ('assessment-workflow-15-closed','case-workflow-15-closed','work-15','{}','{}',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 15); err != nil {
		t.Fatalf("up to v15: %v", err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE workflow_cases SET state='CLOSED' WHERE case_id='case-workflow-15-closed'"); err != nil {
		t.Fatalf("close workflow case: %v", err)
	}

	if _, err := provider.DownTo(ctx, 14); err == nil {
		t.Fatal("down migration accepted a closed workflow case")
	} else if !strings.Contains(err.Error(), "cannot roll back 00015") {
		t.Fatalf("down error = %v, want named rollback error", err)
	}

	var state string
	if err := db.QueryRowContext(ctx, "SELECT state FROM workflow_cases WHERE case_id='case-workflow-15-closed'").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "CLOSED" {
		t.Fatalf("state after failed down=%q, want CLOSED", state)
	}
	var verificationTableCount int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='workflow_verifications'").Scan(&verificationTableCount); err != nil {
		t.Fatal(err)
	}
	if verificationTableCount != 1 {
		t.Fatalf("workflow_verifications table count=%d, want 1", verificationTableCount)
	}
	var oldTableCount int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='workflow_cases_old'").Scan(&oldTableCount); err != nil {
		t.Fatal(err)
	}
	if oldTableCount != 0 {
		t.Fatalf("workflow_cases_old survived failed down migration")
	}
	var assessmentCount int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM workflow_assessments WHERE case_id='case-workflow-15-closed'").Scan(&assessmentCount); err != nil {
		t.Fatal(err)
	}
	if assessmentCount != 1 {
		t.Fatalf("assessment count after failed down=%d, want 1", assessmentCount)
	}
	var foreignKeys int
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys after failed down=%d, want 1", foreignKeys)
	}

	if _, err := provider.DownTo(ctx, 14); err == nil {
		t.Fatal("repeated down migration accepted a closed workflow case")
	} else if !strings.Contains(err.Error(), "cannot roll back 00015") {
		t.Fatalf("repeated down error = %v, want named rollback error", err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE workflow_cases SET state='ACTIVE' WHERE case_id='case-workflow-15-closed'"); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 14); err != nil {
		t.Fatalf("down after closed-case guard cleanup: %v", err)
	}
}

func TestMigration00015DownWithRejectedCase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow-verifications-rejected-down.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	db := store.DB()

	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithTableName("schema_migrations"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 14); err != nil {
		t.Fatalf("down to v14: %v", err)
	}

	now := "2026-09-24T10:00:00Z"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO missions(mission_id,statement,active,created_at,deactivated_at)
		VALUES ('mission-workflow-15-rejected','Verify workflow migration',1,?,NULL)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_cases(case_id,mission_id,source,object_id,revision_id,observation_evidence_id,
			initial_request_json,grant_json,state,current_work_id,next_work_json,completed_steps,max_steps,
			remaining_budget,progress_signature,created_at,updated_at)
		VALUES ('case-workflow-15-rejected','mission-workflow-15-rejected','ado','item-15','r15','observation-15',
			'{}','{"Capabilities":["read"],"Actions":null}','BLOCKED','','{}',1,3,2,'rejected',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_assessments(assessment_id,case_id,work_id,request_json,result_json,created_at)
		VALUES ('assessment-workflow-15-rejected','case-workflow-15-rejected','work-15','{}','{}',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 15); err != nil {
		t.Fatalf("up to v15: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_verifications(verification_id,case_id,verifier_id,verifier_type,snapshot_hash,
			snapshot_json,evidence_ids_json,created_at)
		VALUES ('verification-workflow-15-rejected','case-workflow-15-rejected','', 'REJECT','',
			'{"reason":"publication mismatch"}','[]',?)`, now); err != nil {
		t.Fatal(err)
	}

	if _, err := provider.DownTo(ctx, 14); err != nil {
		t.Fatalf("down rejected case: %v", err)
	}
	var state string
	if err := db.QueryRowContext(ctx, "SELECT state FROM workflow_cases WHERE case_id='case-workflow-15-rejected'").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "BLOCKED" {
		t.Fatalf("state after down=%q, want BLOCKED", state)
	}
	var assessmentCount int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM workflow_assessments WHERE case_id='case-workflow-15-rejected'").Scan(&assessmentCount); err != nil {
		t.Fatal(err)
	}
	if assessmentCount != 1 {
		t.Fatalf("assessment count after down=%d, want 1", assessmentCount)
	}
	var verificationTableCount int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='workflow_verifications'").Scan(&verificationTableCount); err != nil {
		t.Fatal(err)
	}
	if verificationTableCount != 0 {
		t.Fatal("workflow_verifications survived down migration")
	}
}

func TestMigration00015RejectsDuplicateVerificationCase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow-verifications-unique.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	db := store.DB()

	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithTableName("schema_migrations"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 14); err != nil {
		t.Fatalf("down to v14: %v", err)
	}

	now := "2026-09-24T10:00:00Z"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO missions(mission_id,statement,active,created_at,deactivated_at)
		VALUES ('mission-workflow-15-unique','Verify workflow migration',1,?,NULL)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_cases(case_id,mission_id,source,object_id,revision_id,observation_evidence_id,
			initial_request_json,grant_json,state,current_work_id,next_work_json,completed_steps,max_steps,
			remaining_budget,progress_signature,created_at,updated_at)
		VALUES ('case-workflow-15-unique','mission-workflow-15-unique','ado','item-15','r15','observation-15',
			'{}','{"Capabilities":["read"],"Actions":null}','READY_FOR_VERIFICATION','','{}',1,3,2,'ready',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 15); err != nil {
		t.Fatalf("up to v15: %v", err)
	}

	insertVerification := func(id string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_verifications(verification_id,case_id,verifier_id,verifier_type,snapshot_hash,
			snapshot_json,evidence_ids_json,created_at)
		VALUES (?,'case-workflow-15-unique','verifier-15','HUMAN','snapshot-hash','{"state":"verified"}','["evidence-15"]',?)`, id, now); err != nil {
			t.Fatal(err)
		}
	}
	insertVerification("verification-workflow-15")
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_verifications(verification_id,case_id,verifier_id,verifier_type,snapshot_hash,
			snapshot_json,evidence_ids_json,created_at)
		VALUES ('verification-workflow-15-duplicate','case-workflow-15-unique','verifier-15','HUMAN','snapshot-hash',
			'{"state":"verified"}','["evidence-15"]',?)`, now); err == nil {
		t.Fatal("duplicate verification case_id was accepted")
	}

	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM workflow_verifications WHERE case_id='case-workflow-15-unique'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("verification rows=%d,want 1", count)
	}
}

func TestMigration00021RollsBackNonPreferenceRulesAndEnforcesKind(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "adaptation-kind.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, store.DB(), migrations, goose.WithTableName("schema_migrations"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 20); err != nil {
		t.Fatalf("down to v20: %v", err)
	}
	conn := store.DB()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	const ts = "2026-09-24T10:00:00Z"
	for _, kind := range []string{"AUTHORITY_PROMOTION", "EXECUTOR_PREFERENCE"} {
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO experience_rules(rule_id,proposal_id,grant_id,version,adaptation_kind,scope_key,generic_task_class,
				preferred_executor,state,verified_samples,activated_at,created_at,updated_at)
			VALUES (?,?,'g',1,?,'s','REVIEW','codex','ACTIVE',1,?,?,?)`, "rule-"+kind, "p-"+kind, kind, ts, ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := provider.UpTo(ctx, 21); err != nil {
		t.Fatalf("up to v21: %v", err)
	}
	for kind, want := range map[string]string{"AUTHORITY_PROMOTION": "ROLLED_BACK", "EXECUTOR_PREFERENCE": "ACTIVE"} {
		var state string
		if err := conn.QueryRowContext(ctx, `SELECT state FROM experience_rules WHERE rule_id=?`, "rule-"+kind).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != want {
			t.Fatalf("%s rule state=%s want %s", kind, state, want)
		}
	}
	if _, err := conn.ExecContext(ctx, `UPDATE experience_rules SET state='RETIRED' WHERE rule_id='rule-EXECUTOR_PREFERENCE'`); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("experience_rules update guard lost: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO experience_rules(rule_id,proposal_id,grant_id,version,adaptation_kind,scope_key,generic_task_class,
			preferred_executor,state,verified_samples,created_at,updated_at)
		VALUES ('rule-new','p-new','g',1,'AUTHORITY_PROMOTION','s','REVIEW','codex','ACTIVE',1,?,?)`, ts, ts); err == nil ||
		!strings.Contains(err.Error(), "only EXECUTOR_PREFERENCE adaptation is permitted") {
		t.Fatalf("kind trigger missing: %v", err)
	}
	if _, err := provider.DownTo(ctx, 20); err != nil {
		t.Fatalf("down to v20: %v", err)
	}
	var n int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name LIKE '%_kind_%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("kind triggers after down: %d err=%v", n, err)
	}
}

func TestMigrationsApplyOverLegacyRows(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	db := store.DB() // single pooled connection: PRAGMA below applies to every statement
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithTableName("schema_migrations"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 19); err != nil {
		t.Fatalf("down to v19: %v", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	// Legacy shapes written by the retired Python reconciler in production.
	const ts = "2026-09-24T10:00:00Z"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_cases(case_id,mission_id,source,object_id,revision_id,observation_evidence_id,
			initial_request_json,grant_json,state,current_work_id,next_work_json,completed_steps,max_steps,
			remaining_budget,progress_signature,created_at,updated_at)
		VALUES ('case-legacy','m','ado','i','r','o','{}','{"priority": 1000}','ACTIVE','w','{}',0,3,2,'p',?,?)`, ts, ts); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO experience_outcomes(outcome_id,task_id,generic_task_class,scope_key,executor_kind,accepted,
			human_intervention,retry_count,recorded_at)
		VALUES ('outcome-legacy','t','REVIEW','s','codex',1,0,0,'2026-10-19 14:41:15')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE issue_hierarchy (id TEXT PRIMARY KEY, parent_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate legacy DB to head: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name IN ('capability_acquisition_proposals','idx_capability_acquisitions_semantic_state')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("capability acquisition objects after head: %d err=%v", n, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM workflow_cases WHERE grant_json='{"priority": 1000}'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("legacy workflow case lost: %d err=%v", n, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name='issue_hierarchy'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("issue_hierarchy must be left alone: %d err=%v", n, err)
	}
}
