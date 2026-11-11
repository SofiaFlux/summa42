package emergency

import (
	"context"
	"database/sql"
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

const (
	// DefaultHeartbeatTimeout is the Owner's default absence horizon: how long
	// the Collective keeps working without a heartbeat before it enters safe
	// mode. The Owner changes it with a signed heartbeat, never a process flag.
	DefaultHeartbeatTimeout = 7 * 24 * time.Hour

	// breakGlassRevocationReason is recorded on every lease a freeze revokes.
	breakGlassRevocationReason = "break_glass"
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

// EnsureDefaults creates the singleton rows if absent. timeoutSec seeds the
// Owner's horizon only when no row exists yet (zero means the default); an
// existing horizon is never rewritten, whichever process opens the state.
func (s *Service) EnsureDefaults(ctx context.Context, timeoutSec int) error {
	if timeoutSec <= 0 {
		timeoutSec = int(DefaultHeartbeatTimeout.Seconds())
	}
	now := s.clock.Now().UTC()
	nowStr := domain.FormatTime(now)

	_, err := s.store.DB().ExecContext(ctx, `
		INSERT INTO emergency_controls (singleton, break_glass_state, updated_at)
		VALUES (1, 'ARMED', ?)
		ON CONFLICT(singleton) DO NOTHING`,
		nowStr,
	)
	if err != nil {
		return fmt.Errorf("init emergency_controls: %w", err)
	}

	_, err = s.store.DB().ExecContext(ctx, `
		INSERT INTO dead_man_switch (singleton, state, last_heartbeat_at, heartbeat_timeout_seconds, updated_at)
		VALUES (1, 'NOMINAL', ?, ?, ?)
		ON CONFLICT(singleton) DO NOTHING`,
		nowStr, timeoutSec, nowStr,
	)
	if err != nil {
		return fmt.Errorf("init dead_man_switch: %w", err)
	}

	return nil
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

// TriggerBreakGlass atomically freezes all executions, revokes active leases and capability sessions.
// Controlled exclusively by root/Owner authority.
func (s *Service) TriggerBreakGlass(ctx context.Context, callerPrincipalID domain.ID, reason string) error {
	return s.triggerBreakGlass(ctx, callerPrincipalID, reason, auditMeta{})
}

func (s *Service) triggerBreakGlass(ctx context.Context, callerPrincipalID domain.ID, reason string, meta auditMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	callerPrincipalID = domain.ID(strings.TrimSpace(string(callerPrincipalID)))
	reason = strings.TrimSpace(reason)
	if callerPrincipalID == "" || reason == "" {
		return errors.New("caller principal id and reason are required for break glass")
	}

	if !s.isAuthorizedOwner(ctx, callerPrincipalID) {
		return fmt.Errorf("%w: principal %q is not owner", ErrUnauthorizedEmergencyCaller, callerPrincipalID)
	}

	now := s.clock.Now().UTC()
	nowStr := domain.FormatTime(now)
	eventID := meta.eventIDOrNew()

	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin break glass tx: %w", err)
	}
	defer tx.Rollback()

	// Check if already frozen
	var currentState string
	err = tx.QueryRowContext(ctx, `SELECT break_glass_state FROM emergency_controls WHERE singleton = 1`).Scan(&currentState)
	if err == nil && BreakGlassState(currentState) == BreakGlassFrozen {
		return nil // Idempotent: already frozen
	}

	// 1. Update emergency_controls
	_, err = tx.ExecContext(ctx, `
		INSERT INTO emergency_controls (singleton, break_glass_state, frozen_at, frozen_by_principal_id, freeze_reason, updated_at)
		VALUES (1, 'FROZEN', ?, ?, ?, ?)
		ON CONFLICT(singleton) DO UPDATE SET
			break_glass_state = 'FROZEN',
			frozen_at = excluded.frozen_at,
			frozen_by_principal_id = excluded.frozen_by_principal_id,
			freeze_reason = excluded.freeze_reason,
			updated_at = excluded.updated_at`,
		nowStr, callerPrincipalID, reason, nowStr,
	)
	if err != nil {
		return fmt.Errorf("freeze emergency_controls: %w", err)
	}

	// 2. Revoke every active lease through the execution kernel, so each
	// Attempt gets its LEASE_REVOKED event and its Task returns to ELIGIBLE.
	// Leasing stays closed until resume (CheckLeaseAllowedTx).
	if _, err := execution.RevokeActiveLeasesTx(ctx, tx, now, breakGlassRevocationReason); err != nil {
		return fmt.Errorf("revoke attempt leases: %w", err)
	}

	// 3. Atomically revoke all active capability sessions
	_, err = tx.ExecContext(ctx, `
		UPDATE capability_sessions
		SET revoked_at = COALESCE(revoked_at, ?)
		WHERE revoked_at IS NULL`,
		nowStr,
	)
	if err != nil {
		return fmt.Errorf("revoke capability sessions: %w", err)
	}

	// 4. Record immutable audit event
	_, err = tx.ExecContext(ctx, `
		INSERT INTO emergency_audit_events (event_id, event_type, principal_id, reason, created_at, signature)
		VALUES (?, 'BREAK_GLASS_TRIGGERED', ?, ?, ?, ?)`,
		eventID, callerPrincipalID, reason, nowStr, meta.signatureValue(),
	)
	if err != nil {
		return fmt.Errorf("record break glass audit event: %w", err)
	}

	return tx.Commit()
}

// ResumeSystem lifts Break Glass and returns system to RESTORED state.
// Requires Owner authority.
func (s *Service) ResumeSystem(ctx context.Context, callerPrincipalID domain.ID, reason string) error {
	return s.resumeSystem(ctx, callerPrincipalID, reason, auditMeta{})
}

func (s *Service) resumeSystem(ctx context.Context, callerPrincipalID domain.ID, reason string, meta auditMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	callerPrincipalID = domain.ID(strings.TrimSpace(string(callerPrincipalID)))
	reason = strings.TrimSpace(reason)
	if callerPrincipalID == "" || reason == "" {
		return errors.New("caller principal id and reason are required to resume")
	}

	if !s.isAuthorizedOwner(ctx, callerPrincipalID) {
		return fmt.Errorf("%w: principal %q is not owner", ErrUnauthorizedEmergencyCaller, callerPrincipalID)
	}

	now := s.clock.Now().UTC()
	nowStr := domain.FormatTime(now)
	eventID := meta.eventIDOrNew()

	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin resume tx: %w", err)
	}
	defer tx.Rollback()

	var currentState string
	err = tx.QueryRowContext(ctx, `SELECT break_glass_state FROM emergency_controls WHERE singleton = 1`).Scan(&currentState)
	if err != nil {
		return fmt.Errorf("load break glass state: %w", err)
	}
	if BreakGlassState(currentState) != BreakGlassFrozen {
		return fmt.Errorf("%w: system is %s, not FROZEN", ErrInvalidEmergencyState, currentState)
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE emergency_controls
		SET break_glass_state = 'RESTORED',
		    restored_at = ?,
		    restored_by_principal_id = ?,
		    updated_at = ?
		WHERE singleton = 1`,
		nowStr, callerPrincipalID, nowStr,
	)
	if err != nil {
		return fmt.Errorf("restore emergency_controls: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO emergency_audit_events (event_id, event_type, principal_id, reason, created_at, signature)
		VALUES (?, 'SYSTEM_RESUMED', ?, ?, ?, ?)`,
		eventID, callerPrincipalID, reason, nowStr, meta.signatureValue(),
	)
	if err != nil {
		return fmt.Errorf("record resume audit event: %w", err)
	}

	return tx.Commit()
}

// RecordOwnerHeartbeat records an authentic human heartbeat.
// If Dead Man switch was in SAFE_MODE, it transitions back to NOMINAL.
func (s *Service) RecordOwnerHeartbeat(ctx context.Context, callerPrincipalID domain.ID) error {
	return s.recordOwnerHeartbeat(ctx, callerPrincipalID, 0, auditMeta{})
}

// recordOwnerHeartbeat records the heartbeat and, when timeoutSec is positive,
// sets the Owner's horizon to it. Zero keeps the stored horizon.
func (s *Service) recordOwnerHeartbeat(ctx context.Context, callerPrincipalID domain.ID, timeoutSec int, meta auditMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	callerPrincipalID = domain.ID(strings.TrimSpace(string(callerPrincipalID)))
	if callerPrincipalID == "" {
		return errors.New("caller principal id is required for heartbeat")
	}

	if !s.isAuthorizedOwner(ctx, callerPrincipalID) {
		return fmt.Errorf("%w: principal %q is not owner", ErrUnauthorizedEmergencyCaller, callerPrincipalID)
	}

	now := s.clock.Now().UTC()
	nowStr := domain.FormatTime(now)
	eventID := meta.eventIDOrNew()

	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin heartbeat tx: %w", err)
	}
	defer tx.Rollback()

	var currentState string
	err = tx.QueryRowContext(ctx, `SELECT state FROM dead_man_switch WHERE singleton = 1`).Scan(&currentState)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("load dead man switch state: %w", err)
	}

	wasSafeMode := currentState == string(DeadManSafeMode)

	_, err = tx.ExecContext(ctx, `
		INSERT INTO dead_man_switch (singleton, state, last_heartbeat_at, heartbeat_timeout_seconds, safe_mode_triggered_at, updated_at)
		VALUES (1, 'NOMINAL', ?, ?, NULL, ?)
		ON CONFLICT(singleton) DO UPDATE SET
			state = 'NOMINAL',
			last_heartbeat_at = excluded.last_heartbeat_at,
			safe_mode_triggered_at = NULL,
			updated_at = excluded.updated_at`,
		nowStr, int(DefaultHeartbeatTimeout.Seconds()), nowStr,
	)
	if err != nil {
		return fmt.Errorf("update heartbeat: %w", err)
	}
	if timeoutSec > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE dead_man_switch SET heartbeat_timeout_seconds = ? WHERE singleton = 1`, timeoutSec,
		); err != nil {
			return fmt.Errorf("set Owner horizon: %w", err)
		}
	}

	eventType := EventHeartbeatRecorded
	reason := "Owner heartbeat received"
	if wasSafeMode {
		eventType = EventSafeModeCleared
		reason = "Safe mode cleared by owner heartbeat"
	}
	if timeoutSec > 0 {
		reason += fmt.Sprintf("; horizon set to %d seconds", timeoutSec)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO emergency_audit_events (event_id, event_type, principal_id, reason, created_at, signature)
		VALUES (?, ?, ?, ?, ?, ?)`,
		eventID, string(eventType), callerPrincipalID, reason, nowStr, meta.signatureValue(),
	)
	if err != nil {
		return fmt.Errorf("record heartbeat audit event: %w", err)
	}

	return tx.Commit()
}

// EvaluateDeadMan checks if the owner heartbeat has expired.
// If expired, transitions Dead Man state to SAFE_MODE under a write transaction.
func (s *Service) EvaluateDeadMan(ctx context.Context) (DeadManStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock.Now().UTC()
	nowStr := domain.FormatTime(now)

	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return DeadManStatus{}, fmt.Errorf("begin evaluate dead man tx: %w", err)
	}
	defer tx.Rollback()

	var stateStr, lastHeartbeatStr string
	var timeoutSec int
	var safeTriggeredStr sql.NullString
	var updatedStr string

	err = tx.QueryRowContext(ctx, `
		SELECT state, last_heartbeat_at, heartbeat_timeout_seconds, safe_mode_triggered_at, updated_at
		FROM dead_man_switch
		WHERE singleton = 1`,
	).Scan(&stateStr, &lastHeartbeatStr, &timeoutSec, &safeTriggeredStr, &updatedStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeadManStatus{State: DeadManNominal}, nil
		}
		return DeadManStatus{}, fmt.Errorf("query dead man switch: %w", err)
	}

	lastHeartbeat, _ := parseTime(lastHeartbeatStr)
	timeoutDuration := time.Duration(timeoutSec) * time.Second
	expired := now.Sub(lastHeartbeat) > timeoutDuration

	if expired && DeadManState(stateStr) == DeadManNominal {
		stateStr = string(DeadManSafeMode)
		safeTriggeredStr.String = nowStr
		safeTriggeredStr.Valid = true

		_, err = tx.ExecContext(ctx, `
			UPDATE dead_man_switch
			SET state = 'SAFE_MODE',
			    safe_mode_triggered_at = ?,
			    updated_at = ?
			WHERE singleton = 1`,
			nowStr, nowStr,
		)
		if err != nil {
			return DeadManStatus{}, fmt.Errorf("trigger safe mode: %w", err)
		}

		eventID := domain.NewID("emevent")
		_, err = tx.ExecContext(ctx, `
			INSERT INTO emergency_audit_events (event_id, event_type, reason, created_at)
			VALUES (?, 'SAFE_MODE_TRIGGERED', 'Heartbeat timeout exceeded; transitioning to safe-mode', ?)`,
			eventID, nowStr,
		)
		if err != nil {
			return DeadManStatus{}, fmt.Errorf("record safe mode audit event: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return DeadManStatus{}, fmt.Errorf("commit evaluate dead man: %w", err)
	}

	status := DeadManStatus{
		State:               DeadManState(stateStr),
		LastHeartbeatAt:     lastHeartbeat,
		HeartbeatTimeoutSec: timeoutSec,
	}
	if safeTriggeredStr.Valid {
		status.SafeModeTriggeredAt, _ = parseTime(safeTriggeredStr.String)
	}
	status.UpdatedAt, _ = parseTime(updatedStr)

	return status, nil
}

// CheckExecutionAllowed verifies whether an action is permitted under current emergency controls.
// Fails closed on database errors. Uses read-only queries on the nominal path.
func (s *Service) CheckExecutionAllowed(ctx context.Context, actionCategory string) error {
	// 1. Check Break Glass (fail closed on error)
	var bgState string
	err := s.store.DB().QueryRowContext(ctx, `
		SELECT break_glass_state FROM emergency_controls WHERE singleton = 1`,
	).Scan(&bgState)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("fail-closed break glass check: %w", err)
		}
	} else if BreakGlassState(bgState) == BreakGlassFrozen {
		return ErrBreakGlassFrozen
	}

	// 2. Check Dead Man Switch via fast-path read
	var dmState, lastHeartbeatStr string
	var timeoutSec int
	err = s.store.DB().QueryRowContext(ctx, `
		SELECT state, last_heartbeat_at, heartbeat_timeout_seconds FROM dead_man_switch WHERE singleton = 1`,
	).Scan(&dmState, &lastHeartbeatStr, &timeoutSec)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("fail-closed dead man check: %w", err)
		}
		return nil
	}

	lastHeartbeat, _ := parseTime(lastHeartbeatStr)
	timeoutDuration := time.Duration(timeoutSec) * time.Second
	expired := s.clock.Now().UTC().Sub(lastHeartbeat) > timeoutDuration

	if DeadManState(dmState) == DeadManSafeMode || expired {
		if DeadManState(dmState) != DeadManSafeMode {
			// Trigger state mutation safely
			_, _ = s.EvaluateDeadMan(ctx)
		}

		catLower := strings.ToLower(strings.TrimSpace(actionCategory))
		// In safe mode: stops new Attempt leases, strategic expansion, spend, and
		// policy evolution. Diagnostics, evidence, lease recovery and read-only
		// reconciliation of in-flight operations continue.
		prohibitedInSafeMode := []string{
			"attempt_lease",
			"strategic_expansion",
			"expansion",
			"spend",
			"policy_evolution",
			"policy_upgrade",
			"provisioning",
			"external_operation",
		}
		for _, prohibited := range prohibitedInSafeMode {
			if catLower == prohibited || strings.HasPrefix(catLower, prohibited+":") {
				return fmt.Errorf("%w: action category %q is prohibited under dead man safe-mode", ErrSafeModeRestricted, actionCategory)
			}
		}
	}

	return nil
}

// CheckLeaseAllowedTx is the execution kernel's lease gate. It reads the
// emergency state inside the leasing transaction and refuses a new Attempt
// while Break Glass is FROZEN or the Owner's horizon has passed. It never
// writes: the SAFE_MODE transition is recorded by EvaluateDeadMan.
func (s *Service) CheckLeaseAllowedTx(ctx context.Context, tx *sql.Tx) error {
	if tx == nil {
		return errors.New("SQL transaction is required")
	}
	var bgState string
	err := tx.QueryRowContext(ctx, `SELECT break_glass_state FROM emergency_controls WHERE singleton = 1`).Scan(&bgState)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("fail-closed break glass check: %w", err)
	}
	if err == nil && BreakGlassState(bgState) == BreakGlassFrozen {
		return ErrBreakGlassFrozen
	}
	var dmState, lastHeartbeatStr string
	var timeoutSec int
	err = tx.QueryRowContext(ctx, `
		SELECT state, last_heartbeat_at, heartbeat_timeout_seconds FROM dead_man_switch WHERE singleton = 1`,
	).Scan(&dmState, &lastHeartbeatStr, &timeoutSec)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("fail-closed dead man check: %w", err)
	}
	// An unparseable heartbeat is the zero time, which reads as expired.
	lastHeartbeat, _ := parseTime(lastHeartbeatStr)
	expired := s.clock.Now().UTC().Sub(lastHeartbeat) > time.Duration(timeoutSec)*time.Second
	if DeadManState(dmState) == DeadManSafeMode || expired {
		return fmt.Errorf("%w: new Attempt leases wait for the Owner's heartbeat", ErrSafeModeRestricted)
	}
	return nil
}

// GetStatus returns the combined status of Break Glass and Dead Man Switch.
func (s *Service) GetStatus(ctx context.Context) (BreakGlassStatus, DeadManStatus, error) {
	var bg BreakGlassStatus
	var bgState string
	var frozenAt, frozenBy, reason, restoredAt, restoredBy sql.NullString
	var bgUpdated string

	err := s.store.DB().QueryRowContext(ctx, `
		SELECT break_glass_state, frozen_at, frozen_by_principal_id, freeze_reason, restored_at, restored_by_principal_id, updated_at
		FROM emergency_controls
		WHERE singleton = 1`,
	).Scan(&bgState, &frozenAt, &frozenBy, &reason, &restoredAt, &restoredBy, &bgUpdated)
	if err == nil {
		bg.State = BreakGlassState(bgState)
		if frozenAt.Valid {
			bg.FrozenAt, _ = parseTime(frozenAt.String)
		}
		if frozenBy.Valid {
			bg.FrozenByPrincipalID = domain.ID(frozenBy.String)
		}
		if reason.Valid {
			bg.FreezeReason = reason.String
		}
		if restoredAt.Valid {
			bg.RestoredAt, _ = parseTime(restoredAt.String)
		}
		if restoredBy.Valid {
			bg.RestoredByPrincipal = domain.ID(restoredBy.String)
		}
		bg.UpdatedAt, _ = parseTime(bgUpdated)
	} else if errors.Is(err, sql.ErrNoRows) {
		bg.State = BreakGlassArmed
	} else {
		return BreakGlassStatus{}, DeadManStatus{}, fmt.Errorf("query break glass: %w", err)
	}

	deadMan, err := s.EvaluateDeadMan(ctx)
	if err != nil {
		return bg, DeadManStatus{}, err
	}

	return bg, deadMan, nil
}

// ListEvents returns the audit trail of emergency actions.
func (s *Service) ListEvents(ctx context.Context) ([]EmergencyAuditEvent, error) {
	rows, err := s.store.DB().QueryContext(ctx, `
		SELECT event_id, event_type, principal_id, reason, created_at
		FROM emergency_audit_events
		ORDER BY created_at ASC, rowid ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("query emergency audit events: %w", err)
	}
	defer rows.Close()

	var events []EmergencyAuditEvent
	for rows.Next() {
		var ev EmergencyAuditEvent
		var principalStr sql.NullString
		var createdStr string
		if err := rows.Scan(&ev.EventID, &ev.EventType, &principalStr, &ev.Reason, &createdStr); err != nil {
			return nil, fmt.Errorf("scan emergency event: %w", err)
		}
		if principalStr.Valid {
			ev.PrincipalID = domain.ID(principalStr.String)
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
