package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

func (s *Service) Task(ctx context.Context, taskID domain.ID) (domain.Task, error) {
	if err := s.configured(); err != nil {
		return domain.Task{}, err
	}
	taskID = domain.ID(strings.TrimSpace(string(taskID)))
	if taskID == "" {
		return domain.Task{}, errors.New("task id is required")
	}
	return loadTask(ctx, s.store.DB(), taskID)
}

func (s *Service) FindByIdempotencyKey(ctx context.Context, key string) (domain.Task, bool, error) {
	if err := s.configured(); err != nil {
		return domain.Task{}, false, err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return domain.Task{}, false, errors.New("idempotency key is required")
	}
	var taskID domain.ID
	if err := s.store.DB().QueryRowContext(ctx, `SELECT task_id FROM tasks WHERE idempotency_key = ?`, key).Scan(&taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Task{}, false, nil
		}
		return domain.Task{}, false, err
	}
	task, err := loadTask(ctx, s.store.DB(), taskID)
	if err != nil {
		return domain.Task{}, false, err
	}
	return task, true, nil
}

// Challenge is one challenge of a task: the scope it was raised at, the reason
// given for it, and the evidence recorded with it. A challenge is inert, so the
// reason is the only account of why the work stopped, and it is the last thing
// recorded about a task that can no longer run.
type Challenge struct {
	ID          domain.ID
	TaskID      domain.ID
	Scope       domain.ChallengeScope
	Reason      string
	EvidenceIDs []domain.ID
	CreatedAt   time.Time
}

// Challenge returns the most recently recorded challenge of a task. Challenge
// rows are never rewritten or removed, so "the most recent" is the one that
// explains the task's current state, and the challenge ID breaks a tie between
// two written in the same instant the way it does everywhere else in the store.
func (s *Service) Challenge(ctx context.Context, taskID domain.ID) (Challenge, bool, error) {
	if err := s.configured(); err != nil {
		return Challenge{}, false, err
	}
	taskID = domain.ID(strings.TrimSpace(string(taskID)))
	if taskID == "" {
		return Challenge{}, false, errors.New("task id is required")
	}
	var (
		challenge   Challenge
		evidenceRaw string
		createdAt   string
	)
	err := s.store.DB().QueryRowContext(ctx, `
		SELECT challenge_id, task_id, scope, reason, evidence_ids_json, created_at
		FROM task_challenges WHERE task_id = ?
		ORDER BY created_at DESC, challenge_id DESC LIMIT 1`, taskID,
	).Scan(
		&challenge.ID, &challenge.TaskID, &challenge.Scope, &challenge.Reason, &evidenceRaw, &createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Challenge{}, false, nil
	}
	if err != nil {
		return Challenge{}, false, err
	}
	if challenge.EvidenceIDs, err = decodeEvidenceIDs(evidenceRaw); err != nil {
		return Challenge{}, false, fmt.Errorf("decode challenge %s evidence: %w", challenge.ID, err)
	}
	challenge.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return Challenge{}, false, fmt.Errorf("parse challenge %s created_at: %w", challenge.ID, err)
	}
	return challenge, true, nil
}

// decodeEvidenceIDs reads an evidence_ids_json column. A column that is not a
// JSON array of IDs is reported rather than dropped: a reader that treated it as
// empty would report "recorded against no evidence" for a challenge that named
// some, and "no challenge" for a challenge that exists. A column that decodes to
// no IDs is a different answer and a true one - the caller was handed nothing,
// which is a fact about the challenge rather than about the read.
func decodeEvidenceIDs(raw string) ([]domain.ID, error) {
	var ids []domain.ID
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

func (s *Service) Attempt(ctx context.Context, attemptID domain.ID) (domain.Attempt, error) {
	if err := s.configured(); err != nil {
		return domain.Attempt{}, err
	}
	attemptID = domain.ID(strings.TrimSpace(string(attemptID)))
	if attemptID == "" {
		return domain.Attempt{}, errors.New("attempt id is required")
	}
	var attempt domain.Attempt
	var leaseExpires, startedAt string
	var completedAt sql.NullString
	err := s.store.DB().QueryRowContext(ctx, `
		SELECT attempt_id, task_id, state, fence_generation, lease_state, lease_expires_at,
		       executor_kind, started_at, completed_at
		FROM attempts WHERE attempt_id = ?`, attemptID,
	).Scan(
		&attempt.ID, &attempt.TaskID, &attempt.State, &attempt.FenceGeneration, &attempt.LeaseState,
		&leaseExpires, &attempt.ExecutorKind, &startedAt, &completedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Attempt{}, fmt.Errorf("attempt %q not found", attemptID)
	}
	if err != nil {
		return domain.Attempt{}, err
	}
	attempt.LeaseExpiresAt, err = time.Parse(time.RFC3339Nano, leaseExpires)
	if err != nil {
		return domain.Attempt{}, fmt.Errorf("parse attempt lease expiry: %w", err)
	}
	attempt.StartedAt, err = time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return domain.Attempt{}, fmt.Errorf("parse attempt start time: %w", err)
	}
	if completedAt.Valid {
		attempt.CompletedAt, err = time.Parse(time.RFC3339Nano, completedAt.String)
		if err != nil {
			return domain.Attempt{}, fmt.Errorf("parse attempt completion time: %w", err)
		}
	}
	return attempt, nil
}
