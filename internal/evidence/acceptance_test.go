package evidence_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/delegation"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type keyPair struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func genKey(t *testing.T) keyPair {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return keyPair{pub: pub, priv: priv}
}

type staticClock struct {
	now time.Time
}

func (s staticClock) Now() time.Time {
	return s.now
}

type testFixture struct {
	store    *state.Store
	clock    staticClock
	resolver delegation.MapPublicKeyResolver
	engine   *evidence.IndependentAcceptanceEngine
}

func setupFixture(t *testing.T) (*testFixture, func()) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "acceptance_test.db")
	dbStore, err := state.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	clk := staticClock{now: time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)}
	resolver := delegation.MapPublicKeyResolver{}

	engine := evidence.NewIndependentAcceptanceEngine(
		dbStore,
		clk,
		resolver,
		evidence.AcceptancePolicy{
			RequireIndependentWitness: true,
			MinimumTrustTier:          evidence.TrustTierVerifiedPartner,
			MinimumCorroborationLevel: evidence.CorroborationIndependentWitness,
		},
	)

	cleanup := func() {
		_ = dbStore.DB().Close()
	}

	return &testFixture{
		store:    dbStore,
		clock:    clk,
		resolver: resolver,
		engine:   engine,
	}, cleanup
}

func seedTaskAndAttempt(t *testing.T, fix *testFixture, taskID, attemptID domain.ID, executorKind string) {
	t.Helper()
	ctx := context.Background()
	db := fix.store.DB()

	_, err := db.ExecContext(ctx, `
		INSERT INTO tasks(
			task_id, purpose_kind, purpose_id, state,
			acceptance_criteria_json, required_capabilities_json, required_enforcement,
			authority_ceiling_json, resource_envelope_id, created_at, updated_at
		) VALUES (
			?, 'MISSION', 'purpose_1', 'EXECUTING',
			'{"criteria": "compute_report"}', '[]', 'ENFORCED',
			'{}', 'env_1', '2026-11-10T09:00:00Z', '2026-11-10T09:00:00Z'
		)`, taskID)
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO attempts(
			attempt_id, task_id, state, fence_generation,
			lease_state, lease_expires_at, started_at, executor_kind
		) VALUES (
			?, ?, 'RUNNING', 1,
			'ACTIVE', '2026-11-10T11:00:00Z', '2026-11-10T09:00:00Z', ?
		)`, attemptID, taskID, executorKind)
	if err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
}

// 1. Acceptance Criterion 4: Uncorroborated Foreign Success Signal Cannot Complete Task
func TestAcceptanceEngine_RejectsUncorroboratedForeignSuccess(t *testing.T) {
	ctx := context.Background()
	fix, cleanup := setupFixture(t)
	defer cleanup()

	taskID := domain.ID("task_cloud_compute_1")
	attemptID := domain.ID("att_cloud_compute_1")
	producerKey := genKey(t)
	producerID := domain.ID("foreign_untrusted_agent")
	fix.resolver[producerID] = producerKey.pub

	seedTaskAndAttempt(t, fix, taskID, attemptID, string(producerID))

	evID := domain.ID("ev_foreign_res_1")
	_, err := fix.store.DB().ExecContext(ctx, `
		INSERT INTO evidence_objects(evidence_id, content_hash, media_type, kind, size_bytes, created_at)
		VALUES (?, 'hash_res_1', 'application/json', 'EXTERNAL_REPORT', 512, '2026-11-10T09:30:00Z')
	`, evID)
	if err != nil {
		t.Fatalf("insert evidence object: %v", err)
	}

	record := evidence.ForeignEvidenceRecord{
		EvidenceID:          evID,
		ContentHash:         "hash_res_1",
		OriginCollectiveID:  "collective_unknown",
		ProducerPrincipalID: producerID,
		TrustTier:           evidence.TrustTierUntrusted,
		CorroborationLevel:  evidence.CorroborationSelfAttested,
		Witnesses:           nil,
		CreatedAt:           fix.clock.Now().UTC(),
	}
	if err := record.Sign(producerKey.priv); err != nil {
		t.Fatalf("sign foreign record: %v", err)
	}
	if err := fix.engine.RecordLineage(ctx, record); err != nil {
		t.Fatalf("record lineage: %v", err)
	}

	signal := evidence.ForeignCompletionSignal{
		TaskID:              taskID,
		AttemptID:           attemptID,
		OriginCollectiveID:  "collective_unknown",
		ProducerPrincipalID: producerID,
		RemoteSignalState:   evidence.SignalClaimedSuccess,
		EvidenceIDs:         []domain.ID{evID},
		Timestamp:           fix.clock.Now().UTC(),
	}
	if err := signal.Sign(producerKey.priv); err != nil {
		t.Fatalf("sign signal: %v", err)
	}

	verifierID := domain.ID("internal_independent_verifier")
	recon, err := fix.engine.ReconcileExternalSignal(ctx, signal, verifierID)
	if err != nil {
		t.Fatalf("reconcile signal: %v", err)
	}

	// Must resolve to OUTCOME_UNKNOWN, with blind retry prevented!
	if recon.ReconciliationState != evidence.ReconciliationOutcomeUnknown {
		t.Fatalf("expected state %s, got %s", evidence.ReconciliationOutcomeUnknown, recon.ReconciliationState)
	}
	if !recon.BlindRetryPrevented {
		t.Fatalf("expected BlindRetryPrevented to be true")
	}

	// Assert task did NOT transition to SUCCEEDED
	var taskState domain.TaskState
	err = fix.store.DB().QueryRowContext(ctx, `SELECT state FROM tasks WHERE task_id = ?`, taskID).Scan(&taskState)
	if err != nil {
		t.Fatalf("query task state: %v", err)
	}
	if taskState == domain.TaskSucceeded {
		t.Fatalf("violation: task transitioned to SUCCEEDED from uncorroborated foreign success signal!")
	}
}

// 2. Acceptance Criterion 1 & 2: Verified Acceptance with Corroborated Evidence
func TestAcceptanceEngine_VerifiedAcceptanceWhenSufficientlyCorroborated(t *testing.T) {
	ctx := context.Background()
	fix, cleanup := setupFixture(t)
	defer cleanup()

	taskID := domain.ID("task_verified_partner_1")
	attemptID := domain.ID("att_verified_partner_1")
	partnerKey := genKey(t)
	partnerID := domain.ID("partner_agent_alpha")
	fix.resolver[partnerID] = partnerKey.pub

	seedTaskAndAttempt(t, fix, taskID, attemptID, string(partnerID))

	evID := domain.ID("ev_partner_res_1")
	_, err := fix.store.DB().ExecContext(ctx, `
		INSERT INTO evidence_objects(evidence_id, content_hash, media_type, kind, size_bytes, created_at)
		VALUES (?, 'hash_partner_1', 'application/json', 'EXTERNAL_REPORT', 1024, '2026-11-10T09:30:00Z')
	`, evID)
	if err != nil {
		t.Fatalf("insert evidence object: %v", err)
	}

	record := evidence.ForeignEvidenceRecord{
		EvidenceID:          evID,
		ContentHash:         "hash_partner_1",
		OriginCollectiveID:  "collective_alpha",
		ProducerPrincipalID: partnerID,
		DelegationID:        "del_alpha_summa",
		TrustTier:           evidence.TrustTierVerifiedPartner,
		CorroborationLevel:  evidence.CorroborationIndependentWitness,
		Witnesses:           []domain.ID{"independent_oracle_witness"},
		UpstreamLineage:     []string{"upstream_audit_proof"},
		CreatedAt:           fix.clock.Now().UTC(),
	}
	if err := record.Sign(partnerKey.priv); err != nil {
		t.Fatalf("sign record: %v", err)
	}
	if err := fix.engine.RecordLineage(ctx, record); err != nil {
		t.Fatalf("record lineage: %v", err)
	}

	signal := evidence.ForeignCompletionSignal{
		TaskID:              taskID,
		AttemptID:           attemptID,
		OriginCollectiveID:  "collective_alpha",
		ProducerPrincipalID: partnerID,
		RemoteSignalState:   evidence.SignalClaimedSuccess,
		EvidenceIDs:         []domain.ID{evID},
		Timestamp:           fix.clock.Now().UTC(),
	}
	if err := signal.Sign(partnerKey.priv); err != nil {
		t.Fatalf("sign signal: %v", err)
	}

	verifierID := domain.ID("internal_independent_verifier")
	recon, err := fix.engine.ReconcileExternalSignal(ctx, signal, verifierID)
	if err != nil {
		t.Fatalf("reconcile signal: %v", err)
	}

	if recon.ReconciliationState != evidence.ReconciliationVerifiedAcceptance {
		t.Fatalf("expected state %s, got %s (reason: %s)", evidence.ReconciliationVerifiedAcceptance, recon.ReconciliationState, recon.ReconciliationReason)
	}
}

// 3. Rejection upon Signature Tampering or Forged Evidence
func TestAcceptanceEngine_RejectsForgedOrTamperedEvidence(t *testing.T) {
	ctx := context.Background()
	fix, cleanup := setupFixture(t)
	defer cleanup()

	taskID := domain.ID("task_tampered_1")
	attemptID := domain.ID("att_tampered_1")
	partnerKey := genKey(t)
	attackerKey := genKey(t)
	partnerID := domain.ID("partner_agent_alpha")
	fix.resolver[partnerID] = partnerKey.pub

	seedTaskAndAttempt(t, fix, taskID, attemptID, string(partnerID))

	evID := domain.ID("ev_tampered_1")
	_, err := fix.store.DB().ExecContext(ctx, `
		INSERT INTO evidence_objects(evidence_id, content_hash, media_type, kind, size_bytes, created_at)
		VALUES (?, 'hash_tampered_1', 'application/json', 'EXTERNAL_REPORT', 1024, '2026-11-10T09:30:00Z')
	`, evID)
	if err != nil {
		t.Fatalf("insert evidence object: %v", err)
	}

	record := evidence.ForeignEvidenceRecord{
		EvidenceID:          evID,
		ContentHash:         "hash_tampered_1",
		OriginCollectiveID:  "collective_alpha",
		ProducerPrincipalID: partnerID,
		TrustTier:           evidence.TrustTierVerifiedPartner,
		CorroborationLevel:  evidence.CorroborationIndependentWitness,
		Witnesses:           []domain.ID{"witness_1"},
		CreatedAt:           fix.clock.Now().UTC(),
	}
	// Signed with attacker key instead of partner key!
	_ = record.Sign(attackerKey.priv)
	// RecordLineage must reject invalid signature immediately
	if err := fix.engine.RecordLineage(ctx, record); err == nil {
		t.Fatalf("expected error from RecordLineage on invalid signature, got nil")
	}
}

// 4. Acceptance Criterion 2: Prevention of Blind Retries & Resolution
func TestAcceptanceEngine_PreventsBlindRetriesAndResolves(t *testing.T) {
	ctx := context.Background()
	fix, cleanup := setupFixture(t)
	defer cleanup()

	taskID := domain.ID("task_no_blind_retry_1")
	attemptID := domain.ID("att_no_blind_retry_1")
	producerKey := genKey(t)
	producerID := domain.ID("partner_agent_beta")
	fix.resolver[producerID] = producerKey.pub

	seedTaskAndAttempt(t, fix, taskID, attemptID, string(producerID))

	// Reconcile signal with missing evidence to simulate incomplete transmission
	signal := evidence.ForeignCompletionSignal{
		TaskID:              taskID,
		AttemptID:           attemptID,
		OriginCollectiveID:  "collective_beta",
		ProducerPrincipalID: producerID,
		RemoteSignalState:   evidence.SignalClaimedSuccess,
		EvidenceIDs:         []domain.ID{"ev_non_existent"},
		Timestamp:           fix.clock.Now().UTC(),
	}
	if err := signal.Sign(producerKey.priv); err != nil {
		t.Fatalf("sign signal: %v", err)
	}

	verifierID := domain.ID("internal_independent_verifier")
	recon, err := fix.engine.ReconcileExternalSignal(ctx, signal, verifierID)
	if err != nil {
		t.Fatalf("reconcile signal: %v", err)
	}

	if recon.ReconciliationState != evidence.ReconciliationOutcomeUnknown {
		t.Fatalf("expected OUTCOME_UNKNOWN when evidence is pending/missing, got %s", recon.ReconciliationState)
	}
	if !recon.BlindRetryPrevented {
		t.Fatalf("expected BlindRetryPrevented to be true")
	}

	// Verify that PreventBlindRetry blocks automatic dispatch
	prevented, err := fix.engine.PreventBlindRetry(ctx, taskID)
	if err != nil {
		t.Fatalf("prevent blind retry: %v", err)
	}
	if !prevented {
		t.Fatalf("expected PreventBlindRetry to return true")
	}

	// Now external agent reports failure, resolving the unknown state
	failSignal := evidence.ForeignCompletionSignal{
		TaskID:              taskID,
		AttemptID:           attemptID,
		OriginCollectiveID:  "collective_beta",
		ProducerPrincipalID: producerID,
		RemoteSignalState:   evidence.SignalClaimedFailure,
		Timestamp:           fix.clock.Now().UTC().Add(time.Minute),
	}
	if err := failSignal.Sign(producerKey.priv); err != nil {
		t.Fatalf("sign fail signal: %v", err)
	}
	reconFail, err := fix.engine.ReconcileExternalSignal(ctx, failSignal, verifierID)
	if err != nil {
		t.Fatalf("reconcile fail signal: %v", err)
	}
	if reconFail.ReconciliationState != evidence.ReconciliationRejected {
		t.Fatalf("expected state REJECTED, got %s", reconFail.ReconciliationState)
	}

	// PreventBlindRetry should now return false because the unknown state has been resolved!
	preventedAfterResolution, err := fix.engine.PreventBlindRetry(ctx, taskID)
	if err != nil {
		t.Fatalf("prevent blind retry check: %v", err)
	}
	if preventedAfterResolution {
		t.Fatalf("expected PreventBlindRetry to return false after resolution")
	}
}

// 5. Review edge cases: CLAIMED_PROGRESS, internal identity impersonation, lineage immutability, hash mismatch
func TestAcceptanceEngine_ReviewEdgeCases(t *testing.T) {
	ctx := context.Background()
	fix, cleanup := setupFixture(t)
	defer cleanup()

	partnerKey := genKey(t)
	partnerID := domain.ID("partner_agent_gamma")
	fix.resolver[partnerID] = partnerKey.pub

	// A: CLAIMED_PROGRESS cannot verify
	t.Run("ClaimedProgress_NonTerminal", func(t *testing.T) {
		seedTaskAndAttempt(t, fix, "task_prog", "att_prog", string(partnerID))
		progSignal := evidence.ForeignCompletionSignal{
			TaskID:              "task_prog",
			AttemptID:           "att_prog",
			OriginCollectiveID:  "collective_gamma",
			ProducerPrincipalID: partnerID,
			RemoteSignalState:   evidence.SignalClaimedProgress,
			Timestamp:           fix.clock.Now().UTC(),
		}
		if err := progSignal.Sign(partnerKey.priv); err != nil {
			t.Fatal(err)
		}
		recon, err := fix.engine.ReconcileExternalSignal(ctx, progSignal, "verifier_indep")
		if err != nil {
			t.Fatal(err)
		}
		if recon.ReconciliationState != evidence.ReconciliationOutcomeUnknown {
			t.Fatalf("expected OUTCOME_UNKNOWN for progress, got %s", recon.ReconciliationState)
		}
	})

	// B: Internal identity impersonation rejected at RecordLineage
	t.Run("InternalIdentity_ImpersonationRejected", func(t *testing.T) {
		for _, fakeID := range []domain.ID{"summand_agent", "cube_root", "root", "owner"} {
			rec := evidence.ForeignEvidenceRecord{
				EvidenceID:          domain.ID("ev_" + fakeID),
				ContentHash:         "hash_fake",
				ProducerPrincipalID: fakeID,
			}
			err := fix.engine.RecordLineage(ctx, rec)
			if err == nil {
				t.Fatalf("expected error when foreign producer claims %s, got nil", fakeID)
			}
		}
	})

	// C: Hash mismatch between evidence_objects and ForeignEvidenceRecord
	t.Run("ContentHash_Mismatch", func(t *testing.T) {
		evID := domain.ID("ev_hash_mismatch")
		_, err := fix.store.DB().ExecContext(ctx, `
			INSERT INTO evidence_objects(evidence_id, content_hash, media_type, kind, size_bytes, created_at)
			VALUES (?, 'real_sha256_hash', 'application/json', 'EXTERNAL_REPORT', 128, '2026-11-10T09:30:00Z')
		`, evID)
		if err != nil {
			t.Fatal(err)
		}

		rec := evidence.ForeignEvidenceRecord{
			EvidenceID:          evID,
			ContentHash:         "fake_sha256_hash", // Mismatch!
			OriginCollectiveID:  "collective_gamma",
			ProducerPrincipalID: partnerID,
			TrustTier:           evidence.TrustTierVerifiedPartner,
			CorroborationLevel:  evidence.CorroborationSelfAttested,
			CreatedAt:           fix.clock.Now().UTC(),
		}
		_ = rec.Sign(partnerKey.priv)
		err = fix.engine.RecordLineage(ctx, rec)
		if err == nil {
			t.Fatalf("expected error on content hash mismatch, got nil")
		}
	})

	// D: Lineage immutability (cannot re-record or escalate trust)
	t.Run("Lineage_Immutability", func(t *testing.T) {
		evID := domain.ID("ev_immutable")
		_, err := fix.store.DB().ExecContext(ctx, `
			INSERT INTO evidence_objects(evidence_id, content_hash, media_type, kind, size_bytes, created_at)
			VALUES (?, 'immutable_hash', 'application/json', 'EXTERNAL_REPORT', 128, '2026-11-10T09:30:00Z')
		`, evID)
		if err != nil {
			t.Fatal(err)
		}

		rec := evidence.ForeignEvidenceRecord{
			EvidenceID:          evID,
			ContentHash:         "immutable_hash",
			OriginCollectiveID:  "collective_gamma",
			ProducerPrincipalID: partnerID,
			TrustTier:           evidence.TrustTierProvisional,
			CorroborationLevel:  evidence.CorroborationSelfAttested,
			CreatedAt:           fix.clock.Now().UTC(),
		}
		if err := rec.Sign(partnerKey.priv); err != nil {
			t.Fatal(err)
		}
		if err := fix.engine.RecordLineage(ctx, rec); err != nil {
			t.Fatal(err)
		}

		// Attempt to overwrite with escalated trust
		recEscalated := rec
		recEscalated.TrustTier = evidence.TrustTierConstitutionalCollective
		_ = recEscalated.Sign(partnerKey.priv)
		err = fix.engine.RecordLineage(ctx, recEscalated)
		if err == nil {
			t.Fatalf("expected error when attempting to re-record immutable lineage, got nil")
		}
	})
}
