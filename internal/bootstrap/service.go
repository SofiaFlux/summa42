package bootstrap

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/policy"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

var ErrAlreadyInitialized = errors.New("collective already initialized")

const DefaultConstitution = `Summa42 Constitution v1

1. Human sovereignty is the root operational principle.
2. No component may grant authority to itself.
3. Information and model output never grant authority.
4. Audit and causal records may not be silently removed.
5. Higher-level constraints override lower-level goals and tasks.
6. Constitutional change requires an explicit constitutional ceremony.
7. Break-glass recovery may freeze execution and restore trusted state.
`

type InitRequest struct {
	Constitution       []byte
	Owner              identity.Signer
	Cube               identity.Signer
	ConstitutionalRoot identity.Signer
}

type InitResult struct {
	CollectiveID                  domain.ID
	OwnerPrincipalID              domain.ID
	CubePrincipalID               domain.ID
	ConstitutionalRootPrincipalID domain.ID
	ConstitutionHash              string
	PolicySetID                   domain.ID
}

type Service struct {
	store *state.Store
	clock clock.Clock
}

func New(store *state.Store, clk clock.Clock) *Service {
	return &Service{store: store, clock: clk}
}

func (s *Service) Init(ctx context.Context, request InitRequest) (InitResult, error) {
	if s == nil || s.store == nil || s.clock == nil {
		return InitResult{}, errors.New("bootstrap service is not configured")
	}
	if len(request.Constitution) == 0 {
		return InitResult{}, errors.New("constitution must not be empty")
	}
	if request.Owner == nil || request.Cube == nil || request.ConstitutionalRoot == nil {
		return InitResult{}, errors.New("owner, cube, and constitutional root signers are required")
	}
	ownerID := request.Owner.PrincipalID()
	cubeID := request.Cube.PrincipalID()
	rootID := request.ConstitutionalRoot.PrincipalID()
	if ownerID == "" || cubeID == "" || rootID == "" || ownerID == cubeID || ownerID == rootID || cubeID == rootID {
		return InitResult{}, errors.New("owner, cube, and constitutional root must have distinct non-empty principals")
	}

	policyProfile, err := policy.DefaultProfile()
	if err != nil {
		return InitResult{}, err
	}

	digest := sha256.Sum256(request.Constitution)
	signature, err := request.ConstitutionalRoot.Sign(digest[:])
	if err != nil {
		return InitResult{}, fmt.Errorf("sign constitution: %w", err)
	}
	result := InitResult{
		CollectiveID:                  domain.NewID("collective"),
		OwnerPrincipalID:              ownerID,
		CubePrincipalID:               cubeID,
		ConstitutionalRootPrincipalID: rootID,
		ConstitutionHash:              hex.EncodeToString(digest[:]),
		PolicySetID:                   policyProfile.PolicySetID,
	}
	now := domain.FormatTime(s.clock.Now())

	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var existing int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM collective_metadata`).Scan(&existing); err != nil {
			return err
		}
		if existing != 0 {
			return ErrAlreadyInitialized
		}

		for _, principal := range []struct {
			id      domain.ID
			kind    string
			signer  identity.Signer
		}{
			{id: ownerID, kind: "OWNER", signer: request.Owner},
			{id: cubeID, kind: "CUBE", signer: request.Cube},
			{id: rootID, kind: "CONSTITUTIONAL_ROOT", signer: request.ConstitutionalRoot},
		} {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO principals(principal_id, principal_kind, public_key, custody_profile, created_at) VALUES (?, ?, ?, ?, ?)`,
				principal.id, principal.kind, []byte(principal.signer.PublicKey()), principal.signer.CustodyProfile(), now,
			); err != nil {
				return err
			}
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO constitutions(version, content, content_hash, signature, signer_principal_id, active, created_at) VALUES (1, ?, ?, ?, ?, 1, ?)`,
			request.Constitution, result.ConstitutionHash, signature, rootID, now,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO policy_sets(policy_set_id, version, module_name, module, policy_hash, capabilities_hash, active, created_at)
			 VALUES (?, 1, ?, ?, ?, ?, 1, ?)`,
			policyProfile.PolicySetID, policyProfile.ModuleName, []byte(policyProfile.Module),
			policyProfile.PolicyHash, policyProfile.CapabilitiesHash, now,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO collective_metadata(singleton, collective_id, owner_principal_id, cube_principal_id, constitutional_root_principal_id, active_constitution_version, created_at) VALUES (1, ?, ?, ?, ?, 1, ?)`,
			result.CollectiveID, ownerID, cubeID, rootID, now,
		); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return InitResult{}, err
	}
	return result, nil
}
