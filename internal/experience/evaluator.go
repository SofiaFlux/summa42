package experience

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/audit"
	"github.com/SofiaFlux/summa42/internal/domain"
)

type outcomeStat struct {
	samples  int
	accepted int
	costSum  int64
	costN    int
}

func (s *Service) Evaluate(ctx context.Context, proposalID domain.ID) (domain.ExperienceRule, error) {
	if err := s.configured(); err != nil {
		return domain.ExperienceRule{}, err
	}
	proposal, evidenceIDs, err := s.loadProposal(ctx, proposalID)
	if err != nil {
		return domain.ExperienceRule{}, err
	}
	grant, err := s.Grant(ctx, proposal.GrantID)
	if err != nil {
		return domain.ExperienceRule{}, err
	}
	current, hasCurrent, err := s.latestRule(ctx, proposal.ID)
	if err != nil {
		return domain.ExperienceRule{}, err
	}
	if hasCurrent && current.State == domain.ExperienceRolledBack {
		return current, nil
	}

	stats, outcomeIDs, err := s.outcomeStats(ctx, proposal.ScopeKey, proposal.GenericTaskClass)
	if err != nil {
		return domain.ExperienceRule{}, err
	}
	preferred := stats[proposal.PreferredExecutor]
	baseline := outcomeStat{}
	for executor, stat := range stats {
		if executor != proposal.PreferredExecutor {
			baseline = mergeStat(baseline, stat)
		}
	}

	nextState := domain.ExperienceShadow
	expired := !grant.ExpiresAt.After(s.clock.Now().UTC())
	regressed := acceptanceRegressed(preferred, baseline, grant.MaxAcceptanceRegressionBps) ||
		costRegressed(preferred, baseline, grant.MaxCostRegressionBps)
	if hasCurrent && current.State == domain.ExperienceActive && (expired || regressed) {
		nextState = domain.ExperienceRolledBack
	} else if !expired && preferred.samples >= grant.MinVerifiedSamples && !regressed && preferred.accepted > 0 {
		nextState = domain.ExperienceActive
	} else if hasCurrent && current.State == domain.ExperienceActive {
		nextState = domain.ExperienceActive
	}

	if hasCurrent && current.State == nextState && current.VerifiedSamples == preferred.samples {
		return current, nil
	}

	now := s.clock.Now().UTC()
	version := 1
	if hasCurrent {
		version = current.Version + 1
	}
	rule := domain.ExperienceRule{
		ID: domain.NewID("experience-rule"), ProposalID: proposal.ID, GrantID: grant.ID, Version: version,
		AdaptationKind: grant.Kind, ScopeKey: proposal.ScopeKey, GenericTaskClass: proposal.GenericTaskClass,
		PreferredExecutor: proposal.PreferredExecutor, State: nextState, VerifiedSamples: preferred.samples, CreatedAt: now,
	}
	if nextState == domain.ExperienceActive {
		rule.ActivatedAt = now
	}
	if nextState == domain.ExperienceRolledBack {
		rule.RolledBackAt = now
	}

	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO experience_rules(rule_id, proposal_id, grant_id, version, adaptation_kind, scope_key, generic_task_class, preferred_executor, state, verified_samples, activated_at, rolled_back_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			rule.ID, rule.ProposalID, rule.GrantID, rule.Version, rule.AdaptationKind, rule.ScopeKey, rule.GenericTaskClass,
			rule.PreferredExecutor, rule.State, rule.VerifiedSamples, nullableTime(rule.ActivatedAt), nullableTime(rule.RolledBackAt),
			formatTime(now), formatTime(now),
		); err != nil {
			return err
		}
		for _, id := range evidenceIDs {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO experience_rule_evidence(rule_id, observation_id, linked_at) VALUES (?, ?, ?)",
				rule.ID, id, formatTime(now)); err != nil {
				return err
			}
		}
		for _, id := range outcomeIDs {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO experience_rule_outcomes(rule_id, outcome_id, linked_at) VALUES (?, ?, ?)",
				rule.ID, id, formatTime(now)); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE experience_proposals SET state = ?, updated_at = ? WHERE proposal_id = ?",
			rule.State, formatTime(now), proposal.ID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return domain.ExperienceRule{}, err
	}
	if _, err := s.audit.Append(ctx, audit.Event{
		Kind: "EXPERIENCE_RULE_EVALUATED", ActorID: "experience-service", SubjectID: rule.ID,
		Payload: map[string]any{
			"proposal_id": rule.ProposalID, "grant_id": rule.GrantID, "state": rule.State,
			"verified_samples": rule.VerifiedSamples, "evidence_ids": evidenceIDs, "outcome_ids": outcomeIDs,
		},
	}); err != nil {
		return domain.ExperienceRule{}, fmt.Errorf("audit rule evaluation: %w", err)
	}
	return rule, nil
}

func (s *Service) Preference(ctx context.Context, q PreferenceQuery) (Preference, bool, error) {
	if err := s.configured(); err != nil {
		return Preference{}, false, err
	}
	q.ScopeKey = strings.TrimSpace(q.ScopeKey)
	if q.ScopeKey == "" {
		return Preference{}, false, errors.New("preference scope is required")
	}
	args := []any{q.ScopeKey, AdaptationExecutorPreference, domain.ExperienceActive, formatTime(s.clock.Now().UTC())}
	query := "WITH latest AS (" +
		"SELECT r.*, ROW_NUMBER() OVER (PARTITION BY r.proposal_id ORDER BY r.version DESC) AS rn FROM experience_rules r" +
		") SELECT r.rule_id, r.preferred_executor FROM latest r JOIN adaptation_grants g ON g.grant_id = r.grant_id WHERE r.rn = 1 AND r.scope_key = ? AND r.adaptation_kind = ? AND r.state = ? AND g.expires_at > ?"
	if q.GenericTaskClass != "" {
		if !q.GenericTaskClass.Valid() {
			return Preference{}, false, errors.New("invalid preference task class")
		}
		query += " AND r.generic_task_class = ?"
		args = append(args, q.GenericTaskClass)
	}
	query += " ORDER BY r.created_at DESC, r.version DESC LIMIT 2"
	rows, err := s.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return Preference{}, false, err
	}
	defer rows.Close()
	var out []Preference
	for rows.Next() {
		var p Preference
		if err := rows.Scan(&p.RuleID, &p.ExecutorKind); err != nil {
			return Preference{}, false, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return Preference{}, false, err
	}
	if len(out) == 0 {
		return Preference{}, false, nil
	}
	if len(out) > 1 && out[0].ExecutorKind != out[1].ExecutorKind {
		// Ambiguity is not an error: fall back to the baseline and leave an audit record.
		if _, err := s.audit.Append(ctx, audit.Event{
			Kind: "EXPERIENCE_PREFERENCE_AMBIGUOUS", ActorID: "experience-service", SubjectID: domain.ID(q.ScopeKey),
			Payload: map[string]any{"scope_key": q.ScopeKey, "rule_ids": []domain.ID{out[0].RuleID, out[1].RuleID}, "executors": []string{out[0].ExecutorKind, out[1].ExecutorKind}},
		}); err != nil {
			return Preference{}, false, fmt.Errorf("audit ambiguous preference: %w", err)
		}
		return Preference{}, false, nil
	}
	return out[0], true, nil
}

func (s *Service) PreferredExecutor(ctx context.Context, task domain.Task, eligible []string) (string, bool, error) {
	scope := strings.TrimSpace(task.TaskClass)
	if scope == "" {
		return "", false, nil
	}
	preference, found, err := s.Preference(ctx, PreferenceQuery{ScopeKey: scope})
	if err != nil || !found {
		return "", found, err
	}
	for _, executor := range eligible {
		if executor == preference.ExecutorKind {
			return executor, true, nil
		}
	}
	return "", false, nil
}

func (s *Service) loadProposal(ctx context.Context, id domain.ID) (domain.ExperienceProposal, []domain.ID, error) {
	id = domain.ID(strings.TrimSpace(string(id)))
	if id == "" {
		return domain.ExperienceProposal{}, nil, errors.New("proposal id is required")
	}
	var out domain.ExperienceProposal
	var created string
	err := s.store.DB().QueryRowContext(ctx,
		"SELECT proposal_id, grant_id, generic_task_class, scope_key, preferred_executor, state, created_at FROM experience_proposals WHERE proposal_id = ?", id,
	).Scan(&out.ID, &out.GrantID, &out.GenericTaskClass, &out.ScopeKey, &out.PreferredExecutor, &out.State, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ExperienceProposal{}, nil, fmt.Errorf("experience proposal %s not found", id)
	}
	if err != nil {
		return domain.ExperienceProposal{}, nil, err
	}
	out.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return domain.ExperienceProposal{}, nil, err
	}
	rows, err := s.store.DB().QueryContext(ctx, "SELECT observation_id FROM experience_proposal_evidence WHERE proposal_id = ? ORDER BY observation_id", id)
	if err != nil {
		return domain.ExperienceProposal{}, nil, err
	}
	defer rows.Close()
	var evidence []domain.ID
	for rows.Next() {
		var evidenceID domain.ID
		if err := rows.Scan(&evidenceID); err != nil {
			return domain.ExperienceProposal{}, nil, err
		}
		evidence = append(evidence, evidenceID)
	}
	return out, evidence, rows.Err()
}

func (s *Service) latestRule(ctx context.Context, proposalID domain.ID) (domain.ExperienceRule, bool, error) {
	var out domain.ExperienceRule
	var activated, rolledBack sql.NullString
	var created string
	err := s.store.DB().QueryRowContext(ctx,
		"SELECT rule_id, proposal_id, grant_id, version, adaptation_kind, scope_key, generic_task_class, preferred_executor, state, verified_samples, activated_at, rolled_back_at, created_at FROM experience_rules WHERE proposal_id = ? ORDER BY version DESC LIMIT 1",
		proposalID,
	).Scan(&out.ID, &out.ProposalID, &out.GrantID, &out.Version, &out.AdaptationKind, &out.ScopeKey, &out.GenericTaskClass, &out.PreferredExecutor, &out.State, &out.VerifiedSamples, &activated, &rolledBack, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ExperienceRule{}, false, nil
	}
	if err != nil {
		return domain.ExperienceRule{}, false, err
	}
	var parseErr error
	out.CreatedAt, parseErr = time.Parse(time.RFC3339Nano, created)
	if parseErr != nil {
		return domain.ExperienceRule{}, false, parseErr
	}
	if activated.Valid {
		out.ActivatedAt, parseErr = time.Parse(time.RFC3339Nano, activated.String)
		if parseErr != nil {
			return domain.ExperienceRule{}, false, parseErr
		}
	}
	if rolledBack.Valid {
		out.RolledBackAt, parseErr = time.Parse(time.RFC3339Nano, rolledBack.String)
		if parseErr != nil {
			return domain.ExperienceRule{}, false, parseErr
		}
	}
	return out, true, nil
}

func (s *Service) outcomeStats(ctx context.Context, scope string, class domain.GenericTaskClass) (map[string]outcomeStat, []domain.ID, error) {
	rows, err := s.store.DB().QueryContext(ctx,
		"WITH ranked AS ("+
			"SELECT outcome_id, task_id, executor_kind, accepted, cost_units, recorded_at, "+
			"ROW_NUMBER() OVER (PARTITION BY task_id ORDER BY recorded_at DESC, outcome_id DESC) AS rn "+
			"FROM experience_outcomes WHERE scope_key = ? AND generic_task_class = ?) "+
			"SELECT outcome_id, executor_kind, accepted, cost_units FROM ranked WHERE rn = 1",
		scope, class,
	)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	stats := map[string]outcomeStat{}
	var ids []domain.ID
	for rows.Next() {
		var id domain.ID
		var executor string
		var accepted int
		var cost sql.NullInt64
		if err := rows.Scan(&id, &executor, &accepted, &cost); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		stat := stats[executor]
		stat.samples++
		if accepted == 1 {
			stat.accepted++
		}
		if cost.Valid {
			stat.costSum += cost.Int64
			stat.costN++
		}
		stats[executor] = stat
	}
	return stats, ids, rows.Err()
}

func acceptanceRegressed(preferred, baseline outcomeStat, maxBps int) bool {
	if preferred.samples == 0 {
		return false
	}
	preferredBps := preferred.accepted * 10000 / preferred.samples
	if baseline.samples == 0 {
		return preferredBps < 10000
	}
	baselineBps := baseline.accepted * 10000 / baseline.samples
	return baselineBps-preferredBps > maxBps
}
func costRegressed(preferred, baseline outcomeStat, maxBps int) bool {
	if preferred.costN == 0 || baseline.costN == 0 {
		return false
	}
	prefAvg := preferred.costSum / int64(preferred.costN)
	baseAvg := baseline.costSum / int64(baseline.costN)
	if baseAvg <= 0 {
		return prefAvg > 0
	}
	if prefAvg <= baseAvg {
		return false
	}
	return (prefAvg-baseAvg)*10000/baseAvg > int64(maxBps)
}
func mergeStat(a, b outcomeStat) outcomeStat {
	return outcomeStat{samples: a.samples + b.samples, accepted: a.accepted + b.accepted, costSum: a.costSum + b.costSum, costN: a.costN + b.costN}
}
func nullableTime(v time.Time) any {
	if v.IsZero() {
		return nil
	}
	return formatTime(v)
}

func (s *Service) Rules(ctx context.Context) ([]domain.ExperienceRule, error) {
	rows, err := s.store.DB().QueryContext(ctx, "SELECT rule_id FROM experience_rules ORDER BY created_at DESC, version DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []domain.ID
	for rows.Next() {
		var id domain.ID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	var out []domain.ExperienceRule
	for _, id := range ids {
		var rule domain.ExperienceRule
		var activated, rolled sql.NullString
		var created string
		err := s.store.DB().QueryRowContext(ctx,
			"SELECT rule_id, proposal_id, grant_id, version, adaptation_kind, scope_key, generic_task_class, preferred_executor, state, verified_samples, activated_at, rolled_back_at, created_at FROM experience_rules WHERE rule_id = ?", id,
		).Scan(&rule.ID, &rule.ProposalID, &rule.GrantID, &rule.Version, &rule.AdaptationKind, &rule.ScopeKey, &rule.GenericTaskClass, &rule.PreferredExecutor, &rule.State, &rule.VerifiedSamples, &activated, &rolled, &created)
		if err != nil {
			return nil, err
		}
		if err := parseRuleTimes(&rule, created, activated, rolled); err != nil {
			return nil, err
		}
		out = append(out, rule)
	}
	return out, rows.Err()
}

func (s *Service) Rule(ctx context.Context, id domain.ID) (domain.ExperienceRule, error) {
	var rule domain.ExperienceRule
	var activated, rolled sql.NullString
	var created string
	err := s.store.DB().QueryRowContext(ctx,
		"SELECT rule_id, proposal_id, grant_id, version, adaptation_kind, scope_key, generic_task_class, preferred_executor, state, verified_samples, activated_at, rolled_back_at, created_at FROM experience_rules WHERE rule_id = ?", id,
	).Scan(&rule.ID, &rule.ProposalID, &rule.GrantID, &rule.Version, &rule.AdaptationKind, &rule.ScopeKey, &rule.GenericTaskClass, &rule.PreferredExecutor, &rule.State, &rule.VerifiedSamples, &activated, &rolled, &created)
	if err != nil {
		return domain.ExperienceRule{}, err
	}
	if err := parseRuleTimes(&rule, created, activated, rolled); err != nil {
		return domain.ExperienceRule{}, err
	}
	return rule, nil
}

func parseRuleTimes(rule *domain.ExperienceRule, created string, activated, rolled sql.NullString) error {
	var err error
	if rule.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return fmt.Errorf("parse rule created_at: %w", err)
	}
	if activated.Valid {
		if rule.ActivatedAt, err = time.Parse(time.RFC3339Nano, activated.String); err != nil {
			return fmt.Errorf("parse rule activated_at: %w", err)
		}
	}
	if rolled.Valid {
		if rule.RolledBackAt, err = time.Parse(time.RFC3339Nano, rolled.String); err != nil {
			return fmt.Errorf("parse rule rolled_back_at: %w", err)
		}
	}
	return nil
}
