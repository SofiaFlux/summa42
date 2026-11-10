package mission

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

// MissionStore abstracts durable storage for versioned missions.
type MissionStore interface {
	Save(ctx context.Context, mission Mission) error
	Get(ctx context.Context, id domain.ID, version int) (Mission, error)
	GetActive(ctx context.Context, id domain.ID) (Mission, error)
	ListVersions(ctx context.Context, id domain.ID) ([]Mission, error)
	Update(ctx context.Context, mission Mission) error
	ActivateVersion(ctx context.Context, id domain.ID, version int) (Mission, error)
}

func cloneMission(m Mission) Mission {
	cp := m
	if m.DesiredOutcomes != nil {
		cp.DesiredOutcomes = append([]string(nil), m.DesiredOutcomes...)
	}
	if m.Obligations != nil {
		cp.Obligations = append([]string(nil), m.Obligations...)
	}
	if m.Constraints != nil {
		cp.Constraints = append([]string(nil), m.Constraints...)
	}
	if m.NonGoals != nil {
		cp.NonGoals = append([]string(nil), m.NonGoals...)
	}
	if m.TradeOffRules != nil {
		cp.TradeOffRules = append([]TradeOffRule(nil), m.TradeOffRules...)
	}
	if m.HumanTrustBoundaries != nil {
		cp.HumanTrustBoundaries = append([]string(nil), m.HumanTrustBoundaries...)
	}
	if m.Assumptions != nil {
		cp.Assumptions = append([]string(nil), m.Assumptions...)
	}
	if m.Unknowns != nil {
		cp.Unknowns = append([]string(nil), m.Unknowns...)
	}
	if m.RevisionTriggers != nil {
		cp.RevisionTriggers = append([]string(nil), m.RevisionTriggers...)
	}
	if m.Provenance.SourceInputs != nil {
		cp.Provenance.SourceInputs = append([]string(nil), m.Provenance.SourceInputs...)
	}
	if m.Provenance.EvidenceRefs != nil {
		cp.Provenance.EvidenceRefs = append([]string(nil), m.Provenance.EvidenceRefs...)
	}
	if m.Provenance.ProposedAt != nil {
		t := *m.Provenance.ProposedAt
		cp.Provenance.ProposedAt = &t
	}
	if m.Provenance.ApprovedAt != nil {
		t := *m.Provenance.ApprovedAt
		cp.Provenance.ApprovedAt = &t
	}
	return cp
}

// InMemoryMissionStore provides thread-safe in-memory mission persistence.
type InMemoryMissionStore struct {
	mu       sync.RWMutex
	missions map[string]Mission
}

// NewInMemoryMissionStore initializes an in-memory store.
func NewInMemoryMissionStore() *InMemoryMissionStore {
	return &InMemoryMissionStore{
		missions: make(map[string]Mission),
	}
}

func (s *InMemoryMissionStore) Save(ctx context.Context, m Mission) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := fmt.Sprintf("%s:%d", m.ID, m.Version)
	if _, exists := s.missions[key]; exists {
		return ErrDuplicateMissionVersion
	}
	s.missions[key] = cloneMission(m)
	return nil
}

func (s *InMemoryMissionStore) Get(ctx context.Context, id domain.ID, version int) (Mission, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	key := fmt.Sprintf("%s:%d", id, version)
	m, ok := s.missions[key]
	if !ok {
		return Mission{}, ErrMissionNotFound
	}
	return cloneMission(m), nil
}

func (s *InMemoryMissionStore) GetActive(ctx context.Context, id domain.ID) (Mission, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, m := range s.missions {
		if m.ID == id && m.State == MissionStateActive {
			return cloneMission(m), nil
		}
	}
	return Mission{}, ErrActiveMissionNotFound
}

func (s *InMemoryMissionStore) ListVersions(ctx context.Context, id domain.ID) ([]Mission, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var list []Mission
	for _, m := range s.missions {
		if m.ID == id {
			list = append(list, cloneMission(m))
		}
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Version < list[j].Version
	})
	return list, nil
}

func (s *InMemoryMissionStore) Update(ctx context.Context, m Mission) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := fmt.Sprintf("%s:%d", m.ID, m.Version)
	existing, exists := s.missions[key]
	if !exists {
		return ErrMissionNotFound
	}

	// Store-level immutability invariant
	if existing.State != MissionStateDraft && m.State == existing.State {
		// Content update forbidden on non-draft missions
		if existing.Purpose != m.Purpose {
			return ErrMissionImmutable
		}
	}

	s.missions[key] = cloneMission(m)
	return nil
}

// ActivateVersion atomically marks the target version ACTIVE and any prior active version SUPERSEDED.
func (s *InMemoryMissionStore) ActivateVersion(ctx context.Context, id domain.ID, version int) (Mission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	targetKey := fmt.Sprintf("%s:%d", id, version)
	target, exists := s.missions[targetKey]
	if !exists {
		return Mission{}, ErrMissionNotFound
	}
	if target.State != MissionStateApproved {
		return Mission{}, fmt.Errorf("%w: cannot activate from state %s", ErrInvalidStateTransition, target.State)
	}

	now := time.Now().UTC()

	// Atomically supersede any currently active version
	for k, m := range s.missions {
		if m.ID == id && m.State == MissionStateActive && m.Version != version {
			m.State = MissionStateSuperseded
			m.Provenance.SupersededByVersion = version
			m.UpdatedAt = now
			s.missions[k] = m
		}
	}

	target.State = MissionStateActive
	target.UpdatedAt = now
	s.missions[targetKey] = target

	return cloneMission(target), nil
}

// SQLiteMissionStore provides durable persistence in SQLite.
type SQLiteMissionStore struct {
	db *sql.DB
	mu sync.RWMutex
}

// NewSQLiteMissionStore initializes schema and returns a new SQLiteMissionStore.
func NewSQLiteMissionStore(db *sql.DB) (*SQLiteMissionStore, error) {
	s := &SQLiteMissionStore{db: db}
	if err := s.initSchema(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SQLiteMissionStore) initSchema() error {
	schema := `
	CREATE TABLE IF NOT EXISTS mission_spec_versions (
		id TEXT NOT NULL,
		version INTEGER NOT NULL,
		state TEXT NOT NULL,
		purpose TEXT NOT NULL,
		desired_outcomes_json TEXT,
		obligations_json TEXT,
		constraints_json TEXT,
		non_goals_json TEXT,
		trade_off_rules_json TEXT,
		human_trust_boundaries_json TEXT,
		assumptions_json TEXT,
		unknowns_json TEXT,
		revision_triggers_json TEXT,
		provenance_json TEXT NOT NULL,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		PRIMARY KEY (id, version)
	);
	CREATE INDEX IF NOT EXISTS idx_mission_spec_id_version ON mission_spec_versions(id, version);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_mission_spec_single_active ON mission_spec_versions(id) WHERE state = 'ACTIVE';
	`
	_, err := s.db.Exec(schema)
	return err
}

func (s *SQLiteMissionStore) Save(ctx context.Context, m Mission) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	outcomesJSON, _ := json.Marshal(m.DesiredOutcomes)
	obligationsJSON, _ := json.Marshal(m.Obligations)
	constraintsJSON, _ := json.Marshal(m.Constraints)
	nonGoalsJSON, _ := json.Marshal(m.NonGoals)
	tradeOffsJSON, _ := json.Marshal(m.TradeOffRules)
	trustBoundariesJSON, _ := json.Marshal(m.HumanTrustBoundaries)
	assumptionsJSON, _ := json.Marshal(m.Assumptions)
	unknownsJSON, _ := json.Marshal(m.Unknowns)
	revisionTriggersJSON, _ := json.Marshal(m.RevisionTriggers)
	provenanceJSON, _ := json.Marshal(m.Provenance)

	query := `
	INSERT INTO mission_spec_versions (
		id, version, state, purpose, desired_outcomes_json, obligations_json,
		constraints_json, non_goals_json, trade_off_rules_json, human_trust_boundaries_json,
		assumptions_json, unknowns_json, revision_triggers_json, provenance_json,
		created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
	`

	_, err := s.db.ExecContext(ctx, query,
		string(m.ID), m.Version, string(m.State), m.Purpose,
		string(outcomesJSON), string(obligationsJSON), string(constraintsJSON),
		string(nonGoalsJSON), string(tradeOffsJSON), string(trustBoundariesJSON),
		string(assumptionsJSON), string(unknownsJSON), string(revisionTriggersJSON),
		string(provenanceJSON), m.CreatedAt, m.UpdatedAt,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrDuplicateMissionVersion
		}
		return err
	}
	return nil
}

func (s *SQLiteMissionStore) Get(ctx context.Context, id domain.ID, version int) (Mission, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
	SELECT id, version, state, purpose, desired_outcomes_json, obligations_json,
	       constraints_json, non_goals_json, trade_off_rules_json, human_trust_boundaries_json,
	       assumptions_json, unknowns_json, revision_triggers_json, provenance_json,
	       created_at, updated_at
	FROM mission_spec_versions WHERE id = ? AND version = ?;
	`
	row := s.db.QueryRowContext(ctx, query, string(id), version)
	return scanMission(row)
}

func (s *SQLiteMissionStore) GetActive(ctx context.Context, id domain.ID) (Mission, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
	SELECT id, version, state, purpose, desired_outcomes_json, obligations_json,
	       constraints_json, non_goals_json, trade_off_rules_json, human_trust_boundaries_json,
	       assumptions_json, unknowns_json, revision_triggers_json, provenance_json,
	       created_at, updated_at
	FROM mission_spec_versions WHERE id = ? AND state = 'ACTIVE';
	`
	row := s.db.QueryRowContext(ctx, query, string(id))
	m, err := scanMission(row)
	if errors.Is(err, ErrMissionNotFound) {
		return Mission{}, ErrActiveMissionNotFound
	}
	return m, err
}

func (s *SQLiteMissionStore) ListVersions(ctx context.Context, id domain.ID) ([]Mission, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
	SELECT id, version, state, purpose, desired_outcomes_json, obligations_json,
	       constraints_json, non_goals_json, trade_off_rules_json, human_trust_boundaries_json,
	       assumptions_json, unknowns_json, revision_triggers_json, provenance_json,
	       created_at, updated_at
	FROM mission_spec_versions WHERE id = ? ORDER BY version ASC;
	`
	rows, err := s.db.QueryContext(ctx, query, string(id))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []Mission
	for rows.Next() {
		m, err := scanMissionRow(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, m)
	}
	return list, nil
}

func (s *SQLiteMissionStore) Update(ctx context.Context, m Mission) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	outcomesJSON, _ := json.Marshal(m.DesiredOutcomes)
	obligationsJSON, _ := json.Marshal(m.Obligations)
	constraintsJSON, _ := json.Marshal(m.Constraints)
	nonGoalsJSON, _ := json.Marshal(m.NonGoals)
	tradeOffsJSON, _ := json.Marshal(m.TradeOffRules)
	trustBoundariesJSON, _ := json.Marshal(m.HumanTrustBoundaries)
	assumptionsJSON, _ := json.Marshal(m.Assumptions)
	unknownsJSON, _ := json.Marshal(m.Unknowns)
	revisionTriggersJSON, _ := json.Marshal(m.RevisionTriggers)
	provenanceJSON, _ := json.Marshal(m.Provenance)

	query := `
	UPDATE mission_spec_versions SET
		state = ?, purpose = ?, desired_outcomes_json = ?, obligations_json = ?,
		constraints_json = ?, non_goals_json = ?, trade_off_rules_json = ?,
		human_trust_boundaries_json = ?, assumptions_json = ?, unknowns_json = ?,
		revision_triggers_json = ?, provenance_json = ?, updated_at = ?
	WHERE id = ? AND version = ?;
	`

	res, err := s.db.ExecContext(ctx, query,
		string(m.State), m.Purpose, string(outcomesJSON), string(obligationsJSON),
		string(constraintsJSON), string(nonGoalsJSON), string(tradeOffsJSON),
		string(trustBoundariesJSON), string(assumptionsJSON), string(unknownsJSON),
		string(revisionTriggersJSON), string(provenanceJSON), m.UpdatedAt,
		string(m.ID), m.Version,
	)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrMissionNotFound
	}
	return nil
}

// ActivateVersion atomically marks target version ACTIVE and previously active version SUPERSEDED in a transaction.
func (s *SQLiteMissionStore) ActivateVersion(ctx context.Context, id domain.ID, version int) (Mission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Mission{}, err
	}
	defer tx.Rollback()

	// 1. Fetch target version
	queryGet := `
	SELECT id, version, state, purpose, desired_outcomes_json, obligations_json,
	       constraints_json, non_goals_json, trade_off_rules_json, human_trust_boundaries_json,
	       assumptions_json, unknowns_json, revision_triggers_json, provenance_json,
	       created_at, updated_at
	FROM mission_spec_versions WHERE id = ? AND version = ?;
	`
	row := tx.QueryRowContext(ctx, queryGet, string(id), version)
	target, err := scanMission(row)
	if err != nil {
		return Mission{}, err
	}
	if target.State != MissionStateApproved {
		return Mission{}, fmt.Errorf("%w: cannot activate from state %s", ErrInvalidStateTransition, target.State)
	}

	now := time.Now().UTC()

	// 2. Mark any previously active version as SUPERSEDED
	supersedeQuery := `
	UPDATE mission_spec_versions SET
		state = 'SUPERSEDED', updated_at = ?
	WHERE id = ? AND state = 'ACTIVE' AND version != ?;
	`
	if _, err := tx.ExecContext(ctx, supersedeQuery, now, string(id), version); err != nil {
		return Mission{}, err
	}

	// 3. Mark target version ACTIVE
	activateQuery := `
	UPDATE mission_spec_versions SET
		state = 'ACTIVE', updated_at = ?
	WHERE id = ? AND version = ?;
	`
	if _, err := tx.ExecContext(ctx, activateQuery, now, string(id), version); err != nil {
		return Mission{}, err
	}

	if err := tx.Commit(); err != nil {
		return Mission{}, err
	}

	target.State = MissionStateActive
	target.UpdatedAt = now
	return target, nil
}

type scannable interface {
	Scan(dest ...any) error
}

func scanMission(s scannable) (Mission, error) {
	var m Mission
	var idStr, stateStr string
	var outcomesJSON, obligationsJSON, constraintsJSON, nonGoalsJSON string
	var tradeOffsJSON, trustBoundariesJSON, assumptionsJSON, unknownsJSON string
	var revisionTriggersJSON, provenanceJSON string

	err := s.Scan(
		&idStr, &m.Version, &stateStr, &m.Purpose,
		&outcomesJSON, &obligationsJSON, &constraintsJSON, &nonGoalsJSON,
		&tradeOffsJSON, &trustBoundariesJSON, &assumptionsJSON, &unknownsJSON,
		&revisionTriggersJSON, &provenanceJSON, &m.CreatedAt, &m.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Mission{}, ErrMissionNotFound
		}
		return Mission{}, err
	}

	m.ID = domain.ID(idStr)
	m.State = MissionState(stateStr)

	_ = json.Unmarshal([]byte(outcomesJSON), &m.DesiredOutcomes)
	_ = json.Unmarshal([]byte(obligationsJSON), &m.Obligations)
	_ = json.Unmarshal([]byte(constraintsJSON), &m.Constraints)
	_ = json.Unmarshal([]byte(nonGoalsJSON), &m.NonGoals)
	_ = json.Unmarshal([]byte(tradeOffsJSON), &m.TradeOffRules)
	_ = json.Unmarshal([]byte(trustBoundariesJSON), &m.HumanTrustBoundaries)
	_ = json.Unmarshal([]byte(assumptionsJSON), &m.Assumptions)
	_ = json.Unmarshal([]byte(unknownsJSON), &m.Unknowns)
	_ = json.Unmarshal([]byte(revisionTriggersJSON), &m.RevisionTriggers)
	_ = json.Unmarshal([]byte(provenanceJSON), &m.Provenance)

	return m, nil
}

func scanMissionRow(rows *sql.Rows) (Mission, error) {
	return scanMission(rows)
}
