package wake

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/scheduler"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type Kind string

const (
	KindTaskReevaluation Kind = "TASK_REEVALUATION"
	KindStrategicPulse   Kind = "STRATEGIC_PULSE"
)

type CapabilityFreshness interface {
	StaleCapabilityAssessments(ctx context.Context) (int, error)
}

type Wakeup struct {
	ID      domain.ID
	Kind    Kind
	TaskID  domain.ID
	DueAt   time.Time
	FiredAt time.Time
}

type PulseResult struct {
	Dormant                    bool
	AuthorizedWork             bool
	DueObligations             int
	BlockedTasks               int
	KnownUnknowns              int
	StaleCapabilityAssessments int
	NextPulseAt                time.Time
}

type Service struct {
	store     *state.Store
	clock     clock.Clock
	scheduler *scheduler.Service
	freshness CapabilityFreshness
}

func New(store *state.Store, clk clock.Clock, schedulerSvc *scheduler.Service, freshness ...CapabilityFreshness) *Service {
	service := &Service{store: store, clock: clk, scheduler: schedulerSvc}
	if len(freshness) > 0 {
		service.freshness = freshness[0]
	}
	return service
}

func (s *Service) Schedule(ctx context.Context, kind Kind, taskID domain.ID, dueAt time.Time) (Wakeup, error) {
	if err := s.configured(); err != nil {
		return Wakeup{}, err
	}
	if !kind.valid() || dueAt.IsZero() {
		return Wakeup{}, errors.New("valid wakeup kind and due time are required")
	}
	wakeup := Wakeup{ID: domain.NewID("wakeup"), Kind: kind, TaskID: taskID, DueAt: dueAt.UTC()}
	now := s.clock.Now().UTC()
	var taskArg any
	if taskID != "" {
		taskArg = taskID
	}
	if _, err := s.store.DB().ExecContext(ctx, `
		INSERT INTO scheduled_wakeups(wakeup_id, kind, task_id, due_at, state, created_at)
		VALUES (?, ?, ?, ?, 'PENDING', ?)`,
		wakeup.ID, wakeup.Kind, taskArg, formatTime(wakeup.DueAt), formatTime(now),
	); err != nil {
		return Wakeup{}, err
	}
	return wakeup, nil
}

func (s *Service) NextDeadline(ctx context.Context) (*time.Time, error) {
	if err := s.configured(); err != nil {
		return nil, err
	}
	rows, err := s.store.DB().QueryContext(ctx,
		`SELECT due_at FROM scheduled_wakeups WHERE state = 'PENDING'`,
	)
	if err != nil {
		return nil, err
	}
	var earliest time.Time
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return nil, err
		}
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("parse wake deadline: %w", err)
		}
		if earliest.IsZero() || parsed.Before(earliest) {
			earliest = parsed
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if earliest.IsZero() {
		return nil, nil
	}
	return &earliest, nil
}

// Due returns pending wakeups whose deadline has arrived. It deliberately does
// not consume them: acknowledgement happens only after the handler has durably
// committed its work, so a Box crash cannot lose a wakeup between delivery and
// handling.
func (s *Service) Due(ctx context.Context) ([]Wakeup, error) {
	if err := s.configured(); err != nil {
		return nil, err
	}
	now := s.clock.Now().UTC()
	rows, err := s.store.DB().QueryContext(ctx, `
		SELECT wakeup_id, kind, COALESCE(task_id, ''), due_at
		FROM scheduled_wakeups
		WHERE state = 'PENDING'`)
	if err != nil {
		return nil, err
	}
	var due []Wakeup
	for rows.Next() {
		var wakeup Wakeup
		var raw string
		if err := rows.Scan(&wakeup.ID, &wakeup.Kind, &wakeup.TaskID, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		wakeup.DueAt, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("parse wake due_at: %w", err)
		}
		if !wakeup.DueAt.After(now) {
			due = append(due, wakeup)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	sort.Slice(due, func(i, j int) bool {
		if !due[i].DueAt.Equal(due[j].DueAt) {
			return due[i].DueAt.Before(due[j].DueAt)
		}
		return due[i].ID < due[j].ID
	})
	return due, nil
}

func (s *Service) Acknowledge(ctx context.Context, wakeupID domain.ID) error {
	if err := s.configured(); err != nil {
		return err
	}
	if wakeupID == "" {
		return errors.New("wakeup id is required")
	}
	now := s.clock.Now().UTC()
	result, err := s.store.DB().ExecContext(ctx, `
		UPDATE scheduled_wakeups
		SET state = 'FIRED', fired_at = ?
		WHERE wakeup_id = ? AND state = 'PENDING'`,
		formatTime(now), wakeupID,
	)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("pending wakeup %q not found or already acknowledged", wakeupID)
	}
	return nil
}

func (s *Service) RecoverExpiredLeases(ctx context.Context) (int, error) {
	if err := s.configured(); err != nil {
		return 0, err
	}
	now := s.clock.Now().UTC()
	recovered := 0
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT attempt_id, task_id, lease_expires_at
			FROM attempts
			WHERE lease_state = ?`, domain.LeaseActive)
		if err != nil {
			return err
		}
		type expiredAttempt struct {
			attemptID domain.ID
			taskID    domain.ID
		}
		var expired []expiredAttempt
		for rows.Next() {
			var attemptID, taskID domain.ID
			var rawExpiry string
			if err := rows.Scan(&attemptID, &taskID, &rawExpiry); err != nil {
				rows.Close()
				return err
			}
			expires, err := time.Parse(time.RFC3339Nano, rawExpiry)
			if err != nil {
				rows.Close()
				return fmt.Errorf("parse lease expiry for %s: %w", attemptID, err)
			}
			if !expires.After(now) {
				expired = append(expired, expiredAttempt{attemptID: attemptID, taskID: taskID})
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}

		for _, attempt := range expired {
			result, err := tx.ExecContext(ctx, `
				UPDATE attempts
				SET lease_state = ?, state = ?, completed_at = ?
				WHERE attempt_id = ? AND lease_state = ?`,
				domain.LeaseExpired, domain.AttemptExpired, formatTime(now), attempt.attemptID, domain.LeaseActive,
			)
			if err != nil {
				return err
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if changed != 1 {
				continue
			}
			recovered++
			if _, err := tx.ExecContext(ctx, `
				UPDATE tasks
				SET state = ?, updated_at = ?
				WHERE task_id = ? AND current_attempt_id = ? AND state = ?`,
				domain.TaskEligible, formatTime(now), attempt.taskID, attempt.attemptID, domain.TaskExecuting,
			); err != nil {
				return err
			}
			details, err := json.Marshal(map[string]any{"reason": "lease_expired"})
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO execution_events(event_id, task_id, attempt_id, event_type, details_json, created_at)
				VALUES (?, ?, ?, 'LEASE_EXPIRED', ?, ?)`,
				domain.NewID("event"), attempt.taskID, attempt.attemptID, string(details), formatTime(now),
			); err != nil {
				return err
			}
		}
		return nil
	})
	return recovered, err
}

func (s *Service) StrategicPulse(ctx context.Context, capacity scheduler.CapacitySnapshot, interval time.Duration) (PulseResult, error) {
	if err := s.configured(); err != nil {
		return PulseResult{}, err
	}
	if s.scheduler == nil {
		return PulseResult{}, errors.New("strategic pulse requires scheduler")
	}
	if interval <= 0 {
		return PulseResult{}, errors.New("positive strategic pulse interval is required")
	}

	dueObligations, err := s.countDueObligations(ctx)
	if err != nil {
		return PulseResult{}, err
	}
	blockedTasks, err := s.countBlockedTasks(ctx)
	if err != nil {
		return PulseResult{}, err
	}
	knownUnknowns, err := s.countKnownUnknowns(ctx)
	if err != nil {
		return PulseResult{}, err
	}
	staleAssessments, err := s.countStaleCapabilityAssessments(ctx)
	if err != nil {
		return PulseResult{}, err
	}
	candidate, err := s.scheduler.Next(ctx, capacity)
	if err != nil {
		return PulseResult{}, err
	}

	now := s.clock.Now().UTC()
	nextPulse, err := s.ensureStrategicPulse(ctx, now.Add(interval))
	if err != nil {
		return PulseResult{}, err
	}
	result := PulseResult{
		AuthorizedWork:             candidate != nil,
		DueObligations:             dueObligations,
		BlockedTasks:               blockedTasks,
		KnownUnknowns:              knownUnknowns,
		StaleCapabilityAssessments: staleAssessments,
		NextPulseAt:                nextPulse,
	}
	result.Dormant = !result.AuthorizedWork && result.DueObligations == 0 && result.BlockedTasks == 0 && result.KnownUnknowns == 0 && result.StaleCapabilityAssessments == 0
	return result, nil
}

func (s *Service) countDueObligations(ctx context.Context) (int, error) {
	rows, err := s.store.DB().QueryContext(ctx, `
		SELECT t.earliest_start
		FROM tasks t
		JOIN obligations o ON o.obligation_id = t.purpose_id AND o.active = 1
		WHERE t.purpose_kind = ? AND t.state IN (?, ?)`,
		domain.PurposeObligation, domain.TaskEligible, domain.TaskBlocked,
	)
	if err != nil {
		return 0, err
	}
	now := s.clock.Now().UTC()
	count := 0
	for rows.Next() {
		var earliest sql.NullString
		if err := rows.Scan(&earliest); err != nil {
			rows.Close()
			return 0, err
		}
		if !earliest.Valid {
			count++
			continue
		}
		start, err := time.Parse(time.RFC3339Nano, earliest.String)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("parse obligation earliest_start: %w", err)
		}
		if !start.After(now) {
			count++
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Service) countBlockedTasks(ctx context.Context) (int, error) {
	var count int
	if err := s.store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM tasks WHERE state = ?`, domain.TaskBlocked,
	).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Service) countKnownUnknowns(ctx context.Context) (int, error) {
	var count int
	if err := s.store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM external_operations WHERE state = ?`, domain.OperationOutcomeUnknown,
	).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Service) countStaleCapabilityAssessments(ctx context.Context) (int, error) {
	if s.freshness == nil {
		return 0, nil
	}
	count, err := s.freshness.StaleCapabilityAssessments(ctx)
	if err != nil {
		return 0, err
	}
	if count < 0 {
		return 0, errors.New("stale capability assessment count cannot be negative")
	}
	return count, nil
}

func (s *Service) ensureStrategicPulse(ctx context.Context, desired time.Time) (time.Time, error) {
	now := s.clock.Now().UTC()
	var scheduled time.Time
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var wakeupID domain.ID
		var rawDue string
		err := tx.QueryRowContext(ctx, `
			SELECT wakeup_id, due_at
			FROM scheduled_wakeups
			WHERE kind = ? AND state = 'PENDING' AND due_at > ?
			ORDER BY due_at, wakeup_id
			LIMIT 1`, KindStrategicPulse, formatTime(now),
		).Scan(&wakeupID, &rawDue)
		if err == nil {
			existing, err := time.Parse(time.RFC3339Nano, rawDue)
			if err != nil {
				return fmt.Errorf("parse strategic pulse deadline: %w", err)
			}
			if desired.Before(existing) {
				if _, err := tx.ExecContext(ctx,
					`UPDATE scheduled_wakeups SET due_at = ? WHERE wakeup_id = ? AND state = 'PENDING'`,
					formatTime(desired), wakeupID,
				); err != nil {
					return err
				}
				scheduled = desired.UTC()
				return nil
			}
			scheduled = existing
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		wakeupID = domain.NewID("wakeup")
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO scheduled_wakeups(wakeup_id, kind, task_id, due_at, state, created_at)
			VALUES (?, ?, NULL, ?, 'PENDING', ?)`,
			wakeupID, KindStrategicPulse, formatTime(desired), formatTime(now),
		); err != nil {
			return err
		}
		scheduled = desired.UTC()
		return nil
	})
	return scheduled, err
}

func (k Kind) valid() bool {
	switch k {
	case KindTaskReevaluation, KindStrategicPulse:
		return true
	default:
		return false
	}
}

func formatTime(value time.Time) string {
	return domain.FormatTime(value)
}

func (s *Service) configured() error {
	if s == nil || s.store == nil || s.clock == nil {
		return errors.New("wake service is not configured")
	}
	return nil
}
