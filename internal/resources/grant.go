package resources

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

// A resource envelope is the budget a Task spends from. Only the Owner gives
// the Collective a budget: an envelope is created, and its limit changed,
// by a grant signed with the Owner key, verified here against the Owner's
// stored public key and kept with its signature.

// GrantAction is what an Owner grant does to an envelope.
type GrantAction string

const (
	GrantCreate   GrantAction = "CREATE"
	GrantSetLimit GrantAction = "SET_LIMIT"

	// GrantMaxAge bounds how long a signed grant stays acceptable.
	GrantMaxAge = 5 * time.Minute

	grantDomain = "summa42.resources.envelope.v1"
)

var (
	ErrGrantUnauthorized = errors.New("envelope grant is not signed by the Collective's Owner")
	ErrGrantExpired      = errors.New("signed envelope grant is not recent")
	ErrGrantReplayed     = errors.New("signed envelope grant was already applied")
	ErrEnvelopeExists    = errors.New("resource envelope already exists")

	envelopeIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{2,63}$`)
)

// Signer signs for one principal. identity.LocalEd25519 satisfies it.
type Signer interface {
	PrincipalID() domain.ID
	Sign(message []byte) ([]byte, error)
}

// OwnerGrant is one signed change to an envelope. GrantID is the nonce: a
// grant can be applied once.
type OwnerGrant struct {
	GrantID     domain.ID
	EnvelopeID  domain.ID
	Action      GrantAction
	HardLimit   int64
	PrincipalID domain.ID
	Reason      string
	IssuedAt    time.Time
	Signature   []byte
}

// Envelope is one envelope as the ledger sees it now.
type Envelope struct {
	ID        domain.ID `json:"envelope_id"`
	HardLimit int64     `json:"hard_limit"`
	Available int64     `json:"available"`
	CreatedAt time.Time `json:"created_at"`
}

func OwnerGrantSigningMessage(collectiveID domain.ID, grant OwnerGrant) []byte {
	return []byte(strings.Join([]string{
		grantDomain,
		string(collectiveID),
		string(grant.GrantID),
		string(grant.Action),
		string(grant.EnvelopeID),
		strconv.FormatInt(grant.HardLimit, 10),
		string(grant.PrincipalID),
		grant.IssuedAt.UTC().Format(time.RFC3339Nano),
		grant.Reason,
	}, "\n"))
}

func (g OwnerGrant) validate() error {
	if g.GrantID == "" || g.PrincipalID == "" || g.IssuedAt.IsZero() {
		return errors.New("envelope grant needs an id, a principal and an issue time")
	}
	if g.Action != GrantCreate && g.Action != GrantSetLimit {
		return fmt.Errorf("unknown envelope grant action %q", g.Action)
	}
	if !envelopeIDPattern.MatchString(string(g.EnvelopeID)) {
		return fmt.Errorf("envelope id %q must be 3 to 64 characters of a-z, 0-9, _ and -, starting with a letter or digit", g.EnvelopeID)
	}
	if g.HardLimit < 0 {
		return errors.New("envelope limit must not be negative")
	}
	if strings.TrimSpace(g.Reason) == "" || strings.ContainsAny(g.Reason, "\n\r") {
		return errors.New("envelope grant needs a one-line reason")
	}
	return nil
}

// NewOwnerGrant validates and signs one envelope grant for collectiveID.
func NewOwnerGrant(signer Signer, collectiveID domain.ID, action GrantAction, envelopeID domain.ID, hardLimit int64, reason string, now time.Time) (OwnerGrant, error) {
	if signer == nil {
		return OwnerGrant{}, errors.New("Owner signer is required")
	}
	if strings.TrimSpace(string(collectiveID)) == "" {
		return OwnerGrant{}, errors.New("collective id is required")
	}
	grant := OwnerGrant{
		GrantID: domain.NewID("envgrant"), EnvelopeID: envelopeID, Action: action, HardLimit: hardLimit,
		PrincipalID: signer.PrincipalID(), Reason: strings.TrimSpace(reason), IssuedAt: now.UTC(),
	}
	if err := grant.validate(); err != nil {
		return OwnerGrant{}, err
	}
	signature, err := signer.Sign(OwnerGrantSigningMessage(collectiveID, grant))
	if err != nil {
		return OwnerGrant{}, fmt.Errorf("sign envelope grant: %w", err)
	}
	grant.Signature = signature
	return grant, nil
}

// ApplyOwnerGrant verifies that the Collective's Owner signed the grant
// recently and that it was not applied before, then creates the envelope or
// sets its limit, and records the grant. A limit cannot be set below what the
// envelope has already committed (settled, held and unresolved amounts).
func (s *Service) ApplyOwnerGrant(ctx context.Context, grant OwnerGrant) (Envelope, error) {
	if err := s.configured(); err != nil {
		return Envelope{}, err
	}
	grant.Reason = strings.TrimSpace(grant.Reason)
	if err := grant.validate(); err != nil {
		return Envelope{}, err
	}
	var collectiveID, ownerID string
	var publicKey []byte
	err := s.store.DB().QueryRowContext(ctx, `
		SELECT m.collective_id, m.owner_principal_id, p.public_key
		FROM collective_metadata m JOIN principals p ON p.principal_id = m.owner_principal_id
		WHERE m.singleton = 1`,
	).Scan(&collectiveID, &ownerID, &publicKey)
	if err != nil {
		return Envelope{}, fmt.Errorf("load the Collective's Owner: %w", err)
	}
	if grant.PrincipalID != domain.ID(ownerID) {
		return Envelope{}, fmt.Errorf("%w: principal %q is not the Owner", ErrGrantUnauthorized, grant.PrincipalID)
	}
	if len(publicKey) != ed25519.PublicKeySize || len(grant.Signature) != ed25519.SignatureSize ||
		!ed25519.Verify(publicKey, OwnerGrantSigningMessage(domain.ID(collectiveID), grant), grant.Signature) {
		return Envelope{}, fmt.Errorf("%w: signature does not verify", ErrGrantUnauthorized)
	}
	now := s.clock.Now().UTC()
	if age := now.Sub(grant.IssuedAt); age > GrantMaxAge || age < -GrantMaxAge {
		return Envelope{}, ErrGrantExpired
	}
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var seen int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM resource_envelope_grants WHERE grant_id = ?`, grant.GrantID).Scan(&seen)
		if err == nil {
			return ErrGrantReplayed
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check envelope grant replay: %w", err)
		}
		stamp := domain.FormatTime(now)
		switch grant.Action {
		case GrantCreate:
			err := tx.QueryRowContext(ctx, `SELECT 1 FROM resource_envelopes WHERE envelope_id = ?`, grant.EnvelopeID).Scan(&seen)
			if err == nil {
				return fmt.Errorf("%w: %s", ErrEnvelopeExists, grant.EnvelopeID)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO resource_envelopes (envelope_id, hard_limit, created_at) VALUES (?, ?, ?)`,
				grant.EnvelopeID, grant.HardLimit, stamp); err != nil {
				return fmt.Errorf("create envelope: %w", err)
			}
		case GrantSetLimit:
			available, err := availableIn(ctx, tx, grant.EnvelopeID)
			if err != nil {
				return err
			}
			var current int64
			if err := tx.QueryRowContext(ctx, `SELECT hard_limit FROM resource_envelopes WHERE envelope_id = ?`, grant.EnvelopeID).Scan(&current); err != nil {
				return err
			}
			if committed := current - available; grant.HardLimit < committed {
				return fmt.Errorf("envelope %s has already committed %d units; the limit cannot be set to %d", grant.EnvelopeID, committed, grant.HardLimit)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE resource_envelopes SET hard_limit = ? WHERE envelope_id = ?`, grant.HardLimit, grant.EnvelopeID); err != nil {
				return fmt.Errorf("set envelope limit: %w", err)
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO resource_envelope_grants
			(grant_id, envelope_id, action, hard_limit, owner_principal_id, reason, issued_at, signature, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			grant.GrantID, grant.EnvelopeID, grant.Action, grant.HardLimit, grant.PrincipalID, grant.Reason,
			domain.FormatTime(grant.IssuedAt), grant.Signature, stamp)
		if err != nil {
			return fmt.Errorf("record envelope grant: %w", err)
		}
		return nil
	})
	if err != nil {
		return Envelope{}, err
	}
	return s.Envelope(ctx, grant.EnvelopeID)
}

// Envelope returns one envelope with what is still available in it.
func (s *Service) Envelope(ctx context.Context, envelopeID domain.ID) (Envelope, error) {
	if err := s.configured(); err != nil {
		return Envelope{}, err
	}
	envelope := Envelope{ID: envelopeID}
	var createdAt string
	err := s.store.DB().QueryRowContext(ctx, `SELECT hard_limit, created_at FROM resource_envelopes WHERE envelope_id = ?`, envelopeID).Scan(&envelope.HardLimit, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Envelope{}, fmt.Errorf("resource envelope %q not found", envelopeID)
	}
	if err != nil {
		return Envelope{}, err
	}
	if envelope.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Envelope{}, fmt.Errorf("parse envelope %s created_at: %w", envelopeID, err)
	}
	if envelope.Available, err = availableIn(ctx, s.store.DB(), envelopeID); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}

// Envelopes lists every envelope, oldest first.
func (s *Service) Envelopes(ctx context.Context) ([]Envelope, error) {
	if err := s.configured(); err != nil {
		return nil, err
	}
	rows, err := s.store.DB().QueryContext(ctx, `SELECT envelope_id FROM resource_envelopes ORDER BY created_at, envelope_id`)
	if err != nil {
		return nil, fmt.Errorf("list envelopes: %w", err)
	}
	var ids []domain.ID
	for rows.Next() {
		var id domain.ID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	envelopes := make([]Envelope, 0, len(ids))
	for _, id := range ids {
		envelope, err := s.Envelope(ctx, id)
		if err != nil {
			return nil, err
		}
		envelopes = append(envelopes, envelope)
	}
	return envelopes, nil
}
