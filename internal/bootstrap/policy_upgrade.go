package bootstrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/policy"
)

// ErrOperationsInFlight refuses a policy change while an operation prepared
// under the current policy set could still need it at DISPATCH.
var ErrOperationsInFlight = errors.New("operations are PREPARED or DISPATCHED; settle them before changing policy")

// PolicySetSigningDigest is what the Owner signs to activate a policy set.
func PolicySetSigningDigest(profile policy.Profile) []byte {
	digest := sha256.Sum256([]byte("summa42-policy-set\x00" + string(profile.PolicySetID) + "\x00" + profile.PolicyHash + "\x00" + profile.CapabilitiesHash))
	return digest[:]
}

// UpgradePolicy makes the shipped default policy set the active one, signed by
// the Collective Owner. Deactivating the old set and activating the new one is
// a single transaction. It is idempotent once the default set is active.
func (s *Service) UpgradePolicy(ctx context.Context, owner identity.Signer) (domain.ID, error) {
	if s == nil || s.store == nil || s.clock == nil {
		return "", errors.New("bootstrap service is not configured")
	}
	if owner == nil {
		return "", errors.New("Owner signer is required")
	}
	profile, err := policy.DefaultProfile()
	if err != nil {
		return "", err
	}
	signature, err := owner.Sign(PolicySetSigningDigest(profile))
	if err != nil {
		return "", fmt.Errorf("sign policy set: %w", err)
	}
	now := s.clock.Now().UTC().Format(time.RFC3339Nano)
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var ownerID domain.ID
		var publicKey []byte
		if err := tx.QueryRowContext(ctx, `
			SELECT m.owner_principal_id, p.public_key
			FROM collective_metadata m JOIN principals p ON p.principal_id = m.owner_principal_id
			WHERE p.principal_kind = 'OWNER'`,
		).Scan(&ownerID, &publicKey); err != nil {
			return fmt.Errorf("load Collective Owner: %w", err)
		}
		if owner.PrincipalID() != ownerID || !bytes.Equal(publicKey, []byte(owner.PublicKey())) {
			return errors.New("signer is not the Collective Owner")
		}
		var inFlight int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM external_operations WHERE state IN (?, ?)`,
			domain.OperationPrepared, domain.OperationDispatched,
		).Scan(&inFlight); err != nil {
			return err
		}
		if inFlight != 0 {
			return fmt.Errorf("%w (%d)", ErrOperationsInFlight, inFlight)
		}
		var active domain.ID
		if err := tx.QueryRowContext(ctx, `SELECT policy_set_id FROM policy_sets WHERE active = 1`).Scan(&active); err != nil {
			return fmt.Errorf("load active policy set: %w", err)
		}
		if active == profile.PolicySetID {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO policy_sets(policy_set_id, version, module_name, module, policy_hash, capabilities_hash, active, created_at, signer_principal_id, signature)
			SELECT ?, COALESCE(MAX(version), 0) + 1, ?, ?, ?, ?, 0, ?, ?, ? FROM policy_sets`,
			profile.PolicySetID, profile.ModuleName, []byte(profile.Module), profile.PolicyHash, profile.CapabilitiesHash,
			now, ownerID, signature,
		); err != nil {
			return fmt.Errorf("insert policy set %s: %w", profile.PolicySetID, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE policy_sets SET active = 0 WHERE active = 1`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE policy_sets SET active = 1 WHERE policy_set_id = ?`, profile.PolicySetID)
		return err
	})
	if err != nil {
		return "", err
	}
	return profile.PolicySetID, nil
}
