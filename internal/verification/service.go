package verification

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type CompletionManifest struct {
	EvidenceIDs []domain.ID
}

type CompletionRecord struct {
	ID           domain.ID
	TaskID       domain.ID
	AttemptID    domain.ID
	ManifestHash string
	EvidenceIDs  []domain.ID
	CompletedAt  time.Time
}

type AcceptanceRequest struct {
	VerifierID   domain.ID
	VerifierType string
	CriteriaMet  bool
	EvidenceIDs  []domain.ID
}

type AcceptanceRecord struct {
	ID           domain.ID
	TaskID       domain.ID
	AttemptID    domain.ID
	VerifierID   domain.ID
	VerifierType string
	CriteriaMet  bool
	EvidenceIDs  []domain.ID
	CreatedAt    time.Time
}

type Service struct {
	store     *state.Store
	clock     clock.Clock
	execution *execution.Service
}

func New(store *state.Store, clk clock.Clock, executionService *execution.Service) *Service {
	return &Service{store: store, clock: clk, execution: executionService}
}

func (s *Service) CompleteAttempt(ctx context.Context, attemptID domain.ID, manifest CompletionManifest) (CompletionRecord, error) {
	if err := s.configured(); err != nil {
		return CompletionRecord{}, err
	}
	if attemptID == "" {
		return CompletionRecord{}, errors.New("attempt id is required")
	}
	evidenceIDs := normalizeIDs(manifest.EvidenceIDs)
	if len(evidenceIDs) == 0 {
		return CompletionRecord{}, errors.New("completion manifest requires evidence")
	}
	manifestHash, manifestJSON, err := hashManifest(evidenceIDs)
	if err != nil {
		return CompletionRecord{}, err
	}
	now := s.clock.Now().UTC()
	record := CompletionRecord{
		ID: domain.NewID("completion"), AttemptID: attemptID, ManifestHash: manifestHash,
		EvidenceIDs: evidenceIDs, CompletedAt: now,
	}

	err = s.execution.WithGuardedAttempt(ctx, attemptID, []domain.TaskState{domain.TaskExecuting}, func(tx *sql.Tx, guarded execution.GuardedAttempt) error {
		if err := requireEvidence(ctx, tx, evidenceIDs); err != nil {
			return err
		}
		record.TaskID = guarded.TaskID
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO attempt_completion_records(completion_id, attempt_id, task_id, manifest_hash, manifest_json, completed_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			record.ID, attemptID, guarded.TaskID, manifestHash, string(manifestJSON), formatTime(now),
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE attempts SET state = ?, lease_state = ?, completed_at = ? WHERE attempt_id = ?`,
			domain.AttemptCompleted, domain.LeaseRevoked, formatTime(now), attemptID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET state = ?, updated_at = ? WHERE task_id = ?`,
			domain.TaskAwaitingVerification, formatTime(now), guarded.TaskID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO verification_work(verification_id, task_id, attempt_id, completion_id, state, created_at, updated_at)
			 VALUES (?, ?, ?, ?, 'PENDING', ?, ?)`,
			domain.NewID("verification"), guarded.TaskID, attemptID, record.ID, formatTime(now), formatTime(now),
		); err != nil {
			return err
		}
		return appendExecutionEvent(ctx, tx, guarded.TaskID, attemptID, "ATTEMPT_COMPLETED", map[string]any{"manifest_hash": manifestHash}, now)
	})
	if err != nil {
		return CompletionRecord{}, err
	}
	return record, nil
}

func (s *Service) AcceptTask(ctx context.Context, taskID domain.ID, request AcceptanceRequest) (AcceptanceRecord, error) {
	if err := s.configured(); err != nil {
		return AcceptanceRecord{}, err
	}
	request.VerifierType = strings.TrimSpace(request.VerifierType)
	if taskID == "" || request.VerifierID == "" || request.VerifierType == "" {
		return AcceptanceRecord{}, errors.New("task, verifier id, and verifier type are required")
	}
	if !request.CriteriaMet {
		return AcceptanceRecord{}, errors.New("AcceptTask requires satisfied acceptance criteria")
	}
	evidenceIDs := normalizeIDs(request.EvidenceIDs)
	if len(evidenceIDs) == 0 {
		return AcceptanceRecord{}, errors.New("acceptance requires evidence")
	}
	now := s.clock.Now().UTC()
	record := AcceptanceRecord{
		ID: domain.NewID("acceptance"), TaskID: taskID, VerifierID: request.VerifierID,
		VerifierType: request.VerifierType, CriteriaMet: true, EvidenceIDs: evidenceIDs, CreatedAt: now,
	}
	criteriaJSON, _ := json.Marshal(map[string]bool{"met": true})
	evidenceJSON, _ := json.Marshal(evidenceIDs)

	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var taskState domain.TaskState
		var currentAttempt sql.NullString
		if err := tx.QueryRowContext(ctx,
			`SELECT state, current_attempt_id FROM tasks WHERE task_id = ?`, taskID,
		).Scan(&taskState, &currentAttempt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("task %q not found", taskID)
			}
			return err
		}
		if taskState != domain.TaskAwaitingVerification || !currentAttempt.Valid {
			return fmt.Errorf("task %q is not awaiting verification", taskID)
		}
		record.AttemptID = domain.ID(currentAttempt.String)
		var completionID string
		if err := tx.QueryRowContext(ctx,
			`SELECT completion_id FROM attempt_completion_records WHERE task_id = ? AND attempt_id = ?`,
			taskID, record.AttemptID,
		).Scan(&completionID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("durable completion record is required before acceptance")
			}
			return err
		}
		if err := requireEvidence(ctx, tx, evidenceIDs); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO acceptance_records(acceptance_id, task_id, attempt_id, verifier_id, verifier_type, criteria_result_json, evidence_ids_json, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			record.ID, taskID, record.AttemptID, record.VerifierID, record.VerifierType, string(criteriaJSON), string(evidenceJSON), formatTime(now),
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE verification_work SET state = 'ACCEPTED', updated_at = ? WHERE task_id = ? AND attempt_id = ? AND state = 'PENDING'`,
			formatTime(now), taskID, record.AttemptID,
		); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE tasks SET state = ?, updated_at = ? WHERE task_id = ? AND state = ?`,
			domain.TaskSucceeded, formatTime(now), taskID, domain.TaskAwaitingVerification,
		)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return errors.New("task acceptance lost state race")
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks
			SET state = ?, updated_at = ?
			WHERE state = ?
			  AND EXISTS (
			      SELECT 1 FROM task_dependencies d
			      WHERE d.task_id = tasks.task_id AND d.depends_on_task_id = ?
			  )
			  AND NOT EXISTS (
			      SELECT 1
			      FROM task_dependencies d
			      JOIN tasks dependency ON dependency.task_id = d.depends_on_task_id
			      WHERE d.task_id = tasks.task_id AND dependency.state <> ?
			  )`,
			domain.TaskEligible, formatTime(now), domain.TaskCreated, taskID, domain.TaskSucceeded,
		); err != nil {
			return err
		}
		return appendExecutionEvent(ctx, tx, taskID, record.AttemptID, "TASK_ACCEPTED", map[string]any{"acceptance_id": record.ID}, now)
	})
	if err != nil {
		return AcceptanceRecord{}, err
	}
	return record, nil
}

// FindAcceptance returns the durable acceptance for a task, if one exists.
func (s *Service) FindAcceptance(ctx context.Context, taskID domain.ID) (AcceptanceRecord, bool, error) {
	if s == nil || s.store == nil {
		return AcceptanceRecord{}, false, errors.New("verification service is not configured")
	}
	if taskID == "" {
		return AcceptanceRecord{}, false, errors.New("task ID is required")
	}
	var record AcceptanceRecord
	var criteriaJSON, evidenceJSON, createdAt string
	err := s.store.DB().QueryRowContext(ctx,
		`SELECT acceptance_id, task_id, attempt_id, verifier_id, verifier_type, criteria_result_json, evidence_ids_json, created_at
		 FROM acceptance_records WHERE task_id = ?`, taskID,
	).Scan(&record.ID, &record.TaskID, &record.AttemptID, &record.VerifierID, &record.VerifierType,
		&criteriaJSON, &evidenceJSON, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AcceptanceRecord{}, false, nil
	}
	if err != nil {
		return AcceptanceRecord{}, false, fmt.Errorf("read task acceptance: %w", err)
	}
	var criteria struct {
		Met bool `json:"met"`
	}
	if err := json.Unmarshal([]byte(criteriaJSON), &criteria); err != nil {
		return AcceptanceRecord{}, false, fmt.Errorf("decode acceptance criteria: %w", err)
	}
	if err := json.Unmarshal([]byte(evidenceJSON), &record.EvidenceIDs); err != nil {
		return AcceptanceRecord{}, false, fmt.Errorf("decode acceptance evidence: %w", err)
	}
	record.CriteriaMet = criteria.Met
	record.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return AcceptanceRecord{}, false, fmt.Errorf("parse acceptance time: %w", err)
	}
	return record, true, nil
}

func requireEvidence(ctx context.Context, tx *sql.Tx, ids []domain.ID) error {
	for _, id := range ids {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM evidence_objects WHERE evidence_id = ?`, id).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("evidence %q not found", id)
			}
			return err
		}
	}
	return nil
}

func hashManifest(evidenceIDs []domain.ID) (string, []byte, error) {
	body, err := json.Marshal(struct {
		Version     int         `json:"version"`
		EvidenceIDs []domain.ID `json:"evidence_ids"`
	}{Version: 1, EvidenceIDs: evidenceIDs})
	if err != nil {
		return "", nil, err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), body, nil
}

func normalizeIDs(ids []domain.ID) []domain.ID {
	set := make(map[domain.ID]struct{}, len(ids))
	for _, id := range ids {
		if id != "" {
			set[id] = struct{}{}
		}
	}
	out := make([]domain.ID, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func appendExecutionEvent(ctx context.Context, tx *sql.Tx, taskID, attemptID domain.ID, eventType string, details map[string]any, now time.Time) error {
	encoded, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO execution_events(event_id, task_id, attempt_id, event_type, details_json, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		domain.NewID("event"), taskID, attemptID, eventType, string(encoded), formatTime(now),
	)
	return err
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func (s *Service) configured() error {
	if s == nil || s.store == nil || s.clock == nil || s.execution == nil {
		return errors.New("verification service is not configured")
	}
	return nil
}
