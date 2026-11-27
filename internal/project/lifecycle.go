package project

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/resources"
)

// TransitionAction is an Owner-signed lifecycle change on a Project.
type TransitionAction string

const (
	ActionPause   TransitionAction = "PAUSE"
	ActionResume  TransitionAction = "RESUME"
	ActionArchive TransitionAction = "ARCHIVE"

	transitionDomain = "summa42.project.transition.v1"
)

// TransitionCommand is an Owner-signed PAUSE, RESUME, or ARCHIVE command.
type TransitionCommand struct {
	EventID     domain.ID        `json:"event_id"`
	ProjectID   domain.ID        `json:"project_id"`
	Slug        string           `json:"slug"`
	Action      TransitionAction `json:"action"`
	Reason      string           `json:"reason"`
	PrincipalID domain.ID        `json:"principal_id"`
	IssuedAt    time.Time        `json:"issued_at"`
	Signature   []byte           `json:"signature"`
}

// TransitionSigningMessage builds the canonical byte slice signed by the Owner key.
func TransitionSigningMessage(collectiveID domain.ID, cmd TransitionCommand) []byte {
	return []byte(strings.Join([]string{
		transitionDomain,
		string(collectiveID),
		string(cmd.EventID),
		string(cmd.ProjectID),
		strings.TrimSpace(cmd.Slug),
		string(cmd.Action),
		string(cmd.PrincipalID),
		cmd.IssuedAt.UTC().Format(time.RFC3339Nano),
		strings.TrimSpace(cmd.Reason),
	}, "\n"))
}

// NewTransitionCommand validates and signs a Project lifecycle command.
func NewTransitionCommand(signer Signer, collectiveID, projectID domain.ID, slug string, action TransitionAction, reason string, now time.Time) (TransitionCommand, error) {
	if signer == nil {
		return TransitionCommand{}, errors.New("Owner signer is required")
	}
	if strings.TrimSpace(string(collectiveID)) == "" {
		return TransitionCommand{}, errors.New("collective id is required")
	}
	cmd := TransitionCommand{
		EventID:     domain.NewID("projevent"),
		ProjectID:   projectID,
		Slug:        strings.TrimSpace(slug),
		Action:      action,
		Reason:      strings.TrimSpace(reason),
		PrincipalID: signer.PrincipalID(),
		IssuedAt:    now.UTC(),
	}
	if cmd.Reason == "" {
		cmd.Reason = strings.ToLower(string(action)) + " project " + cmd.Slug
	}
	if err := cmd.validate(); err != nil {
		return TransitionCommand{}, err
	}
	sig, err := signer.Sign(TransitionSigningMessage(collectiveID, cmd))
	if err != nil {
		return TransitionCommand{}, fmt.Errorf("sign project transition: %w", err)
	}
	cmd.Signature = sig
	return cmd, nil
}

func (c TransitionCommand) validate() error {
	if c.EventID == "" || c.ProjectID == "" || c.PrincipalID == "" || c.IssuedAt.IsZero() {
		return errors.New("transition command needs event_id, project_id, principal_id and issued_at")
	}
	if strings.TrimSpace(c.Slug) == "" || strings.ContainsAny(c.Slug, "\n\r\t ") {
		return errors.New("project slug must be a single non-blank token")
	}
	switch c.Action {
	case ActionPause, ActionResume, ActionArchive:
	default:
		return fmt.Errorf("unknown project transition action %q", c.Action)
	}
	if strings.TrimSpace(c.Reason) == "" || strings.ContainsAny(c.Reason, "\n\r") {
		return errors.New("transition command needs a one-line reason")
	}
	return nil
}

// TransitionSigned verifies the Owner's Ed25519 signature, freshness, and
// anti-replay nonce, applies PAUSE/RESUME/ARCHIVE atomically in SQLite, revokes
// active Project Attempt leases on PAUSE, and records an immutable audit event.
func (s *Service) TransitionSigned(ctx context.Context, cmd TransitionCommand) (Project, error) {
	if err := s.configured(); err != nil {
		return Project{}, err
	}
	cmd.Slug = strings.TrimSpace(cmd.Slug)
	cmd.Reason = strings.TrimSpace(cmd.Reason)
	if err := cmd.validate(); err != nil {
		return Project{}, err
	}
	collectiveID, ownerID, publicKey, err := s.loadOwnerKey(ctx)
	if err != nil {
		return Project{}, err
	}
	if cmd.PrincipalID != domain.ID(ownerID) || len(publicKey) != ed25519.PublicKeySize ||
		len(cmd.Signature) != ed25519.SignatureSize ||
		!ed25519.Verify(publicKey, TransitionSigningMessage(domain.ID(collectiveID), cmd), cmd.Signature) {
		return Project{}, fmt.Errorf("%w: transition signature does not verify", ErrUnauthorized)
	}
	now := s.clock.Now().UTC()
	if age := now.Sub(cmd.IssuedAt); age > CommandMaxAge || age < -CommandMaxAge {
		return Project{}, ErrExpired
	}
	stamp := domain.FormatTime(now)

	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var seen int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM project_events WHERE event_id = ?`, cmd.EventID).Scan(&seen)
		if err == nil {
			return ErrReplayed
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var dbState string
		err = tx.QueryRowContext(ctx, `SELECT state FROM projects WHERE project_id = ? AND slug = ?`, cmd.ProjectID, cmd.Slug).Scan(&dbState)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrProjectNotFound
		}
		if err != nil {
			return err
		}
		fromState := State(dbState)
		var toState State
		var archivedAt any
		switch cmd.Action {
		case ActionPause:
			if fromState != StateActive {
				return fmt.Errorf("cannot pause project in state %s", fromState)
			}
			toState = StatePaused
			if _, err := execution.RevokeProjectLeasesTx(ctx, tx, cmd.ProjectID, now, "project_paused"); err != nil {
				return fmt.Errorf("revoke active leases on project pause: %w", err)
			}
		case ActionResume:
			if fromState != StatePaused {
				return fmt.Errorf("cannot resume project in state %s", fromState)
			}
			toState = StateActive
		case ActionArchive:
			if fromState == StateArchived {
				return fmt.Errorf("%w: project %s is already archived", domain.ErrProjectArchived, cmd.Slug)
			}
			if err := ensureArchiveSafeTx(ctx, tx, cmd.ProjectID); err != nil {
				return err
			}
			toState = StateArchived
			archivedAt = stamp
		}
		if archivedAt != nil {
			_, err = tx.ExecContext(ctx,
				`UPDATE projects SET state = ?, archived_at = ?, updated_at = ? WHERE project_id = ?`,
				toState, archivedAt, stamp, cmd.ProjectID,
			)
		} else {
			_, err = tx.ExecContext(ctx,
				`UPDATE projects SET state = ?, updated_at = ? WHERE project_id = ?`,
				toState, stamp, cmd.ProjectID,
			)
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO project_events(event_id, project_id, action, from_state, to_state, manifest_sha256, owner_principal_id, reason, issued_at, signature, created_at)
			VALUES (?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?)`,
			cmd.EventID, cmd.ProjectID, cmd.Action, fromState, toState, cmd.PrincipalID, cmd.Reason, domain.FormatTime(cmd.IssuedAt), cmd.Signature, stamp,
		)
		return err
	})
	if err != nil {
		return Project{}, err
	}
	return s.Get(ctx, string(cmd.ProjectID))
}

func ensureArchiveSafeTx(ctx context.Context, tx *sql.Tx, projectID domain.ID) error {
	var activeLeases int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM attempts a
		JOIN tasks t ON t.task_id = a.task_id
		LEFT JOIN resource_envelopes e ON e.envelope_id = t.resource_envelope_id
		WHERE a.lease_state = ?
		  AND COALESCE(NULLIF(t.project_id, ''), NULLIF(e.project_id, ''), '') = ?`,
		domain.LeaseActive, projectID,
	).Scan(&activeLeases); err != nil {
		return err
	}
	if activeLeases > 0 {
		return fmt.Errorf("cannot archive project %s while %d active lease(s) remain; pause first", projectID, activeLeases)
	}
	var pendingOps int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM external_operations o
		JOIN tasks t ON t.task_id = o.task_id
		LEFT JOIN resource_envelopes e ON e.envelope_id = t.resource_envelope_id
		WHERE o.state IN ('DISPATCHED', 'OUTCOME_UNKNOWN')
		  AND COALESCE(NULLIF(t.project_id, ''), NULLIF(e.project_id, ''), '') = ?`,
		projectID,
	).Scan(&pendingOps); err != nil {
		return err
	}
	if pendingOps > 0 {
		return fmt.Errorf("cannot archive project %s while %d non-final external operation(s) remain; reconcile first", projectID, pendingOps)
	}
	return nil
}

// EnvelopeApplier applies Owner-signed resource envelope grants.
type EnvelopeApplier interface {
	ApplyOwnerGrant(context.Context, resources.OwnerGrant) (resources.Envelope, error)
}

// FundSigned verifies the Owner-signed envelope grant targets ref's envelope
// and delegates strict tree-partitioned funding to resSvc.
func (s *Service) FundSigned(ctx context.Context, resSvc EnvelopeApplier, ref string, grant resources.OwnerGrant) (resources.Envelope, error) {
	if err := s.configured(); err != nil {
		return resources.Envelope{}, err
	}
	if resSvc == nil {
		return resources.Envelope{}, errors.New("resource service is required")
	}
	proj, err := s.Get(ctx, ref)
	if err != nil {
		return resources.Envelope{}, err
	}
	if proj.State == StateArchived {
		return resources.Envelope{}, fmt.Errorf("%w: %s", domain.ErrProjectArchived, proj.Slug)
	}
	if grant.Action != resources.GrantSetLimit || grant.EnvelopeID != proj.EnvelopeID {
		return resources.Envelope{}, fmt.Errorf("funding grant must target SET_LIMIT on project %s envelope %s", proj.Slug, proj.EnvelopeID)
	}
	return resSvc.ApplyOwnerGrant(ctx, grant)
}
