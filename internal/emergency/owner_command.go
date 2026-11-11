package emergency

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

// OwnerAction is one emergency control the Owner can sign.
type OwnerAction string

const (
	// ActionFreeze triggers Break Glass.
	ActionFreeze OwnerAction = "FREEZE"
	// ActionResume lifts Break Glass.
	ActionResume OwnerAction = "RESUME"
	// ActionHeartbeat records the Owner's presence and may set the horizon.
	ActionHeartbeat OwnerAction = "HEARTBEAT"

	// OwnerCommandMaxAge bounds how long a signed command stays acceptable.
	OwnerCommandMaxAge = 5 * time.Minute
	// MinHeartbeatTimeout is the shortest horizon the Owner can set.
	MinHeartbeatTimeout = time.Minute

	ownerCommandDomain = "summa42.emergency.v1"
)

var (
	// ErrOwnerCommandExpired is returned for a signed command outside OwnerCommandMaxAge.
	ErrOwnerCommandExpired = errors.New("signed emergency command is not recent")
	// ErrOwnerCommandReplayed is returned when a command's event ID was already recorded.
	ErrOwnerCommandReplayed = errors.New("signed emergency command was already applied")
)

// Signer signs for one principal. identity.LocalEd25519 satisfies it.
type Signer interface {
	PrincipalID() domain.ID
	Sign(message []byte) ([]byte, error)
}

// OwnerCommand is an emergency control signed by the Owner key. EventID is
// both the nonce and the ID of the audit event the command produces, so a
// command can be applied once.
type OwnerCommand struct {
	EventID        domain.ID
	Action         OwnerAction
	PrincipalID    domain.ID
	Reason         string
	TimeoutSeconds int
	IssuedAt       time.Time
	Signature      []byte
}

// auditMeta carries a signed command's identity into the audit event.
type auditMeta struct {
	eventID   domain.ID
	signature []byte
}

func (m auditMeta) eventIDOrNew() domain.ID {
	if m.eventID != "" {
		return m.eventID
	}
	return domain.NewID("emevent")
}

func (m auditMeta) signatureValue() any {
	if len(m.signature) == 0 {
		return nil
	}
	return m.signature
}

// OwnerCommandSigningMessage is the exact byte string the Owner signs. Reason
// comes last, so no text in it can be read as another field.
func OwnerCommandSigningMessage(collectiveID domain.ID, command OwnerCommand) []byte {
	return []byte(strings.Join([]string{
		ownerCommandDomain,
		string(collectiveID),
		string(command.EventID),
		string(command.Action),
		string(command.PrincipalID),
		command.IssuedAt.UTC().Format(time.RFC3339Nano),
		strconv.Itoa(command.TimeoutSeconds),
		command.Reason,
	}, "\n"))
}

// NewOwnerCommand validates and signs one emergency control for collectiveID.
func NewOwnerCommand(signer Signer, collectiveID domain.ID, action OwnerAction, reason string, timeoutSeconds int, now time.Time) (OwnerCommand, error) {
	if signer == nil {
		return OwnerCommand{}, errors.New("Owner signer is required")
	}
	command := OwnerCommand{
		EventID: domain.NewID("emevent"), Action: action, PrincipalID: signer.PrincipalID(),
		Reason: strings.TrimSpace(reason), TimeoutSeconds: timeoutSeconds, IssuedAt: now.UTC(),
	}
	if err := command.validate(); err != nil {
		return OwnerCommand{}, err
	}
	if strings.TrimSpace(string(collectiveID)) == "" {
		return OwnerCommand{}, errors.New("collective id is required")
	}
	signature, err := signer.Sign(OwnerCommandSigningMessage(collectiveID, command))
	if err != nil {
		return OwnerCommand{}, fmt.Errorf("sign emergency command: %w", err)
	}
	command.Signature = signature
	return command, nil
}

func (c OwnerCommand) validate() error {
	if c.EventID == "" || c.PrincipalID == "" || c.IssuedAt.IsZero() {
		return errors.New("emergency command needs an event id, a principal and an issue time")
	}
	switch c.Action {
	case ActionFreeze, ActionResume:
		if c.Reason == "" {
			return fmt.Errorf("%s needs a reason", c.Action)
		}
		if c.TimeoutSeconds != 0 {
			return fmt.Errorf("%s does not take a timeout", c.Action)
		}
	case ActionHeartbeat:
		if c.TimeoutSeconds != 0 && time.Duration(c.TimeoutSeconds)*time.Second < MinHeartbeatTimeout {
			return fmt.Errorf("heartbeat timeout must be at least %s", MinHeartbeatTimeout)
		}
	default:
		return fmt.Errorf("unknown emergency action %q", c.Action)
	}
	return nil
}

// ApplyOwnerCommand verifies that the Collective's Owner signed command
// recently and that it was not applied before, then performs it. The signature
// is stored on the audit event.
func (s *Service) ApplyOwnerCommand(ctx context.Context, command OwnerCommand) error {
	command.Reason = strings.TrimSpace(command.Reason)
	if err := command.validate(); err != nil {
		return err
	}
	var collectiveID, ownerID string
	var publicKey []byte
	err := s.store.DB().QueryRowContext(ctx, `
		SELECT m.collective_id, m.owner_principal_id, p.public_key
		FROM collective_metadata m JOIN principals p ON p.principal_id = m.owner_principal_id
		WHERE m.singleton = 1`,
	).Scan(&collectiveID, &ownerID, &publicKey)
	if err != nil {
		return fmt.Errorf("load the Collective's Owner: %w", err)
	}
	if command.PrincipalID != domain.ID(ownerID) {
		return fmt.Errorf("%w: principal %q is not the Owner", ErrUnauthorizedEmergencyCaller, command.PrincipalID)
	}
	if len(publicKey) != ed25519.PublicKeySize || len(command.Signature) != ed25519.SignatureSize ||
		!ed25519.Verify(publicKey, OwnerCommandSigningMessage(domain.ID(collectiveID), command), command.Signature) {
		return fmt.Errorf("%w: Owner signature does not verify", ErrUnauthorizedEmergencyCaller)
	}
	age := s.clock.Now().UTC().Sub(command.IssuedAt)
	if age > OwnerCommandMaxAge || age < -OwnerCommandMaxAge {
		return ErrOwnerCommandExpired
	}
	var seen int
	err = s.store.DB().QueryRowContext(ctx, `SELECT 1 FROM emergency_audit_events WHERE event_id = ?`, command.EventID).Scan(&seen)
	if err == nil {
		return ErrOwnerCommandReplayed
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check emergency command replay: %w", err)
	}
	meta := auditMeta{eventID: command.EventID, signature: command.Signature}
	switch command.Action {
	case ActionFreeze:
		return s.triggerBreakGlass(ctx, command.PrincipalID, command.Reason, meta)
	case ActionResume:
		return s.resumeSystem(ctx, command.PrincipalID, command.Reason, meta)
	default:
		return s.recordOwnerHeartbeat(ctx, command.PrincipalID, command.TimeoutSeconds, meta)
	}
}
