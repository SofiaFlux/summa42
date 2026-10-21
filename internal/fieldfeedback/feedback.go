package fieldfeedback

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

var (
	safeCategoryToken = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	safeExecutorToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

type CandidateInput struct {
	ObservationIDs    []domain.ID
	GenericTaskClass  domain.GenericTaskClass
	Category          string
	ExpectedBehavior  string
	ObservedBehavior  string
	StateTransitions  []string
	Metrics           NormalizedMetrics
	HumanIntervention bool
	RecoveryResult    string
	RuntimeVersion    string
	ExecutorKind      string
	ExecutorVersion   string
	Enforcement       domain.EnforcementLevel
}

type NormalizedMetrics struct {
	RetryCount *int64 `json:"retry_count,omitempty"`
	LatencyMs  *int64 `json:"latency_ms,omitempty"`
	CostUnits  *int64 `json:"cost_units,omitempty"`
}

type Feedback struct {
	store    *state.Store
	clock    clock.Clock
	emission *emissionRuntime
}

func NewFeedback(store *state.Store, clk clock.Clock) *Feedback {
	return &Feedback{store: store, clock: clk}
}

func (s *Feedback) CreateCandidate(ctx context.Context, input CandidateInput) (domain.FeedbackCandidate, error) {
	if err := s.configured(); err != nil {
		return domain.FeedbackCandidate{}, err
	}
	normalized, transitionsJSON, metricsJSON, correlationKey, err := normalizeCandidateInput(input)
	if err != nil {
		return domain.FeedbackCandidate{}, err
	}

	now := s.clock.Now().UTC()
	result := domain.FeedbackCandidate{
		ID: domain.NewID("feedback"), State: domain.FeedbackStateCandidate,
		GenericTaskClass: normalized.GenericTaskClass, Category: normalized.Category,
		ExpectedBehavior: normalized.ExpectedBehavior, ObservedBehavior: normalized.ObservedBehavior,
		StateTransitionJSON: transitionsJSON, MetricsJSON: metricsJSON,
		HumanIntervention: normalized.HumanIntervention, RecoveryResult: normalized.RecoveryResult,
		RuntimeVersion: normalized.RuntimeVersion, ExecutorKind: normalized.ExecutorKind,
		ExecutorVersion: normalized.ExecutorVersion, Enforcement: normalized.Enforcement,
		CorrelationKey: correlationKey, CreatedAt: now,
	}

	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		for _, observationID := range normalized.ObservationIDs {
			var exists int
			if err := tx.QueryRowContext(ctx,
				"SELECT 1 FROM field_observations WHERE observation_id = ?", observationID,
			).Scan(&exists); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("observation %s not found", observationID)
				}
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO feedback_candidates("+
				"candidate_id, state, generic_task_class, category, expected_behavior, observed_behavior, "+
				"state_transition_json, metrics_json, human_intervention, recovery_result, runtime_version, "+
				"executor_kind, executor_version, enforcement, correlation_key, created_at, updated_at"+
				") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			result.ID, result.State, result.GenericTaskClass, result.Category,
			result.ExpectedBehavior, result.ObservedBehavior, result.StateTransitionJSON, result.MetricsJSON,
			boolToInt(result.HumanIntervention), result.RecoveryResult, result.RuntimeVersion,
			result.ExecutorKind, result.ExecutorVersion, result.Enforcement, result.CorrelationKey,
			formatTime(now), formatTime(now),
		); err != nil {
			return fmt.Errorf("insert feedback candidate: %w", err)
		}
		for _, observationID := range normalized.ObservationIDs {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO feedback_candidate_observations(candidate_id, observation_id, linked_at) VALUES (?, ?, ?)",
				result.ID, observationID, formatTime(now),
			); err != nil {
				return fmt.Errorf("link feedback candidate observation: %w", err)
			}
		}
		return nil
	})
	return result, err
}

func (s *Feedback) Candidate(ctx context.Context, id domain.ID) (domain.FeedbackCandidate, error) {
	if err := s.configured(); err != nil {
		return domain.FeedbackCandidate{}, err
	}
	id = domain.ID(strings.TrimSpace(string(id)))
	if id == "" {
		return domain.FeedbackCandidate{}, errors.New("candidate id is required")
	}
	return loadCandidate(ctx, s.store.DB(), id)
}

func (s *Feedback) Candidates(ctx context.Context, stateValue domain.FeedbackCandidateState) ([]domain.FeedbackCandidate, error) {
	if err := s.configured(); err != nil {
		return nil, err
	}
	query := "SELECT candidate_id FROM feedback_candidates"
	args := []any{}
	if stateValue != "" {
		if !validCandidateState(stateValue) {
			return nil, fmt.Errorf("invalid feedback candidate state %q", stateValue)
		}
		query += " WHERE state = ?"
		args = append(args, stateValue)
	}
	query += " ORDER BY created_at, candidate_id"
	rows, err := s.store.DB().QueryContext(ctx, query, args...)
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
	out := make([]domain.FeedbackCandidate, 0, len(ids))
	for _, id := range ids {
		candidate, err := s.Candidate(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, candidate)
	}
	return out, nil
}

func (s *Feedback) transitionCandidate(ctx context.Context, id domain.ID, to domain.FeedbackCandidateState) error {
	if err := s.configured(); err != nil {
		return err
	}
	return s.store.WithTx(ctx, func(tx *sql.Tx) error {
		return s.transitionCandidateInTx(ctx, tx, id, to)
	})
}

func (s *Feedback) transitionCandidateInTx(ctx context.Context, tx *sql.Tx, id domain.ID, to domain.FeedbackCandidateState) error {
	if tx == nil {
		return errors.New("candidate transition requires transaction")
	}
	id = domain.ID(strings.TrimSpace(string(id)))
	if id == "" || !validCandidateState(to) {
		return errors.New("candidate id and valid target state are required")
	}
	current, err := loadCandidate(ctx, tx, id)
	if err != nil {
		return err
	}
	if current.State == to {
		return nil
	}
	if !candidateTransitionAllowed(current.State, to) {
		return fmt.Errorf("illegal feedback candidate transition %s -> %s", current.State, to)
	}
	result, err := tx.ExecContext(ctx,
		"UPDATE feedback_candidates SET state = ?, updated_at = ? WHERE candidate_id = ? AND state = ?",
		to, formatTime(s.clock.Now().UTC()), id, current.State,
	)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("feedback candidate was concurrently changed")
	}
	return nil
}

func normalizeCandidateInput(input CandidateInput) (CandidateInput, string, string, string, error) {
	input.ObservationIDs = cleanIDs(input.ObservationIDs)
	input.Category = strings.TrimSpace(input.Category)
	input.ExpectedBehavior = strings.TrimSpace(input.ExpectedBehavior)
	input.ObservedBehavior = strings.TrimSpace(input.ObservedBehavior)
	input.RecoveryResult = strings.TrimSpace(input.RecoveryResult)
	input.RuntimeVersion = strings.TrimSpace(input.RuntimeVersion)
	input.ExecutorKind = strings.TrimSpace(input.ExecutorKind)
	input.ExecutorVersion = strings.TrimSpace(input.ExecutorVersion)

	if len(input.ObservationIDs) == 0 {
		return CandidateInput{}, "", "", "", errors.New("candidate requires at least one observation")
	}
	if !input.GenericTaskClass.Valid() {
		return CandidateInput{}, "", "", "", fmt.Errorf("invalid privacy-safe generic task class %q", input.GenericTaskClass)
	}
	if input.Category == "" || !safeCategoryToken.MatchString(input.Category) {
		return CandidateInput{}, "", "", "", errors.New("candidate category must be a privacy-safe categorical token")
	}
	if input.ExpectedBehavior == "" || input.ObservedBehavior == "" {
		return CandidateInput{}, "", "", "", errors.New("candidate expected and observed behavior are required")
	}
	if input.ExecutorKind != "" && !safeExecutorToken.MatchString(input.ExecutorKind) {
		return CandidateInput{}, "", "", "", errors.New("executor kind must be a privacy-safe categorical token")
	}
	switch input.Enforcement {
	case domain.EnforcementEnforced, domain.EnforcementPartial, domain.EnforcementUnenforced:
	default:
		return CandidateInput{}, "", "", "", fmt.Errorf("invalid enforcement level %q", input.Enforcement)
	}
	for _, value := range []*int64{input.Metrics.RetryCount, input.Metrics.LatencyMs, input.Metrics.CostUnits} {
		if value != nil && *value < 0 {
			return CandidateInput{}, "", "", "", errors.New("normalized candidate metrics cannot be negative")
		}
	}

	transitions := make([]string, 0, len(input.StateTransitions))
	for _, transition := range input.StateTransitions {
		transition = strings.TrimSpace(transition)
		if !safeTransitionToken(transition) {
			return CandidateInput{}, "", "", "", fmt.Errorf("unsafe or unknown state transition token %q", transition)
		}
		transitions = append(transitions, transition)
	}
	input.StateTransitions = transitions

	transitionsEncoded, err := json.Marshal(transitions)
	if err != nil {
		return CandidateInput{}, "", "", "", err
	}
	metricsEncoded, err := json.Marshal(input.Metrics)
	if err != nil {
		return CandidateInput{}, "", "", "", err
	}
	correlationKey, err := candidateCorrelationKey(input.Category, input.GenericTaskClass, input.ExecutorKind, input.Enforcement, transitions)
	if err != nil {
		return CandidateInput{}, "", "", "", err
	}
	return input, string(transitionsEncoded), string(metricsEncoded), correlationKey, nil
}

func candidateCorrelationKey(
	category string,
	taskClass domain.GenericTaskClass,
	executorKind string,
	enforcement domain.EnforcementLevel,
	transitions []string,
) (string, error) {
	material := struct {
		Category         string                  `json:"category"`
		GenericTaskClass domain.GenericTaskClass `json:"generic_task_class"`
		ExecutorKind     string                  `json:"executor_kind,omitempty"`
		Enforcement      domain.EnforcementLevel `json:"enforcement"`
		StateTransitions []string                `json:"state_transitions"`
	}{
		Category: category, GenericTaskClass: taskClass, ExecutorKind: executorKind,
		Enforcement: enforcement, StateTransitions: append([]string(nil), transitions...),
	}
	encoded, err := json.Marshal(material)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

var safeTransitionTokens = map[string]struct{}{
	"CREATED": {}, "ELIGIBLE": {}, "EXECUTING": {}, "AWAITING_VERIFICATION": {},
	"SUCCEEDED": {}, "FAILED": {}, "BLOCKED": {}, "CANCELLED": {}, "CHALLENGED": {}, "EXPIRED": {},
	"LEASED": {}, "RUNNING": {}, "COMPLETED": {},
	"ACTIVE": {}, "REVOKED": {},
	"PREPARED": {}, "DISPATCHED": {}, "CONFIRMED_EFFECT": {}, "CONFIRMED_NO_EFFECT": {}, "OUTCOME_UNKNOWN": {},
	"PENDING": {}, "APPROVED": {}, "REJECTED": {}, "CONSUMED": {},
	"OPERATION_PREPARED": {}, "OPERATION_DISPATCH_COMMITTED": {}, "OPERATION_OUTCOME_UNKNOWN": {},
	"OPERATION_OUTCOME_SETTLED": {}, "OPERATION_CANCELLED_BEFORE_DISPATCH": {},
	"HUMAN_INTERVENTION": {}, "VERIFICATION_ACCEPTED": {}, "VERIFICATION_REJECTED": {},
}

func safeTransitionToken(token string) bool {
	_, ok := safeTransitionTokens[token]
	return ok
}

func candidateTransitionAllowed(from, to domain.FeedbackCandidateState) bool {
	switch from {
	case domain.FeedbackStateCandidate:
		return to == domain.FeedbackStateSanitizing || to == domain.FeedbackStateLocalOnly
	case domain.FeedbackStateSanitizing:
		return to == domain.FeedbackStateSanitized || to == domain.FeedbackStateRejectedUnsafe || to == domain.FeedbackStateLocalOnly
	case domain.FeedbackStateSanitized:
		return to == domain.FeedbackStateSanitizing || to == domain.FeedbackStateApprovalPending || to == domain.FeedbackStateExportReady ||
			to == domain.FeedbackStateDuplicate || to == domain.FeedbackStateLocalOnly
	case domain.FeedbackStateApprovalPending:
		return to == domain.FeedbackStateExportReady || to == domain.FeedbackStateRejectedPolicy ||
			to == domain.FeedbackStateDuplicate || to == domain.FeedbackStateLocalOnly
	case domain.FeedbackStateExportReady:
		return to == domain.FeedbackStateReported || to == domain.FeedbackStateRejectedPolicy ||
			to == domain.FeedbackStateDuplicate || to == domain.FeedbackStateLocalOnly
	default:
		return false
	}
}

func validCandidateState(value domain.FeedbackCandidateState) bool {
	switch value {
	case domain.FeedbackStateCandidate, domain.FeedbackStateSanitizing, domain.FeedbackStateSanitized,
		domain.FeedbackStateApprovalPending, domain.FeedbackStateExportReady, domain.FeedbackStateReported,
		domain.FeedbackStateLocalOnly, domain.FeedbackStateRejectedUnsafe, domain.FeedbackStateRejectedPolicy,
		domain.FeedbackStateDuplicate:
		return true
	default:
		return false
	}
}

type candidateQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadCandidate(ctx context.Context, q candidateQuery, id domain.ID) (domain.FeedbackCandidate, error) {
	var out domain.FeedbackCandidate
	var human int
	var createdAt string
	err := q.QueryRowContext(ctx,
		"SELECT candidate_id, state, generic_task_class, category, expected_behavior, observed_behavior, "+
			"state_transition_json, metrics_json, human_intervention, recovery_result, runtime_version, "+
			"executor_kind, executor_version, enforcement, correlation_key, created_at "+
			"FROM feedback_candidates WHERE candidate_id = ?", id,
	).Scan(&out.ID, &out.State, &out.GenericTaskClass, &out.Category, &out.ExpectedBehavior,
		&out.ObservedBehavior, &out.StateTransitionJSON, &out.MetricsJSON, &human, &out.RecoveryResult,
		&out.RuntimeVersion, &out.ExecutorKind, &out.ExecutorVersion, &out.Enforcement,
		&out.CorrelationKey, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.FeedbackCandidate{}, fmt.Errorf("feedback candidate %s not found", id)
	}
	if err != nil {
		return domain.FeedbackCandidate{}, err
	}
	out.HumanIntervention = human == 1
	out.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return domain.FeedbackCandidate{}, fmt.Errorf("parse candidate created_at: %w", err)
	}
	return out, nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *Feedback) configured() error {
	if s == nil || s.store == nil || s.store.DB() == nil || s.clock == nil {
		return errors.New("feedback service is not configured")
	}
	return nil
}
