package delegation

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

// PublicKeyResolver resolves the public key for a principal ID.
type PublicKeyResolver interface {
	ResolvePublicKey(ctx context.Context, principalID domain.ID) (ed25519.PublicKey, error)
}

// MapPublicKeyResolver is an in-memory key resolver for tests and local registry.
type MapPublicKeyResolver map[domain.ID]ed25519.PublicKey

func (m MapPublicKeyResolver) ResolvePublicKey(_ context.Context, principalID domain.ID) (ed25519.PublicKey, error) {
	key, ok := m[principalID]
	if !ok {
		return nil, fmt.Errorf("public key not found for principal %s", principalID)
	}
	return key, nil
}

// Validator validates foreign principals and inter-collective delegation chains.
type Validator struct {
	resolver             PublicKeyResolver
	internalPrefixes     []string
	allowInternalSummand bool // strictly false in production
}

func NewValidator(resolver PublicKeyResolver) *Validator {
	return &Validator{
		resolver: resolver,
		internalPrefixes: []string{
			"summand_",
			"cube_",
			"principal_owner",
			"owner_",
			"system_",
			"root",
		},
		allowInternalSummand: false,
	}
}

// IsInternalIdentity returns true if a principal ID represents an internal system/summand entity.
func (v *Validator) IsInternalIdentity(id domain.ID) bool {
	str := strings.ToLower(strings.TrimSpace(string(id)))
	if str == "" {
		return false
	}
	for _, prefix := range v.internalPrefixes {
		if strings.HasPrefix(str, prefix) || str == prefix {
			return true
		}
	}
	return false
}

// ValidateForeignPrincipal verifies basic integrity and guarantees foreign principals
// cannot impersonate internal Summand or Box identities (Acceptance Criterion 3).
func (v *Validator) ValidateForeignPrincipal(fp *ForeignPrincipal) error {
	if fp == nil {
		return ErrInvalidPrincipal
	}
	if strings.TrimSpace(string(fp.PrincipalID)) == "" {
		return fmt.Errorf("%w: principal_id is required", ErrInvalidPrincipal)
	}
	if strings.TrimSpace(string(fp.OriginCollectiveID)) == "" {
		return fmt.Errorf("%w: origin_collective_id is required", ErrInvalidPrincipal)
	}
	if len(fp.PublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: invalid ed25519 public key length", ErrInvalidPrincipal)
	}

	// Criterion 3: Foreign principal must not assume internal summand identity
	if v.IsInternalIdentity(fp.PrincipalID) {
		return fmt.Errorf("%w: foreign principal %s claims internal identity", ErrInternalIdentityAssigned, fp.PrincipalID)
	}

	return nil
}

// ValidateChain validates a full delegation chain from root issuer down to leaf subject
// and verifies that requested capabilities and financial commitments are fully covered.
func (v *Validator) ValidateChain(
	ctx context.Context,
	chain []*DelegationRecord,
	requestedCap string,
	requestedExposure int64,
	now time.Time,
) (*DelegationContext, error) {
	if len(chain) == 0 {
		return nil, ErrInvalidDelegation
	}

	var prevRecord *DelegationRecord

	for i, record := range chain {
		if record == nil {
			return nil, ErrInvalidDelegation
		}

		// 1. Identity isolation: no internal identity assignment
		if v.IsInternalIdentity(record.SubjectPrincipalID) {
			return nil, fmt.Errorf("%w: subject %s is internal summand", ErrInternalIdentityAssigned, record.SubjectPrincipalID)
		}

		// 2. Temporal validity
		if now.Before(record.ValidFrom) || now.After(record.ValidUntil) {
			return nil, fmt.Errorf("%w: record %s (window %s - %s, current %s)",
				ErrExpiredDelegation, record.DelegationID,
				record.ValidFrom.Format(time.RFC3339),
				record.ValidUntil.Format(time.RFC3339),
				now.Format(time.RFC3339),
			)
		}

		// 3. Revocation status
		if record.Revoked {
			return nil, fmt.Errorf("%w: record %s revoked (reason: %s)",
				ErrRevokedDelegation, record.DelegationID, record.RevocationReason,
			)
		}

		// 4. Cryptographic signature check
		issuerKey, err := v.resolver.ResolvePublicKey(ctx, record.IssuerPrincipalID)
		if err != nil {
			return nil, fmt.Errorf("resolve issuer key for %s: %w", record.IssuerPrincipalID, err)
		}
		if !record.VerifySignature(issuerKey) {
			return nil, fmt.Errorf("%w: record %s signature invalid", ErrInvalidSignature, record.DelegationID)
		}

		// 5. Chain continuity and attenuation
		if i == 0 {
			if record.ParentDelegationID != "" {
				return nil, fmt.Errorf("%w: root record %s has parent_delegation_id", ErrBrokenDelegationChain, record.DelegationID)
			}
		} else {
			if record.ParentDelegationID != prevRecord.DelegationID {
				return nil, fmt.Errorf("%w: record %s parent %s != prev %s",
					ErrBrokenDelegationChain, record.DelegationID, record.ParentDelegationID, prevRecord.DelegationID,
				)
			}
			if record.IssuerPrincipalID != prevRecord.SubjectPrincipalID {
				return nil, fmt.Errorf("%w: record %s issuer %s != prev subject %s",
					ErrBrokenDelegationChain, record.DelegationID, record.IssuerPrincipalID, prevRecord.SubjectPrincipalID,
				)
			}
			// Acceptance Criterion 2: Requested authority must not exceed issuer ceiling
			if !prevRecord.Scope.Subsumes(record.Scope) {
				return nil, fmt.Errorf("%w: record %s expands authority beyond parent %s",
					ErrCeilingExceeded, record.DelegationID, prevRecord.DelegationID,
				)
			}
		}

		prevRecord = record
	}

	leaf := chain[len(chain)-1]

	// 6. Check requested capability against leaf ceiling
	if requestedCap != "" && !leaf.Scope.AllowsCapability(requestedCap) {
		return nil, fmt.Errorf("%w: capability %s not permitted by delegation scope %v",
			ErrCapabilityNotGranted, requestedCap, leaf.Scope.AllowedCapabilities,
		)
	}

	// 7. Check requested exposure against leaf ceiling
	if requestedExposure > 0 && requestedExposure > leaf.Scope.MaxExposureAmount {
		return nil, fmt.Errorf("%w: requested %d exceeds ceiling %d",
			ErrExposureCeilingExceeded, requestedExposure, leaf.Scope.MaxExposureAmount,
		)
	}

	// ponytail: future trust telemetry and inter-collective gateway integration hook
	return &DelegationContext{
		SubjectPrincipalID: leaf.SubjectPrincipalID,
		IssuerPrincipalID:  leaf.IssuerPrincipalID,
		RepresentedOrg:     leaf.RepresentedOrg,
		EffectiveScope:     leaf.Scope,
		ValidUntil:         leaf.ValidUntil,
		ChainLength:        len(chain),
	}, nil
}
