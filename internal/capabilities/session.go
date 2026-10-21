package capabilities

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
)

// Session is a primitive: Registry.OpenSession and Session have no production
// caller yet (only tests and the unwired MCP adapter use them).
type Session struct {
	registry *Registry
	token    string
}

type sessionRecord struct {
	ID                  domain.ID
	CollectiveID        domain.ID
	TaskID              domain.ID
	AttemptID           domain.ID
	FenceGeneration     int64
	LeaseExpiresAt      time.Time
	VisibleCapabilities map[string]domain.ID
}

type taskCapabilityControl struct {
	Authority           []string
	RequiredEnforcement domain.EnforcementLevel
}

func (r *Registry) OpenSession(ctx context.Context, collectiveID, attemptID domain.ID, visibleCapabilities []string) (*Session, error) {
	if err := r.configured(); err != nil {
		return nil, err
	}
	collectiveID = domain.ID(strings.TrimSpace(string(collectiveID)))
	visibleCapabilities = normalizeStrings(visibleCapabilities)
	if collectiveID == "" || attemptID == "" || len(visibleCapabilities) == 0 {
		return nil, errors.New("collective, attempt, and visible capabilities are required")
	}
	rawToken, err := newSessionToken()
	if err != nil {
		return nil, err
	}
	tokenHash := hashToken(rawToken)
	now := r.clock.Now().UTC()
	sessionID := domain.NewID("capsession")

	err = r.execution.WithGuardedAttempt(ctx, attemptID, []domain.TaskState{domain.TaskExecuting}, func(tx *sql.Tx, guarded execution.GuardedAttempt) error {
		control, err := loadTaskCapabilityControl(ctx, tx, guarded.TaskID)
		if err != nil {
			return err
		}
		visible := make(map[string]domain.ID, len(visibleCapabilities))
		for _, name := range visibleCapabilities {
			definition, err := r.definitionByNameTx(ctx, tx, name)
			if err != nil {
				return err
			}
			if _, ok := r.Provider(definition.Access.Provider); !ok {
				return fmt.Errorf("capability provider %q is not registered", definition.Access.Provider)
			}
			if !authorityContains(control.Authority, definition.Authority.Capabilities) {
				return fmt.Errorf("%w: capability %q exceeds task authority", domain.ErrPolicyDenied, name)
			}
			if err := ensureCurrentAssessmentEligible(ctx, tx, definition, control, name); err != nil {
				return err
			}
			visible[name] = definition.ID
		}
		visibleJSON, err := json.Marshal(visible)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO capability_sessions(
				session_id, token_hash, collective_id, task_id, attempt_id, fence_generation,
				lease_expires_at, visible_capabilities_json, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sessionID, tokenHash, collectiveID, guarded.TaskID, guarded.AttemptID, guarded.FenceGeneration,
			formatTime(guarded.LeaseExpiresAt), string(visibleJSON), formatTime(now),
		)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &Session{registry: r, token: rawToken}, nil
}

func (s *Session) Revoke(ctx context.Context) error {
	if s == nil || s.registry == nil || strings.TrimSpace(s.token) == "" {
		return errors.New("capability session is not configured")
	}
	if err := s.registry.configured(); err != nil {
		return err
	}
	result, err := s.registry.store.DB().ExecContext(ctx, `
		UPDATE capability_sessions
		SET revoked_at = COALESCE(revoked_at, ?)
		WHERE token_hash = ?`,
		formatTime(s.registry.clock.Now().UTC()), hashToken(s.token),
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return errors.New("capability session token is invalid")
	}
	return nil
}

func (s *Session) Call(ctx context.Context, name string, request any) (any, error) {
	if s == nil || s.registry == nil || strings.TrimSpace(s.token) == "" {
		return nil, errors.New("capability session is not configured")
	}
	if err := s.registry.configured(); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("capability name is required")
	}
	tokenHash := hashToken(s.token)
	initial, err := loadSession(ctx, s.registry.store.DB(), tokenHash)
	if err != nil {
		return nil, err
	}
	if !initial.LeaseExpiresAt.After(s.registry.clock.Now().UTC()) {
		return nil, domain.ErrLeaseInactive
	}

	var provider Provider
	var semanticName string
	err = s.registry.execution.WithGuardedAttempt(ctx, initial.AttemptID, []domain.TaskState{domain.TaskExecuting}, func(tx *sql.Tx, guarded execution.GuardedAttempt) error {
		current, err := loadSession(ctx, tx, tokenHash)
		if err != nil {
			return err
		}
		if current.TaskID != guarded.TaskID || current.AttemptID != guarded.AttemptID || current.FenceGeneration != guarded.FenceGeneration {
			return domain.ErrStaleAttempt
		}
		if !current.LeaseExpiresAt.After(s.registry.clock.Now().UTC()) {
			return domain.ErrLeaseInactive
		}
		definitionID, ok := current.VisibleCapabilities[name]
		if !ok {
			return fmt.Errorf("%w: capability %q is not visible in this session", domain.ErrPolicyDenied, name)
		}
		definition, err := s.registry.definitionByIDTx(ctx, tx, definitionID)
		if err != nil {
			return err
		}
		control, err := loadTaskCapabilityControl(ctx, tx, guarded.TaskID)
		if err != nil {
			return err
		}
		if !authorityContains(control.Authority, definition.Authority.Capabilities) {
			return fmt.Errorf("%w: capability %q no longer fits task authority", domain.ErrPolicyDenied, name)
		}
		if err := ensureCurrentAssessmentEligible(ctx, tx, definition, control, name); err != nil {
			return err
		}
		provider, ok = s.registry.Provider(definition.Access.Provider)
		if !ok || provider == nil {
			return fmt.Errorf("capability provider %q is not registered", definition.Access.Provider)
		}
		semanticName = definition.Skill.Name
		return nil
	})
	if err != nil {
		return nil, err
	}
	return provider.Call(ctx, semanticName, request)
}

func ensureCurrentAssessmentEligible(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, definition Definition, control taskCapabilityControl, name string) error {
	assessment, err := loadLatestAssessmentByCapability(ctx, q, definition.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: capability %q has no observed assessment", domain.ErrPolicyDenied, name)
		}
		return err
	}
	if assessment.Provider != definition.Access.Provider || assessment.AccessContext != definition.Access.Context {
		return fmt.Errorf("%w: capability %q assessment does not match active access path", domain.ErrPolicyDenied, name)
	}
	if !assessment.Available || assessment.Health == HealthUnhealthy {
		return fmt.Errorf("%w: capability %q is not currently available and healthy enough", domain.ErrPolicyDenied, name)
	}
	if !enforcementAtLeast(assessment.Enforcement, definition.Environment.MinimumEnforcement) || !enforcementAtLeast(assessment.Enforcement, control.RequiredEnforcement) {
		return fmt.Errorf("%w: capability %q observed enforcement %s is insufficient", domain.ErrPolicyDenied, name, assessment.Enforcement)
	}
	return nil
}

func loadSession(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, tokenHash string) (sessionRecord, error) {
	var record sessionRecord
	var leaseExpiry string
	var visibleJSON string
	var revoked sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT session_id, collective_id, task_id, attempt_id, fence_generation,
		       lease_expires_at, visible_capabilities_json, revoked_at
		FROM capability_sessions
		WHERE token_hash = ?`, tokenHash,
	).Scan(
		&record.ID, &record.CollectiveID, &record.TaskID, &record.AttemptID, &record.FenceGeneration,
		&leaseExpiry, &visibleJSON, &revoked,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return sessionRecord{}, errors.New("capability session token is invalid")
	}
	if err != nil {
		return sessionRecord{}, err
	}
	if revoked.Valid {
		return sessionRecord{}, domain.ErrLeaseInactive
	}
	record.LeaseExpiresAt, err = time.Parse(time.RFC3339Nano, leaseExpiry)
	if err != nil {
		return sessionRecord{}, fmt.Errorf("parse capability session lease expiry: %w", err)
	}
	if err := json.Unmarshal([]byte(visibleJSON), &record.VisibleCapabilities); err != nil {
		return sessionRecord{}, fmt.Errorf("decode visible capabilities: %w", err)
	}
	return record, nil
}

func loadLatestAssessmentByCapability(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, capabilityID domain.ID) (Assessment, error) {
	var assessment Assessment
	var assessedAt string
	var evidenceJSON, costJSON string
	var available int
	err := q.QueryRowContext(ctx, `
		SELECT assessment_id, capability_id, provider, enforcement_level, access_context,
		       assessed_at, evidence_json, cost_metadata_json, health, available
		FROM capability_assessments
		WHERE capability_id = ?
		ORDER BY assessed_at DESC, rowid DESC
		LIMIT 1`, capabilityID,
	).Scan(
		&assessment.ID, &assessment.CapabilityID, &assessment.Provider, &assessment.Enforcement,
		&assessment.AccessContext, &assessedAt, &evidenceJSON, &costJSON, &assessment.Health, &available,
	)
	if err != nil {
		return Assessment{}, err
	}
	assessment.AssessedAt, err = time.Parse(time.RFC3339Nano, assessedAt)
	if err != nil {
		return Assessment{}, fmt.Errorf("parse capability assessment time: %w", err)
	}
	if err := json.Unmarshal([]byte(evidenceJSON), &assessment.Evidence); err != nil {
		return Assessment{}, fmt.Errorf("decode capability assessment evidence: %w", err)
	}
	if err := json.Unmarshal([]byte(costJSON), &assessment.CostMetadata); err != nil {
		return Assessment{}, fmt.Errorf("decode capability assessment cost metadata: %w", err)
	}
	assessment.Available = available == 1
	return assessment, nil
}

func loadTaskCapabilityControl(ctx context.Context, tx *sql.Tx, taskID domain.ID) (taskCapabilityControl, error) {
	var control taskCapabilityControl
	var rawAuthority string
	if err := tx.QueryRowContext(ctx,
		`SELECT authority_ceiling_json, required_enforcement FROM tasks WHERE task_id = ?`, taskID,
	).Scan(&rawAuthority, &control.RequiredEnforcement); err != nil {
		return taskCapabilityControl{}, err
	}
	if err := json.Unmarshal([]byte(rawAuthority), &control.Authority); err != nil {
		return taskCapabilityControl{}, fmt.Errorf("decode task authority: %w", err)
	}
	return control, nil
}

func authorityContains(authority, required []string) bool {
	set := make(map[string]struct{}, len(authority))
	for _, capability := range authority {
		set[capability] = struct{}{}
	}
	for _, capability := range required {
		if _, ok := set[capability]; !ok {
			return false
		}
	}
	return true
}

func enforcementAtLeast(actual, required domain.EnforcementLevel) bool {
	return enforcementRank(actual) >= enforcementRank(required)
}

func enforcementRank(level domain.EnforcementLevel) int {
	switch level {
	case domain.EnforcementUnenforced:
		return 1
	case domain.EnforcementPartial:
		return 2
	case domain.EnforcementEnforced:
		return 3
	default:
		return 0
	}
}

func newSessionToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate capability session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func hashToken(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}
