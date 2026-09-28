package execution

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/purpose"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type TaskRequest struct {
	Purpose              domain.PurposeRef
	TaskClass            string
	Objective            string
	PayloadJSON          json.RawMessage
	IdempotencyKey       string
	AcceptanceCriteria   []string
	RequiredCapabilities []string
	RequiredEnforcement  domain.EnforcementLevel
	AuthorityCeiling     []string
	ResourceEnvelopeID   domain.ID
	Priority             int
	EarliestStart        time.Time
	Deadline             time.Time
}

type GuardedAttempt struct {
	TaskID          domain.ID
	AttemptID       domain.ID
	FenceGeneration int64
	LeaseExpiresAt  time.Time
	TaskState       domain.TaskState
}

type AttemptStartRecorder interface {
	RecordAttemptStartInTx(context.Context, *sql.Tx, domain.Attempt, domain.Task) error
}

type Service struct {
	store          *state.Store
	clock          clock.Clock
	purpose        *purpose.Service
	startRecorders []AttemptStartRecorder
}

func New(store *state.Store, clk clock.Clock, purposes *purpose.Service, recorders ...AttemptStartRecorder) *Service {
	active := make([]AttemptStartRecorder, 0, len(recorders))
	for _, recorder := range recorders {
		if recorder != nil {
			active = append(active, recorder)
		}
	}
	return &Service{store: store, clock: clk, purpose: purposes, startRecorders: active}
}

func (s *Service) CreateTask(ctx context.Context, request TaskRequest) (domain.Task, error) {
	return s.CreateTaskWithGuard(ctx, request, nil)
}

// TaskGuard checks an external precondition in the same transaction that
// inserts or replays a Task. A guard should only read through tx.
type TaskGuard func(context.Context, *sql.Tx) error

func (s *Service) CreateTaskWithGuard(ctx context.Context, request TaskRequest, guard TaskGuard) (domain.Task, error) {
	if err := s.configured(); err != nil {
		return domain.Task{}, err
	}
	normalized, err := normalizeRootRequest(request)
	if err != nil {
		return domain.Task{}, err
	}
	return s.insertTask(ctx, "", normalized, guard)
}

func (s *Service) CreateChildTask(ctx context.Context, parentTaskID domain.ID, request TaskRequest) (domain.Task, error) {
	if err := s.configured(); err != nil {
		return domain.Task{}, err
	}
	parent, err := loadTask(ctx, s.store.DB(), parentTaskID)
	if err != nil {
		return domain.Task{}, err
	}

	if request.Purpose.Kind == "" && request.Purpose.ID == "" {
		request.Purpose = parent.Purpose
	} else if request.Purpose != parent.Purpose {
		return domain.Task{}, errors.New("child task cannot change inherited purpose")
	}

	request.TaskClass = strings.TrimSpace(request.TaskClass)
	if request.TaskClass == "" {
		request.TaskClass = parent.TaskClass
	}
	request.RequiredCapabilities = normalizeStrings(request.RequiredCapabilities)
	parentCapabilities := stringSet(parent.AuthorityCeiling)
	for _, capability := range request.RequiredCapabilities {
		if _, ok := parentCapabilities[capability]; !ok {
			return domain.Task{}, fmt.Errorf("child capability %q exceeds parent authority ceiling", capability)
		}
	}

	if len(request.AuthorityCeiling) == 0 {
		request.AuthorityCeiling = append([]string(nil), parent.AuthorityCeiling...)
	} else {
		request.AuthorityCeiling = normalizeStrings(request.AuthorityCeiling)
		for _, capability := range request.AuthorityCeiling {
			if _, ok := parentCapabilities[capability]; !ok {
				return domain.Task{}, fmt.Errorf("child authority %q exceeds parent authority ceiling", capability)
			}
		}
	}
	childAuthority := stringSet(request.AuthorityCeiling)
	for _, capability := range request.RequiredCapabilities {
		if _, ok := childAuthority[capability]; !ok {
			return domain.Task{}, fmt.Errorf("required capability %q exceeds child authority ceiling", capability)
		}
	}

	if request.ResourceEnvelopeID == "" {
		request.ResourceEnvelopeID = parent.ResourceEnvelopeID
	} else if request.ResourceEnvelopeID != parent.ResourceEnvelopeID {
		return domain.Task{}, errors.New("child task cannot replace inherited resource envelope")
	}

	if request.RequiredEnforcement == "" {
		request.RequiredEnforcement = parent.RequiredEnforcement
	} else if !validEnforcement(request.RequiredEnforcement) {
		return domain.Task{}, fmt.Errorf("invalid enforcement level %q", request.RequiredEnforcement)
	} else if enforcementRank(request.RequiredEnforcement) < enforcementRank(parent.RequiredEnforcement) {
		return domain.Task{}, errors.New("child task cannot weaken parent enforcement requirement")
	}

	request.AcceptanceCriteria = normalizeCriteria(request.AcceptanceCriteria)
	if len(request.AcceptanceCriteria) == 0 {
		return domain.Task{}, errors.New("child task requires acceptance criteria")
	}
	if request.EarliestStart.IsZero() {
		request.EarliestStart = parent.EarliestStart
	} else if !parent.EarliestStart.IsZero() && request.EarliestStart.Before(parent.EarliestStart) {
		return domain.Task{}, errors.New("child task cannot start before parent time window")
	}
	if request.Deadline.IsZero() {
		request.Deadline = parent.Deadline
	} else if !parent.Deadline.IsZero() && request.Deadline.After(parent.Deadline) {
		return domain.Task{}, errors.New("child task cannot extend parent deadline")
	}
	if request.Priority == 0 {
		request.Priority = parent.Priority
	}
	if err := validateTimeWindow(request.EarliestStart, request.Deadline); err != nil {
		return domain.Task{}, err
	}

	return s.insertTask(ctx, parent.ID, request, nil)
}

func (s *Service) StartAttempt(ctx context.Context, taskID domain.ID, executorKind string, leaseDuration time.Duration) (domain.Attempt, error) {
	if err := s.configured(); err != nil {
		return domain.Attempt{}, err
	}
	if taskID == "" || strings.TrimSpace(executorKind) == "" || leaseDuration <= 0 {
		return domain.Attempt{}, errors.New("task, executor kind, and positive lease duration are required")
	}

	now := s.clock.Now().UTC()
	expires := now.Add(leaseDuration)
	attempt := domain.Attempt{
		ID:             domain.NewID("attempt"),
		TaskID:         taskID,
		State:          domain.AttemptLeased,
		LeaseState:     domain.LeaseActive,
		LeaseExpiresAt: expires,
		ExecutorKind:   strings.TrimSpace(executorKind),
		StartedAt:      now,
	}

	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var taskState domain.TaskState
		var fence int64
		if err := tx.QueryRowContext(ctx,
			`SELECT state, current_fence FROM tasks WHERE task_id = ?`, taskID,
		).Scan(&taskState, &fence); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("task %q not found", taskID)
			}
			return err
		}
		if taskState != domain.TaskEligible {
			return fmt.Errorf("task %q is %s, not ELIGIBLE", taskID, taskState)
		}
		task, err := loadTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		attempt.FenceGeneration = fence + 1
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO attempts(attempt_id, task_id, state, fence_generation, lease_state, lease_expires_at, started_at, executor_kind)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			attempt.ID, attempt.TaskID, attempt.State, attempt.FenceGeneration, attempt.LeaseState,
			formatTime(expires), formatTime(now), attempt.ExecutorKind,
		); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE tasks SET state = ?, current_attempt_id = ?, current_fence = ?, updated_at = ?
			 WHERE task_id = ? AND state = ? AND current_fence = ?`,
			domain.TaskExecuting, attempt.ID, attempt.FenceGeneration, formatTime(now),
			taskID, domain.TaskEligible, fence,
		)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return domain.ErrStaleAttempt
		}
		for _, recorder := range s.startRecorders {
			if err := recorder.RecordAttemptStartInTx(ctx, tx, attempt, task); err != nil {
				return fmt.Errorf("record Attempt start provenance: %w", err)
			}
		}
		return appendEvent(ctx, tx, taskID, attempt.ID, "ATTEMPT_LEASED", now)
	})
	if err != nil {
		return domain.Attempt{}, err
	}
	return attempt, nil
}

func (s *Service) RenewLease(ctx context.Context, attemptID domain.ID, leaseDuration time.Duration) error {
	if leaseDuration <= 0 {
		return errors.New("positive lease duration is required")
	}
	return s.WithGuardedAttempt(ctx, attemptID, []domain.TaskState{domain.TaskExecuting}, func(tx *sql.Tx, guarded GuardedAttempt) error {
		expires := s.clock.Now().UTC().Add(leaseDuration)
		_, err := tx.ExecContext(ctx,
			`UPDATE attempts SET lease_expires_at = ? WHERE attempt_id = ? AND lease_state = ?`,
			formatTime(expires), guarded.AttemptID, domain.LeaseActive,
		)
		return err
	})
}

func (s *Service) RevokeLease(ctx context.Context, attemptID domain.ID) error {
	if err := s.configured(); err != nil {
		return err
	}
	now := s.clock.Now().UTC()
	return s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var taskID domain.ID
		var leaseState domain.LeaseState
		if err := tx.QueryRowContext(ctx,
			`SELECT task_id, lease_state FROM attempts WHERE attempt_id = ?`, attemptID,
		).Scan(&taskID, &leaseState); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return domain.ErrStaleAttempt
			}
			return err
		}
		if leaseState != domain.LeaseActive {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE attempts SET lease_state = ?, state = ?, completed_at = ? WHERE attempt_id = ?`,
			domain.LeaseRevoked, domain.AttemptCancelled, formatTime(now), attemptID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET state = CASE WHEN current_attempt_id = ? AND state = ? THEN ? ELSE state END, updated_at = ? WHERE task_id = ?`,
			attemptID, domain.TaskExecuting, domain.TaskEligible, formatTime(now), taskID,
		); err != nil {
			return err
		}
		return appendEvent(ctx, tx, taskID, attemptID, "LEASE_REVOKED", now)
	})
}

func (s *Service) GuardAttempt(ctx context.Context, tx *sql.Tx, attemptID domain.ID, allowedTaskStates ...domain.TaskState) (GuardedAttempt, error) {
	if err := s.configured(); err != nil {
		return GuardedAttempt{}, err
	}
	if tx == nil {
		return GuardedAttempt{}, errors.New("guard requires transaction")
	}

	var guarded GuardedAttempt
	var leaseState domain.LeaseState
	var leaseExpires string
	var currentAttempt sql.NullString
	var currentFence int64
	if err := tx.QueryRowContext(ctx, `
		SELECT a.task_id, a.attempt_id, a.fence_generation, a.lease_state, a.lease_expires_at,
		       t.current_attempt_id, t.current_fence, t.state
		FROM attempts a
		JOIN tasks t ON t.task_id = a.task_id
		WHERE a.attempt_id = ?`, attemptID,
	).Scan(
		&guarded.TaskID, &guarded.AttemptID, &guarded.FenceGeneration, &leaseState, &leaseExpires,
		&currentAttempt, &currentFence, &guarded.TaskState,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return GuardedAttempt{}, domain.ErrStaleAttempt
		}
		return GuardedAttempt{}, err
	}

	if !currentAttempt.Valid || domain.ID(currentAttempt.String) != guarded.AttemptID || currentFence != guarded.FenceGeneration {
		return GuardedAttempt{}, domain.ErrStaleAttempt
	}
	expires, err := time.Parse(time.RFC3339Nano, leaseExpires)
	if err != nil {
		return GuardedAttempt{}, fmt.Errorf("parse lease expiry: %w", err)
	}
	guarded.LeaseExpiresAt = expires
	if leaseState != domain.LeaseActive || !expires.After(s.clock.Now().UTC()) {
		return GuardedAttempt{}, domain.ErrLeaseInactive
	}
	if !stateAllowed(guarded.TaskState, allowedTaskStates) {
		return GuardedAttempt{}, domain.ErrStaleAttempt
	}
	return guarded, nil
}

func (s *Service) WithGuardedAttempt(ctx context.Context, attemptID domain.ID, allowedTaskStates []domain.TaskState, fn func(*sql.Tx, GuardedAttempt) error) error {
	if fn == nil {
		return errors.New("guarded mutation callback is required")
	}
	return s.store.WithTx(ctx, func(tx *sql.Tx) error {
		guarded, err := s.GuardAttempt(ctx, tx, attemptID, allowedTaskStates...)
		if err != nil {
			return err
		}
		return fn(tx, guarded)
	})
}

func (s *Service) FailAttempt(ctx context.Context, attemptID domain.ID, class domain.FailureClass, signature string, evidenceIDs []domain.ID) error {
	signature = strings.TrimSpace(signature)
	if signature == "" || !validFailureClass(class) {
		return errors.New("valid failure class and signature are required")
	}
	evidenceJSON, err := json.Marshal(evidenceIDs)
	if err != nil {
		return err
	}
	now := s.clock.Now().UTC()
	return s.WithGuardedAttempt(ctx, attemptID, []domain.TaskState{domain.TaskExecuting}, func(tx *sql.Tx, guarded GuardedAttempt) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO attempt_failures(failure_id, attempt_id, task_id, failure_class, signature, evidence_ids_json, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			domain.NewID("failure"), attemptID, guarded.TaskID, class, signature, string(evidenceJSON), formatTime(now),
		); err != nil {
			return err
		}
		var repeats int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM attempt_failures WHERE task_id = ? AND signature = ?`, guarded.TaskID, signature,
		).Scan(&repeats); err != nil {
			return err
		}
		newState := domain.TaskEligible
		if repeats > 1 {
			newState = domain.TaskBlocked
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE attempts SET state = ?, lease_state = ?, completed_at = ? WHERE attempt_id = ?`,
			domain.AttemptFailed, domain.LeaseRevoked, formatTime(now), attemptID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET state = ?, updated_at = ? WHERE task_id = ?`,
			newState, formatTime(now), guarded.TaskID,
		); err != nil {
			return err
		}
		return appendEvent(ctx, tx, guarded.TaskID, attemptID, "ATTEMPT_FAILED", now)
	})
}

func (s *Service) ChallengeTask(ctx context.Context, taskID domain.ID, scope domain.ChallengeScope, reason string, evidenceIDs []domain.ID) error {
	if err := s.configured(); err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	if taskID == "" || reason == "" || !validChallengeScope(scope) {
		return errors.New("task, valid challenge scope, and reason are required")
	}
	evidenceJSON, err := json.Marshal(evidenceIDs)
	if err != nil {
		return err
	}
	now := s.clock.Now().UTC()
	return s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var currentAttempt sql.NullString
		if err := tx.QueryRowContext(ctx,
			`SELECT current_attempt_id FROM tasks WHERE task_id = ?`, taskID,
		).Scan(&currentAttempt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("task %q not found", taskID)
			}
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO task_challenges(challenge_id, task_id, scope, reason, evidence_ids_json, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			domain.NewID("challenge"), taskID, scope, reason, string(evidenceJSON), formatTime(now),
		); err != nil {
			return err
		}
		if currentAttempt.Valid {
			if _, err := tx.ExecContext(ctx,
				`UPDATE attempts SET lease_state = ?, state = ?, completed_at = COALESCE(completed_at, ?) WHERE attempt_id = ? AND lease_state = ?`,
				domain.LeaseRevoked, domain.AttemptCancelled, formatTime(now), currentAttempt.String, domain.LeaseActive,
			); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET state = ?, updated_at = ? WHERE task_id = ?`,
			domain.TaskChallenged, formatTime(now), taskID,
		); err != nil {
			return err
		}
		return appendEvent(ctx, tx, taskID, domain.ID(currentAttempt.String), "TASK_CHALLENGED", now)
	})
}

// ChallengeTaskIfInStates challenges a task only when it is currently in one of
// the supplied states. The caller must re-read the task when changed is false,
// because the state may have moved since it was read. An accepted task can
// never be returned to CHALLENGED, which is the property the unguarded
// ChallengeTask lacks.
func (s *Service) ChallengeTaskIfInStates(ctx context.Context, taskID domain.ID, states []domain.TaskState, scope domain.ChallengeScope, reason string, evidenceIDs []domain.ID) (changed bool, err error) {
	if err := s.configured(); err != nil {
		return false, err
	}
	taskID = domain.ID(strings.TrimSpace(string(taskID)))
	reason = strings.TrimSpace(reason)
	if taskID == "" || reason == "" || !validChallengeScope(scope) {
		return false, errors.New("task, valid challenge scope, and reason are required")
	}
	allowed := make([]string, 0, len(states))
	for _, state := range states {
		if trimmed := strings.TrimSpace(string(state)); trimmed != "" {
			allowed = append(allowed, trimmed)
		}
	}
	if len(allowed) == 0 {
		return false, errors.New("at least one task state is required")
	}
	evidenceJSON, err := json.Marshal(evidenceIDs)
	if err != nil {
		return false, err
	}
	now := s.clock.Now().UTC()

	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var currentAttempt sql.NullString
		if err := tx.QueryRowContext(ctx,
			`SELECT current_attempt_id FROM tasks WHERE task_id = ?`, taskID,
		).Scan(&currentAttempt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("task %q not found", taskID)
			}
			return err
		}

		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(allowed)), ",")
		args := []any{domain.TaskChallenged, formatTime(now), taskID}
		for _, state := range allowed {
			args = append(args, state)
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE tasks SET state = ?, updated_at = ? WHERE task_id = ? AND state IN (`+placeholders+`)`,
			args...)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return nil
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO task_challenges(challenge_id, task_id, scope, reason, evidence_ids_json, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			domain.NewID("challenge"), taskID, scope, reason, string(evidenceJSON), formatTime(now),
		); err != nil {
			return err
		}
		if currentAttempt.Valid {
			if _, err := tx.ExecContext(ctx,
				`UPDATE attempts SET lease_state = ?, state = ?, completed_at = COALESCE(completed_at, ?) WHERE attempt_id = ? AND lease_state = ?`,
				domain.LeaseRevoked, domain.AttemptCancelled, formatTime(now), currentAttempt.String, domain.LeaseActive,
			); err != nil {
				return err
			}
		}
		if err := appendEvent(ctx, tx, taskID, domain.ID(currentAttempt.String), "TASK_CHALLENGED", now); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

func (s *Service) insertTask(ctx context.Context, parentID domain.ID, request TaskRequest, guard TaskGuard) (domain.Task, error) {
	var task domain.Task
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		task, err = s.insertTaskTx(ctx, tx, parentID, request, guard)
		return err
	})
	if err != nil {
		return domain.Task{}, err
	}
	return task, nil
}

// CreateTaskWithGuardInTx inserts or replays a Task inside an enclosing
// transaction so canonical writes that must commit together share one
// transaction. SQLite runs a single connection, so callers must never open a
// nested transaction.
func (s *Service) CreateTaskWithGuardInTx(ctx context.Context, tx *sql.Tx, request TaskRequest, guard TaskGuard) (domain.Task, error) {
	if err := s.configured(); err != nil {
		return domain.Task{}, err
	}
	if tx == nil {
		return domain.Task{}, errors.New("SQL transaction is required")
	}
	normalized, err := normalizeRootRequest(request)
	if err != nil {
		return domain.Task{}, err
	}
	return s.insertTaskTx(ctx, tx, domain.ID(""), normalized, guard)
}

func (s *Service) insertTaskTx(ctx context.Context, tx *sql.Tx, parentID domain.ID, request TaskRequest, guard TaskGuard) (domain.Task, error) {
	request, err := normalizeTaskIntent(request)
	if err != nil {
		return domain.Task{}, err
	}
	var requestHash string
	if request.IdempotencyKey != "" {
		encoded, err := json.Marshal(struct {
			ParentTaskID domain.ID
			TaskRequest
		}{parentID, request})
		if err != nil {
			return domain.Task{}, err
		}
		digest := sha256.Sum256(encoded)
		requestHash = hex.EncodeToString(digest[:])
	}
	now := s.clock.Now().UTC()
	task := domain.Task{
		ID:                   domain.NewID("task"),
		ParentTaskID:         parentID,
		Purpose:              request.Purpose,
		TaskClass:            request.TaskClass,
		Objective:            request.Objective,
		PayloadJSON:          append(json.RawMessage(nil), request.PayloadJSON...),
		IdempotencyKey:       request.IdempotencyKey,
		State:                domain.TaskEligible,
		AcceptanceCriteria:   append([]string(nil), request.AcceptanceCriteria...),
		RequiredCapabilities: append([]string(nil), request.RequiredCapabilities...),
		RequiredEnforcement:  request.RequiredEnforcement,
		AuthorityCeiling:     append([]string(nil), request.AuthorityCeiling...),
		ResourceEnvelopeID:   request.ResourceEnvelopeID,
		Priority:             request.Priority,
		EarliestStart:        request.EarliestStart,
		Deadline:             request.Deadline,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	criteriaJSON, err := json.Marshal(task.AcceptanceCriteria)
	if err != nil {
		return domain.Task{}, err
	}
	capabilitiesJSON, err := json.Marshal(task.RequiredCapabilities)
	if err != nil {
		return domain.Task{}, err
	}
	authorityJSON, err := json.Marshal(task.AuthorityCeiling)
	if err != nil {
		return domain.Task{}, err
	}

	if guard != nil {
		if err := guard(ctx, tx); err != nil {
			return domain.Task{}, err
		}
	}
	result, err := tx.ExecContext(ctx, `
	INSERT INTO tasks(
		task_id, parent_task_id, purpose_kind, purpose_id, task_class, objective, payload_json,
		idempotency_key, request_hash, state, current_fence,
		acceptance_criteria_json, required_capabilities_json, required_enforcement,
		authority_ceiling_json, resource_envelope_id, priority, earliest_start, deadline,
		created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING`,
		task.ID, nullableID(parentID), task.Purpose.Kind, task.Purpose.ID, task.TaskClass,
		task.Objective, string(task.PayloadJSON), nullableString(task.IdempotencyKey), nullableString(requestHash), task.State,
		string(criteriaJSON), string(capabilitiesJSON), task.RequiredEnforcement,
		string(authorityJSON), task.ResourceEnvelopeID, task.Priority,
		nullableTime(task.EarliestStart), nullableTime(task.Deadline), formatTime(now), formatTime(now),
	)
	if err != nil {
		return domain.Task{}, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return domain.Task{}, err
	}
	if inserted == 1 {
		if err := s.purpose.ValidatePurposeTx(ctx, tx, task.Purpose); err != nil {
			return domain.Task{}, err
		}
		return task, nil
	}
	var existingID domain.ID
	var existingHash string
	if err := tx.QueryRowContext(ctx,
		`SELECT task_id, request_hash FROM tasks WHERE idempotency_key = ?`, task.IdempotencyKey,
	).Scan(&existingID, &existingHash); err != nil {
		return domain.Task{}, err
	}
	if existingHash != requestHash {
		return domain.Task{}, fmt.Errorf("task idempotency key %q conflicts with a different request", task.IdempotencyKey)
	}
	task, err = loadTask(ctx, tx, existingID)
	if err != nil {
		return domain.Task{}, err
	}
	return task, nil
}

func loadTask(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, taskID domain.ID) (domain.Task, error) {
	var task domain.Task
	var parentID, currentAttempt, earliest, deadline sql.NullString
	var purposeKind string
	var stateValue string
	var enforcement string
	var criteriaJSON, capabilitiesJSON, authorityJSON, payloadJSON string
	var idempotencyKey sql.NullString
	var createdAt, updatedAt string
	if err := q.QueryRowContext(ctx, `
		SELECT task_id, parent_task_id, purpose_kind, purpose_id, task_class, objective, payload_json,
		       idempotency_key, state, current_attempt_id, current_fence,
		       acceptance_criteria_json, required_capabilities_json, required_enforcement,
		       authority_ceiling_json, resource_envelope_id, priority, earliest_start, deadline,
		       created_at, updated_at
		FROM tasks WHERE task_id = ?`, taskID,
	).Scan(
		&task.ID, &parentID, &purposeKind, &task.Purpose.ID, &task.TaskClass, &task.Objective, &payloadJSON,
		&idempotencyKey, &stateValue, &currentAttempt, &task.CurrentFence,
		&criteriaJSON, &capabilitiesJSON, &enforcement, &authorityJSON, &task.ResourceEnvelopeID,
		&task.Priority, &earliest, &deadline, &createdAt, &updatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Task{}, fmt.Errorf("task %q not found", taskID)
		}
		return domain.Task{}, err
	}
	task.ParentTaskID = domain.ID(parentID.String)
	task.PayloadJSON = json.RawMessage(payloadJSON)
	task.IdempotencyKey = idempotencyKey.String
	task.CurrentAttemptID = domain.ID(currentAttempt.String)
	task.Purpose.Kind = domain.PurposeKind(purposeKind)
	task.State = domain.TaskState(stateValue)
	task.RequiredEnforcement = domain.EnforcementLevel(enforcement)
	if err := json.Unmarshal([]byte(criteriaJSON), &task.AcceptanceCriteria); err != nil {
		return domain.Task{}, err
	}
	if err := json.Unmarshal([]byte(capabilitiesJSON), &task.RequiredCapabilities); err != nil {
		return domain.Task{}, err
	}
	if err := json.Unmarshal([]byte(authorityJSON), &task.AuthorityCeiling); err != nil {
		return domain.Task{}, err
	}
	var err error
	if task.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return domain.Task{}, err
	}
	if task.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return domain.Task{}, err
	}
	if earliest.Valid {
		if task.EarliestStart, err = time.Parse(time.RFC3339Nano, earliest.String); err != nil {
			return domain.Task{}, err
		}
	}
	if deadline.Valid {
		if task.Deadline, err = time.Parse(time.RFC3339Nano, deadline.String); err != nil {
			return domain.Task{}, err
		}
	}
	return task, nil
}

func normalizeRootRequest(request TaskRequest) (TaskRequest, error) {
	request.TaskClass = strings.TrimSpace(request.TaskClass)
	request.AcceptanceCriteria = normalizeCriteria(request.AcceptanceCriteria)
	if len(request.AcceptanceCriteria) == 0 {
		return TaskRequest{}, errors.New("task requires acceptance criteria")
	}
	request.RequiredCapabilities = normalizeStrings(request.RequiredCapabilities)
	if request.RequiredEnforcement == "" {
		request.RequiredEnforcement = domain.EnforcementUnenforced
	}
	if !validEnforcement(request.RequiredEnforcement) {
		return TaskRequest{}, fmt.Errorf("invalid enforcement level %q", request.RequiredEnforcement)
	}
	if request.ResourceEnvelopeID == "" {
		return TaskRequest{}, errors.New("task requires resource envelope")
	}
	request.AuthorityCeiling = normalizeStrings(request.AuthorityCeiling)
	if len(request.AuthorityCeiling) == 0 {
		request.AuthorityCeiling = append([]string(nil), request.RequiredCapabilities...)
	}
	authority := stringSet(request.AuthorityCeiling)
	for _, capability := range request.RequiredCapabilities {
		if _, ok := authority[capability]; !ok {
			return TaskRequest{}, fmt.Errorf("required capability %q exceeds task authority ceiling", capability)
		}
	}
	if err := validateTimeWindow(request.EarliestStart, request.Deadline); err != nil {
		return TaskRequest{}, err
	}
	return request, nil
}

func normalizeTaskIntent(request TaskRequest) (TaskRequest, error) {
	request.Objective = strings.TrimSpace(request.Objective)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if len(strings.TrimSpace(string(request.PayloadJSON))) == 0 {
		request.PayloadJSON = json.RawMessage(`{}`)
	} else {
		var value any
		decoder := json.NewDecoder(strings.NewReader(string(request.PayloadJSON)))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return TaskRequest{}, fmt.Errorf("invalid task payload JSON: %w", err)
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return TaskRequest{}, errors.New("invalid task payload JSON: multiple values")
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return TaskRequest{}, fmt.Errorf("normalize task payload JSON: %w", err)
		}
		request.PayloadJSON = encoded
	}
	request.EarliestStart = request.EarliestStart.UTC()
	request.Deadline = request.Deadline.UTC()
	return request, nil
}

func normalizeCriteria(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func normalizeStrings(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			set[value] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func stringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

func stateAllowed(state domain.TaskState, allowed []domain.TaskState) bool {
	for _, candidate := range allowed {
		if state == candidate {
			return true
		}
	}
	return false
}

func validEnforcement(level domain.EnforcementLevel) bool {
	switch level {
	case domain.EnforcementEnforced, domain.EnforcementPartial, domain.EnforcementUnenforced:
		return true
	default:
		return false
	}
}

func enforcementRank(level domain.EnforcementLevel) int {
	switch level {
	case domain.EnforcementEnforced:
		return 2
	case domain.EnforcementPartial:
		return 1
	default:
		return 0
	}
}

func validFailureClass(class domain.FailureClass) bool {
	switch class {
	case domain.FailureTransient, domain.FailureCapability, domain.FailureEpistemic,
		domain.FailurePlanning, domain.FailureResource, domain.FailureAuthority,
		domain.FailureObjectiveImpossible, domain.FailureExecution:
		return true
	default:
		return false
	}
}

func validChallengeScope(scope domain.ChallengeScope) bool {
	switch scope {
	case domain.ChallengeTask, domain.ChallengeParent, domain.ChallengeGoal, domain.ChallengeMissionAssumption:
		return true
	default:
		return false
	}
}

func validateTimeWindow(earliest, deadline time.Time) error {
	if !earliest.IsZero() && !deadline.IsZero() && deadline.Before(earliest) {
		return errors.New("task deadline precedes earliest start")
	}
	return nil
}

func appendEvent(ctx context.Context, tx *sql.Tx, taskID, attemptID domain.ID, eventType string, now time.Time) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO execution_events(event_id, task_id, attempt_id, event_type, details_json, created_at) VALUES (?, ?, ?, ?, '{}', ?)`,
		domain.NewID("event"), taskID, nullableID(attemptID), eventType, formatTime(now),
	)
	return err
}

func nullableID(id domain.ID) any {
	if id == "" {
		return nil
	}
	return string(id)
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return formatTime(value.UTC())
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func (s *Service) configured() error {
	if s == nil || s.store == nil || s.clock == nil || s.purpose == nil {
		return errors.New("execution service is not configured")
	}
	return nil
}
