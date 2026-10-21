package fieldfeedback

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

type detectorCandidate struct {
	key   string
	input ObservationInput
}

type detectorEnforcementContextKey struct{}

func (s *Observer) Detect(ctx context.Context, input DetectorInput) ([]domain.FieldObservation, error) {
	if err := s.configured(); err != nil {
		return nil, err
	}
	input.TaskID = domain.ID(strings.TrimSpace(string(input.TaskID)))
	enforcement, err := s.loadDetectorEnforcement(ctx, input.TaskID)
	if err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, detectorEnforcementContextKey{}, enforcement)
	detectors := []func(context.Context, domain.ID) ([]detectorCandidate, error){
		s.detectRepeatedFailures,
		s.detectAttemptRecovery,
		s.detectPolicyAuthorityFriction,
		s.detectUnresolvedOperations,
		s.detectHumanIntervention,
		s.detectVerificationChallengeAfterAcceptance,
		s.detectCostLatencyOutliers,
	}
	var candidates []detectorCandidate
	for _, detector := range detectors {
		found, err := detector(ctx, input.TaskID)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, found...)
	}
	out := make([]domain.FieldObservation, 0, len(candidates))
	for _, candidate := range candidates {
		observation, err := s.recordWithDetectorKey(ctx, candidate.input, candidate.key)
		if err != nil {
			return nil, err
		}
		out = append(out, observation)
	}
	return out, nil
}

func (s *Observer) Scan(ctx context.Context) ([]domain.FieldObservation, error) {
	return s.Detect(ctx, DetectorInput{})
}

func (s *Observer) detectRepeatedFailures(ctx context.Context, taskFilter domain.ID) ([]detectorCandidate, error) {
	query := "SELECT task_id, attempt_id, signature, evidence_ids_json FROM attempt_failures"
	args := []any{}
	if taskFilter != "" {
		query += " WHERE task_id = ?"
		args = append(args, taskFilter)
	}
	query += " ORDER BY task_id, signature, created_at, failure_id"
	rows, err := s.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type group struct {
		taskID     domain.ID
		attemptID  domain.ID
		signature  string
		count      int
		evidenceID []domain.ID
	}
	groups := map[string]*group{}
	for rows.Next() {
		var taskID, attemptID domain.ID
		var signature, evidenceJSON string
		if err := rows.Scan(&taskID, &attemptID, &signature, &evidenceJSON); err != nil {
			return nil, err
		}
		key := string(taskID) + "\x00" + signature
		g := groups[key]
		if g == nil {
			g = &group{taskID: taskID, signature: signature}
			groups[key] = g
		}
		g.count++
		g.attemptID = attemptID
		evidenceIDs, err := parseIDs(evidenceJSON)
		if err != nil {
			return nil, fmt.Errorf("decode attempt failure evidence: %w", err)
		}
		g.evidenceID = append(g.evidenceID, evidenceIDs...)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []detectorCandidate
	for _, g := range groups {
		if g.count < 2 {
			continue
		}
		enforcement, err := s.taskEnforcement(ctx, g.taskID)
		if err != nil {
			return nil, err
		}
		out = append(out, detectorCandidate{
			key: "repeated-attempt-failure|" + string(g.taskID) + "|" + stableHash(g.signature),
			input: ObservationInput{
				TaskID: g.taskID, AttemptID: g.attemptID, Category: "REPEATED_ATTEMPT_FAILURE",
				BasisClass: "DETERMINISTIC_RUNTIME_PATTERN", SourceKind: "ATTEMPT_FAILURES",
				SummaryLocal: fmt.Sprintf("Attempt failure signature repeated %d times: %s", g.count, g.signature),
				Metrics:      map[string]any{"failure_count": g.count}, Enforcement: enforcement,
				EvidenceIDs: cleanIDs(g.evidenceID),
			},
		})
	}
	return out, nil
}

func (s *Observer) detectAttemptRecovery(ctx context.Context, taskFilter domain.ID) ([]detectorCandidate, error) {
	query := "SELECT old.task_id, old.attempt_id, newer.attempt_id " +
		"FROM attempts old JOIN attempts newer ON newer.task_id = old.task_id AND newer.fence_generation = (" +
		"SELECT MIN(n2.fence_generation) FROM attempts n2 WHERE n2.task_id = old.task_id AND n2.fence_generation > old.fence_generation" +
		") WHERE old.lease_state IN ('REVOKED','EXPIRED') AND newer.attempt_id IS NOT NULL"
	args := []any{}
	if taskFilter != "" {
		query += " AND old.task_id = ?"
		args = append(args, taskFilter)
	}
	query += " ORDER BY old.task_id, old.fence_generation"
	rows, err := s.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []detectorCandidate
	for rows.Next() {
		var taskID, oldAttempt, newAttempt domain.ID
		if err := rows.Scan(&taskID, &oldAttempt, &newAttempt); err != nil {
			return nil, err
		}
		enforcement, err := s.taskEnforcement(ctx, taskID)
		if err != nil {
			return nil, err
		}
		out = append(out, detectorCandidate{
			key: "attempt-recovery|" + string(oldAttempt) + "|" + string(newAttempt),
			input: ObservationInput{
				TaskID: taskID, AttemptID: newAttempt, Category: "ATTEMPT_RECOVERY",
				BasisClass: "DETERMINISTIC_RUNTIME_PATTERN", SourceKind: "ATTEMPT_LEASES",
				SummaryLocal: fmt.Sprintf("Attempt %s was recovered by replacement %s", oldAttempt, newAttempt),
				Metrics:      map[string]any{"recovered_attempt_id": oldAttempt}, Enforcement: enforcement,
			},
		})
	}
	return out, rows.Err()
}

func (s *Observer) detectPolicyAuthorityFriction(ctx context.Context, taskFilter domain.ID) ([]detectorCandidate, error) {
	query := "SELECT task_id, count(*) FROM execution_events WHERE event_type = 'OPERATION_CANCELLED_BEFORE_DISPATCH'"
	args := []any{}
	if taskFilter != "" {
		query += " AND task_id = ?"
		args = append(args, taskFilter)
	}
	query += " GROUP BY task_id HAVING count(*) >= 2 ORDER BY task_id"
	rows, err := s.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []detectorCandidate
	for rows.Next() {
		var taskID domain.ID
		var count int
		if err := rows.Scan(&taskID, &count); err != nil {
			return nil, err
		}
		enforcement, err := s.taskEnforcement(ctx, taskID)
		if err != nil {
			return nil, err
		}
		out = append(out, detectorCandidate{
			key: "policy-authority-friction|" + string(taskID),
			input: ObservationInput{
				TaskID: taskID, Category: "POLICY_AUTHORITY_FRICTION",
				BasisClass: "DETERMINISTIC_RUNTIME_PATTERN", SourceKind: "EXECUTION_EVENTS",
				SummaryLocal: fmt.Sprintf("Consequential operations were cancelled before dispatch %d times", count),
				Metrics:      map[string]any{"denial_count": count}, Enforcement: enforcement,
			},
		})
	}
	return out, rows.Err()
}

func (s *Observer) detectUnresolvedOperations(ctx context.Context, taskFilter domain.ID) ([]detectorCandidate, error) {
	query := "SELECT operation_id, task_id FROM external_operations WHERE (state = 'OUTCOME_UNKNOWN' OR reconciliation_required = 1)"
	args := []any{}
	if taskFilter != "" {
		query += " AND task_id = ?"
		args = append(args, taskFilter)
	}
	query += " ORDER BY created_at, operation_id"
	rows, err := s.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []detectorCandidate
	for rows.Next() {
		var operationID, taskID domain.ID
		if err := rows.Scan(&operationID, &taskID); err != nil {
			return nil, err
		}
		enforcement, err := s.taskEnforcement(ctx, taskID)
		if err != nil {
			return nil, err
		}
		out = append(out, detectorCandidate{
			key: "unresolved-operation|" + string(operationID),
			input: ObservationInput{
				OperationID: operationID, Category: "UNRESOLVED_OPERATION",
				BasisClass: "DETERMINISTIC_RUNTIME_PATTERN", SourceKind: "EXTERNAL_OPERATIONS",
				SummaryLocal: "External operation requires outcome reconciliation",
				Metrics:      map[string]any{}, Enforcement: enforcement,
			},
		})
	}
	return out, rows.Err()
}

func (s *Observer) detectHumanIntervention(ctx context.Context, taskFilter domain.ID) ([]detectorCandidate, error) {
	query := "SELECT event_id, task_id, COALESCE(attempt_id, '') FROM execution_events WHERE event_type = 'HUMAN_INTERVENTION'"
	args := []any{}
	if taskFilter != "" {
		query += " AND task_id = ?"
		args = append(args, taskFilter)
	}
	query += " ORDER BY created_at, event_id"
	rows, err := s.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []detectorCandidate
	for rows.Next() {
		var eventID, taskID, attemptID domain.ID
		if err := rows.Scan(&eventID, &taskID, &attemptID); err != nil {
			return nil, err
		}
		enforcement, err := s.taskEnforcement(ctx, taskID)
		if err != nil {
			return nil, err
		}
		out = append(out, detectorCandidate{
			key: "human-intervention|" + string(eventID),
			input: ObservationInput{
				TaskID: taskID, AttemptID: attemptID, Category: "HUMAN_INTERVENTION",
				BasisClass: "EXPLICIT_OPERATOR_EVENT", SourceKind: "EXECUTION_EVENTS",
				SummaryLocal: "Human intervention was required during Collective work",
				Metrics:      map[string]any{}, Enforcement: enforcement,
			},
		})
	}
	return out, rows.Err()
}

func (s *Observer) detectVerificationChallengeAfterAcceptance(ctx context.Context, taskFilter domain.ID) ([]detectorCandidate, error) {
	query := "SELECT c.challenge_id, c.task_id, a.attempt_id, c.evidence_ids_json, a.evidence_ids_json " +
		"FROM task_challenges c JOIN acceptance_records a ON a.task_id = c.task_id " +
		"WHERE c.created_at > a.created_at AND a.evidence_ids_json <> '[]'"
	args := []any{}
	if taskFilter != "" {
		query += " AND c.task_id = ?"
		args = append(args, taskFilter)
	}
	query += " ORDER BY c.created_at, c.challenge_id"
	rows, err := s.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []detectorCandidate
	for rows.Next() {
		var challengeID, taskID, attemptID domain.ID
		var challengeEvidence, acceptanceEvidence string
		if err := rows.Scan(&challengeID, &taskID, &attemptID, &challengeEvidence, &acceptanceEvidence); err != nil {
			return nil, err
		}
		enforcement, err := s.taskEnforcement(ctx, taskID)
		if err != nil {
			return nil, err
		}
		challengeIDs, err := parseIDs(challengeEvidence)
		if err != nil {
			return nil, fmt.Errorf("decode challenge evidence: %w", err)
		}
		acceptanceIDs, err := parseIDs(acceptanceEvidence)
		if err != nil {
			return nil, fmt.Errorf("decode acceptance evidence: %w", err)
		}
		evidence := append(challengeIDs, acceptanceIDs...)
		out = append(out, detectorCandidate{
			key: "verification-challenge-after-acceptance|" + string(challengeID),
			input: ObservationInput{
				TaskID: taskID, AttemptID: attemptID, Category: "VERIFICATION_CHALLENGE_AFTER_ACCEPTANCE",
				BasisClass: "CANONICAL_VERIFICATION_CONTRADICTION", SourceKind: "VERIFICATION_STATE",
				SummaryLocal: "Task was challenged after an earlier evidence-backed acceptance",
				Metrics:      map[string]any{}, Enforcement: enforcement, EvidenceIDs: cleanIDs(evidence),
			},
		})
	}
	return out, rows.Err()
}

type outcomeSample struct {
	id         domain.ID
	taskID     domain.ID
	taskClass  string
	scopeKey   string
	executor   string
	cost       sql.NullInt64
	latency    sql.NullInt64
	recordedAt time.Time
}

func (s *Observer) detectCostLatencyOutliers(ctx context.Context, taskFilter domain.ID) ([]detectorCandidate, error) {
	query := "SELECT outcome_id, task_id, generic_task_class, scope_key, executor_kind, cost_units, latency_ms, recorded_at " +
		"FROM experience_outcomes WHERE accepted = 1"
	args := []any{}
	if taskFilter != "" {
		query += " AND task_id = ?"
		args = append(args, taskFilter)
	}
	query += " ORDER BY scope_key, generic_task_class, executor_kind, recorded_at, outcome_id"
	rows, err := s.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	history := map[string][]outcomeSample{}
	var out []detectorCandidate
	var malformed []string
	var malformedTask domain.ID
	for rows.Next() {
		var sample outcomeSample
		var recorded string
		if err := rows.Scan(&sample.id, &sample.taskID, &sample.taskClass, &sample.scopeKey, &sample.executor,
			&sample.cost, &sample.latency, &recorded); err != nil {
			return nil, err
		}
		sample.recordedAt, err = time.Parse(time.RFC3339Nano, recorded)
		if err != nil {
			// One legacy row must not abort the whole pass; skip it and report once below.
			malformed = append(malformed, string(sample.id))
			if malformedTask == "" {
				malformedTask = sample.taskID
			}
			continue
		}
		groupKey := sample.scopeKey + "\x00" + sample.taskClass + "\x00" + sample.executor
		previous := history[groupKey]
		if len(previous) >= 5 && isHighSignalOutlier(sample, previous) {
			enforcement, err := s.taskEnforcement(ctx, sample.taskID)
			if err != nil {
				return nil, err
			}
			metrics := map[string]any{"baseline_samples": len(previous)}
			if sample.cost.Valid {
				metrics["cost_units"] = sample.cost.Int64
			}
			if sample.latency.Valid {
				metrics["latency_ms"] = sample.latency.Int64
			}
			out = append(out, detectorCandidate{
				key: "cost-latency-outlier|" + string(sample.id),
				input: ObservationInput{
					TaskID: sample.taskID, Category: "COST_LATENCY_OUTLIER",
					BasisClass: "DETERMINISTIC_VERIFIED_OUTLIER", SourceKind: "EXPERIENCE_OUTCOMES",
					SummaryLocal: "Verified outcome exceeded the conservative cost/latency outlier threshold",
					Metrics:      metrics, ExecutorKind: sample.executor, Enforcement: enforcement,
				},
			})
		}
		history[groupKey] = append(previous, sample)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(malformed) > 0 {
		sum := sha256.Sum256([]byte(strings.Join(malformed, "\x00")))
		enforcement, err := s.taskEnforcement(ctx, malformedTask)
		if err != nil {
			return nil, err
		}
		out = append(out, detectorCandidate{
			key: "malformed-canonical-row|experience_outcomes|" + hex.EncodeToString(sum[:8]),
			input: ObservationInput{
				TaskID: malformedTask, Category: "MALFORMED_CANONICAL_ROW",
				BasisClass: "DETERMINISTIC_CANONICAL_ROW", SourceKind: "EXPERIENCE_OUTCOMES",
				SummaryLocal: "Experience outcome rows with unparseable recorded_at were skipped",
				Metrics:      map[string]any{"malformed_rows": len(malformed)}, Enforcement: enforcement,
			},
		})
	}
	return out, nil
}

func isHighSignalOutlier(sample outcomeSample, previous []outcomeSample) bool {
	var maxCost, maxLatency int64
	var costSamples, latencySamples int
	for _, prior := range previous {
		if prior.cost.Valid {
			costSamples++
			if prior.cost.Int64 > maxCost {
				maxCost = prior.cost.Int64
			}
		}
		if prior.latency.Valid {
			latencySamples++
			if prior.latency.Int64 > maxLatency {
				maxLatency = prior.latency.Int64
			}
		}
	}
	costOutlier := sample.cost.Valid && costSamples >= 5 && maxCost > 0 && sample.cost.Int64 > 2*maxCost
	latencyOutlier := sample.latency.Valid && latencySamples >= 5 && maxLatency > 0 && sample.latency.Int64 > 2*maxLatency
	return costOutlier || latencyOutlier
}

func (s *Observer) taskEnforcement(ctx context.Context, taskID domain.ID) (domain.EnforcementLevel, error) {
	if levels, ok := ctx.Value(detectorEnforcementContextKey{}).(map[domain.ID]domain.EnforcementLevel); ok {
		level, found := levels[taskID]
		if !found {
			return "", fmt.Errorf("task %s has no enforcement level", taskID)
		}
		return level, nil
	}
	var level domain.EnforcementLevel
	if err := s.store.DB().QueryRowContext(ctx,
		"SELECT required_enforcement FROM tasks WHERE task_id = ?", taskID,
	).Scan(&level); err != nil {
		return "", err
	}
	return level, nil
}

func (s *Observer) loadDetectorEnforcement(ctx context.Context, taskFilter domain.ID) (map[domain.ID]domain.EnforcementLevel, error) {
	query := "SELECT task_id, required_enforcement FROM tasks"
	args := []any{}
	if taskFilter != "" {
		query += " WHERE task_id = ?"
		args = append(args, taskFilter)
	}
	rows, err := s.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	levels := make(map[domain.ID]domain.EnforcementLevel)
	for rows.Next() {
		var taskID domain.ID
		var level domain.EnforcementLevel
		if err := rows.Scan(&taskID, &level); err != nil {
			return nil, err
		}
		levels[taskID] = level
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return levels, nil
}

func parseIDs(raw string) ([]domain.ID, error) {
	var ids []domain.ID
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, err
	}
	return cleanIDs(ids), nil
}

func stableHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
