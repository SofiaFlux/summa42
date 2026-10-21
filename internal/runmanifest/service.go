package runmanifest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/teb"
)

const manifestVersion = 1

var (
	ErrNotFound   = errors.New("run manifest not found")
	ErrIncomplete = errors.New("run manifest provenance is incomplete")
)

type BuildMetadata struct {
	RuntimeVersion string `json:"runtime_version,omitempty"`
	RuntimeCommit  string `json:"runtime_commit,omitempty"`
}

type PolicySnapshot struct {
	PolicySetID            domain.ID `json:"policy_set_id,omitempty"`
	PolicySetHash          string    `json:"policy_set_hash,omitempty"`
	PolicyCapabilitiesHash string    `json:"policy_capabilities_hash,omitempty"`
}

type StaticContext struct {
	Build      BuildMetadata
	Policy     PolicySnapshot
	TEBProfile teb.Profile
}

type TEBSnapshot struct {
	Name       string                  `json:"name"`
	Level      domain.EnforcementLevel `json:"level"`
	Guarantees []string                `json:"guarantees"`
}

type CapabilitySnapshot struct {
	SemanticName        string                  `json:"semantic_name"`
	CapabilityID        domain.ID               `json:"capability_id,omitempty"`
	SemanticVersion     string                  `json:"semantic_version,omitempty"`
	Provider            string                  `json:"provider,omitempty"`
	AccessContext       string                  `json:"access_context,omitempty"`
	MinimumEnforcement  domain.EnforcementLevel `json:"minimum_enforcement,omitempty"`
	AssessmentID        domain.ID               `json:"assessment_id,omitempty"`
	ObservedEnforcement domain.EnforcementLevel `json:"observed_enforcement,omitempty"`
	Health              string                  `json:"health,omitempty"`
	Available           *bool                   `json:"available,omitempty"`
	AssessedAt          *time.Time              `json:"assessed_at,omitempty"`
}

type EvidenceRef struct {
	ID          domain.ID `json:"id"`
	ContentHash string    `json:"content_hash"`
}

// Manifest is a descriptive lease-time snapshot. In the current MVC there is
// no separate materialized capability-session projection, so the executor-visible
// set is derived from Task.RequiredCapabilities at lease time. Missing IDs remain
// empty rather than being inferred.
type Manifest struct {
	Version               int                  `json:"version"`
	TaskID                domain.ID            `json:"task_id"`
	TaskObjective         string               `json:"task_objective"`
	TaskPayloadHash       string               `json:"task_payload_hash"`
	AttemptID             domain.ID            `json:"attempt_id"`
	FenceGeneration       int64                `json:"fence_generation"`
	ExecutorKind          string               `json:"executor_kind"`
	ExecutorVersion       string               `json:"executor_version,omitempty"`
	ExecutorModel         string               `json:"executor_model,omitempty"`
	Runtime               BuildMetadata        `json:"runtime"`
	Policy                PolicySnapshot       `json:"policy"`
	TEB                   TEBSnapshot          `json:"teb"`
	VisibleCapabilities   []CapabilitySnapshot `json:"visible_capabilities"`
	InputEvidence         []EvidenceRef        `json:"input_evidence"`
	ResourceEnvelopeID    domain.ID            `json:"resource_envelope_id"`
	ContextProjectionHash string               `json:"context_projection_hash,omitempty"`
	StartedAt             time.Time            `json:"started_at"`
}

type Record struct {
	AttemptID    domain.ID
	TaskID       domain.ID
	ManifestHash string
	ManifestJSON string
	Manifest     Manifest
	CreatedAt    time.Time
}

type Provenance struct {
	Run                 Record
	OutputEvidence      []EvidenceRef
	SettledExternalCost *int64
}

type Service struct {
	store  *state.Store
	static StaticContext
}

func New(store *state.Store, static StaticContext) *Service {
	static.Build.RuntimeVersion = strings.TrimSpace(static.Build.RuntimeVersion)
	static.Build.RuntimeCommit = strings.TrimSpace(static.Build.RuntimeCommit)
	static.Policy.PolicySetID = domain.ID(strings.TrimSpace(string(static.Policy.PolicySetID)))
	static.Policy.PolicySetHash = strings.TrimSpace(static.Policy.PolicySetHash)
	static.Policy.PolicyCapabilitiesHash = strings.TrimSpace(static.Policy.PolicyCapabilitiesHash)
	static.TEBProfile.Name = strings.TrimSpace(static.TEBProfile.Name)
	return &Service{store: store, static: static}
}

// RecordAttemptStartInTx snapshots descriptive execution provenance in the same
// transaction that creates the Attempt. The record is descriptive only: it
// never grants authority, extends a lease, or changes Task/Attempt semantics.
func (s *Service) RecordAttemptStartInTx(ctx context.Context, tx *sql.Tx, attempt domain.Attempt, task domain.Task) error {
	if err := s.configured(); err != nil {
		return err
	}
	if tx == nil {
		return errors.New("attempt run manifest requires transaction")
	}
	if attempt.ID == "" || attempt.TaskID == "" || attempt.TaskID != task.ID || attempt.FenceGeneration <= 0 {
		return errors.New("attempt run manifest requires a valid Attempt and matching Task")
	}
	if strings.TrimSpace(attempt.ExecutorKind) == "" || attempt.StartedAt.IsZero() {
		return errors.New("attempt run manifest requires executor kind and start time")
	}
	if err := s.static.TEBProfile.Validate(); err != nil {
		return fmt.Errorf("invalid TEB snapshot: %w", err)
	}
	if !validPolicySnapshot(s.static.Policy) {
		return errors.New("attempt run manifest policy provenance must be either complete or absent")
	}

	capabilities, err := loadCapabilitySnapshots(ctx, tx, task.RequiredCapabilities)
	if err != nil {
		return err
	}
	guarantees := make([]string, 0, len(s.static.TEBProfile.Guarantees))
	for guarantee, enabled := range s.static.TEBProfile.Guarantees {
		if enabled {
			guarantees = append(guarantees, string(guarantee))
		}
	}
	sort.Strings(guarantees)
	payloadDigest := sha256.Sum256(task.PayloadJSON)

	manifest := Manifest{
		Version:         manifestVersion,
		TaskID:          task.ID,
		TaskObjective:   task.Objective,
		TaskPayloadHash: hex.EncodeToString(payloadDigest[:]),
		AttemptID:       attempt.ID,
		FenceGeneration: attempt.FenceGeneration,
		ExecutorKind:    strings.TrimSpace(attempt.ExecutorKind),
		Runtime:         s.static.Build,
		Policy:          s.static.Policy,
		TEB: TEBSnapshot{
			Name:       s.static.TEBProfile.Name,
			Level:      s.static.TEBProfile.Level,
			Guarantees: guarantees,
		},
		VisibleCapabilities: capabilities,
		InputEvidence:       []EvidenceRef{},
		ResourceEnvelopeID:  task.ResourceEnvelopeID,
		StartedAt:           attempt.StartedAt.UTC(),
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode attempt run manifest: %w", err)
	}
	digest := sha256.Sum256(encoded)
	hash := hex.EncodeToString(digest[:])

	_, err = tx.ExecContext(ctx,
		`INSERT INTO attempt_run_manifests(attempt_id, task_id, manifest_hash, manifest_json, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		attempt.ID, task.ID, hash, string(encoded), formatTime(attempt.StartedAt),
	)
	if err != nil {
		return fmt.Errorf("persist attempt run manifest: %w", err)
	}
	return nil
}

func (s *Service) Manifest(ctx context.Context, attemptID domain.ID) (Record, error) {
	if err := s.configured(); err != nil {
		return Record{}, err
	}
	attemptID = domain.ID(strings.TrimSpace(string(attemptID)))
	if attemptID == "" {
		return Record{}, errors.New("attempt id is required")
	}
	var record Record
	var createdAt string
	err := s.store.DB().QueryRowContext(ctx,
		`SELECT attempt_id, task_id, manifest_hash, manifest_json, created_at
		 FROM attempt_run_manifests WHERE attempt_id = ?`, attemptID,
	).Scan(&record.AttemptID, &record.TaskID, &record.ManifestHash, &record.ManifestJSON, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, fmt.Errorf("%w: attempt %s has no run manifest", ErrNotFound, attemptID)
	}
	if err != nil {
		return Record{}, err
	}
	record.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return Record{}, fmt.Errorf("parse attempt run manifest created_at: %w", err)
	}
	digest := sha256.Sum256([]byte(record.ManifestJSON))
	if hex.EncodeToString(digest[:]) != record.ManifestHash {
		return Record{}, errors.New("attempt run manifest hash mismatch")
	}
	decoder := json.NewDecoder(strings.NewReader(record.ManifestJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record.Manifest); err != nil {
		return Record{}, fmt.Errorf("decode attempt run manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Record{}, errors.New("attempt run manifest contains trailing JSON")
		}
		return Record{}, fmt.Errorf("decode attempt run manifest trailer: %w", err)
	}
	if record.Manifest.Version != manifestVersion ||
		record.Manifest.AttemptID != record.AttemptID ||
		record.Manifest.TaskID != record.TaskID {
		return Record{}, errors.New("attempt run manifest identity mismatch")
	}
	return record, nil
}

func (s *Service) Provenance(ctx context.Context, attemptID domain.ID) (Provenance, error) {
	record, err := s.Manifest(ctx, attemptID)
	if err != nil {
		return Provenance{}, err
	}
	evidenceIDs, err := s.outputEvidenceIDs(ctx, attemptID)
	if err != nil {
		return Provenance{}, err
	}
	outputEvidence := make([]EvidenceRef, 0, len(evidenceIDs))
	for _, evidenceID := range evidenceIDs {
		var contentHash string
		if err := s.store.DB().QueryRowContext(ctx,
			`SELECT content_hash FROM evidence_objects WHERE evidence_id = ?`, evidenceID,
		).Scan(&contentHash); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return Provenance{}, fmt.Errorf("output evidence %s is not present in canonical evidence", evidenceID)
			}
			return Provenance{}, err
		}
		outputEvidence = append(outputEvidence, EvidenceRef{ID: evidenceID, ContentHash: contentHash})
	}

	var settledCount int
	var settledTotal int64
	if err := s.store.DB().QueryRowContext(ctx, `
		SELECT count(*), COALESCE(SUM(r.settled_amount), 0)
		FROM external_operations o
		JOIN resource_reservations r ON r.reservation_id = o.reservation_id
		WHERE o.attempt_id = ? AND r.state = 'SETTLED' AND r.settled_amount IS NOT NULL`, attemptID,
	).Scan(&settledCount, &settledTotal); err != nil {
		return Provenance{}, err
	}
	var settled *int64
	if settledCount > 0 {
		value := settledTotal
		settled = &value
	}
	return Provenance{Run: record, OutputEvidence: outputEvidence, SettledExternalCost: settled}, nil
}

func (s *Service) outputEvidenceIDs(ctx context.Context, attemptID domain.ID) ([]domain.ID, error) {
	set := map[domain.ID]struct{}{}
	completionFound := false
	var completionJSON string
	err := s.store.DB().QueryRowContext(ctx,
		`SELECT manifest_json FROM attempt_completion_records WHERE attempt_id = ?`, attemptID,
	).Scan(&completionJSON)
	if err == nil {
		completionFound = true
		var completion struct {
			EvidenceIDs []domain.ID `json:"evidence_ids"`
		}
		if err := json.Unmarshal([]byte(completionJSON), &completion); err != nil {
			return nil, fmt.Errorf("decode completion evidence: %w", err)
		}
		for _, id := range completion.EvidenceIDs {
			if id != "" {
				set[id] = struct{}{}
			}
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	rows, err := s.store.DB().QueryContext(ctx,
		`SELECT evidence_ids_json FROM attempt_failures WHERE attempt_id = ? ORDER BY created_at, failure_id`, attemptID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return nil, err
		}
		var ids []domain.ID
		if err := json.Unmarshal([]byte(raw), &ids); err != nil {
			rows.Close()
			return nil, fmt.Errorf("decode attempt failure evidence: %w", err)
		}
		for _, id := range ids {
			if id != "" {
				set[id] = struct{}{}
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if !completionFound && len(set) == 0 {
		return nil, ErrIncomplete
	}
	result := make([]domain.ID, 0, len(set))
	for id := range set {
		result = append(result, id)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

func loadCapabilitySnapshots(ctx context.Context, tx *sql.Tx, semanticNames []string) ([]CapabilitySnapshot, error) {
	names := normalizeStrings(semanticNames)
	sort.Strings(names)
	result := make([]CapabilitySnapshot, 0, len(names))
	for _, semanticName := range names {
		snapshot := CapabilitySnapshot{SemanticName: semanticName}
		var (
			capabilityID, semanticVersion, provider, accessContext, minEnforcement string
			assessmentID, observedEnforcement, health, assessedAt                  sql.NullString
			available                                                              sql.NullInt64
		)
		err := tx.QueryRowContext(ctx, `
			SELECT d.capability_id, d.semantic_version, d.provider, d.access_context, d.minimum_enforcement,
			       a.assessment_id, a.enforcement_level, a.health, a.available, a.assessed_at
			FROM capability_definitions d
			LEFT JOIN capability_assessments a ON a.assessment_id = (
				SELECT a2.assessment_id
				FROM capability_assessments a2
					WHERE a2.capability_id = d.capability_id
					  AND a2.provider = d.provider
					  AND a2.access_context = d.access_context
				ORDER BY a2.assessed_at DESC, a2.rowid DESC
				LIMIT 1
			)
			WHERE d.semantic_name = ? AND d.active = 1`, semanticName,
		).Scan(
			&capabilityID, &semanticVersion, &provider, &accessContext, &minEnforcement,
			&assessmentID, &observedEnforcement, &health, &available, &assessedAt,
		)
		if errors.Is(err, sql.ErrNoRows) {
			result = append(result, snapshot)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("load capability snapshot %s: %w", semanticName, err)
		}
		snapshot.CapabilityID = domain.ID(capabilityID)
		snapshot.SemanticVersion = semanticVersion
		snapshot.Provider = provider
		snapshot.AccessContext = accessContext
		snapshot.MinimumEnforcement = domain.EnforcementLevel(minEnforcement)
		if assessmentID.Valid {
			snapshot.AssessmentID = domain.ID(assessmentID.String)
			snapshot.ObservedEnforcement = domain.EnforcementLevel(observedEnforcement.String)
			snapshot.Health = health.String
			isAvailable := available.Valid && available.Int64 == 1
			snapshot.Available = &isAvailable
			if assessedAt.Valid {
				parsed, err := time.Parse(time.RFC3339Nano, assessedAt.String)
				if err != nil {
					return nil, fmt.Errorf("parse capability assessment time: %w", err)
				}
				snapshot.AssessedAt = &parsed
			}
		}
		result = append(result, snapshot)
	}
	return result, nil
}

func validPolicySnapshot(snapshot PolicySnapshot) bool {
	count := 0
	if snapshot.PolicySetID != "" {
		count++
	}
	if snapshot.PolicySetHash != "" {
		count++
	}
	if snapshot.PolicyCapabilitiesHash != "" {
		count++
	}
	return count == 0 || count == 3
}

func normalizeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func formatTime(value time.Time) string {
	return domain.FormatTime(value)
}

func (s *Service) configured() error {
	if s == nil || s.store == nil || s.store.DB() == nil {
		return errors.New("attempt run manifest service is not configured")
	}
	return nil
}
