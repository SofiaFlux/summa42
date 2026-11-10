package containment

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
	"github.com/SofiaFlux/summa42/internal/execution"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type Service struct {
	store            *state.Store
	clock            clock.Clock
	ownerPrincipalID domain.ID
	mu               sync.RWMutex
}

func NewService(store *state.Store, clk clock.Clock, ownerPrincipalID domain.ID) *Service {
	return &Service{
		store:            store,
		clock:            clk,
		ownerPrincipalID: ownerPrincipalID,
	}
}

func (s *Service) isAuthorizedOwner(ctx context.Context, caller domain.ID) bool {
	caller = domain.ID(strings.TrimSpace(string(caller)))
	if caller == "" {
		return false
	}
	if s.ownerPrincipalID != "" && caller == s.ownerPrincipalID {
		return true
	}
	var metaOwner, metaRoot string
	err := s.store.DB().QueryRowContext(ctx, `
		SELECT owner_principal_id, constitutional_root_principal_id
		FROM collective_metadata
		WHERE singleton = 1`,
	).Scan(&metaOwner, &metaRoot)
	if err == nil {
		if caller == domain.ID(metaOwner) || caller == domain.ID(metaRoot) {
			return true
		}
	}
	return false
}

// ApplySignal records a violation signal and transitions the attempt along the containment ladder:
// OBSERVE -> WARN -> RESTRICT -> TERMINATE.
func (s *Service) ApplySignal(ctx context.Context, attemptID, taskID domain.ID, signalName, enforcedBy string, step LadderStep, details map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	attemptID = domain.ID(strings.TrimSpace(string(attemptID)))
	taskID = domain.ID(strings.TrimSpace(string(taskID)))
	signalName = strings.TrimSpace(signalName)
	enforcedBy = strings.TrimSpace(enforcedBy)
	if attemptID == "" || taskID == "" || signalName == "" {
		return errors.New("attempt id, task id, and signal name are required")
	}

	detailsJSON := "{}"
	if details != nil {
		if b, err := json.Marshal(details); err == nil {
			detailsJSON = string(b)
		}
	}

	now := s.clock.Now().UTC()
	nowStr := domain.FormatTime(now)
	eventID := domain.NewID("contevent")

	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin containment tx: %w", err)
	}
	defer tx.Rollback()

	// Load existing containment state
	var existingStateStr, existingSignalsJSON string
	var existingInvTask sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT ladder_state, violation_signals_json, investigation_task_id
		FROM attempt_containments
		WHERE attempt_id = ?`,
		attemptID,
	).Scan(&existingStateStr, &existingSignalsJSON, &existingInvTask)

	var signals []string
	currentState := StateNominal
	if err == nil {
		currentState = LadderState(existingStateStr)
		_ = json.Unmarshal([]byte(existingSignalsJSON), &signals)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("load attempt containment: %w", err)
	}

	// Append signal if new
	hasSignal := false
	for _, sig := range signals {
		if sig == signalName {
			hasSignal = true
			break
		}
	}
	if !hasSignal {
		signals = append(signals, signalName)
	}
	signalsJSONBytes, _ := json.Marshal(signals)
	signalsJSON := string(signalsJSONBytes)

	// Determine next state (ladder escalations only; cannot downgrade via ApplySignal)
	targetState := currentState
	switch step {
	case StepObserve:
		// Observe logs the signal into the audit trail while keeping nominal or existing state
		if currentState == "" {
			targetState = StateNominal
		}
	case StepWarn:
		if currentState == StateNominal {
			targetState = StateWarning
		}
	case StepRestrict:
		if currentState == StateNominal || currentState == StateWarning {
			targetState = StateRestricted
		}
	case StepTerminate:
		targetState = StateSuspectedRogue
	default:
		return fmt.Errorf("%w: unknown ladder step %q", ErrInvalidLadderTransition, step)
	}

	var investigationTaskID *domain.ID
	if targetState == StateSuspectedRogue {
		// 1. Revoke the attempt lease through the execution kernel, so the
		// revocation is on the execution trail. An Attempt the kernel does not
		// know has no lease to revoke.
		if err := execution.RevokeLeaseTx(ctx, tx, attemptID, now, "containment:"+signalName); err != nil && !errors.Is(err, domain.ErrStaleAttempt) {
			return fmt.Errorf("revoke attempt lease on rogue termination: %w", err)
		}

		// 2. Revoke capability sessions immediately
		_, err = tx.ExecContext(ctx, `
			UPDATE capability_sessions
			SET revoked_at = COALESCE(revoked_at, ?)
			WHERE attempt_id = ?`,
			nowStr, attemptID,
		)
		if err != nil {
			return fmt.Errorf("revoke capability sessions on rogue termination: %w", err)
		}

		// 3. Mark the task as FAILED to prevent worker wedging, detaching current_attempt_id
		_, err = tx.ExecContext(ctx, `
			UPDATE tasks
			SET state = 'FAILED',
			    current_attempt_id = NULL,
			    updated_at = ?
			WHERE task_id = ?`,
			nowStr, taskID,
		)
		if err != nil {
			return fmt.Errorf("mark task failed on rogue termination: %w", err)
		}

		// 3. Create Investigation Task for the Owner if not already created
		if !existingInvTask.Valid || existingInvTask.String == "" {
			invTaskID := domain.NewID("task")
			envelopeID := domain.NewID("envelope")
			_, err = tx.ExecContext(ctx, `
				INSERT INTO resource_envelopes (envelope_id, hard_limit, created_at)
				VALUES (?, 0, ?)`,
				envelopeID, nowStr,
			)
			if err != nil {
				return fmt.Errorf("create investigation envelope: %w", err)
			}

			criteriaJSON, _ := json.Marshal([]string{
				fmt.Sprintf("Forensic investigation for suspected rogue attempt %s (signal: %s)", attemptID, signalName),
				"Verify forensic logs, manifests, and isolated workspace",
			})
			_, err = tx.ExecContext(ctx, `
				INSERT INTO tasks (
					task_id, parent_task_id, purpose_kind, purpose_id, state,
					current_fence, acceptance_criteria_json, required_capabilities_json,
					required_enforcement, authority_ceiling_json, resource_envelope_id,
					priority, created_at, updated_at
				) VALUES (
					?, ?, 'RECOVERY', 'recovery_investigation', 'ELIGIBLE',
					0, ?, '["diagnostics.read_logs","evidence.export"]',
					'ENFORCED', '["diagnostics.read_logs","evidence.export"]', ?,
					100, ?, ?
				)`,
				invTaskID, taskID, string(criteriaJSON), envelopeID, nowStr, nowStr,
			)
			if err != nil {
				return fmt.Errorf("create investigation task: %w", err)
			}
			investigationTaskID = &invTaskID
		} else {
			existingID := domain.ID(existingInvTask.String)
			investigationTaskID = &existingID
		}
	}

	// Persist attempt_containments row
	var invIDVal any
	if investigationTaskID != nil {
		invIDVal = string(*investigationTaskID)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO attempt_containments (
			attempt_id, task_id, ladder_state, violation_signals_json,
			investigation_task_id, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(attempt_id) DO UPDATE SET
			ladder_state = excluded.ladder_state,
			violation_signals_json = excluded.violation_signals_json,
			investigation_task_id = COALESCE(attempt_containments.investigation_task_id, excluded.investigation_task_id),
			updated_at = excluded.updated_at`,
		attemptID, taskID, string(targetState), signalsJSON,
		invIDVal, nowStr, nowStr,
	)
	if err != nil {
		return fmt.Errorf("upsert attempt containment: %w", err)
	}

	// Record immutable audit event
	_, err = tx.ExecContext(ctx, `
		INSERT INTO attempt_containment_events (
			event_id, attempt_id, task_id, ladder_step,
			signal_name, details_json, enforced_by, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		eventID, attemptID, taskID, string(step),
		signalName, detailsJSON, enforcedBy, nowStr,
	)
	if err != nil {
		return fmt.Errorf("record containment audit event: %w", err)
	}

	return tx.Commit()
}

// CheckAttemptAllowed verifies if an action is permitted for the given attempt.
func (s *Service) CheckAttemptAllowed(ctx context.Context, attemptID domain.ID, actionCategory string) error {
	attemptID = domain.ID(strings.TrimSpace(string(attemptID)))
	if attemptID == "" {
		return nil
	}

	var stateStr string
	err := s.store.DB().QueryRowContext(ctx, `
		SELECT ladder_state FROM attempt_containments WHERE attempt_id = ?`,
		attemptID,
	).Scan(&stateStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // Nominal, uncontained
		}
		return fmt.Errorf("query attempt containment: %w", err)
	}

	state := LadderState(stateStr)
	if state == StateSuspectedRogue {
		return fmt.Errorf("%w: attempt %q is quarantined as SUSPECTED_ROGUE", ErrAttemptSuspectedRogue, attemptID)
	}

	if state == StateRestricted {
		catLower := strings.ToLower(strings.TrimSpace(actionCategory))
		restrictedOps := []string{
			"create_child_task",
			"child_task",
			"external_operation",
			"credential_request",
			"authority_change",
		}
		for _, rop := range restrictedOps {
			if catLower == rop || strings.HasPrefix(catLower, rop+":") {
				return fmt.Errorf("%w: category %q is prohibited under attempt restriction", ErrAttemptRestricted, actionCategory)
			}
		}
	}

	return nil
}

// ClearRogueState clears the rogue containment state.
// Workers and attempts are strictly forbidden from clearing their own rogue state.
// Requires Owner authority.
func (s *Service) ClearRogueState(ctx context.Context, attemptID domain.ID, callerPrincipalID domain.ID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	attemptID = domain.ID(strings.TrimSpace(string(attemptID)))
	callerPrincipalID = domain.ID(strings.TrimSpace(string(callerPrincipalID)))
	reason = strings.TrimSpace(reason)
	if attemptID == "" || callerPrincipalID == "" || reason == "" {
		return errors.New("attempt id, caller principal id, and reason are required to clear rogue state")
	}

	if !s.isAuthorizedOwner(ctx, callerPrincipalID) {
		return fmt.Errorf("%w: caller %q is not owner", ErrSelfClearDenied, callerPrincipalID)
	}

	now := s.clock.Now().UTC()
	nowStr := domain.FormatTime(now)
	eventID := domain.NewID("contevent")

	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin clear rogue tx: %w", err)
	}
	defer tx.Rollback()

	var taskID domain.ID
	var currentState LadderState
	err = tx.QueryRowContext(ctx, `SELECT task_id, ladder_state FROM attempt_containments WHERE attempt_id = ?`, attemptID).Scan(&taskID, &currentState)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAttemptNotFound
		}
		return fmt.Errorf("query attempt containment: %w", err)
	}

	if currentState != StateSuspectedRogue && currentState != StateRestricted {
		return fmt.Errorf("attempt %s is in state %s; only RESTRICTED or SUSPECTED_ROGUE can be cleared", attemptID, currentState)
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE attempt_containments
		SET ladder_state = 'NOMINAL',
		    updated_at = ?
		WHERE attempt_id = ?`,
		nowStr, attemptID,
	)
	if err != nil {
		return fmt.Errorf("update attempt containment state: %w", err)
	}

	// Reset task to ELIGIBLE so it can be re-attempted safely by workers
	_, err = tx.ExecContext(ctx, `
		UPDATE tasks
		SET state = 'ELIGIBLE',
		    current_attempt_id = NULL,
		    updated_at = ?
		WHERE task_id = ? AND state = 'FAILED'`,
		nowStr, taskID,
	)
	if err != nil {
		return fmt.Errorf("reset task to eligible on rogue clear: %w", err)
	}

	detailsPayload, _ := json.Marshal(map[string]any{"reason": reason})
	_, err = tx.ExecContext(ctx, `
		INSERT INTO attempt_containment_events (
			event_id, attempt_id, task_id, ladder_step,
			signal_name, details_json, enforced_by, created_at
		) VALUES (?, ?, ?, 'OBSERVE', 'OWNER_CLEARED', ?, ?, ?)`,
		eventID, attemptID, taskID, string(detailsPayload), string(callerPrincipalID), nowStr,
	)
	if err != nil {
		return fmt.Errorf("record clear rogue audit event: %w", err)
	}

	return tx.Commit()
}

// GetRecord returns the containment status for an attempt.
func (s *Service) GetRecord(ctx context.Context, attemptID domain.ID) (ContainmentRecord, error) {
	attemptID = domain.ID(strings.TrimSpace(string(attemptID)))
	if attemptID == "" {
		return ContainmentRecord{}, errors.New("attempt id is required")
	}

	var rec ContainmentRecord
	var stateStr, signalsJSON, createdStr, updatedStr string
	var invTask sql.NullString

	err := s.store.DB().QueryRowContext(ctx, `
		SELECT attempt_id, task_id, ladder_state, violation_signals_json,
		       investigation_task_id, created_at, updated_at
		FROM attempt_containments
		WHERE attempt_id = ?`,
		attemptID,
	).Scan(
		&rec.AttemptID,
		&rec.TaskID,
		&stateStr,
		&signalsJSON,
		&invTask,
		&createdStr,
		&updatedStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ContainmentRecord{}, ErrAttemptNotFound
		}
		return ContainmentRecord{}, fmt.Errorf("query attempt containment record: %w", err)
	}

	rec.LadderState = LadderState(stateStr)
	_ = json.Unmarshal([]byte(signalsJSON), &rec.ViolationSignals)
	if invTask.Valid {
		rec.InvestigationTaskID = domain.ID(invTask.String)
	}
	rec.CreatedAt, _ = parseTime(createdStr)
	rec.UpdatedAt, _ = parseTime(updatedStr)

	return rec, nil
}

// ListEvents returns the audit trail of containment actions for an attempt.
func (s *Service) ListEvents(ctx context.Context, attemptID domain.ID) ([]ContainmentEvent, error) {
	attemptID = domain.ID(strings.TrimSpace(string(attemptID)))
	if attemptID == "" {
		return nil, errors.New("attempt id is required")
	}

	rows, err := s.store.DB().QueryContext(ctx, `
		SELECT event_id, attempt_id, task_id, ladder_step,
		       signal_name, details_json, enforced_by, created_at
		FROM attempt_containment_events
		WHERE attempt_id = ?
		ORDER BY created_at ASC, rowid ASC`,
		attemptID,
	)
	if err != nil {
		return nil, fmt.Errorf("query containment events: %w", err)
	}
	defer rows.Close()

	var events []ContainmentEvent
	for rows.Next() {
		var ev ContainmentEvent
		var createdStr string
		var details sql.NullString
		if err := rows.Scan(&ev.EventID, &ev.AttemptID, &ev.TaskID, &ev.LadderStep, &ev.SignalName, &details, &ev.EnforcedBy, &createdStr); err != nil {
			return nil, fmt.Errorf("scan containment event: %w", err)
		}
		if details.Valid {
			ev.DetailsJSON = details.String
		}
		ev.CreatedAt, _ = parseTime(createdStr)
		events = append(events, ev)
	}
	return events, rows.Err()
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(domain.TimeLayout, s)
	if err != nil {
		return time.Parse(time.RFC3339Nano, s)
	}
	return t, nil
}
