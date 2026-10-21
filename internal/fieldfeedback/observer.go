package fieldfeedback

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type ObservationInput struct {
	TaskID          domain.ID
	AttemptID       domain.ID
	OperationID     domain.ID
	Category        string
	BasisClass      string
	SourceKind      string
	SummaryLocal    string
	Metrics         map[string]any
	RuntimeVersion  string
	ExecutorKind    string
	ExecutorVersion string
	Enforcement     domain.EnforcementLevel
	EvidenceIDs     []domain.ID
}

type DetectorInput struct {
	TaskID domain.ID
}

type Observer struct {
	store        *state.Store
	clock        clock.Clock
	collectiveID domain.ID
}

func NewObserver(store *state.Store, clk clock.Clock, collectiveID domain.ID) *Observer {
	return &Observer{store: store, clock: clk, collectiveID: domain.ID(strings.TrimSpace(string(collectiveID)))}
}

func (s *Observer) Record(ctx context.Context, input ObservationInput) (domain.FieldObservation, error) {
	return s.recordWithDetectorKey(ctx, input, "")
}

func (s *Observer) Observation(ctx context.Context, id domain.ID) (domain.FieldObservation, error) {
	if err := s.configured(); err != nil {
		return domain.FieldObservation{}, err
	}
	id = domain.ID(strings.TrimSpace(string(id)))
	if id == "" {
		return domain.FieldObservation{}, errors.New("observation id is required")
	}
	return loadObservation(ctx, s.store.DB(), id)
}

func (s *Observer) ForTask(ctx context.Context, taskID domain.ID) ([]domain.FieldObservation, error) {
	if err := s.configured(); err != nil {
		return nil, err
	}
	taskID = domain.ID(strings.TrimSpace(string(taskID)))
	if taskID == "" {
		return nil, errors.New("task id is required")
	}
	rows, err := s.store.DB().QueryContext(ctx,
		"SELECT observation_id FROM field_observations WHERE task_id = ? ORDER BY created_at, observation_id", taskID)
	if err != nil {
		return nil, err
	}
	var ids []domain.ID
	for rows.Next() {
		var id domain.ID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make([]domain.FieldObservation, 0, len(ids))
	for _, id := range ids {
		observation, err := s.Observation(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, observation)
	}
	return out, nil
}

func (s *Observer) recordWithDetectorKey(ctx context.Context, input ObservationInput, detectorKey string) (domain.FieldObservation, error) {
	if err := s.configured(); err != nil {
		return domain.FieldObservation{}, err
	}
	normalized, metricsJSON, evidenceIDs, err := s.validateInput(ctx, input)
	if err != nil {
		return domain.FieldObservation{}, err
	}
	detectorKey = strings.TrimSpace(detectorKey)
	now := s.clock.Now().UTC()
	var result domain.FieldObservation
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if detectorKey != "" {
			existing, err := loadObservationByDetectorKey(ctx, tx, detectorKey)
			if err == nil {
				result = existing
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}

		normalized, err = validateReferencesTx(ctx, tx, normalized, evidenceIDs)
		if err != nil {
			return err
		}
		result = domain.FieldObservation{
			ID: domain.NewID("observation"), CollectiveID: s.collectiveID,
			TaskID: normalized.TaskID, AttemptID: normalized.AttemptID, OperationID: normalized.OperationID,
			Category: normalized.Category, BasisClass: normalized.BasisClass, SourceKind: normalized.SourceKind,
			SummaryLocal: normalized.SummaryLocal, MetricsJSON: metricsJSON,
			RuntimeVersion: normalized.RuntimeVersion, ExecutorKind: normalized.ExecutorKind,
			ExecutorVersion: normalized.ExecutorVersion, Enforcement: normalized.Enforcement, CreatedAt: now,
		}
		var detectorValue any
		if detectorKey != "" {
			detectorValue = detectorKey
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO field_observations("+
				"observation_id, collective_id, task_id, attempt_id, operation_id, detector_key, "+
				"category, basis_class, source_kind, summary_local, metrics_json, runtime_version, "+
				"executor_kind, executor_version, enforcement, created_at"+
				") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			result.ID, result.CollectiveID, nullableID(result.TaskID), nullableID(result.AttemptID),
			nullableID(result.OperationID), detectorValue, result.Category, result.BasisClass, result.SourceKind,
			result.SummaryLocal, result.MetricsJSON, result.RuntimeVersion, result.ExecutorKind,
			result.ExecutorVersion, result.Enforcement, formatTime(now),
		); err != nil {
			if detectorKey != "" && strings.Contains(strings.ToLower(err.Error()), "unique") {
				existing, loadErr := loadObservationByDetectorKey(ctx, tx, detectorKey)
				if loadErr == nil {
					result = existing
					return nil
				}
			}
			return fmt.Errorf("insert field observation: %w", err)
		}
		for _, evidenceID := range evidenceIDs {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO field_observation_evidence(observation_id, evidence_id, linked_at) VALUES (?, ?, ?)",
				result.ID, evidenceID, formatTime(now)); err != nil {
				return fmt.Errorf("link observation evidence: %w", err)
			}
		}
		payload, err := json.Marshal(map[string]any{
			"observation_id": result.ID,
			"category": result.Category,
			"task_id": result.TaskID,
			"attempt_id": result.AttemptID,
			"operation_id": result.OperationID,
			"source_kind": result.SourceKind,
		})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO audit_events(audit_id, kind, actor_id, subject_id, payload_json, created_at) VALUES (?, ?, ?, ?, ?, ?)",
			domain.NewID("audit"), "FIELD_OBSERVATION_RECORDED", s.collectiveID, result.ID, string(payload), formatTime(now),
		); err != nil {
			return fmt.Errorf("append observation audit event: %w", err)
		}
		return nil
	})
	return result, err
}

func (s *Observer) validateInput(ctx context.Context, input ObservationInput) (ObservationInput, string, []domain.ID, error) {
	input.TaskID = domain.ID(strings.TrimSpace(string(input.TaskID)))
	input.AttemptID = domain.ID(strings.TrimSpace(string(input.AttemptID)))
	input.OperationID = domain.ID(strings.TrimSpace(string(input.OperationID)))
	input.Category = strings.TrimSpace(input.Category)
	input.BasisClass = strings.TrimSpace(input.BasisClass)
	input.SourceKind = strings.TrimSpace(input.SourceKind)
	input.SummaryLocal = strings.TrimSpace(input.SummaryLocal)
	input.RuntimeVersion = strings.TrimSpace(input.RuntimeVersion)
	input.ExecutorKind = strings.TrimSpace(input.ExecutorKind)
	input.ExecutorVersion = strings.TrimSpace(input.ExecutorVersion)
	if input.Category == "" || input.BasisClass == "" || input.SourceKind == "" || input.SummaryLocal == "" {
		return ObservationInput{}, "", nil, errors.New("observation category, basis class, source kind, and local summary are required")
	}
	switch input.Enforcement {
	case domain.EnforcementEnforced, domain.EnforcementPartial, domain.EnforcementUnenforced:
	default:
		return ObservationInput{}, "", nil, fmt.Errorf("invalid observation enforcement %q", input.Enforcement)
	}
	if input.Metrics == nil {
		input.Metrics = map[string]any{}
	}
	metrics, err := json.Marshal(input.Metrics)
	if err != nil {
		return ObservationInput{}, "", nil, fmt.Errorf("encode local observation metrics: %w", err)
	}
	evidenceIDs := cleanIDs(input.EvidenceIDs)
	return input, string(metrics), evidenceIDs, nil
}

func validateReferencesTx(ctx context.Context, tx *sql.Tx, input ObservationInput, evidenceIDs []domain.ID) (ObservationInput, error) {
	if input.OperationID != "" {
		var taskID, attemptID domain.ID
		if err := tx.QueryRowContext(ctx,
			"SELECT task_id, attempt_id FROM external_operations WHERE operation_id = ?", input.OperationID,
		).Scan(&taskID, &attemptID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ObservationInput{}, fmt.Errorf("operation %s not found", input.OperationID)
			}
			return ObservationInput{}, err
		}
		if input.TaskID != "" && input.TaskID != taskID {
			return ObservationInput{}, errors.New("operation task does not match observation task")
		}
		if input.AttemptID != "" && input.AttemptID != attemptID {
			return ObservationInput{}, errors.New("operation attempt does not match observation attempt")
		}
		input.TaskID = taskID
		input.AttemptID = attemptID
	}
	if input.AttemptID != "" {
		var taskID domain.ID
		if err := tx.QueryRowContext(ctx, "SELECT task_id FROM attempts WHERE attempt_id = ?", input.AttemptID).Scan(&taskID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ObservationInput{}, fmt.Errorf("attempt %s not found", input.AttemptID)
			}
			return ObservationInput{}, err
		}
		if input.TaskID != "" && input.TaskID != taskID {
			return ObservationInput{}, errors.New("attempt task does not match observation task")
		}
		input.TaskID = taskID
	}
	if input.TaskID != "" {
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM tasks WHERE task_id = ?", input.TaskID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ObservationInput{}, fmt.Errorf("task %s not found", input.TaskID)
			}
			return ObservationInput{}, err
		}
	}
	for _, evidenceID := range evidenceIDs {
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM evidence_objects WHERE evidence_id = ?", evidenceID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ObservationInput{}, fmt.Errorf("evidence %s not found", evidenceID)
			}
			return ObservationInput{}, err
		}
	}
	return input, nil
}

type observationQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadObservation(ctx context.Context, q observationQuery, id domain.ID) (domain.FieldObservation, error) {
	var out domain.FieldObservation
	var taskID, attemptID, operationID sql.NullString
	var createdAt string
	err := q.QueryRowContext(ctx,
		"SELECT observation_id, collective_id, task_id, attempt_id, operation_id, category, basis_class, source_kind, "+
			"summary_local, metrics_json, runtime_version, executor_kind, executor_version, enforcement, created_at "+
			"FROM field_observations WHERE observation_id = ?", id,
	).Scan(&out.ID, &out.CollectiveID, &taskID, &attemptID, &operationID, &out.Category, &out.BasisClass,
		&out.SourceKind, &out.SummaryLocal, &out.MetricsJSON, &out.RuntimeVersion, &out.ExecutorKind,
		&out.ExecutorVersion, &out.Enforcement, &createdAt)
	if err != nil {
		return domain.FieldObservation{}, err
	}
	out.TaskID = domain.ID(taskID.String)
	out.AttemptID = domain.ID(attemptID.String)
	out.OperationID = domain.ID(operationID.String)
	out.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return domain.FieldObservation{}, fmt.Errorf("parse observation created_at: %w", err)
	}
	return out, nil
}

func loadObservationByDetectorKey(ctx context.Context, q observationQuery, key string) (domain.FieldObservation, error) {
	var id domain.ID
	if err := q.QueryRowContext(ctx,
		"SELECT observation_id FROM field_observations WHERE detector_key = ?", key,
	).Scan(&id); err != nil {
		return domain.FieldObservation{}, err
	}
	return loadObservation(ctx, q, id)
}

func cleanIDs(values []domain.ID) []domain.ID {
	seen := make(map[domain.ID]struct{}, len(values))
	out := make([]domain.ID, 0, len(values))
	for _, value := range values {
		value = domain.ID(strings.TrimSpace(string(value)))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func nullableID(id domain.ID) any {
	if id == "" {
		return nil
	}
	return id
}

func formatTime(value time.Time) string {
	return domain.FormatTime(value)
}

func (s *Observer) configured() error {
	if s == nil || s.store == nil || s.store.DB() == nil || s.clock == nil || s.collectiveID == "" {
		return errors.New("field observation service is not configured")
	}
	return nil
}
