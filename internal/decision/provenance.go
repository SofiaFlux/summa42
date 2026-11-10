package decision

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

var (
	ErrDecisionNotFound = errors.New("decision record not found")
)

// ProvenanceRecord preserves the complete decision lineage and downstream outcome.
type ProvenanceRecord struct {
	DecisionID      domain.ID      `json:"decision_id"`
	DecisionType    DecisionType   `json:"decision_type"`
	SubjectRef      SubjectRef     `json:"subject_ref"`
	Provider        ProviderInfo   `json:"provider"`
	SelectedOutcome string         `json:"selected_outcome"`
	Confidence      float64        `json:"confidence"`
	ConfidenceKind  ConfidenceKind `json:"confidence_kind"`
	Abstained       bool           `json:"abstained"`
	AbstainReason   string         `json:"abstain_reason,omitempty"`
	ProjectionHash  string         `json:"projection_hash"`
	Latency         time.Duration  `json:"latency"`
	Usage           DecisionUsage  `json:"usage"`
	DecidedAt       time.Time      `json:"decided_at"`
	ActualOutcome   string         `json:"actual_outcome,omitempty"`
	DownstreamCost  int64          `json:"downstream_cost,omitempty"`
	Success         *bool          `json:"success,omitempty"`
	OutcomeLinkedAt *time.Time     `json:"outcome_linked_at,omitempty"`
}

// ProvenanceFilter specifies criteria for querying historical decisions.
type ProvenanceFilter struct {
	DecisionType DecisionType
	SubjectKind  string
	SubjectID    domain.ID
	Limit        int
}

// ProvenanceStore provides persistence for decision provenance and outcome linkage.
type ProvenanceStore interface {
	RecordDecision(ctx context.Context, req DecisionRequest, res DecisionResult) error
	RecordOutcome(ctx context.Context, decisionID domain.ID, actualOutcome string, downstreamCost int64, success bool) error
	Get(ctx context.Context, decisionID domain.ID) (ProvenanceRecord, error)
	List(ctx context.Context, filter ProvenanceFilter) ([]ProvenanceRecord, error)
}

// TelemetryEmitter handles telemetry events when decisions are made and linked.
type TelemetryEmitter interface {
	EmitDecision(ctx context.Context, rec ProvenanceRecord)
	EmitOutcome(ctx context.Context, rec ProvenanceRecord)
}

// RecordingTelemetryEmitter records telemetry events in-memory for testing and local inspection.
type RecordingTelemetryEmitter struct {
	mu     sync.Mutex
	events []ProvenanceRecord
}

// NewRecordingTelemetryEmitter creates a new recording telemetry emitter.
func NewRecordingTelemetryEmitter() *RecordingTelemetryEmitter {
	return &RecordingTelemetryEmitter{
		events: make([]ProvenanceRecord, 0),
	}
}

func (e *RecordingTelemetryEmitter) EmitDecision(ctx context.Context, rec ProvenanceRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, rec)
}

func (e *RecordingTelemetryEmitter) EmitOutcome(ctx context.Context, rec ProvenanceRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, rec)
}

// Events returns a copy of captured telemetry events.
func (e *RecordingTelemetryEmitter) Events() []ProvenanceRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	cp := make([]ProvenanceRecord, len(e.events))
	copy(cp, e.events)
	return cp
}

// InMemoryProvenanceStore is a thread-safe in-memory store for decision provenance.
type InMemoryProvenanceStore struct {
	mu      sync.RWMutex
	records map[domain.ID]ProvenanceRecord
	order   []domain.ID
}

// NewInMemoryProvenanceStore creates a new in-memory provenance store.
func NewInMemoryProvenanceStore() *InMemoryProvenanceStore {
	return &InMemoryProvenanceStore{
		records: make(map[domain.ID]ProvenanceRecord),
		order:   make([]domain.ID, 0),
	}
}

func (s *InMemoryProvenanceStore) RecordDecision(ctx context.Context, req DecisionRequest, res DecisionResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	decidedAt := res.DecidedAt
	if decidedAt.IsZero() {
		decidedAt = time.Now().UTC()
	}

	rec := ProvenanceRecord{
		DecisionID:      req.DecisionID,
		DecisionType:    req.DecisionType,
		SubjectRef:      req.SubjectRef,
		Provider:        res.Provider,
		SelectedOutcome: res.SelectedOutcome,
		Confidence:      res.Confidence,
		ConfidenceKind:  res.ConfidenceKind,
		Abstained:       res.Abstained,
		AbstainReason:   res.AbstainReason,
		ProjectionHash:  res.ProjectionHash,
		Latency:         res.Latency,
		Usage:           res.Usage,
		DecidedAt:       decidedAt,
	}

	if _, exists := s.records[req.DecisionID]; !exists {
		s.order = append(s.order, req.DecisionID)
	}
	s.records[req.DecisionID] = rec
	return nil
}

func (s *InMemoryProvenanceStore) RecordOutcome(ctx context.Context, decisionID domain.ID, actualOutcome string, downstreamCost int64, success bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, exists := s.records[decisionID]
	if !exists {
		return ErrDecisionNotFound
	}

	now := time.Now().UTC()
	rec.ActualOutcome = actualOutcome
	rec.DownstreamCost = downstreamCost
	rec.Success = &success
	rec.OutcomeLinkedAt = &now

	s.records[decisionID] = rec
	return nil
}

func (s *InMemoryProvenanceStore) Get(ctx context.Context, decisionID domain.ID) (ProvenanceRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rec, exists := s.records[decisionID]
	if !exists {
		return ProvenanceRecord{}, ErrDecisionNotFound
	}
	return rec, nil
}

func (s *InMemoryProvenanceStore) List(ctx context.Context, filter ProvenanceFilter) ([]ProvenanceRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]ProvenanceRecord, 0)
	for _, id := range s.order {
		rec := s.records[id]
		if filter.DecisionType != "" && rec.DecisionType != filter.DecisionType {
			continue
		}
		if filter.SubjectKind != "" && rec.SubjectRef.Kind != filter.SubjectKind {
			continue
		}
		if filter.SubjectID != "" && rec.SubjectRef.ID != filter.SubjectID {
			continue
		}
		result = append(result, rec)
		if filter.Limit > 0 && len(result) >= filter.Limit {
			break
		}
	}
	return result, nil
}

// SQLiteProvenanceStore implements ProvenanceStore over database/sql with SQLite.
type SQLiteProvenanceStore struct {
	db *sql.DB
}

// NewSQLiteProvenanceStore creates a new SQLite-backed provenance store.
func NewSQLiteProvenanceStore(db *sql.DB) *SQLiteProvenanceStore {
	return &SQLiteProvenanceStore{db: db}
}

// EnsureSchema creates the necessary tables and indexes if they do not exist.
func (s *SQLiteProvenanceStore) EnsureSchema(ctx context.Context) error {
	query := `
	CREATE TABLE IF NOT EXISTS decision_provenance (
		decision_id TEXT PRIMARY KEY,
		decision_type TEXT NOT NULL,
		subject_kind TEXT NOT NULL,
		subject_id TEXT NOT NULL,
		provider_kind TEXT NOT NULL,
		provider_name TEXT NOT NULL,
		provider_version TEXT,
		selected_outcome TEXT NOT NULL,
		confidence REAL NOT NULL,
		confidence_kind TEXT NOT NULL,
		abstained INTEGER NOT NULL,
		abstain_reason TEXT,
		projection_hash TEXT NOT NULL,
		latency_ns INTEGER NOT NULL,
		tokens_in INTEGER NOT NULL,
		tokens_out INTEGER NOT NULL,
		cost_micro_cents INTEGER NOT NULL,
		decided_at TEXT NOT NULL,
		actual_outcome TEXT,
		downstream_cost INTEGER,
		success INTEGER,
		outcome_linked_at TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_decision_provenance_type ON decision_provenance (decision_type, decided_at);
	`
	_, err := s.db.ExecContext(ctx, query)
	return err
}

func (s *SQLiteProvenanceStore) RecordDecision(ctx context.Context, req DecisionRequest, res DecisionResult) error {
	query := `
	INSERT INTO decision_provenance (
		decision_id, decision_type, subject_kind, subject_id,
		provider_kind, provider_name, provider_version,
		selected_outcome, confidence, confidence_kind,
		abstained, abstain_reason, projection_hash,
		latency_ns, tokens_in, tokens_out, cost_micro_cents, decided_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(decision_id) DO UPDATE SET
		selected_outcome = excluded.selected_outcome,
		confidence = excluded.confidence,
		confidence_kind = excluded.confidence_kind,
		abstained = excluded.abstained,
		abstain_reason = excluded.abstain_reason,
		projection_hash = excluded.projection_hash,
		latency_ns = excluded.latency_ns,
		tokens_in = excluded.tokens_in,
		tokens_out = excluded.tokens_out,
		cost_micro_cents = excluded.cost_micro_cents,
		decided_at = excluded.decided_at;
	`
	decidedAt := res.DecidedAt
	if decidedAt.IsZero() {
		decidedAt = time.Now().UTC()
	}

	abstainedInt := 0
	if res.Abstained {
		abstainedInt = 1
	}

	_, err := s.db.ExecContext(ctx, query,
		string(req.DecisionID),
		string(req.DecisionType),
		req.SubjectRef.Kind,
		string(req.SubjectRef.ID),
		string(res.Provider.Kind),
		res.Provider.Name,
		res.Provider.Version,
		res.SelectedOutcome,
		res.Confidence,
		string(res.ConfidenceKind),
		abstainedInt,
		res.AbstainReason,
		res.ProjectionHash,
		int64(res.Latency),
		res.Usage.TokensIn,
		res.Usage.TokensOut,
		res.Usage.CostMicroCents,
		decidedAt.Format(time.RFC3339Nano),
	)
	return err
}

func (s *SQLiteProvenanceStore) RecordOutcome(ctx context.Context, decisionID domain.ID, actualOutcome string, downstreamCost int64, success bool) error {
	query := `
	UPDATE decision_provenance
	SET actual_outcome = ?,
	    downstream_cost = ?,
	    success = ?,
	    outcome_linked_at = ?
	WHERE decision_id = ?;
	`
	now := time.Now().UTC()
	successInt := 0
	if success {
		successInt = 1
	}

	res, err := s.db.ExecContext(ctx, query,
		actualOutcome,
		downstreamCost,
		successInt,
		now.Format(time.RFC3339Nano),
		string(decisionID),
	)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrDecisionNotFound
	}
	return nil
}

func (s *SQLiteProvenanceStore) Get(ctx context.Context, decisionID domain.ID) (ProvenanceRecord, error) {
	query := `
	SELECT decision_id, decision_type, subject_kind, subject_id,
	       provider_kind, provider_name, provider_version,
	       selected_outcome, confidence, confidence_kind,
	       abstained, abstain_reason, projection_hash,
	       latency_ns, tokens_in, tokens_out, cost_micro_cents, decided_at,
	       actual_outcome, downstream_cost, success, outcome_linked_at
	FROM decision_provenance
	WHERE decision_id = ?;
	`
	row := s.db.QueryRowContext(ctx, query, string(decisionID))
	return scanRecord(row)
}

func (s *SQLiteProvenanceStore) List(ctx context.Context, filter ProvenanceFilter) ([]ProvenanceRecord, error) {
	query := `
	SELECT decision_id, decision_type, subject_kind, subject_id,
	       provider_kind, provider_name, provider_version,
	       selected_outcome, confidence, confidence_kind,
	       abstained, abstain_reason, projection_hash,
	       latency_ns, tokens_in, tokens_out, cost_micro_cents, decided_at,
	       actual_outcome, downstream_cost, success, outcome_linked_at
	FROM decision_provenance
	WHERE 1=1
	`
	args := make([]any, 0)
	if filter.DecisionType != "" {
		query += " AND decision_type = ?"
		args = append(args, string(filter.DecisionType))
	}
	if filter.SubjectKind != "" {
		query += " AND subject_kind = ?"
		args = append(args, filter.SubjectKind)
	}
	if filter.SubjectID != "" {
		query += " AND subject_id = ?"
		args = append(args, string(filter.SubjectID))
	}
	query += " ORDER BY decided_at DESC"
	if filter.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, filter.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := make([]ProvenanceRecord, 0)
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRecord(s scanner) (ProvenanceRecord, error) {
	var (
		rec             ProvenanceRecord
		decID           string
		decType         string
		subjKind        string
		subjID          string
		provKind        string
		provName        string
		provVer         sql.NullString
		selOutcome      string
		conf            float64
		confKind        string
		abstained       int
		abstainReason   sql.NullString
		projHash        string
		latencyNS       int64
		tokensIn        int
		tokensOut       int
		costMicroCents  int64
		decidedAtStr    string
		actualOutcome   sql.NullString
		downstreamCost  sql.NullInt64
		successInt      sql.NullInt64
		outcomeLinkedAt sql.NullString
	)

	err := s.Scan(
		&decID, &decType, &subjKind, &subjID,
		&provKind, &provName, &provVer,
		&selOutcome, &conf, &confKind,
		&abstained, &abstainReason, &projHash,
		&latencyNS, &tokensIn, &tokensOut, &costMicroCents, &decidedAtStr,
		&actualOutcome, &downstreamCost, &successInt, &outcomeLinkedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProvenanceRecord{}, ErrDecisionNotFound
		}
		return ProvenanceRecord{}, err
	}

	decidedAt, err := time.Parse(time.RFC3339Nano, decidedAtStr)
	if err != nil {
		decidedAt, _ = time.Parse(time.RFC3339, decidedAtStr)
	}

	rec = ProvenanceRecord{
		DecisionID:   domain.ID(decID),
		DecisionType: DecisionType(decType),
		SubjectRef: SubjectRef{
			Kind: subjKind,
			ID:   domain.ID(subjID),
		},
		Provider: ProviderInfo{
			Kind:    ProviderKind(provKind),
			Name:    provName,
			Version: provVer.String,
		},
		SelectedOutcome: selOutcome,
		Confidence:      conf,
		ConfidenceKind:  ConfidenceKind(confKind),
		Abstained:       abstained == 1,
		AbstainReason:   abstainReason.String,
		ProjectionHash:  projHash,
		Latency:         time.Duration(latencyNS),
		Usage: DecisionUsage{
			TokensIn:       tokensIn,
			TokensOut:      tokensOut,
			CostMicroCents: costMicroCents,
		},
		DecidedAt: decidedAt,
	}

	if actualOutcome.Valid {
		rec.ActualOutcome = actualOutcome.String
	}
	if downstreamCost.Valid {
		rec.DownstreamCost = downstreamCost.Int64
	}
	if successInt.Valid {
		succ := successInt.Int64 == 1
		rec.Success = &succ
	}
	if outcomeLinkedAt.Valid {
		tLinked, err := time.Parse(time.RFC3339Nano, outcomeLinkedAt.String)
		if err != nil {
			tLinked, _ = time.Parse(time.RFC3339, outcomeLinkedAt.String)
		}
		rec.OutcomeLinkedAt = &tLinked
	}

	return rec, nil
}
