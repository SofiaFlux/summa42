package obligation

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

const (
	BaseObligationPriority = 1000
	DefaultEnvelopeID      = "envelope_default"
)

type Service struct {
	store *state.Store
	clock clock.Clock
	mu    sync.Mutex
}

func NewService(store *state.Store, clk clock.Clock) *Service {
	return &Service{
		store: store,
		clock: clk,
	}
}

type CreateObligationRequest struct {
	Source             string        `json:"source"`
	Statement          string        `json:"statement"`
	EntityID           domain.ID     `json:"entity_id,omitempty"`
	DueAt              time.Time     `json:"due_at"`
	GracePeriod        time.Duration `json:"grace_period"`
	Consequences       Consequences  `json:"consequences"`
	CostOfDelayRate    int64         `json:"cost_of_delay_rate"`
	LeadTime           time.Duration `json:"lead_time"`
	RequiredCapability string        `json:"required_capability,omitempty"`
}

// CreateObligation creates a first-class compliance or contractual obligation.
func (s *Service) CreateObligation(ctx context.Context, req CreateObligationRequest) (Obligation, error) {
	req.Source = strings.TrimSpace(req.Source)
	req.Statement = strings.TrimSpace(req.Statement)
	if req.Source == "" {
		return Obligation{}, errors.New("obligation source is required")
	}
	if req.Statement == "" {
		return Obligation{}, errors.New("obligation statement is required")
	}
	if req.DueAt.IsZero() {
		return Obligation{}, errors.New("obligation due date is required")
	}
	if req.LeadTime <= 0 {
		req.LeadTime = 24 * time.Hour
	}
	if req.CostOfDelayRate < 0 {
		return Obligation{}, errors.New("cost of delay rate cannot be negative")
	}

	id := domain.NewID("ob")
	now := s.clock.Now().UTC()
	nowStr := domain.FormatTime(now)
	dueStr := domain.FormatTime(req.DueAt.UTC())
	consequencesBytes, _ := json.Marshal(req.Consequences)

	var entityIDVal any
	if req.EntityID != "" {
		entityIDVal = string(req.EntityID)
	}

	_, err := s.store.DB().ExecContext(ctx, `
		INSERT INTO obligations (
			obligation_id, source, statement, active, due_at,
			grace_period_seconds, consequences_json, cost_of_delay_rate,
			lead_time_seconds, lifecycle_state, entity_id, created_at, updated_at
		) VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, 'PENDING', ?, ?, ?)`,
		id, req.Source, req.Statement, dueStr,
		int(req.GracePeriod.Seconds()), string(consequencesBytes), req.CostOfDelayRate,
		int(req.LeadTime.Seconds()), entityIDVal, nowStr, nowStr,
	)
	if err != nil {
		return Obligation{}, fmt.Errorf("insert obligation: %w", err)
	}

	return Obligation{
		ID:              id,
		Source:          req.Source,
		Statement:       req.Statement,
		Active:          true,
		DueAt:           req.DueAt.UTC(),
		GracePeriod:     req.GracePeriod,
		Consequences:    req.Consequences,
		CostOfDelayRate: req.CostOfDelayRate,
		LeadTime:        req.LeadTime,
		LifecycleState:  StatePending,
		EntityID:        req.EntityID,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

// CalculatePriority computes a deadline-aware, starvation-free priority score.
// Compliance obligations receive a high baseline floor plus urgency increments.
func CalculatePriority(now, dueAt time.Time, costOfDelayRate int64) int {
	priority := BaseObligationPriority

	// Add cost of delay component
	if costOfDelayRate > 0 {
		priority += int(costOfDelayRate)
	}

	// Add urgency bonus as deadline approaches
	timeRemaining := dueAt.Sub(now)
	if timeRemaining <= 0 {
		// Past due (in grace period) - highest urgency
		priority += 1000
	} else if timeRemaining <= 24*time.Hour {
		// Within 24 hours of due date
		priority += 500
	} else if timeRemaining <= 72*time.Hour {
		// Within 3 days
		priority += 200
	}

	return priority
}

// EvaluateAndSchedule evaluates active obligations, marks breached items, and synthesizes
// governed task candidates for obligations entering their lead-time thresholds.
func (s *Service) EvaluateAndSchedule(ctx context.Context) ([]Obligation, []domain.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock.Now().UTC()
	nowStr := domain.FormatTime(now)

	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin evaluate tx: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT obligation_id, source, statement, active, due_at,
		       grace_period_seconds, consequences_json, cost_of_delay_rate,
		       lead_time_seconds, lifecycle_state, synthesized_task_id,
		       entity_id, created_at, updated_at, resolved_at
		FROM obligations
		WHERE active = 1 AND lifecycle_state IN ('PENDING', 'SCHEDULED', 'ACTIVE')
		ORDER BY due_at ASC`,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("query active obligations: %w", err)
	}
	defer rows.Close()

	var obligations []Obligation
	for rows.Next() {
		var o Obligation
		var crAt string
		var dueStr, upAt, resAt sql.NullString
		var synthTask, entID sql.NullString
		var graceSec, leadSec int
		var consJSON string

		if err := rows.Scan(
			&o.ID, &o.Source, &o.Statement, &o.Active, &dueStr,
			&graceSec, &consJSON, &o.CostOfDelayRate,
			&leadSec, &o.LifecycleState, &synthTask,
			&entID, &crAt, &upAt, &resAt,
		); err != nil {
			return nil, nil, fmt.Errorf("scan obligation: %w", err)
		}

		if dueStr.Valid && dueStr.String != "" {
			o.DueAt, _ = time.Parse(time.RFC3339Nano, dueStr.String)
		}
		o.GracePeriod = time.Duration(graceSec) * time.Second
		o.LeadTime = time.Duration(leadSec) * time.Second
		_ = json.Unmarshal([]byte(consJSON), &o.Consequences)
		o.CreatedAt, _ = time.Parse(time.RFC3339Nano, crAt)
		if upAt.Valid && upAt.String != "" {
			o.UpdatedAt, _ = time.Parse(time.RFC3339Nano, upAt.String)
		} else {
			o.UpdatedAt = o.CreatedAt
		}
		if resAt.Valid && resAt.String != "" {
			t, _ := time.Parse(time.RFC3339Nano, resAt.String)
			o.ResolvedAt = &t
		}
		if synthTask.Valid {
			o.SynthesizedTaskID = domain.ID(synthTask.String)
		}
		if entID.Valid {
			o.EntityID = domain.ID(entID.String)
		}

		obligations = append(obligations, o)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate obligations: %w", err)
	}

	var evaluated []Obligation
	var synthesizedTasks []domain.Task

	for _, ob := range obligations {
		if ob.DueAt.IsZero() {
			evaluated = append(evaluated, ob)
			continue
		}

		// 1. Check if synthesized task has finished or failed
		if ob.SynthesizedTaskID != "" {
			var taskState string
			err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE task_id = ?`, ob.SynthesizedTaskID).Scan(&taskState)
			if err == nil {
				if taskState == string(domain.TaskSucceeded) {
					// Obligation fulfilled!
					_, err = tx.ExecContext(ctx, `
						UPDATE obligations
						SET lifecycle_state = 'FULFILLED',
						    active = 0,
						    resolved_at = ?,
						    updated_at = ?
						WHERE obligation_id = ?`,
						nowStr, nowStr, ob.ID,
					)
					if err != nil {
						return nil, nil, fmt.Errorf("update fulfilled obligation: %w", err)
					}
					ob.LifecycleState = StateFulfilled
					ob.Active = false
					ob.ResolvedAt = &now
					ob.UpdatedAt = now
					evaluated = append(evaluated, ob)
					continue
				} else if taskState == string(domain.TaskFailed) || taskState == string(domain.TaskCancelled) || taskState == string(domain.TaskExpired) {
					// Synthesized task failed or expired; clear link so a fresh attempt can be made within lead time
					_, _ = tx.ExecContext(ctx, `
						UPDATE obligations
						SET synthesized_task_id = NULL,
						    lifecycle_state = 'PENDING',
						    updated_at = ?
						WHERE obligation_id = ?`,
						nowStr, ob.ID,
					)
					ob.SynthesizedTaskID = ""
					ob.LifecycleState = StatePending
					ob.UpdatedAt = now
				}
			}
		}

		// 2. Check breach condition (past due date + grace period)
		breachThreshold := ob.DueAt.Add(ob.GracePeriod)
		if now.After(breachThreshold) {
			_, err = tx.ExecContext(ctx, `
				UPDATE obligations
				SET lifecycle_state = 'BREACHED',
				    active = 0,
				    resolved_at = ?,
				    updated_at = ?
				WHERE obligation_id = ?`,
				nowStr, nowStr, ob.ID,
			)
			if err != nil {
				return nil, nil, fmt.Errorf("update breached obligation: %w", err)
			}
			ob.LifecycleState = StateBreached
			ob.Active = false
			ob.ResolvedAt = &now
			ob.UpdatedAt = now
			evaluated = append(evaluated, ob)
			continue
		}

		// 3. Check lead time trigger to synthesize task
		leadTimeThreshold := ob.DueAt.Add(-ob.LeadTime)
		if (now.After(leadTimeThreshold) || now.Equal(leadTimeThreshold)) && ob.SynthesizedTaskID == "" {
			taskID := domain.NewID("task_ob")
			priority := CalculatePriority(now, ob.DueAt, ob.CostOfDelayRate)

			criteriaJSON, _ := json.Marshal([]string{ob.Statement})
			capabilitiesJSON, _ := json.Marshal([]string{})
			authorityJSON, _ := json.Marshal([]string{})

			dueStr := domain.FormatTime(ob.DueAt.UTC())

			// Insert synthesized governed task into tasks table
			_, err = tx.ExecContext(ctx, `
				INSERT INTO tasks (
					task_id, purpose_kind, purpose_id, state, acceptance_criteria_json,
					required_capabilities_json, required_enforcement, authority_ceiling_json,
					resource_envelope_id, priority, deadline, created_at, updated_at
				) VALUES (?, 'OBLIGATION', ?, 'ELIGIBLE', ?, ?, 'ENFORCED', ?, ?, ?, ?, ?, ?)`,
				taskID, ob.ID, string(criteriaJSON), string(capabilitiesJSON),
				string(authorityJSON), DefaultEnvelopeID, priority, dueStr, nowStr, nowStr,
			)
			if err != nil {
				return nil, nil, fmt.Errorf("synthesize obligation task: %w", err)
			}

			// Update obligation with synthesized task ID
			_, err = tx.ExecContext(ctx, `
				UPDATE obligations
				SET synthesized_task_id = ?,
				    lifecycle_state = 'SCHEDULED',
				    updated_at = ?
				WHERE obligation_id = ?`,
				taskID, nowStr, ob.ID,
			)
			if err != nil {
				return nil, nil, fmt.Errorf("link synthesized task to obligation: %w", err)
			}

			ob.SynthesizedTaskID = taskID
			ob.LifecycleState = StateScheduled
			ob.UpdatedAt = now

			synthesizedTasks = append(synthesizedTasks, domain.Task{
				ID:                  taskID,
				Purpose:             domain.PurposeRef{Kind: domain.PurposeObligation, ID: ob.ID},
				State:               domain.TaskEligible,
				Priority:            priority,
				Deadline:            ob.DueAt.UTC(),
				ResourceEnvelopeID:  DefaultEnvelopeID,
				RequiredEnforcement: domain.EnforcementEnforced,
				CreatedAt:           now,
				UpdatedAt:           now,
			})
		}

		evaluated = append(evaluated, ob)
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit evaluate tx: %w", err)
	}

	return evaluated, synthesizedTasks, nil
}

// GetObligation retrieves an obligation by ID.
func (s *Service) GetObligation(ctx context.Context, id domain.ID) (Obligation, error) {
	var o Obligation
	var crAt string
	var dueStr, upAt, resAt, synthTask, entID sql.NullString
	var graceSec, leadSec int
	var consJSON string

	err := s.store.DB().QueryRowContext(ctx, `
		SELECT obligation_id, source, statement, active, due_at,
		       grace_period_seconds, consequences_json, cost_of_delay_rate,
		       lead_time_seconds, lifecycle_state, synthesized_task_id,
		       entity_id, created_at, updated_at, resolved_at
		FROM obligations
		WHERE obligation_id = ?`,
		id,
	).Scan(
		&o.ID, &o.Source, &o.Statement, &o.Active, &dueStr,
		&graceSec, &consJSON, &o.CostOfDelayRate,
		&leadSec, &o.LifecycleState, &synthTask,
		&entID, &crAt, &upAt, &resAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Obligation{}, ErrObligationNotFound
		}
		return Obligation{}, fmt.Errorf("query obligation: %w", err)
	}

	if dueStr.Valid && dueStr.String != "" {
		o.DueAt, _ = time.Parse(time.RFC3339Nano, dueStr.String)
	}
	o.GracePeriod = time.Duration(graceSec) * time.Second
	o.LeadTime = time.Duration(leadSec) * time.Second
	_ = json.Unmarshal([]byte(consJSON), &o.Consequences)
	o.CreatedAt, _ = time.Parse(time.RFC3339Nano, crAt)
	if upAt.Valid && upAt.String != "" {
		o.UpdatedAt, _ = time.Parse(time.RFC3339Nano, upAt.String)
	} else {
		o.UpdatedAt = o.CreatedAt
	}
	if resAt.Valid && resAt.String != "" {
		t, _ := time.Parse(time.RFC3339Nano, resAt.String)
		o.ResolvedAt = &t
	}
	if synthTask.Valid {
		o.SynthesizedTaskID = domain.ID(synthTask.String)
	}
	if entID.Valid {
		o.EntityID = domain.ID(entID.String)
	}

	return o, nil
}

// ListObligations lists obligations optionally filtered by lifecycle state.
func (s *Service) ListObligations(ctx context.Context, state *ObligationState) ([]Obligation, error) {
	query := `
		SELECT obligation_id, source, statement, active, due_at,
		       grace_period_seconds, consequences_json, cost_of_delay_rate,
		       lead_time_seconds, lifecycle_state, synthesized_task_id,
		       entity_id, created_at, updated_at, resolved_at
		FROM obligations`
	var args []any
	if state != nil {
		query += " WHERE lifecycle_state = ?"
		args = append(args, string(*state))
	}
	query += " ORDER BY due_at ASC"

	rows, err := s.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query obligations: %w", err)
	}
	defer rows.Close()

	var list []Obligation
	for rows.Next() {
		var o Obligation
		var crAt string
		var dueStr, upAt, resAt, synthTask, entID sql.NullString
		var graceSec, leadSec int
		var consJSON string

		if err := rows.Scan(
			&o.ID, &o.Source, &o.Statement, &o.Active, &dueStr,
			&graceSec, &consJSON, &o.CostOfDelayRate,
			&leadSec, &o.LifecycleState, &synthTask,
			&entID, &crAt, &upAt, &resAt,
		); err != nil {
			return nil, fmt.Errorf("scan obligation: %w", err)
		}

		if dueStr.Valid && dueStr.String != "" {
			o.DueAt, _ = time.Parse(time.RFC3339Nano, dueStr.String)
		}
		o.GracePeriod = time.Duration(graceSec) * time.Second
		o.LeadTime = time.Duration(leadSec) * time.Second
		_ = json.Unmarshal([]byte(consJSON), &o.Consequences)
		o.CreatedAt, _ = time.Parse(time.RFC3339Nano, crAt)
		if upAt.Valid && upAt.String != "" {
			o.UpdatedAt, _ = time.Parse(time.RFC3339Nano, upAt.String)
		} else {
			o.UpdatedAt = o.CreatedAt
		}
		if resAt.Valid && resAt.String != "" {
			t, _ := time.Parse(time.RFC3339Nano, resAt.String)
			o.ResolvedAt = &t
		}
		if synthTask.Valid {
			o.SynthesizedTaskID = domain.ID(synthTask.String)
		}
		if entID.Valid {
			o.EntityID = domain.ID(entID.String)
		}
		list = append(list, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate obligations: %w", err)
	}

	return list, nil
}
