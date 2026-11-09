package company

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type Service struct {
	store    *state.Store
	clock    clock.Clock
	mu       sync.RWMutex
	adapters map[string]ProjectionAdapter
}

func NewService(store *state.Store, clk clock.Clock) *Service {
	s := &Service{
		store:    store,
		clock:    clk,
		adapters: make(map[string]ProjectionAdapter),
	}
	s.RegisterAdapter(&CRMAdapter{})
	s.RegisterAdapter(&ERPAdapter{})
	s.RegisterAdapter(&CloudAdapter{})
	s.RegisterAdapter(&GitAdapter{})
	return s
}

func (s *Service) RegisterAdapter(a ProjectionAdapter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adapters[a.SourceSystem()] = a
}

func (s *Service) getAdapter(source string) (ProjectionAdapter, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.adapters[source]
	return a, ok
}

type ApplyTransitionRequest struct {
	EntityID          domain.ID      `json:"entity_id"`
	EntityKind        EntityKind     `json:"entity_kind,omitempty"` // Required if creating a new entity
	Name              string         `json:"name,omitempty"`
	TaskID            domain.ID      `json:"task_id"`
	EvidenceID        domain.ID      `json:"evidence_id"`
	TransitionKind    string         `json:"transition_kind"`
	NewStatus         string         `json:"new_status"`
	NewAttributes     map[string]any `json:"new_attributes"`
	NewExposureAmount *int64         `json:"new_exposure_amount,omitempty"`
}

// ApplyTransition updates CompanyState in an auditable manner, strictly requiring
// that the associated Task has succeeded with a PASS acceptance verdict and verified evidence.
func (s *Service) ApplyTransition(ctx context.Context, req ApplyTransitionRequest) (StateTransition, error) {
	if strings.TrimSpace(string(req.EntityID)) == "" {
		return StateTransition{}, errors.New("entity_id is required")
	}
	if strings.TrimSpace(string(req.TaskID)) == "" {
		return StateTransition{}, errors.New("task_id is required")
	}
	if strings.TrimSpace(string(req.EvidenceID)) == "" {
		return StateTransition{}, errors.New("evidence_id is required")
	}
	if strings.TrimSpace(req.TransitionKind) == "" {
		return StateTransition{}, errors.New("transition_kind is required")
	}
	if strings.TrimSpace(req.NewStatus) == "" {
		return StateTransition{}, errors.New("new_status is required")
	}

	now := s.clock.Now().UTC()
	nowStr := domain.FormatTime(now)
	transitionID := domain.NewID("trans")

	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return StateTransition{}, fmt.Errorf("begin transition tx: %w", err)
	}
	defer tx.Rollback()

	// 1. Verify Task exists and state is SUCCEEDED, capturing current_attempt_id
	var taskState string
	var currentAttemptID sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT state, current_attempt_id FROM tasks WHERE task_id = ?`,
		req.TaskID,
	).Scan(&taskState, &currentAttemptID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return StateTransition{}, ErrTaskNotAccepted
		}
		return StateTransition{}, fmt.Errorf("query task state: %w", err)
	}
	if taskState != string(domain.TaskSucceeded) {
		return StateTransition{}, ErrTaskNotAccepted
	}

	// 2. Verify Acceptance Record exists for this task and attempt, with PASS verdict
	var criteriaResultJSON, evidenceIDsJSON string
	var accAttemptID string
	if currentAttemptID.Valid && currentAttemptID.String != "" {
		accAttemptID = currentAttemptID.String
		err = tx.QueryRowContext(ctx, `
			SELECT criteria_result_json, evidence_ids_json
			FROM acceptance_records
			WHERE task_id = ? AND attempt_id = ?`,
			req.TaskID, accAttemptID,
		).Scan(&criteriaResultJSON, &evidenceIDsJSON)
	} else {
		err = tx.QueryRowContext(ctx, `
			SELECT criteria_result_json, evidence_ids_json
			FROM acceptance_records
			WHERE task_id = ?
			ORDER BY created_at DESC LIMIT 1`,
			req.TaskID,
		).Scan(&criteriaResultJSON, &evidenceIDsJSON)
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return StateTransition{}, ErrTaskNotAccepted
		}
		return StateTransition{}, fmt.Errorf("query acceptance record: %w", err)
	}

	var critResult struct {
		Met     *bool  `json:"met"`
		Verdict string `json:"verdict"`
	}
	_ = json.Unmarshal([]byte(criteriaResultJSON), &critResult)
	isMet := (critResult.Met != nil && *critResult.Met) || strings.ToUpper(critResult.Verdict) == "PASS"
	if !isMet {
		return StateTransition{}, ErrAcceptanceVerdictFailed
	}

	// 3. Verify Evidence is explicitly bound to this acceptance record
	var evidenceIDs []string
	_ = json.Unmarshal([]byte(evidenceIDsJSON), &evidenceIDs)
	foundInAcceptance := false
	for _, id := range evidenceIDs {
		if id == string(req.EvidenceID) {
			foundInAcceptance = true
			break
		}
	}
	if !foundInAcceptance {
		return StateTransition{}, ErrMissingEvidence
	}

	// 4. Query current entity state
	var existingKind, existingName, existingStatus, existingAttrs string
	var existingExposure int64
	var createdAtStr string
	var exists bool

	err = tx.QueryRowContext(ctx, `
		SELECT kind, name, status, attributes_json, exposure_amount, created_at
		FROM company_entities
		WHERE entity_id = ?`,
		req.EntityID,
	).Scan(&existingKind, &existingName, &existingStatus, &existingAttrs, &existingExposure, &createdAtStr)
	if err == nil {
		exists = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return StateTransition{}, fmt.Errorf("query existing entity: %w", err)
	}

	var previousState map[string]any
	mergedAttributes := make(map[string]any)

	if exists {
		var prevAttrs map[string]any
		_ = json.Unmarshal([]byte(existingAttrs), &prevAttrs)
		for k, v := range prevAttrs {
			mergedAttributes[k] = v
		}
		previousState = map[string]any{
			"name":            existingName,
			"status":          existingStatus,
			"attributes":      prevAttrs,
			"exposure_amount": existingExposure,
		}
	} else {
		if !req.EntityKind.Valid() {
			return StateTransition{}, ErrInvalidEntityKind
		}
		if req.Name == "" {
			return StateTransition{}, errors.New("name is required for new entity")
		}
	}

	// Merge incoming attributes
	for k, v := range req.NewAttributes {
		mergedAttributes[k] = v
	}

	var newExposure int64
	if req.NewExposureAmount != nil {
		newExposure = *req.NewExposureAmount
	} else if exists {
		newExposure = existingExposure
	}

	finalName := req.Name
	if exists && finalName == "" {
		finalName = existingName
	}

	newState := map[string]any{
		"name":            finalName,
		"status":          req.NewStatus,
		"attributes":      mergedAttributes,
		"exposure_amount": newExposure,
	}

	prevJSONBytes, _ := json.Marshal(previousState)
	newJSONBytes, _ := json.Marshal(newState)
	attrsJSONBytes, _ := json.Marshal(mergedAttributes)

	var prevJSONVal any
	if previousState != nil {
		prevJSONVal = string(prevJSONBytes)
	}

	// 5. Update or Insert Entity (parent row must exist before audit log FK)
	if exists {
		_, err = tx.ExecContext(ctx, `
			UPDATE company_entities
			SET name = ?,
			    status = ?,
			    attributes_json = ?,
			    exposure_amount = ?,
			    updated_at = ?
			WHERE entity_id = ?`,
			finalName, req.NewStatus, string(attrsJSONBytes), newExposure, nowStr, req.EntityID,
		)
		if err != nil {
			return StateTransition{}, fmt.Errorf("update company entity: %w", err)
		}
	} else {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO company_entities (
				entity_id, kind, name, status, attributes_json, exposure_amount, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			req.EntityID, req.EntityKind, finalName, req.NewStatus, string(attrsJSONBytes),
			newExposure, nowStr, nowStr,
		)
		if err != nil {
			return StateTransition{}, fmt.Errorf("insert company entity: %w", err)
		}
	}

	// 6. Record State Transition in audit log
	_, err = tx.ExecContext(ctx, `
		INSERT INTO company_state_transitions (
			transition_id, entity_id, task_id, evidence_id, transition_kind,
			previous_state_json, new_state_json, recorded_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		transitionID, req.EntityID, req.TaskID, req.EvidenceID, req.TransitionKind,
		prevJSONVal, string(newJSONBytes), nowStr,
	)
	if err != nil {
		return StateTransition{}, fmt.Errorf("insert state transition: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return StateTransition{}, fmt.Errorf("commit transition: %w", err)
	}

	return StateTransition{
		TransitionID:   transitionID,
		EntityID:       req.EntityID,
		TaskID:         req.TaskID,
		EvidenceID:     req.EvidenceID,
		TransitionKind: req.TransitionKind,
		PreviousState:  previousState,
		NewState:       newState,
		RecordedAt:     now,
	}, nil
}

// IngestExternalProjection ingests a projection from an external system (CRM, ERP, cloud, Git)
// mapping it into a canonical CompanyState entity, a durable projection record, and an audit transition.
func (s *Service) IngestExternalProjection(
	ctx context.Context,
	source string,
	externalID string,
	payload map[string]any,
) (Entity, ExternalProjection, error) {
	if strings.TrimSpace(source) == "" {
		return Entity{}, ExternalProjection{}, errors.New("source is required")
	}
	if strings.TrimSpace(externalID) == "" {
		return Entity{}, ExternalProjection{}, errors.New("external_id is required")
	}

	adapter, ok := s.getAdapter(source)
	if !ok {
		return Entity{}, ExternalProjection{}, fmt.Errorf("unsupported projection source system: %s", source)
	}

	trans, err := adapter.Translate(ctx, externalID, payload)
	if err != nil {
		return Entity{}, ExternalProjection{}, fmt.Errorf("adapter translate: %w", err)
	}

	now := s.clock.Now().UTC()
	nowStr := domain.FormatTime(now)

	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return Entity{}, ExternalProjection{}, fmt.Errorf("begin projection tx: %w", err)
	}
	defer tx.Rollback()

	// Check if projection already mapped to an entity
	var existingProjID, existingEntityID domain.ID
	err = tx.QueryRowContext(ctx, `
		SELECT projection_id, entity_id FROM company_projections WHERE source_system = ? AND external_id = ?`,
		source, externalID,
	).Scan(&existingProjID, &existingEntityID)

	var targetProjID, targetEntityID domain.ID
	var previousState map[string]any
	var createdAt time.Time

	attrsBytes, _ := json.Marshal(trans.Attributes)
	payloadBytes, _ := json.Marshal(payload)

	if err == nil {
		targetProjID = existingProjID
		targetEntityID = existingEntityID

		// Read previous state for audit
		var prevName, prevStatus, prevAttrs, prevCrAt string
		var prevExposure int64
		_ = tx.QueryRowContext(ctx, `
			SELECT name, status, attributes_json, exposure_amount, created_at
			FROM company_entities WHERE entity_id = ?`,
			targetEntityID,
		).Scan(&prevName, &prevStatus, &prevAttrs, &prevExposure, &prevCrAt)
		createdAt, _ = time.Parse(time.RFC3339Nano, prevCrAt)

		var prevAttrsMap map[string]any
		_ = json.Unmarshal([]byte(prevAttrs), &prevAttrsMap)
		previousState = map[string]any{
			"name":            prevName,
			"status":          prevStatus,
			"attributes":      prevAttrsMap,
			"exposure_amount": prevExposure,
		}

		// Update existing entity
		_, err = tx.ExecContext(ctx, `
			UPDATE company_entities
			SET name = ?,
			    status = ?,
			    attributes_json = ?,
			    exposure_amount = ?,
			    updated_at = ?
			WHERE entity_id = ?`,
			trans.Name, trans.Status, string(attrsBytes), trans.ExposureAmount, nowStr, targetEntityID,
		)
		if err != nil {
			return Entity{}, ExternalProjection{}, fmt.Errorf("update projected entity: %w", err)
		}

		// Update projection record
		_, err = tx.ExecContext(ctx, `
			UPDATE company_projections
			SET payload_json = ?,
			    synced_at = ?
			WHERE source_system = ? AND external_id = ?`,
			string(payloadBytes), nowStr, source, externalID,
		)
		if err != nil {
			return Entity{}, ExternalProjection{}, fmt.Errorf("update projection record: %w", err)
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		targetProjID = domain.NewID("proj")
		targetEntityID = domain.NewID("ent")
		createdAt = now

		// Insert new entity
		_, err = tx.ExecContext(ctx, `
			INSERT INTO company_entities (
				entity_id, kind, name, status, attributes_json, exposure_amount, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			targetEntityID, trans.Kind, trans.Name, trans.Status, string(attrsBytes), trans.ExposureAmount, nowStr, nowStr,
		)
		if err != nil {
			return Entity{}, ExternalProjection{}, fmt.Errorf("insert projected entity: %w", err)
		}

		// Insert projection mapping
		_, err = tx.ExecContext(ctx, `
			INSERT INTO company_projections (
				projection_id, source_system, external_id, entity_id, payload_json, synced_at
			) VALUES (?, ?, ?, ?, ?, ?)`,
			targetProjID, source, externalID, targetEntityID, string(payloadBytes), nowStr,
		)
		if err != nil {
			return Entity{}, ExternalProjection{}, fmt.Errorf("insert projection record: %w", err)
		}
	} else {
		return Entity{}, ExternalProjection{}, fmt.Errorf("query projection mapping: %w", err)
	}

	// Record auditable transition for the projection
	newState := map[string]any{
		"name":            trans.Name,
		"status":          trans.Status,
		"attributes":      trans.Attributes,
		"exposure_amount": trans.ExposureAmount,
	}
	prevJSONBytes, _ := json.Marshal(previousState)
	newJSONBytes, _ := json.Marshal(newState)
	var prevVal any
	if previousState != nil {
		prevVal = string(prevJSONBytes)
	}

	transitionID := domain.NewID("trans")
	taskID := domain.ID("system.projection." + source)
	evidenceID := domain.ID("proj." + externalID)

	_, err = tx.ExecContext(ctx, `
		INSERT INTO company_state_transitions (
			transition_id, entity_id, task_id, evidence_id, transition_kind,
			previous_state_json, new_state_json, recorded_at
		) VALUES (?, ?, ?, ?, 'EXTERNAL_PROJECTION_INGESTION', ?, ?, ?)`,
		transitionID, targetEntityID, taskID, evidenceID, prevVal, string(newJSONBytes), nowStr,
	)
	if err != nil {
		return Entity{}, ExternalProjection{}, fmt.Errorf("insert projection state transition: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Entity{}, ExternalProjection{}, fmt.Errorf("commit projection: %w", err)
	}

	entity := Entity{
		EntityID:       targetEntityID,
		Kind:           trans.Kind,
		Name:           trans.Name,
		Status:         trans.Status,
		Attributes:     trans.Attributes,
		ExposureAmount: trans.ExposureAmount,
		CreatedAt:      createdAt,
		UpdatedAt:      now,
	}

	projection := ExternalProjection{
		ProjectionID: targetProjID,
		SourceSystem: source,
		ExternalID:   externalID,
		EntityID:     targetEntityID,
		Payload:      payload,
		SyncedAt:     now,
	}

	return entity, projection, nil
}

// GetEntity retrieves a single company entity by ID.
func (s *Service) GetEntity(ctx context.Context, entityID domain.ID) (Entity, error) {
	var ent Entity
	var attrsJSON, crAt, upAt string
	err := s.store.DB().QueryRowContext(ctx, `
		SELECT entity_id, kind, name, status, attributes_json, exposure_amount, created_at, updated_at
		FROM company_entities
		WHERE entity_id = ?`,
		entityID,
	).Scan(&ent.EntityID, &ent.Kind, &ent.Name, &ent.Status, &attrsJSON, &ent.ExposureAmount, &crAt, &upAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Entity{}, ErrEntityNotFound
		}
		return Entity{}, fmt.Errorf("query entity: %w", err)
	}

	_ = json.Unmarshal([]byte(attrsJSON), &ent.Attributes)
	ent.CreatedAt, _ = time.Parse(time.RFC3339Nano, crAt)
	ent.UpdatedAt, _ = time.Parse(time.RFC3339Nano, upAt)
	return ent, nil
}

type EntityFilter struct {
	Kind   *EntityKind
	Status *string
}

// ListEntities retrieves company entities matching the optional filter criteria.
func (s *Service) ListEntities(ctx context.Context, filter EntityFilter) ([]Entity, error) {
	var query strings.Builder
	query.WriteString(`
		SELECT entity_id, kind, name, status, attributes_json, exposure_amount, created_at, updated_at
		FROM company_entities
		WHERE 1=1`)

	var args []any
	if filter.Kind != nil {
		query.WriteString(" AND kind = ?")
		args = append(args, *filter.Kind)
	}
	if filter.Status != nil {
		query.WriteString(" AND status = ?")
		args = append(args, *filter.Status)
	}
	query.WriteString(" ORDER BY created_at ASC")

	rows, err := s.store.DB().QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("query entities: %w", err)
	}
	defer rows.Close()

	var entities []Entity
	for rows.Next() {
		var ent Entity
		var attrsJSON, crAt, upAt string
		if err := rows.Scan(
			&ent.EntityID, &ent.Kind, &ent.Name, &ent.Status, &attrsJSON,
			&ent.ExposureAmount, &crAt, &upAt,
		); err != nil {
			return nil, fmt.Errorf("scan entity: %w", err)
		}
		_ = json.Unmarshal([]byte(attrsJSON), &ent.Attributes)
		ent.CreatedAt, _ = time.Parse(time.RFC3339Nano, crAt)
		ent.UpdatedAt, _ = time.Parse(time.RFC3339Nano, upAt)
		entities = append(entities, ent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate entities: %w", err)
	}

	return entities, nil
}

// GetAuditHistory retrieves the immutable state transition audit history for an entity.
func (s *Service) GetAuditHistory(ctx context.Context, entityID domain.ID) ([]StateTransition, error) {
	rows, err := s.store.DB().QueryContext(ctx, `
		SELECT transition_id, entity_id, task_id, evidence_id, transition_kind,
		       previous_state_json, new_state_json, recorded_at
		FROM company_state_transitions
		WHERE entity_id = ?
		ORDER BY recorded_at ASC`,
		entityID,
	)
	if err != nil {
		return nil, fmt.Errorf("query audit history: %w", err)
	}
	defer rows.Close()

	var history []StateTransition
	for rows.Next() {
		var t StateTransition
		var prevJSON sql.NullString
		var newJSON, recAt string
		if err := rows.Scan(
			&t.TransitionID, &t.EntityID, &t.TaskID, &t.EvidenceID,
			&t.TransitionKind, &prevJSON, &newJSON, &recAt,
		); err != nil {
			return nil, fmt.Errorf("scan transition: %w", err)
		}
		if prevJSON.Valid && prevJSON.String != "" {
			_ = json.Unmarshal([]byte(prevJSON.String), &t.PreviousState)
		}
		_ = json.Unmarshal([]byte(newJSON), &t.NewState)
		t.RecordedAt, _ = time.Parse(time.RFC3339Nano, recAt)
		history = append(history, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate audit history: %w", err)
	}

	return history, nil
}

// GetCompanySummary aggregates high-level company profile metrics.
func (s *Service) GetCompanySummary(ctx context.Context) (CompanySummary, error) {
	summary := CompanySummary{
		EntitiesByKind: make(map[EntityKind]int),
	}

	rows, err := s.store.DB().QueryContext(ctx, `
		SELECT kind, count(*), sum(exposure_amount)
		FROM company_entities
		GROUP BY kind`,
	)
	if err != nil {
		return summary, fmt.Errorf("query entities by kind: %w", err)
	}
	defer rows.Close()

	var totalExposure int64
	var totalCount int
	for rows.Next() {
		var kind EntityKind
		var count int
		var exposure sql.NullInt64
		if err := rows.Scan(&kind, &count, &exposure); err != nil {
			return summary, fmt.Errorf("scan kind summary: %w", err)
		}
		summary.EntitiesByKind[kind] = count
		totalCount += count
		if exposure.Valid {
			totalExposure += exposure.Int64
		}
	}
	if err := rows.Err(); err != nil {
		return summary, fmt.Errorf("iterate kind summary: %w", err)
	}

	summary.TotalEntities = totalCount
	summary.TotalExposureAmount = totalExposure

	_ = s.store.DB().QueryRowContext(ctx, `
		SELECT count(*) FROM company_entities WHERE kind = 'COMMITMENT' AND status = 'ACTIVE'`,
	).Scan(&summary.ActiveCommitments)

	_ = s.store.DB().QueryRowContext(ctx, `
		SELECT count(*) FROM company_entities WHERE kind = 'CONTRACT' AND status = 'ACTIVE'`,
	).Scan(&summary.ActiveContracts)

	_ = s.store.DB().QueryRowContext(ctx, `
		SELECT count(*) FROM company_entities WHERE kind = 'RISK' AND status = 'ACTIVE'`,
	).Scan(&summary.ActiveRisks)

	return summary, nil
}
