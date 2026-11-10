package dtu

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

var (
	// ErrShadowSideEffectDenied is returned when a shadow execution attempt attempts an external mutation or unallowed side effect.
	ErrShadowSideEffectDenied = errors.New("shadow execution denied external side-effect authority")

	// ErrPromotionRecordNotFound is returned when a requested promotion record is not found.
	ErrPromotionRecordNotFound = errors.New("promotion record not found")

	// ErrDuplicatePromotionRecord is returned when attempting to insert an already existing promotion record.
	ErrDuplicatePromotionRecord = errors.New("promotion record already exists: append-only audit invariant")

	// ErrInvalidRollbackState is returned when attempting to rollback a record that was not promoted or is already rolled back.
	ErrInvalidRollbackState = errors.New("invalid rollback state: only PROMOTED active records can be rolled back")

	// ErrRollbackReasonRequired is returned when an empty reason is provided for rollback.
	ErrRollbackReasonRequired = errors.New("rollback reason is required")

	// ErrEmptyCorpus is returned when no valid scenario requests are provided for the targeted scope.
	ErrEmptyCorpus = errors.New("corpus cannot be empty or have zero matching requests for scope")
)

// PromotionStatus represents the outcome of an autonomous promotion gate evaluation.
type PromotionStatus string

const (
	PromotionStatusPromoted PromotionStatus = "PROMOTED"
	PromotionStatusRejected PromotionStatus = "REJECTED"
	PromotionStatusVetoed   PromotionStatus = "VETOED"
)

// PromotionScope configures granular promotion boundaries (TaskClass, capability, risk tier).
type PromotionScope struct {
	TaskClass  string `json:"task_class,omitempty"`
	RiskTier   string `json:"risk_tier,omitempty"`
	Capability string `json:"capability,omitempty"`
}

func (s PromotionScope) Matches(req ScenarioEvaluationRequest) bool {
	if s.TaskClass != "" && s.TaskClass != req.TaskClass {
		return false
	}
	if s.RiskTier != "" && s.RiskTier != req.RiskTier {
		return false
	}
	if s.Capability != "" && s.Capability != req.Capability {
		return false
	}
	return true
}

// PromotionPolicy sets multidimensional thresholds for candidate promotion.
type PromotionPolicy struct {
	MinAcceptanceRate    float64 `json:"min_acceptance_rate"`
	MaxAllowedCostDelta  float64 `json:"max_allowed_cost_delta"`
	MaxLatencyRegression float64 `json:"max_latency_regression"`
}

// DefaultPromotionPolicy returns standard evaluation gates.
func DefaultPromotionPolicy() PromotionPolicy {
	return PromotionPolicy{
		MinAcceptanceRate:    0.80,
		MaxAllowedCostDelta:  0.0,
		MaxLatencyRegression: 0.20,
	}
}

// ScenarioEvaluationRequest describes a single DTU scenario run for comparative evaluation.
type ScenarioEvaluationRequest struct {
	ScenarioID string `json:"scenario_id"`
	TaskClass  string `json:"task_class,omitempty"`
	RiskTier   string `json:"risk_tier,omitempty"`
	Capability string `json:"capability,omitempty"`
}

// ExecutionEvaluation captures multi-dimensional telemetry for a candidate's scenario run.
type ExecutionEvaluation struct {
	ScenarioID           string        `json:"scenario_id"`
	TaskClass            string        `json:"task_class,omitempty"`
	RiskTier             string        `json:"risk_tier,omitempty"`
	Capability           string        `json:"capability,omitempty"`
	AcceptancePassed     bool          `json:"acceptance_passed"`
	HardInvariantsPassed bool          `json:"hard_invariants_passed"`
	InvariantsViolated   []string      `json:"invariants_violated,omitempty"`
	CostTokens           int64         `json:"cost_tokens"`
	Latency              time.Duration `json:"latency"`
	FailureClass         string        `json:"failure_class,omitempty"`
}

// MultiDimensionalReport summarizes comparative metrics between Blue (Champion) and Green (Challenger).
type MultiDimensionalReport struct {
	BlueAcceptanceRate  float64       `json:"blue_acceptance_rate"`
	GreenAcceptanceRate float64       `json:"green_acceptance_rate"`
	BlueViolations      int           `json:"blue_violations"`
	GreenViolations     int           `json:"green_violations"`
	BlueAvgCost         int64         `json:"blue_avg_cost"`
	GreenAvgCost        int64         `json:"green_avg_cost"`
	BlueAvgLatency      time.Duration `json:"blue_avg_latency"`
	GreenAvgLatency     time.Duration `json:"green_avg_latency"`
}

// PromotionDecision captures the gate's verdict, reason, and any safety vetoes.
type PromotionDecision struct {
	Status PromotionStatus `json:"status"`
	Reason string          `json:"reason"`
	Vetoes []string        `json:"vetoes,omitempty"`
	Scope  PromotionScope  `json:"scope"`
}

// PromotionProvenanceRecord is the durable audit record linking decisions to evidence hashes.
type PromotionProvenanceRecord struct {
	RecordID          string            `json:"record_id"`
	ChampionID        string            `json:"champion_id"`
	ChallengerID      string            `json:"challenger_id"`
	Decision          PromotionDecision `json:"decision"`
	EvidenceHash      string            `json:"evidence_hash"`
	Scope             PromotionScope    `json:"scope"`
	Timestamp         time.Time         `json:"timestamp"`
	RolledBack        bool              `json:"rolled_back"`
	RollbackReason    string            `json:"rollback_reason,omitempty"`
	RollbackTimestamp time.Time         `json:"rollback_timestamp,omitempty"`
}

func cloneRecord(r PromotionProvenanceRecord) PromotionProvenanceRecord {
	cp := r
	if r.Decision.Vetoes != nil {
		cp.Decision.Vetoes = append([]string(nil), r.Decision.Vetoes...)
	}
	return cp
}

// PromotionStore abstracts durable persistence for promotion records.
type PromotionStore interface {
	Save(record PromotionProvenanceRecord) error
	Get(recordID string) (PromotionProvenanceRecord, error)
	ListByChallenger(challengerID string) ([]PromotionProvenanceRecord, error)
	Update(record PromotionProvenanceRecord) error
}

// InMemoryPromotionStore provides thread-safe in-memory storage for promotion records.
type InMemoryPromotionStore struct {
	mu      sync.RWMutex
	records map[string]PromotionProvenanceRecord
}

func NewInMemoryPromotionStore() *InMemoryPromotionStore {
	return &InMemoryPromotionStore{
		records: make(map[string]PromotionProvenanceRecord),
	}
}

func (s *InMemoryPromotionStore) Save(record PromotionProvenanceRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.records[record.RecordID]; exists {
		return ErrDuplicatePromotionRecord
	}
	s.records[record.RecordID] = cloneRecord(record)
	return nil
}

func (s *InMemoryPromotionStore) Get(recordID string) (PromotionProvenanceRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.records[recordID]
	if !ok {
		return PromotionProvenanceRecord{}, ErrPromotionRecordNotFound
	}
	return cloneRecord(r), nil
}

func (s *InMemoryPromotionStore) ListByChallenger(challengerID string) ([]PromotionProvenanceRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var list []PromotionProvenanceRecord
	for _, r := range s.records {
		if r.ChallengerID == challengerID {
			list = append(list, cloneRecord(r))
		}
	}
	return list, nil
}

func (s *InMemoryPromotionStore) Update(record PromotionProvenanceRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[record.RecordID]; !ok {
		return ErrPromotionRecordNotFound
	}
	s.records[record.RecordID] = cloneRecord(record)
	return nil
}

// SQLitePromotionStore provides durable persistence in SQLite.
type SQLitePromotionStore struct {
	db *sql.DB
	mu sync.RWMutex
}

func NewSQLitePromotionStore(db *sql.DB) (*SQLitePromotionStore, error) {
	s := &SQLitePromotionStore{db: db}
	if err := s.initSchema(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SQLitePromotionStore) initSchema() error {
	schema := `
	CREATE TABLE IF NOT EXISTS promotion_provenance (
		record_id TEXT PRIMARY KEY,
		champion_id TEXT NOT NULL,
		challenger_id TEXT NOT NULL,
		decision_status TEXT NOT NULL,
		decision_reason TEXT NOT NULL,
		vetoes_json TEXT,
		scope_json TEXT NOT NULL,
		evidence_hash TEXT NOT NULL,
		rolled_back INTEGER NOT NULL DEFAULT 0,
		rollback_reason TEXT,
		rollback_timestamp DATETIME,
		created_at DATETIME NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_promo_challenger ON promotion_provenance(challenger_id);
	`
	_, err := s.db.Exec(schema)
	return err
}

func (s *SQLitePromotionStore) Save(record PromotionProvenanceRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	vetoesJSON, err := json.Marshal(record.Decision.Vetoes)
	if err != nil {
		return err
	}
	scopeJSON, err := json.Marshal(record.Scope)
	if err != nil {
		return err
	}

	query := `
	INSERT INTO promotion_provenance (
		record_id, champion_id, challenger_id, decision_status, decision_reason,
		vetoes_json, scope_json, evidence_hash, rolled_back, rollback_reason,
		rollback_timestamp, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
	`
	var rolledBackInt int
	if record.RolledBack {
		rolledBackInt = 1
	}

	_, err = s.db.Exec(query,
		record.RecordID, record.ChampionID, record.ChallengerID,
		string(record.Decision.Status), record.Decision.Reason,
		string(vetoesJSON), string(scopeJSON), record.EvidenceHash,
		rolledBackInt, record.RollbackReason, record.RollbackTimestamp,
		record.Timestamp,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrDuplicatePromotionRecord
		}
		return err
	}
	return nil
}

func (s *SQLitePromotionStore) Get(recordID string) (PromotionProvenanceRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
	SELECT record_id, champion_id, challenger_id, decision_status, decision_reason,
	       vetoes_json, scope_json, evidence_hash, rolled_back, rollback_reason,
	       rollback_timestamp, created_at
	FROM promotion_provenance WHERE record_id = ?;
	`
	row := s.db.QueryRow(query, recordID)

	var rec PromotionProvenanceRecord
	var statusStr, vetoesJSON, scopeJSON string
	var rolledBackInt int
	var rollbackReason sql.NullString
	var rollbackTime sql.NullTime

	err := row.Scan(
		&rec.RecordID, &rec.ChampionID, &rec.ChallengerID, &statusStr, &rec.Decision.Reason,
		&vetoesJSON, &scopeJSON, &rec.EvidenceHash, &rolledBackInt, &rollbackReason,
		&rollbackTime, &rec.Timestamp,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PromotionProvenanceRecord{}, ErrPromotionRecordNotFound
		}
		return PromotionProvenanceRecord{}, err
	}

	rec.Decision.Status = PromotionStatus(statusStr)
	rec.RolledBack = (rolledBackInt == 1)
	if rollbackReason.Valid {
		rec.RollbackReason = rollbackReason.String
	}
	if rollbackTime.Valid {
		rec.RollbackTimestamp = rollbackTime.Time
	}

	if err := json.Unmarshal([]byte(vetoesJSON), &rec.Decision.Vetoes); err != nil {
		rec.Decision.Vetoes = []string{}
	}
	if err := json.Unmarshal([]byte(scopeJSON), &rec.Scope); err != nil {
		return PromotionProvenanceRecord{}, err
	}
	rec.Decision.Scope = rec.Scope

	return rec, nil
}

func (s *SQLitePromotionStore) ListByChallenger(challengerID string) ([]PromotionProvenanceRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
	SELECT record_id, champion_id, challenger_id, decision_status, decision_reason,
	       vetoes_json, scope_json, evidence_hash, rolled_back, rollback_reason,
	       rollback_timestamp, created_at
	FROM promotion_provenance WHERE challenger_id = ? ORDER BY created_at ASC;
	`
	rows, err := s.db.Query(query, challengerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []PromotionProvenanceRecord
	for rows.Next() {
		var rec PromotionProvenanceRecord
		var statusStr, vetoesJSON, scopeJSON string
		var rolledBackInt int
		var rollbackReason sql.NullString
		var rollbackTime sql.NullTime

		if err := rows.Scan(
			&rec.RecordID, &rec.ChampionID, &rec.ChallengerID, &statusStr, &rec.Decision.Reason,
			&vetoesJSON, &scopeJSON, &rec.EvidenceHash, &rolledBackInt, &rollbackReason,
			&rollbackTime, &rec.Timestamp,
		); err != nil {
			return nil, err
		}

		rec.Decision.Status = PromotionStatus(statusStr)
		rec.RolledBack = (rolledBackInt == 1)
		if rollbackReason.Valid {
			rec.RollbackReason = rollbackReason.String
		}
		if rollbackTime.Valid {
			rec.RollbackTimestamp = rollbackTime.Time
		}

		_ = json.Unmarshal([]byte(vetoesJSON), &rec.Decision.Vetoes)
		_ = json.Unmarshal([]byte(scopeJSON), &rec.Scope)
		rec.Decision.Scope = rec.Scope

		list = append(list, rec)
	}

	return list, nil
}

func (s *SQLitePromotionStore) Update(record PromotionProvenanceRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var rolledBackInt int
	if record.RolledBack {
		rolledBackInt = 1
	}

	query := `
	UPDATE promotion_provenance SET
		rolled_back = ?, rollback_reason = ?, rollback_timestamp = ?
	WHERE record_id = ?;
	`
	res, err := s.db.Exec(query, rolledBackInt, record.RollbackReason, record.RollbackTimestamp, record.RecordID)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrPromotionRecordNotFound
	}
	return nil
}

// PromotionGate evaluates candidates against champions and enforces hard invariant vetoes.
type PromotionGate struct {
	store  PromotionStore
	policy PromotionPolicy
}

func NewPromotionGate(store PromotionStore) *PromotionGate {
	return NewPromotionGateWithPolicy(store, DefaultPromotionPolicy())
}

func NewPromotionGateWithPolicy(store PromotionStore, policy PromotionPolicy) *PromotionGate {
	return &PromotionGate{
		store:  store,
		policy: policy,
	}
}

func (g *PromotionGate) Store() PromotionStore {
	return g.store
}

// EvaluateDecision computes the promotion verdict based on multi-dimensional telemetry, policy, and hard vetoes.
func (g *PromotionGate) EvaluateDecision(report MultiDimensionalReport, scope PromotionScope, championID, challengerID string) (PromotionDecision, string, error) {
	var vetoes []string

	// Hard Invariant VETO: candidate regressed on safety/containment invariants
	if report.GreenViolations > 0 {
		vetoes = append(vetoes, fmt.Sprintf("HARD_INVARIANT_REGRESSION: green had %d invariant violations", report.GreenViolations))
	}

	// Acceptance regression veto: candidate acceptance must not regress below champion
	if report.GreenAcceptanceRate < report.BlueAcceptanceRate {
		vetoes = append(vetoes, fmt.Sprintf("ACCEPTANCE_REGRESSION: green rate %.2f < blue rate %.2f", report.GreenAcceptanceRate, report.BlueAcceptanceRate))
	}

	// Absolute acceptance floor check: 0% acceptance or failing minimum threshold MUST NOT promote
	if report.GreenAcceptanceRate < g.policy.MinAcceptanceRate {
		vetoes = append(vetoes, fmt.Sprintf("BELOW_MINIMUM_ACCEPTANCE: green rate %.2f < minimum required %.2f", report.GreenAcceptanceRate, g.policy.MinAcceptanceRate))
	}

	// Latency regression check
	if report.BlueAvgLatency > 0 && g.policy.MaxLatencyRegression >= 0 {
		maxAllowedLatency := time.Duration(float64(report.BlueAvgLatency) * (1.0 + g.policy.MaxLatencyRegression))
		if report.GreenAvgLatency > maxAllowedLatency {
			vetoes = append(vetoes, fmt.Sprintf("LATENCY_REGRESSION: green latency %s exceeded max allowed %s", report.GreenAvgLatency, maxAllowedLatency))
		}
	}

	evidenceBytes, err := json.Marshal(struct {
		Report MultiDimensionalReport `json:"report"`
		Scope  PromotionScope         `json:"scope"`
		Champ  string                 `json:"champ"`
		Chal   string                 `json:"chal"`
		Vetoes []string               `json:"vetoes,omitempty"`
	}{
		Report: report,
		Scope:  scope,
		Champ:  championID,
		Chal:   challengerID,
		Vetoes: vetoes,
	})
	if err != nil {
		return PromotionDecision{}, "", fmt.Errorf("failed to marshal evidence for hashing: %w", err)
	}

	hash := sha256.Sum256(evidenceBytes)
	evidenceHash := hex.EncodeToString(hash[:])

	if len(vetoes) > 0 {
		return PromotionDecision{
			Status: PromotionStatusVetoed,
			Reason: "Hard invariant, acceptance, or latency regression vetoes encountered",
			Vetoes: vetoes,
			Scope:  scope,
		}, evidenceHash, nil
	}

	// Compare cost & acceptance
	if report.GreenAvgCost <= report.BlueAvgCost && report.GreenAcceptanceRate >= report.BlueAcceptanceRate {
		return PromotionDecision{
			Status: PromotionStatusPromoted,
			Reason: "Challenger met all invariant gates and demonstrated equal or superior performance",
			Scope:  scope,
		}, evidenceHash, nil
	}

	return PromotionDecision{
		Status: PromotionStatusRejected,
		Reason: "Challenger did not demonstrate performance or cost superiority",
		Scope:  scope,
	}, evidenceHash, nil
}

// Rollback atomically marks a previously promoted record as rolled back.
func (g *PromotionGate) Rollback(recordID string, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ErrRollbackReasonRequired
	}

	rec, err := g.store.Get(recordID)
	if err != nil {
		return err
	}
	if rec.Decision.Status != PromotionStatusPromoted || rec.RolledBack {
		return ErrInvalidRollbackState
	}

	rec.RolledBack = true
	rec.RollbackReason = reason
	rec.RollbackTimestamp = time.Now().UTC()
	return g.store.Update(rec)
}

// CandidateExecutor runs a candidate against a scenario evaluation request.
type CandidateExecutor func(ctx context.Context, req ScenarioEvaluationRequest) ExecutionEvaluation

// BlueGreenEngine orchestrates concurrent evaluations of Champion (Blue) and Challenger (Green).
type BlueGreenEngine struct {
	blueExec  CandidateExecutor
	greenExec CandidateExecutor
	gate      *PromotionGate
}

func NewBlueGreenEngine(blue, green CandidateExecutor, gate *PromotionGate) *BlueGreenEngine {
	if blue == nil || green == nil || gate == nil {
		panic("executors and gate cannot be nil")
	}
	return &BlueGreenEngine{
		blueExec:  blue,
		greenExec: green,
		gate:      gate,
	}
}

// EvaluateAndDecide executes the scoped scenario corpus across both candidates and records durable provenance.
func (e *BlueGreenEngine) EvaluateAndDecide(ctx context.Context, corpus []ScenarioEvaluationRequest, championID, challengerID string, scope PromotionScope) (MultiDimensionalReport, PromotionDecision, error) {
	if err := ctx.Err(); err != nil {
		return MultiDimensionalReport{}, PromotionDecision{}, err
	}

	// Filter corpus strictly by targeted promotion scope
	var scopedCorpus []ScenarioEvaluationRequest
	for _, req := range corpus {
		if scope.Matches(req) {
			scopedCorpus = append(scopedCorpus, req)
		}
	}
	if len(scopedCorpus) == 0 {
		return MultiDimensionalReport{}, PromotionDecision{}, ErrEmptyCorpus
	}

	var bluePassed, greenPassed int
	var blueViolations, greenViolations int
	var blueTotalCost, greenTotalCost int64
	var blueTotalLatency, greenTotalLatency time.Duration

	var wg sync.WaitGroup
	blueResults := make([]ExecutionEvaluation, len(scopedCorpus))
	greenResults := make([]ExecutionEvaluation, len(scopedCorpus))

	// Concurrent corpus execution
	wg.Add(len(scopedCorpus) * 2)
	for i, req := range scopedCorpus {
		go func(idx int, r ScenarioEvaluationRequest) {
			defer wg.Done()
			blueResults[idx] = e.blueExec(ctx, r)
		}(i, req)

		go func(idx int, r ScenarioEvaluationRequest) {
			defer wg.Done()
			greenResults[idx] = e.greenExec(ctx, r)
		}(i, req)
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return MultiDimensionalReport{}, PromotionDecision{}, err
	}

	for i := range scopedCorpus {
		b := blueResults[i]
		g := greenResults[i]

		if b.AcceptancePassed {
			bluePassed++
		}
		if !b.HardInvariantsPassed || len(b.InvariantsViolated) > 0 {
			blueViolations += len(b.InvariantsViolated)
			if len(b.InvariantsViolated) == 0 {
				blueViolations++
			}
		}
		blueTotalCost += b.CostTokens
		blueTotalLatency += b.Latency

		if g.AcceptancePassed {
			greenPassed++
		}
		if !g.HardInvariantsPassed || len(g.InvariantsViolated) > 0 {
			greenViolations += len(g.InvariantsViolated)
			if len(g.InvariantsViolated) == 0 {
				greenViolations++
			}
		}
		greenTotalCost += g.CostTokens
		greenTotalLatency += g.Latency
	}

	n := float64(len(scopedCorpus))
	report := MultiDimensionalReport{
		BlueAcceptanceRate:  float64(bluePassed) / n,
		GreenAcceptanceRate: float64(greenPassed) / n,
		BlueViolations:      blueViolations,
		GreenViolations:     greenViolations,
		BlueAvgCost:         blueTotalCost / int64(len(scopedCorpus)),
		GreenAvgCost:        greenTotalCost / int64(len(scopedCorpus)),
		BlueAvgLatency:      blueTotalLatency / time.Duration(len(scopedCorpus)),
		GreenAvgLatency:     greenTotalLatency / time.Duration(len(scopedCorpus)),
	}

	decision, evidenceHash, err := e.gate.EvaluateDecision(report, scope, championID, challengerID)
	if err != nil {
		return report, decision, err
	}

	// Persist durable record with cryptographically unique domain ID
	recordID := string(domain.NewID(fmt.Sprintf("promo_%s_%s", championID, challengerID)))
	record := PromotionProvenanceRecord{
		RecordID:     recordID,
		ChampionID:   championID,
		ChallengerID: challengerID,
		Decision:     decision,
		EvidenceHash: evidenceHash,
		Scope:        scope,
		Timestamp:    time.Now().UTC(),
	}
	if err := e.gate.Store().Save(record); err != nil {
		return report, decision, fmt.Errorf("failed to save promotion provenance: %w", err)
	}

	return report, decision, nil
}

// ShadowExecutionRouter mediates authoritative live executions and strictly side-effect-denied shadow evaluations.
type ShadowExecutionRouter struct {
	liveProv             EnvironmentProvider
	allowedShadowActions map[string]bool
}

// NewShadowExecutionRouter creates a new ShadowExecutionRouter with fail-closed security policy.
func NewShadowExecutionRouter(liveProv EnvironmentProvider) *ShadowExecutionRouter {
	if liveProv == nil {
		panic("liveProv cannot be nil")
	}
	return &ShadowExecutionRouter{
		liveProv: liveProv,
		allowedShadowActions: map[string]bool{
			"get":         true,
			"read":        true,
			"query":       true,
			"list":        true,
			"inspect":     true,
			"describe":    true,
			"check":       true,
			"get_balance": true,
		},
	}
}

// ExecuteAuthoritative executes an operation with full live side-effect authority.
func (r *ShadowExecutionRouter) ExecuteAuthoritative(ctx context.Context, req InvocationRequest) (InvocationResponse, error) {
	return r.liveProv.Execute(ctx, req)
}

// ExecuteShadow executes an operation under shadow mode.
// Invariant: Non-read-only actions are strictly intercepted and blocked with ErrShadowSideEffectDenied (Fail-Closed).
func (r *ShadowExecutionRouter) ExecuteShadow(ctx context.Context, req InvocationRequest) (InvocationResponse, error) {
	act := strings.ToLower(strings.TrimSpace(req.Action))

	// Fail-closed: action MUST be explicitly registered as an allowed read-only action
	if !r.allowedShadowActions[act] {
		return InvocationResponse{
			Success: false,
			Error:   ErrShadowSideEffectDenied.Error(),
		}, ErrShadowSideEffectDenied
	}

	return r.liveProv.Execute(ctx, req)
}
