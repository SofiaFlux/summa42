package quarantine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type Service struct {
	store *state.Store
	clock clock.Clock
}

func NewService(store *state.Store, clk clock.Clock) *Service {
	return &Service{
		store: store,
		clock: clk,
	}
}

// RegisterCube inserts a new Cube in the ACTIVE state.
func (s *Service) RegisterCube(ctx context.Context, cubeID, principalID domain.ID) (Cube, error) {
	cubeID = domain.ID(strings.TrimSpace(string(cubeID)))
	principalID = domain.ID(strings.TrimSpace(string(principalID)))
	if cubeID == "" || principalID == "" {
		return Cube{}, errors.New("cube id and principal id are required")
	}

	now := s.clock.Now().UTC()
	nowStr := domain.FormatTime(now)

	_, err := s.store.DB().ExecContext(ctx, `
		INSERT INTO cubes(cube_id, principal_id, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(cube_id) DO UPDATE SET
			principal_id = excluded.principal_id,
			updated_at = excluded.updated_at`,
		cubeID, principalID, string(CubeActive), nowStr, nowStr,
	)
	if err != nil {
		return Cube{}, fmt.Errorf("register cube: %w", err)
	}

	return Cube{
		CubeID:      cubeID,
		PrincipalID: principalID,
		State:       CubeActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

// GetCube retrieves the current state of a Cube.
func (s *Service) GetCube(ctx context.Context, cubeID domain.ID) (Cube, error) {
	cubeID = domain.ID(strings.TrimSpace(string(cubeID)))
	if cubeID == "" {
		return Cube{}, errors.New("cube id is required")
	}

	var c Cube
	var stateStr string
	var reason, quarantinedAtStr, quarantinedByStr, modeStr sql.NullString
	var createdAtStr, updatedAtStr string

	err := s.store.DB().QueryRowContext(ctx, `
		SELECT cube_id, principal_id, state, quarantine_reason, quarantined_at, quarantined_by, quarantine_mode, created_at, updated_at
		FROM cubes
		WHERE cube_id = ?`,
		cubeID,
	).Scan(
		&c.CubeID,
		&c.PrincipalID,
		&stateStr,
		&reason,
		&quarantinedAtStr,
		&quarantinedByStr,
		&modeStr,
		&createdAtStr,
		&updatedAtStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Cube{}, ErrCubeNotFound
		}
		return Cube{}, fmt.Errorf("get cube: %w", err)
	}

	c.State = CubeState(stateStr)
	if reason.Valid {
		c.QuarantineReason = reason.String
	}
	if quarantinedAtStr.Valid && quarantinedAtStr.String != "" {
		if t, err := parseTime(quarantinedAtStr.String); err == nil {
			c.QuarantinedAt = t
		}
	}
	if quarantinedByStr.Valid {
		c.QuarantinedBy = domain.ID(quarantinedByStr.String)
	}
	if modeStr.Valid {
		c.QuarantineMode = QuarantineMode(modeStr.String)
	}
	if t, err := parseTime(createdAtStr); err == nil {
		c.CreatedAt = t
	}
	if t, err := parseTime(updatedAtStr); err == nil {
		c.UpdatedAt = t
	}

	return c, nil
}

// QuarantineCube transitions a Cube into QUARANTINED state.
func (s *Service) QuarantineCube(ctx context.Context, cubeID domain.ID, reason string, enforcedBy domain.ID, mode QuarantineMode) error {
	cubeID = domain.ID(strings.TrimSpace(string(cubeID)))
	enforcedBy = domain.ID(strings.TrimSpace(string(enforcedBy)))
	reason = strings.TrimSpace(reason)
	if cubeID == "" || enforcedBy == "" || reason == "" {
		return errors.New("cube id, enforcedBy, and reason are required to quarantine")
	}

	if mode == "" {
		mode = PassiveWait
	}
	if mode != PassiveWait && mode != ActiveRecon {
		return fmt.Errorf("unsupported quarantine mode: %s", mode)
	}

	now := s.clock.Now().UTC()
	nowStr := domain.FormatTime(now)
	eventID := domain.NewID("qevent")

	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin quarantine tx: %w", err)
	}
	defer tx.Rollback()

	// Ensure cube exists and validate state transition
	var existingState string
	err = tx.QueryRowContext(ctx, `SELECT state FROM cubes WHERE cube_id = ?`, cubeID).Scan(&existingState)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCubeNotFound
		}
		return fmt.Errorf("lookup cube: %w", err)
	}

	if CubeState(existingState) == CubeQuarantined {
		return ErrAlreadyQuarantined
	}
	if CubeState(existingState) == CubeTerminated {
		return fmt.Errorf("%w: cannot quarantine terminated cube", ErrInvalidCubeState)
	}

	// Update cube state
	_, err = tx.ExecContext(ctx, `
		UPDATE cubes
		SET state = ?,
		    quarantine_reason = ?,
		    quarantined_at = ?,
		    quarantined_by = ?,
		    quarantine_mode = ?,
		    updated_at = ?
		WHERE cube_id = ?`,
		string(CubeQuarantined), reason, nowStr, enforcedBy, string(mode), nowStr, cubeID,
	)
	if err != nil {
		return fmt.Errorf("update cube quarantine state: %w", err)
	}

	// Record audit event
	_, err = tx.ExecContext(ctx, `
		INSERT INTO cube_quarantine_events(
			event_id, cube_id, action, reason, enforced_by_principal_id, mode, created_at
		) VALUES (?, ?, 'QUARANTINE', ?, ?, ?, ?)`,
		eventID, cubeID, reason, enforcedBy, string(mode), nowStr,
	)
	if err != nil {
		return fmt.Errorf("record quarantine audit event: %w", err)
	}

	return tx.Commit()
}

// UnquarantineCube transitions a Cube out of QUARANTINED state back to ACTIVE.
// Quarantine CANNOT be cleared by the quarantined Cube itself.
// The caller must have authority (e.g. Owner or constitutional root).
func (s *Service) UnquarantineCube(ctx context.Context, cubeID domain.ID, callerPrincipalID domain.ID, reason string) error {
	cubeID = domain.ID(strings.TrimSpace(string(cubeID)))
	callerPrincipalID = domain.ID(strings.TrimSpace(string(callerPrincipalID)))
	reason = strings.TrimSpace(reason)
	if cubeID == "" || callerPrincipalID == "" || reason == "" {
		return errors.New("cube id, caller principal id, and reason are required to unquarantine")
	}

	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin unquarantine tx: %w", err)
	}
	defer tx.Rollback()

	var principalID domain.ID
	var currentState string
	err = tx.QueryRowContext(ctx, `SELECT principal_id, state FROM cubes WHERE cube_id = ?`, cubeID).Scan(&principalID, &currentState)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCubeNotFound
		}
		return fmt.Errorf("lookup cube: %w", err)
	}

	// Invariant: Quarantine cannot be cleared by the quarantined Cube itself.
	if callerPrincipalID == principalID || callerPrincipalID == cubeID {
		return fmt.Errorf("%w: cube %q (principal %q) attempted self-unquarantine", ErrSelfUnquarantineDenied, cubeID, callerPrincipalID)
	}

	// Authority check: if collective_metadata exists, ensure caller is owner or constitutional root
	var ownerID, rootID string
	row := tx.QueryRowContext(ctx, `SELECT owner_principal_id, constitutional_root_principal_id FROM collective_metadata WHERE singleton = 1`)
	if err := row.Scan(&ownerID, &rootID); err == nil {
		if callerPrincipalID != domain.ID(ownerID) && callerPrincipalID != domain.ID(rootID) {
			return fmt.Errorf("%w: caller %q is not authorized to lift quarantine", ErrUnauthorizedPrincipal, callerPrincipalID)
		}
	}

	if CubeState(currentState) != CubeQuarantined {
		return fmt.Errorf("%w: cube is in %s state, not QUARANTINED", ErrInvalidCubeState, currentState)
	}

	now := s.clock.Now().UTC()
	nowStr := domain.FormatTime(now)
	eventID := domain.NewID("qevent")

	_, err = tx.ExecContext(ctx, `
		UPDATE cubes
		SET state = ?,
		    quarantine_reason = NULL,
		    quarantined_at = NULL,
		    quarantined_by = NULL,
		    quarantine_mode = NULL,
		    updated_at = ?
		WHERE cube_id = ?`,
		string(CubeActive), nowStr, cubeID,
	)
	if err != nil {
		return fmt.Errorf("clear cube quarantine state: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO cube_quarantine_events(
			event_id, cube_id, action, reason, enforced_by_principal_id, mode, created_at
		) VALUES (?, ?, 'UNQUARANTINE', ?, ?, NULL, ?)`,
		eventID, cubeID, reason, callerPrincipalID, nowStr,
	)
	if err != nil {
		return fmt.Errorf("record unquarantine audit event: %w", err)
	}

	return tx.Commit()
}

// AllowedQuarantineCapabilities is the strict, exact set of capabilities
// permitted under quarantine. No wildcard or destructive operations are allowed.
var allowedQuarantineCapabilities = map[string]bool{
	// Diagnostics (strictly read-only / ping)
	"diagnostics.ping":      true,
	"diagnostics:ping":      true,
	"diagnostics.health":    true,
	"diagnostics:health":    true,
	"diagnostics.status":    true,
	"diagnostics:status":    true,
	"diagnostics.read_logs": true,
	"diagnostics:read_logs": true,
	"diagnostics":           true,
	"health":                true,
	"probe":                 true,

	// Evidence export
	"evidence.export": true,
	"evidence:export": true,
	"evidence.read":   true,
	"evidence:read":   true,
	"evidence":        true,

	// Integrity verification
	"integrity.verify":   true,
	"integrity:verify":   true,
	"integrity.checksum": true,
	"integrity:checksum": true,
	"integrity":          true,

	// Recovery communication
	"recovery.reconcile":   true,
	"recovery:reconcile":   true,
	"recovery.ping":        true,
	"recovery:ping":        true,
	"recovery.heartbeat":   true,
	"recovery:heartbeat":   true,
	"recovery.communicate": true,
	"recovery:communicate": true,
	"recovery":             true,
	"wake":                 true,
}

// IsAllowedInQuarantine returns true only if the capability is in the strict
// quarantine allowlist. Any destructive action or unlisted capability returns false.
func IsAllowedInQuarantine(capability string) bool {
	capLower := strings.ToLower(strings.TrimSpace(capability))
	if capLower == "" {
		return false
	}
	return allowedQuarantineCapabilities[capLower]
}

// MaskCapabilities applies capability masking at the Cube dispatch boundary.
// Invariant: Quarantine can only REDUCE authority, never elevate it.
func MaskCapabilities(state CubeState, requestedCaps []string) []string {
	if state != CubeQuarantined {
		out := make([]string, len(requestedCaps))
		copy(out, requestedCaps)
		return out
	}

	filtered := make([]string, 0, len(requestedCaps))
	seen := make(map[string]bool)
	for _, capName := range requestedCaps {
		trimmed := strings.TrimSpace(capName)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		if IsAllowedInQuarantine(trimmed) {
			filtered = append(filtered, trimmed)
		}
	}
	return filtered
}

// CheckCapabilityPermitted checks whether a specific capability is allowed for the Cube.
func (s *Service) CheckCapabilityPermitted(ctx context.Context, cubeID domain.ID, capability string) error {
	cube, err := s.GetCube(ctx, cubeID)
	if err != nil {
		if errors.Is(err, ErrCubeNotFound) {
			return nil
		}
		return err
	}
	if cube.State != CubeQuarantined {
		return nil
	}
	if !IsAllowedInQuarantine(capability) {
		return fmt.Errorf("%w: capability %q is prohibited for quarantined cube %q", ErrCubeQuarantined, capability, cubeID)
	}
	return nil
}

// CheckOperationPermitted verifies if an operation is allowed.
// Under quarantine, DEFAULT-DENY applies: ALL external operations, mutations,
// provisioning, authority changes, sensitive writes, and credential issuance
// are strictly prohibited.
func (s *Service) CheckOperationPermitted(ctx context.Context, cubeID domain.ID, operationCategory string) error {
	cube, err := s.GetCube(ctx, cubeID)
	if err != nil {
		if errors.Is(err, ErrCubeNotFound) {
			return nil
		}
		return err
	}
	if cube.State != CubeQuarantined {
		return nil
	}

	// Quarantined cubes are strictly prohibited from performing any external operations
	return fmt.Errorf("%w: external operation or category %q is prohibited for quarantined cube %q", ErrProhibitedInQuarantine, operationCategory, cubeID)
}

// ListQuarantineEvents returns the audit trail of quarantine actions for a Cube.
func (s *Service) ListQuarantineEvents(ctx context.Context, cubeID domain.ID) ([]QuarantineEvent, error) {
	cubeID = domain.ID(strings.TrimSpace(string(cubeID)))
	if cubeID == "" {
		return nil, errors.New("cube id is required")
	}

	rows, err := s.store.DB().QueryContext(ctx, `
		SELECT event_id, cube_id, action, reason, enforced_by_principal_id, mode, created_at
		FROM cube_quarantine_events
		WHERE cube_id = ?
		ORDER BY created_at ASC, rowid ASC`,
		cubeID,
	)
	if err != nil {
		return nil, fmt.Errorf("query quarantine events: %w", err)
	}
	defer rows.Close()

	var events []QuarantineEvent
	for rows.Next() {
		var ev QuarantineEvent
		var modeStr sql.NullString
		var createdAtStr string
		if err := rows.Scan(&ev.EventID, &ev.CubeID, &ev.Action, &ev.Reason, &ev.EnforcedByPrincipalID, &modeStr, &createdAtStr); err != nil {
			return nil, fmt.Errorf("scan quarantine event: %w", err)
		}
		if modeStr.Valid {
			ev.Mode = QuarantineMode(modeStr.String)
		}
		if t, err := parseTime(createdAtStr); err == nil {
			ev.CreatedAt = t
		}
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
