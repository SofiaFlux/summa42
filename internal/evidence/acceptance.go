package evidence

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/delegation"
	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type ExternalSignalState string

const (
	SignalClaimedSuccess  ExternalSignalState = "CLAIMED_SUCCESS"
	SignalClaimedFailure  ExternalSignalState = "CLAIMED_FAILURE"
	SignalClaimedProgress ExternalSignalState = "CLAIMED_PROGRESS"
)

type ReconciliationState string

const (
	ReconciliationVerifiedAcceptance ReconciliationState = "VERIFIED_ACCEPTANCE"
	ReconciliationRejected           ReconciliationState = "REJECTED"
	ReconciliationOutcomeUnknown     ReconciliationState = "OUTCOME_UNKNOWN"
)

var (
	ErrVerifierIsProducer  = errors.New("verifier must be independent and cannot be the signal producer or witness")
	ErrAttemptNotFound     = errors.New("attempt not found")
	ErrMissingEvidence     = errors.New("signal references missing evidence object")
	ErrUncorroboratedClaim = errors.New("external signal lacks required corroboration")
	ErrLineageExists       = errors.New("foreign evidence lineage already recorded and is immutable")
	ErrHashMismatch        = errors.New("lineage content hash does not match physical evidence object")
)

type ForeignCompletionSignal struct {
	TaskID              domain.ID           `json:"task_id"`
	AttemptID           domain.ID           `json:"attempt_id"`
	OriginCollectiveID  domain.ID           `json:"origin_collective_id"`
	ProducerPrincipalID domain.ID           `json:"producer_principal_id"`
	RemoteSignalState   ExternalSignalState `json:"remote_signal_state"`
	EvidenceIDs         []domain.ID         `json:"evidence_ids"`
	SignalSignature     []byte              `json:"signal_signature,omitempty"`
	Timestamp           time.Time           `json:"timestamp"`
}

// CanonicalBytes returns the deterministic serialization for signing a completion signal.
func (s *ForeignCompletionSignal) CanonicalBytes() []byte {
	evIDs := make([]string, 0, len(s.EvidenceIDs))
	for _, id := range s.EvidenceIDs {
		evIDs = append(evIDs, string(id))
	}
	sort.Strings(evIDs)

	type canonicalSignal struct {
		DomainTag           string   `json:"_domain"`
		TaskID              string   `json:"task_id"`
		AttemptID           string   `json:"attempt_id"`
		OriginCollectiveID  string   `json:"origin_collective_id"`
		ProducerPrincipalID string   `json:"producer_principal_id"`
		RemoteSignalState   string   `json:"remote_signal_state"`
		EvidenceIDs         []string `json:"evidence_ids"`
		TimestampRFC3339    string   `json:"timestamp"`
	}

	payload := canonicalSignal{
		DomainTag:           "summa42:foreign_signal:v1",
		TaskID:              string(s.TaskID),
		AttemptID:           string(s.AttemptID),
		OriginCollectiveID:  string(s.OriginCollectiveID),
		ProducerPrincipalID: string(s.ProducerPrincipalID),
		RemoteSignalState:   string(s.RemoteSignalState),
		EvidenceIDs:         evIDs,
		TimestampRFC3339:    s.Timestamp.UTC().Format(time.RFC3339Nano),
	}

	raw, _ := json.Marshal(payload)
	return raw
}

func (s *ForeignCompletionSignal) Sign(privKey ed25519.PrivateKey) error {
	if len(privKey) == 0 {
		return errors.New("private key is empty")
	}
	s.SignalSignature = ed25519.Sign(privKey, s.CanonicalBytes())
	return nil
}

func (s *ForeignCompletionSignal) VerifySignature(pubKey ed25519.PublicKey) error {
	if len(pubKey) == 0 {
		return errors.New("public key is empty")
	}
	if len(s.SignalSignature) == 0 {
		return errors.New("signal signature is empty")
	}
	if !ed25519.Verify(pubKey, s.CanonicalBytes(), s.SignalSignature) {
		return errors.New("invalid signal signature")
	}
	return nil
}

type ReconciliationRecord struct {
	ReconciliationID     domain.ID           `json:"reconciliation_id"`
	TaskID               domain.ID           `json:"task_id"`
	AttemptID            domain.ID           `json:"attempt_id"`
	OriginCollectiveID   domain.ID           `json:"origin_collective_id"`
	ProducerPrincipalID  domain.ID           `json:"producer_principal_id"`
	ExternalSignalState  ExternalSignalState `json:"external_signal_state"`
	ReconciliationState  ReconciliationState `json:"reconciliation_state"`
	ReconciliationReason string              `json:"reconciliation_reason"`
	EvidenceIDs          []domain.ID         `json:"evidence_ids"`
	BlindRetryPrevented  bool                `json:"blind_retry_prevented"`
	EvaluatedAt          time.Time           `json:"evaluated_at"`
}

type AcceptancePolicy struct {
	RequireIndependentWitness bool
	MinimumTrustTier          TrustTier
	MinimumCorroborationLevel CorroborationLevel
}

type IndependentAcceptanceEngine struct {
	store     *state.Store
	clock     clock.Clock
	resolver  delegation.PublicKeyResolver
	validator *delegation.Validator
	policy    AcceptancePolicy
}

func NewIndependentAcceptanceEngine(
	store *state.Store,
	clk clock.Clock,
	resolver delegation.PublicKeyResolver,
	policy AcceptancePolicy,
) *IndependentAcceptanceEngine {
	if clk == nil {
		clk = clock.System{}
	}
	return &IndependentAcceptanceEngine{
		store:     store,
		clock:     clk,
		resolver:  resolver,
		validator: delegation.NewValidator(resolver),
		policy:    policy,
	}
}

// RecordLineage stores the provenance and cryptographic verification metadata for an incoming foreign evidence.
// It strictly validates the signature, ensures content hash matches the underlying evidence object,
// and rejects internal identity impersonation.
func (e *IndependentAcceptanceEngine) RecordLineage(ctx context.Context, record ForeignEvidenceRecord) error {
	if e == nil || e.store == nil || e.resolver == nil {
		return errors.New("acceptance engine is not configured")
	}
	if record.EvidenceID == "" {
		return errors.New("evidence ID is required")
	}

	// 1. Isolation: foreign producer cannot impersonate internal identity
	if e.validator.IsInternalIdentity(record.ProducerPrincipalID) {
		return fmt.Errorf("producer %s is reserved internal identity", record.ProducerPrincipalID)
	}

	// 2. Cross-check content hash with underlying evidence_objects
	var storedHash string
	err := e.store.DB().QueryRowContext(ctx, `SELECT content_hash FROM evidence_objects WHERE evidence_id = ?`, record.EvidenceID).Scan(&storedHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: evidence object %s must exist prior to recording lineage", ErrEvidenceNotFound, record.EvidenceID)
		}
		return err
	}
	if storedHash != record.ContentHash {
		return fmt.Errorf("%w: object hash %s != record hash %s", ErrHashMismatch, storedHash, record.ContentHash)
	}

	// 3. Cryptographic signature verification upon registration
	pubKey, err := e.resolver.ResolvePublicKey(ctx, record.ProducerPrincipalID)
	if err != nil {
		return fmt.Errorf("resolve producer public key: %w", err)
	}
	if err := record.VerifySignature(pubKey); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidEvidenceSignature, err)
	}

	now := e.clock.Now().UTC()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	verifiedAtStr := domain.FormatTime(now)

	witnessesJSON, _ := json.Marshal(record.Witnesses)
	upstreamJSON, _ := json.Marshal(record.UpstreamLineage)

	// 4. Immutability: insert once, fail on conflict rather than allowing trust escalation
	query := `
		INSERT INTO foreign_evidence_lineage (
			evidence_id, content_hash, origin_collective_id, producer_principal_id,
			delegation_id, signature_hex, trust_tier, corroboration_level,
			witnesses_json, upstream_lineage_json, created_at, verified_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err = e.store.DB().ExecContext(ctx, query,
		record.EvidenceID,
		record.ContentHash,
		record.OriginCollectiveID,
		record.ProducerPrincipalID,
		record.DelegationID,
		hex.EncodeToString(record.Signature),
		string(record.TrustTier),
		string(record.CorroborationLevel),
		string(witnessesJSON),
		string(upstreamJSON),
		domain.FormatTime(record.CreatedAt),
		verifiedAtStr,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY KEY") {
			return fmt.Errorf("%w: evidence %s", ErrLineageExists, record.EvidenceID)
		}
		return err
	}
	return nil
}

// GetLineage retrieves the lineage record for an evidence ID.
func (e *IndependentAcceptanceEngine) GetLineage(ctx context.Context, evidenceID domain.ID) (ForeignEvidenceRecord, error) {
	if e == nil || e.store == nil {
		return ForeignEvidenceRecord{}, errors.New("acceptance engine is not configured")
	}

	var (
		rec              ForeignEvidenceRecord
		sigHex           string
		witnessesJSON    string
		upstreamJSON     string
		createdAtStr     string
		verifiedAtStr    sql.NullString
		trustTierStr     string
		corroborationStr string
	)

	query := `
		SELECT evidence_id, content_hash, origin_collective_id, producer_principal_id,
		       delegation_id, signature_hex, trust_tier, corroboration_level,
		       witnesses_json, upstream_lineage_json, created_at, verified_at
		FROM foreign_evidence_lineage
		WHERE evidence_id = ?;
	`
	err := e.store.DB().QueryRowContext(ctx, query, evidenceID).Scan(
		&rec.EvidenceID,
		&rec.ContentHash,
		&rec.OriginCollectiveID,
		&rec.ProducerPrincipalID,
		&rec.DelegationID,
		&sigHex,
		&trustTierStr,
		&corroborationStr,
		&witnessesJSON,
		&upstreamJSON,
		&createdAtStr,
		&verifiedAtStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ForeignEvidenceRecord{}, ErrEvidenceNotFound
		}
		return ForeignEvidenceRecord{}, err
	}

	rec.TrustTier = TrustTier(trustTierStr)
	rec.CorroborationLevel = CorroborationLevel(corroborationStr)

	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return ForeignEvidenceRecord{}, fmt.Errorf("%w: decode signature: %v", ErrEvidenceCorrupt, err)
	}
	rec.Signature = sig

	rec.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAtStr)
	if err != nil {
		return ForeignEvidenceRecord{}, fmt.Errorf("%w: parse created_at: %v", ErrEvidenceCorrupt, err)
	}
	if verifiedAtStr.Valid {
		t, err := time.Parse(time.RFC3339Nano, verifiedAtStr.String)
		if err != nil {
			return ForeignEvidenceRecord{}, fmt.Errorf("%w: parse verified_at: %v", ErrEvidenceCorrupt, err)
		}
		rec.VerifiedAt = &t
	}
	if err := json.Unmarshal([]byte(witnessesJSON), &rec.Witnesses); err != nil {
		return ForeignEvidenceRecord{}, fmt.Errorf("%w: unmarshal witnesses: %v", ErrEvidenceCorrupt, err)
	}
	if err := json.Unmarshal([]byte(upstreamJSON), &rec.UpstreamLineage); err != nil {
		return ForeignEvidenceRecord{}, fmt.Errorf("%w: unmarshal upstream lineage: %v", ErrEvidenceCorrupt, err)
	}

	return rec, nil
}

// ReconcileExternalSignal reconciles an incoming completion claim from a foreign agent.
// It strictly differentiates between VERIFIED_ACCEPTANCE, REJECTED, and OUTCOME_UNKNOWN,
// ensuring blind retries are prevented and remote success signals never blindly complete tasks.
func (e *IndependentAcceptanceEngine) ReconcileExternalSignal(
	ctx context.Context,
	signal ForeignCompletionSignal,
	verifierID domain.ID,
) (ReconciliationRecord, error) {
	if e == nil || e.store == nil || e.resolver == nil {
		return ReconciliationRecord{}, errors.New("acceptance engine is not configured")
	}
	now := e.clock.Now().UTC()
	recon := ReconciliationRecord{
		ReconciliationID:    domain.NewID("recon"),
		TaskID:              signal.TaskID,
		AttemptID:           signal.AttemptID,
		OriginCollectiveID:  signal.OriginCollectiveID,
		ProducerPrincipalID: signal.ProducerPrincipalID,
		ExternalSignalState: signal.RemoteSignalState,
		EvidenceIDs:         signal.EvidenceIDs,
		EvaluatedAt:         now,
		BlindRetryPrevented: true,
	}

	// 1. Verifier independence: Verifier cannot match the producer principal
	if verifierID != "" && strings.EqualFold(string(verifierID), string(signal.ProducerPrincipalID)) {
		recon.ReconciliationState = ReconciliationRejected
		recon.ReconciliationReason = "verifier identity cannot match foreign producer principal"
		if err := e.persistReconciliation(ctx, recon); err != nil {
			return recon, err
		}
		return recon, nil
	}

	// 2. Validate signal signature if present
	if len(signal.SignalSignature) > 0 {
		pubKey, err := e.resolver.ResolvePublicKey(ctx, signal.ProducerPrincipalID)
		if err != nil {
			recon.ReconciliationState = ReconciliationRejected
			recon.ReconciliationReason = fmt.Sprintf("failed resolving key for signal producer %s: %v", signal.ProducerPrincipalID, err)
			if err := e.persistReconciliation(ctx, recon); err != nil {
				return recon, err
			}
			return recon, nil
		}
		if err := signal.VerifySignature(pubKey); err != nil {
			recon.ReconciliationState = ReconciliationRejected
			recon.ReconciliationReason = fmt.Sprintf("signal signature verification failed: %v", err)
			if err := e.persistReconciliation(ctx, recon); err != nil {
				return recon, err
			}
			return recon, nil
		}
	}

	// 3. Handle explicit failure from remote
	if signal.RemoteSignalState == SignalClaimedFailure {
		recon.ReconciliationState = ReconciliationRejected
		recon.ReconciliationReason = "external agent reported execution failure"
		if err := e.persistReconciliation(ctx, recon); err != nil {
			return recon, err
		}
		return recon, nil
	}

	// 4. Handle non-terminal progress signal
	if signal.RemoteSignalState == SignalClaimedProgress {
		recon.ReconciliationState = ReconciliationOutcomeUnknown
		recon.ReconciliationReason = "external execution in progress; non-terminal state"
		if err := e.persistReconciliation(ctx, recon); err != nil {
			return recon, err
		}
		return recon, nil
	}

	// 5. For CLAIMED_SUCCESS, verify evidence artifacts
	if signal.RemoteSignalState != SignalClaimedSuccess {
		recon.ReconciliationState = ReconciliationRejected
		recon.ReconciliationReason = fmt.Sprintf("unsupported external signal state %s", signal.RemoteSignalState)
		if err := e.persistReconciliation(ctx, recon); err != nil {
			return recon, err
		}
		return recon, nil
	}

	if len(signal.EvidenceIDs) == 0 {
		recon.ReconciliationState = ReconciliationOutcomeUnknown
		recon.ReconciliationReason = "claimed success provided no supporting evidence artifacts"
		if err := e.persistReconciliation(ctx, recon); err != nil {
			return recon, err
		}
		return recon, nil
	}

	for _, evID := range signal.EvidenceIDs {
		lineage, err := e.GetLineage(ctx, evID)
		if err != nil {
			if errors.Is(err, ErrEvidenceNotFound) {
				recon.ReconciliationState = ReconciliationOutcomeUnknown
				recon.ReconciliationReason = fmt.Sprintf("evidence artifact %s not yet ingested or resolved", evID)
				if err := e.persistReconciliation(ctx, recon); err != nil {
					return recon, err
				}
				return recon, nil
			}
			return ReconciliationRecord{}, err
		}

		// Ensure lineage producer matches signal producer
		if lineage.ProducerPrincipalID != signal.ProducerPrincipalID {
			recon.ReconciliationState = ReconciliationRejected
			recon.ReconciliationReason = fmt.Sprintf("evidence %s producer %s differs from signal producer %s",
				evID, lineage.ProducerPrincipalID, signal.ProducerPrincipalID)
			if err := e.persistReconciliation(ctx, recon); err != nil {
				return recon, err
			}
			return recon, nil
		}

		// Ensure verifier is neither producer nor witness
		if verifierID != "" {
			if strings.EqualFold(string(verifierID), string(lineage.ProducerPrincipalID)) {
				recon.ReconciliationState = ReconciliationRejected
				recon.ReconciliationReason = fmt.Sprintf("verifier cannot be the evidence producer %s", lineage.ProducerPrincipalID)
				if err := e.persistReconciliation(ctx, recon); err != nil {
					return recon, err
				}
				return recon, nil
			}
			for _, w := range lineage.Witnesses {
				if strings.EqualFold(string(verifierID), string(w)) {
					recon.ReconciliationState = ReconciliationRejected
					recon.ReconciliationReason = fmt.Sprintf("verifier cannot be witness %s", w)
					if err := e.persistReconciliation(ctx, recon); err != nil {
						return recon, err
					}
					return recon, nil
				}
			}
		}

		// Verify signature against resolver
		pubKey, err := e.resolver.ResolvePublicKey(ctx, lineage.ProducerPrincipalID)
		if err != nil {
			recon.ReconciliationState = ReconciliationRejected
			recon.ReconciliationReason = fmt.Sprintf("failed resolving public key for producer %s: %v", lineage.ProducerPrincipalID, err)
			if err := e.persistReconciliation(ctx, recon); err != nil {
				return recon, err
			}
			return recon, nil
		}
		if err := lineage.VerifySignature(pubKey); err != nil {
			recon.ReconciliationState = ReconciliationRejected
			recon.ReconciliationReason = fmt.Sprintf("evidence %s signature verification failed: %v", evID, err)
			if err := e.persistReconciliation(ctx, recon); err != nil {
				return recon, err
			}
			return recon, nil
		}

		// Trust & Corroboration verification
		if err := lineage.MeetsRequirements(e.policy.MinimumTrustTier, e.policy.MinimumCorroborationLevel, e.policy.RequireIndependentWitness); err != nil {
			recon.ReconciliationState = ReconciliationOutcomeUnknown
			recon.ReconciliationReason = fmt.Sprintf("evidence %s uncorroborated: %v", evID, err)
			if err := e.persistReconciliation(ctx, recon); err != nil {
				return recon, err
			}
			return recon, nil
		}
	}

	// All evidence passed cryptographic verification, provenance checks, and corroboration thresholds
	recon.ReconciliationState = ReconciliationVerifiedAcceptance
	recon.ReconciliationReason = "all foreign evidence verified and independently corroborated"
	recon.BlindRetryPrevented = false

	if err := e.persistReconciliation(ctx, recon); err != nil {
		return ReconciliationRecord{}, err
	}
	return recon, nil
}

// PreventBlindRetry checks if a task is currently in an unverified or uncertain state
// where blind automatic retry must be blocked.
func (e *IndependentAcceptanceEngine) PreventBlindRetry(ctx context.Context, taskID domain.ID) (bool, error) {
	if e == nil || e.store == nil {
		return false, errors.New("acceptance engine is not configured")
	}

	// Fetch latest reconciliation record for this task by insertion order
	var latestState string
	var blindRetry int
	query := `
		SELECT reconciliation_state, blind_retry_prevented
		FROM external_task_reconciliations
		WHERE task_id = ?
		ORDER BY rowid DESC
		LIMIT 1;
	`
	err := e.store.DB().QueryRowContext(ctx, query, taskID).Scan(&latestState, &blindRetry)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	// If latest state was OUTCOME_UNKNOWN and blind retry is prevented
	return latestState == string(ReconciliationOutcomeUnknown) && blindRetry == 1, nil
}

func (e *IndependentAcceptanceEngine) persistReconciliation(ctx context.Context, r ReconciliationRecord) error {
	evidenceJSON, _ := json.Marshal(r.EvidenceIDs)
	blindRetryInt := 0
	if r.BlindRetryPrevented {
		blindRetryInt = 1
	}

	query := `
		INSERT INTO external_task_reconciliations (
			reconciliation_id, task_id, attempt_id, origin_collective_id, producer_principal_id,
			external_signal_state, reconciliation_state, reconciliation_reason,
			evidence_ids_json, blind_retry_prevented, evaluated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
	`
	_, err := e.store.DB().ExecContext(ctx, query,
		r.ReconciliationID,
		r.TaskID,
		r.AttemptID,
		string(r.OriginCollectiveID),
		string(r.ProducerPrincipalID),
		string(r.ExternalSignalState),
		string(r.ReconciliationState),
		r.ReconciliationReason,
		string(evidenceJSON),
		blindRetryInt,
		domain.FormatTime(r.EvaluatedAt),
	)
	return err
}
