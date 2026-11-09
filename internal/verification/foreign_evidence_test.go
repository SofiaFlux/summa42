package verification_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/purpose"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/verification"
)

func TestAcceptTask_RejectsUncorroboratedForeignEvidence(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "verification_foreign.db")
	dbStore, err := state.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer dbStore.DB().Close()

	clk := clock.System{}
	purposes := purpose.New(dbStore, clk)
	execSvc := execution.New(dbStore, clk, purposes)
	verSvc := verification.New(dbStore, clk, execSvc)

	missionID, err := purposes.CreateMission(ctx, "Foreign verification mission")
	if err != nil {
		t.Fatalf("create mission: %v", err)
	}

	task, err := execSvc.CreateTask(ctx, execution.TaskRequest{
		Purpose:              domain.PurposeRef{Kind: domain.PurposeMission, ID: missionID},
		AcceptanceCriteria:   []string{"result is independently verified"},
		RequiredCapabilities: []string{"compute"},
		RequiredEnforcement:  domain.EnforcementPartial,
		ResourceEnvelopeID:   "envelope_test",
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	attempt, err := execSvc.StartAttempt(ctx, task.ID, "foreign_executor", time.Hour)
	if err != nil {
		t.Fatalf("lease task: %v", err)
	}

	now := time.Now().UTC()
	evID := domain.ID("ev_foreign_item_1")

	_, err = dbStore.DB().ExecContext(ctx, `
		INSERT INTO evidence_objects(evidence_id, content_hash, media_type, kind, size_bytes, created_at)
		VALUES (?, 'hash_item_1', 'application/json', 'EXTERNAL_REPORT', 256, ?);
	`, evID, domain.FormatTime(now))
	if err != nil {
		t.Fatalf("insert evidence object: %v", err)
	}

	_, err = verSvc.CompleteAttempt(ctx, attempt.ID, verification.CompletionManifest{
		EvidenceIDs: []domain.ID{evID},
	})
	if err != nil {
		t.Fatalf("complete attempt: %v", err)
	}

	// 1. Mark evidence as UNTRUSTED and SELF_ATTESTED in foreign_evidence_lineage
	_, err = dbStore.DB().ExecContext(ctx, `
		INSERT INTO foreign_evidence_lineage(
			evidence_id, content_hash, origin_collective_id, producer_principal_id,
			signature_hex, trust_tier, corroboration_level, witnesses_json, created_at
		) VALUES (
			?, 'hash_item_1', 'collective_external', 'external_producer_1',
			'deadbeef', 'UNTRUSTED', 'SELF_ATTESTED', '[]', ?
		);
	`, evID, domain.FormatTime(now))
	if err != nil {
		t.Fatalf("seed foreign evidence lineage: %v", err)
	}

	// Try accepting with uncorroborated evidence
	_, err = verSvc.AcceptTask(ctx, task.ID, verification.AcceptanceRequest{
		VerifierID:   "internal_independent_verifier",
		VerifierType: "SUMMA_ORACLE",
		CriteriaMet:  true,
		EvidenceIDs:  []domain.ID{evID},
	})
	if err == nil {
		t.Fatalf("expected error when accepting task with uncorroborated foreign evidence, got nil")
	}

	// 2. Try accepting where verifier is the producer of the foreign evidence
	_, err = verSvc.AcceptTask(ctx, task.ID, verification.AcceptanceRequest{
		VerifierID:   "external_producer_1", // Producer tries to verify own evidence!
		VerifierType: "SUMMA_ORACLE",
		CriteriaMet:  true,
		EvidenceIDs:  []domain.ID{evID},
	})
	if err == nil {
		t.Fatalf("expected error when producer verifies own evidence, got nil")
	}

	// 3. Elevate evidence to VERIFIED_PARTNER with INDEPENDENT_WITNESS
	_, err = dbStore.DB().ExecContext(ctx, `
		UPDATE foreign_evidence_lineage
		SET trust_tier = 'VERIFIED_PARTNER', corroboration_level = 'INDEPENDENT_WITNESS', witnesses_json = '["oracle_witness_1"]'
		WHERE evidence_id = ?;
	`, evID)
	if err != nil {
		t.Fatalf("update foreign evidence lineage: %v", err)
	}

	// AcceptTask should now succeed
	accepted, err := verSvc.AcceptTask(ctx, task.ID, verification.AcceptanceRequest{
		VerifierID:   "internal_independent_verifier",
		VerifierType: "SUMMA_ORACLE",
		CriteriaMet:  true,
		EvidenceIDs:  []domain.ID{evID},
	})
	if err != nil {
		t.Fatalf("expected successful acceptance with corroborated evidence, got: %v", err)
	}
	if !accepted.CriteriaMet || accepted.TaskID != task.ID {
		t.Fatalf("unexpected acceptance record: %+v", accepted)
	}

	// Task should now be in SUCCEEDED state
	var taskState domain.TaskState
	err = dbStore.DB().QueryRowContext(ctx, `SELECT state FROM tasks WHERE task_id = ?`, task.ID).Scan(&taskState)
	if err != nil {
		t.Fatalf("query task state: %v", err)
	}
	if taskState != domain.TaskSucceeded {
		t.Fatalf("expected task state %s, got %s", domain.TaskSucceeded, taskState)
	}
}
