package fieldfeedback

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

type observerHarness struct {
	ctx      context.Context
	observer *Observer
	store    interface{ DB() interface{} }
	taskID   domain.ID
	attempt  domain.ID
	evidence domain.ID
}

func newObserverHarness(t *testing.T) (*Observer, domain.ID, domain.ID, domain.ID, *testutil.Clock) {
	t.Helper()
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC))
	taskID := domain.ID("task_feedback")
	attemptID := domain.ID("attempt_feedback")
	evidenceID := domain.ID("evidence_feedback")
	now := clk.Now().UTC().Format(time.RFC3339Nano)

	if _, err := store.DB().ExecContext(ctx,
		"INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES ('env_feedback', 1000, ?)", now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO tasks(
			task_id, purpose_kind, purpose_id, state, current_fence,
			acceptance_criteria_json, required_capabilities_json, required_enforcement,
			authority_ceiling_json, resource_envelope_id, priority, created_at, updated_at
		) VALUES (?, 'OWNER_DIRECTIVE', 'owner-feedback', 'EXECUTING', 1, '[]', '[]', 'ENFORCED', '[]', 'env_feedback', 0, ?, ?)`,
		taskID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO attempts(
			attempt_id, task_id, state, fence_generation, lease_state, lease_expires_at, started_at, executor_kind
		) VALUES (?, ?, 'RUNNING', 1, 'ACTIVE', ?, ?, 'test-executor')`,
		attemptID, taskID, clk.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO evidence_objects(evidence_id, content_hash, media_type, kind, size_bytes, created_at)
		VALUES (?, 'hash-feedback', 'text/plain', 'TEST', 1, ?)`, evidenceID, now); err != nil {
		t.Fatal(err)
	}
	return NewObserver(store, clk, "collective_feedback"), taskID, attemptID, evidenceID, clk
}

func TestObservationRecordValidatesReferencesAndKeepsLocalSummaryOutOfAudit(t *testing.T) {
	observer, taskID, attemptID, evidenceID, _ := newObserverHarness(t)
	ctx := context.Background()
	secretSummary := "client-internal-path /secret/repo/file.go"

	if _, err := observer.Record(ctx, ObservationInput{
		TaskID: taskID, AttemptID: attemptID, Category: "APPROVAL_FRICTION",
		BasisClass: "OPERATOR_OBSERVATION", SourceKind: "OPERATOR", SummaryLocal: secretSummary,
		Metrics:        map[string]any{"retry_count": 2, "arbitrary_local_note": "keep local"},
		RuntimeVersion: "test", ExecutorKind: "test-executor", Enforcement: domain.EnforcementEnforced,
		EvidenceIDs: []domain.ID{evidenceID},
	}); err != nil {
		t.Fatal(err)
	}

	var payload string
	if err := observer.store.DB().QueryRowContext(ctx,
		"SELECT payload_json FROM audit_events WHERE kind = 'FIELD_OBSERVATION_RECORDED' ORDER BY created_at DESC LIMIT 1",
	).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, secretSummary) || strings.Contains(payload, "arbitrary_local_note") {
		t.Fatalf("audit payload leaked local observation content: %s", payload)
	}

	var metrics string
	if err := observer.store.DB().QueryRowContext(ctx,
		"SELECT metrics_json FROM field_observations ORDER BY created_at DESC LIMIT 1",
	).Scan(&metrics); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(metrics), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["arbitrary_local_note"] != "keep local" {
		t.Fatalf("local metrics not preserved: %v", decoded)
	}
}

func TestObservationRejectsMissingReferences(t *testing.T) {
	observer, taskID, _, _, _ := newObserverHarness(t)
	ctx := context.Background()
	base := ObservationInput{
		TaskID: taskID, Category: "TEST", BasisClass: "TEST", SourceKind: "TEST",
		SummaryLocal: "local", Enforcement: domain.EnforcementEnforced,
	}
	badAttempt := base
	badAttempt.AttemptID = "missing-attempt"
	if _, err := observer.Record(ctx, badAttempt); err == nil {
		t.Fatal("missing attempt reference was accepted")
	}
	badOperation := base
	badOperation.OperationID = "missing-operation"
	if _, err := observer.Record(ctx, badOperation); err == nil {
		t.Fatal("missing operation reference was accepted")
	}
	badEvidence := base
	badEvidence.EvidenceIDs = []domain.ID{"missing-evidence"}
	if _, err := observer.Record(ctx, badEvidence); err == nil {
		t.Fatal("missing evidence reference was accepted")
	}
}

func TestRepeatedFailureDetectorIsIdempotent(t *testing.T) {
	observer, taskID, attemptID, evidenceID, clk := newObserverHarness(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		failureID := domain.NewID("failure")
		if _, err := observer.store.DB().ExecContext(ctx, `
			INSERT INTO attempt_failures(
				failure_id, attempt_id, task_id, failure_class, signature, evidence_ids_json, created_at
			) VALUES (?, ?, ?, 'EXECUTION', 'same-signature', ?, ?)`,
			failureID, attemptID, taskID, "[\""+string(evidenceID)+"\"]",
			clk.Now().Add(time.Duration(i)*time.Second).UTC().Format(time.RFC3339Nano),
		); err != nil {
			t.Fatal(err)
		}
	}

	first, err := observer.Detect(ctx, DetectorInput{TaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 {
		t.Fatal("repeated failure detector produced no observation")
	}
	second, err := observer.Detect(ctx, DetectorInput{TaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) == 0 || second[0].ID != first[0].ID {
		t.Fatalf("detector was not idempotent: first=%v second=%v", first, second)
	}
	var count int
	if err := observer.store.DB().QueryRowContext(ctx,
		"SELECT count(*) FROM field_observations WHERE category = 'REPEATED_ATTEMPT_FAILURE' AND task_id = ?", taskID,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("repeated detector observations = %d, want 1", count)
	}
}
