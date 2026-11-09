package delegation_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/delegation"
	"github.com/SofiaFlux/summa42/internal/domain"
)

type keyPair struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func genKey(t *testing.T) keyPair {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	return keyPair{pub: pub, priv: priv}
}

// 1. Valid Single-Hop and Multi-Hop Delegation Chains
func TestDelegationChain_ValidSingleAndMultiHop(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	// Keys for Root Issuer, Middle Agent, Leaf Worker
	rootKey := genKey(t)
	middleKey := genKey(t)
	leafKey := genKey(t)

	rootID := domain.ID("foreign_collective_root")
	middleID := domain.ID("foreign_agent_partner")
	leafID := domain.ID("foreign_worker_subagent")

	resolver := delegation.MapPublicKeyResolver{
		rootID:   rootKey.pub,
		middleID: middleKey.pub,
		leafID:   leafKey.pub,
	}
	v := delegation.NewValidator(resolver)

	// Step 1: Root delegates broad scope to Middle Agent
	rootRecord := &delegation.DelegationRecord{
		DelegationID:       domain.ID("del_root_middle"),
		IssuerPrincipalID:  rootID,
		IssuerCollectiveID: domain.ID("collective_alpha"),
		SubjectPrincipalID: middleID,
		RepresentedOrg:     "AlphaCorp Inc.",
		Scope: delegation.CapabilityCeiling{
			AllowedCapabilities: []string{"github.issue.*", "http.get"},
			MaxExposureAmount:   50000,
			MaxResourceCredits:  1000,
		},
		ValidFrom:  now.Add(-1 * time.Hour),
		ValidUntil: now.Add(24 * time.Hour),
		CreatedAt:  now.Add(-1 * time.Hour),
	}
	if err := rootRecord.Sign(rootKey.priv); err != nil {
		t.Fatalf("sign root record: %v", err)
	}

	// Single hop validation
	delCtx, err := v.ValidateChain(ctx, []*delegation.DelegationRecord{rootRecord}, "github.issue.read", 10000, now)
	if err != nil {
		t.Fatalf("validate single hop: %v", err)
	}
	if delCtx.SubjectPrincipalID != middleID || delCtx.ChainLength != 1 {
		t.Fatalf("unexpected context: %+v", delCtx)
	}

	// Step 2: Middle Agent attenuates scope and delegates to Leaf Worker
	leafRecord := &delegation.DelegationRecord{
		DelegationID:       domain.ID("del_middle_leaf"),
		ParentDelegationID: rootRecord.DelegationID,
		IssuerPrincipalID:  middleID,
		IssuerCollectiveID: domain.ID("collective_alpha"),
		SubjectPrincipalID: leafID,
		RepresentedOrg:     "AlphaCorp Inc.",
		Scope: delegation.CapabilityCeiling{
			AllowedCapabilities: []string{"github.issue.read"},
			MaxExposureAmount:   10000,
			MaxResourceCredits:  200,
		},
		ValidFrom:  now.Add(-30 * time.Minute),
		ValidUntil: now.Add(6 * time.Hour),
		CreatedAt:  now.Add(-30 * time.Minute),
	}
	if err := leafRecord.Sign(middleKey.priv); err != nil {
		t.Fatalf("sign leaf record: %v", err)
	}

	// Multi hop validation
	delCtx2, err := v.ValidateChain(ctx, []*delegation.DelegationRecord{rootRecord, leafRecord}, "github.issue.read", 5000, now)
	if err != nil {
		t.Fatalf("validate multi hop: %v", err)
	}
	if delCtx2.SubjectPrincipalID != leafID || delCtx2.ChainLength != 2 {
		t.Fatalf("unexpected context for multi-hop: %+v", delCtx2)
	}
}

// 2. Acceptance Criterion 2: Rejects Credential When Authority Exceeds Ceiling
func TestDelegationChain_RejectsAuthorityExceedingCeiling(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	rootKey := genKey(t)
	middleKey := genKey(t)
	rootID := domain.ID("foreign_collective_root")
	middleID := domain.ID("foreign_agent_partner")

	resolver := delegation.MapPublicKeyResolver{
		rootID:   rootKey.pub,
		middleID: middleKey.pub,
	}
	v := delegation.NewValidator(resolver)

	rootRecord := &delegation.DelegationRecord{
		DelegationID:       domain.ID("del_root_middle"),
		IssuerPrincipalID:  rootID,
		IssuerCollectiveID: domain.ID("collective_alpha"),
		SubjectPrincipalID: middleID,
		Scope: delegation.CapabilityCeiling{
			AllowedCapabilities: []string{"github.issue.read"},
			MaxExposureAmount:   10000,
			MaxResourceCredits:  500,
		},
		ValidFrom:  now.Add(-1 * time.Hour),
		ValidUntil: now.Add(24 * time.Hour),
	}
	_ = rootRecord.Sign(rootKey.priv)

	// Child expands capability scope beyond parent
	expandedCapRecord := &delegation.DelegationRecord{
		DelegationID:       domain.ID("del_child_expanded_cap"),
		ParentDelegationID: rootRecord.DelegationID,
		IssuerPrincipalID:  middleID,
		SubjectPrincipalID: domain.ID("foreign_worker_x"),
		Scope: delegation.CapabilityCeiling{
			AllowedCapabilities: []string{"github.issue.read", "github.issue.write"}, // write was NOT granted!
			MaxExposureAmount:   5000,
			MaxResourceCredits:  100,
		},
		ValidFrom:  now.Add(-1 * time.Hour),
		ValidUntil: now.Add(12 * time.Hour),
	}
	_ = expandedCapRecord.Sign(middleKey.priv)

	_, err := v.ValidateChain(ctx, []*delegation.DelegationRecord{rootRecord, expandedCapRecord}, "", 0, now)
	if err == nil {
		t.Fatalf("expected error when child expands capability scope, got nil")
	}

	// Child expands financial exposure ceiling beyond parent
	expandedExposureRecord := &delegation.DelegationRecord{
		DelegationID:       domain.ID("del_child_expanded_exposure"),
		ParentDelegationID: rootRecord.DelegationID,
		IssuerPrincipalID:  middleID,
		SubjectPrincipalID: domain.ID("foreign_worker_y"),
		Scope: delegation.CapabilityCeiling{
			AllowedCapabilities: []string{"github.issue.read"},
			MaxExposureAmount:   25000, // Parent only allowed 10000!
			MaxResourceCredits:  100,
		},
		ValidFrom:  now.Add(-1 * time.Hour),
		ValidUntil: now.Add(12 * time.Hour),
	}
	_ = expandedExposureRecord.Sign(middleKey.priv)

	_, err = v.ValidateChain(ctx, []*delegation.DelegationRecord{rootRecord, expandedExposureRecord}, "", 0, now)
	if err == nil {
		t.Fatalf("expected error when child expands financial ceiling, got nil")
	}
}

// 3. Expired Delegations and Broken Chains
func TestDelegationChain_RejectsExpiredOrBrokenChain(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	rootKey := genKey(t)
	rootID := domain.ID("foreign_collective_root")
	resolver := delegation.MapPublicKeyResolver{rootID: rootKey.pub}
	v := delegation.NewValidator(resolver)

	// Past expiration
	expiredRecord := &delegation.DelegationRecord{
		DelegationID:       domain.ID("del_expired"),
		IssuerPrincipalID:  rootID,
		SubjectPrincipalID: domain.ID("foreign_subject"),
		Scope: delegation.CapabilityCeiling{
			AllowedCapabilities: []string{"http.get"},
			MaxExposureAmount:   1000,
		},
		ValidFrom:  now.Add(-48 * time.Hour),
		ValidUntil: now.Add(-2 * time.Hour), // Expired 2 hours ago
	}
	_ = expiredRecord.Sign(rootKey.priv)

	_, err := v.ValidateChain(ctx, []*delegation.DelegationRecord{expiredRecord}, "http.get", 0, now)
	if err == nil {
		t.Fatalf("expected error for expired delegation, got nil")
	}

	// Revoked record
	revokedRecord := &delegation.DelegationRecord{
		DelegationID:       domain.ID("del_revoked"),
		IssuerPrincipalID:  rootID,
		SubjectPrincipalID: domain.ID("foreign_subject"),
		Scope: delegation.CapabilityCeiling{
			AllowedCapabilities: []string{"http.get"},
			MaxExposureAmount:   1000,
		},
		ValidFrom:        now.Add(-1 * time.Hour),
		ValidUntil:       now.Add(24 * time.Hour),
		Revoked:          true,
		RevocationReason: "Key compromise detected",
	}
	_ = revokedRecord.Sign(rootKey.priv)

	_, err = v.ValidateChain(ctx, []*delegation.DelegationRecord{revokedRecord}, "http.get", 0, now)
	if err == nil {
		t.Fatalf("expected error for revoked delegation, got nil")
	}
}

// 4. Invalid Signature Rejection
func TestDelegationChain_RejectsInvalidSignature(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	rootKey := genKey(t)
	attackerKey := genKey(t)
	rootID := domain.ID("foreign_collective_root")
	resolver := delegation.MapPublicKeyResolver{rootID: rootKey.pub}
	v := delegation.NewValidator(resolver)

	tamperedRecord := &delegation.DelegationRecord{
		DelegationID:       domain.ID("del_tampered"),
		IssuerPrincipalID:  rootID, // Claims to be issued by root
		SubjectPrincipalID: domain.ID("attacker"),
		Scope: delegation.CapabilityCeiling{
			AllowedCapabilities: []string{"*"},
			MaxExposureAmount:   1000000,
		},
		ValidFrom:  now.Add(-1 * time.Hour),
		ValidUntil: now.Add(24 * time.Hour),
	}
	// Signed by attacker key instead of root key
	_ = tamperedRecord.Sign(attackerKey.priv)

	_, err := v.ValidateChain(ctx, []*delegation.DelegationRecord{tamperedRecord}, "any.cap", 0, now)
	if err == nil {
		t.Fatalf("expected signature verification failure, got nil")
	}
}

// 5. Acceptance Criterion 3: No Internal Summand Identity Assumption
func TestDelegationChain_RejectsInternalSummandImpersonation(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	rootKey := genKey(t)
	rootID := domain.ID("foreign_collective_root")
	resolver := delegation.MapPublicKeyResolver{rootID: rootKey.pub}
	v := delegation.NewValidator(resolver)

	// Foreign principal claiming to be internal summand
	for _, forbiddenID := range []domain.ID{
		"summand_alpha",
		"cube_root",
		"principal_owner",
		"owner_admin",
		"root",
	} {
		fp := &delegation.ForeignPrincipal{
			PrincipalID:        forbiddenID,
			OriginCollectiveID: "foreign_collective",
			RepresentedOrg:     "Attacker Corp",
			PublicKey:          rootKey.pub,
			TrustTier:          delegation.TrustUnknown,
			RegisteredAt:       now,
		}
		if err := v.ValidateForeignPrincipal(fp); err == nil {
			t.Fatalf("expected error when foreign principal claims %s, got nil", forbiddenID)
		}

		// Subject claiming internal summand in delegation record
		badRecord := &delegation.DelegationRecord{
			DelegationID:       domain.ID("del_bad_subject"),
			IssuerPrincipalID:  rootID,
			SubjectPrincipalID: forbiddenID,
			Scope: delegation.CapabilityCeiling{
				AllowedCapabilities: []string{"github.issue.read"},
				MaxExposureAmount:   1000,
			},
			ValidFrom:  now.Add(-1 * time.Hour),
			ValidUntil: now.Add(24 * time.Hour),
		}
		_ = badRecord.Sign(rootKey.priv)

		_, err := v.ValidateChain(ctx, []*delegation.DelegationRecord{badRecord}, "github.issue.read", 0, now)
		if err == nil {
			t.Fatalf("expected delegation to reject internal summand subject %s, got nil", forbiddenID)
		}
	}
}

// 6. Review fixes coverage: RateLimit, ValidUntil monotonicity, delimiter matching, max depth, issuer isolation
func TestDelegationChain_ReviewCases(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	rootKey := genKey(t)
	middleKey := genKey(t)
	rootID := domain.ID("foreign_collective_root")
	middleID := domain.ID("foreign_agent_partner")

	resolver := delegation.MapPublicKeyResolver{
		rootID:   rootKey.pub,
		middleID: middleKey.pub,
	}
	v := delegation.NewValidator(resolver)

	// A: Identity isolation boundary checks
	t.Run("IdentityIsolation_Boundaries", func(t *testing.T) {
		forbidden := []domain.ID{
			"summand.alpha",
			"cube.beta",
			"owner.service",
			"system.daemon",
			"constitutional_root_principal",
			"root_worker",
			"root.task",
			"summand_root",
			"cube_root",
			"principal_root_master",
		}
		for _, id := range forbidden {
			if !v.IsInternalIdentity(id) {
				t.Errorf("expected %s to be recognized as internal identity", id)
			}
		}

		allowed := []domain.ID{
			"foreign_collective_root",
			"rooted_foreign_agent",
			"rootless_org",
			"summary_generator",
			"ownership_dao",
			"foreign_partner",
		}
		for _, id := range allowed {
			if v.IsInternalIdentity(id) {
				t.Errorf("expected %s to NOT be recognized as internal identity", id)
			}
		}
	})

	// B: Internal identity as issuer rejected
	t.Run("InternalIssuer_Rejected", func(t *testing.T) {
		badIssuerRecord := &delegation.DelegationRecord{
			DelegationID:       domain.ID("del_bad_issuer"),
			IssuerPrincipalID:  domain.ID("summand.kernel"),
			SubjectPrincipalID: middleID,
			Scope: delegation.CapabilityCeiling{
				AllowedCapabilities: []string{"http.get"},
			},
			ValidFrom:  now.Add(-1 * time.Hour),
			ValidUntil: now.Add(24 * time.Hour),
		}
		_ = badIssuerRecord.Sign(rootKey.priv)
		_, err := v.ValidateChain(ctx, []*delegation.DelegationRecord{badIssuerRecord}, "http.get", 0, now)
		if err == nil {
			t.Fatalf("expected error when issuer is internal identity")
		}
	})

	// C: ValidUntil monotonicity violation
	t.Run("ValidUntil_Monotonicity", func(t *testing.T) {
		rootRecord := &delegation.DelegationRecord{
			DelegationID:       domain.ID("del_root"),
			IssuerPrincipalID:  rootID,
			SubjectPrincipalID: middleID,
			Scope: delegation.CapabilityCeiling{
				AllowedCapabilities: []string{"http.get"},
			},
			ValidFrom:  now.Add(-1 * time.Hour),
			ValidUntil: now.Add(5 * time.Hour),
			CreatedAt:  now.Add(-1 * time.Hour),
		}
		_ = rootRecord.Sign(rootKey.priv)

		childRecord := &delegation.DelegationRecord{
			DelegationID:       domain.ID("del_child"),
			ParentDelegationID: rootRecord.DelegationID,
			IssuerPrincipalID:  middleID,
			SubjectPrincipalID: domain.ID("foreign_leaf"),
			Scope: delegation.CapabilityCeiling{
				AllowedCapabilities: []string{"http.get"},
			},
			ValidFrom:  now.Add(-1 * time.Hour),
			ValidUntil: now.Add(6 * time.Hour), // Parent expires at +5h, child cannot outlive parent!
			CreatedAt:  now.Add(-1 * time.Hour),
		}
		_ = childRecord.Sign(middleKey.priv)

		_, err := v.ValidateChain(ctx, []*delegation.DelegationRecord{rootRecord, childRecord}, "http.get", 0, now)
		if err == nil {
			t.Fatalf("expected error when child ValidUntil > parent ValidUntil, got nil")
		}
	})

	// D: RateLimit attenuation violation
	t.Run("RateLimit_Attenuation", func(t *testing.T) {
		rootRecord := &delegation.DelegationRecord{
			DelegationID:       domain.ID("del_root_rate"),
			IssuerPrincipalID:  rootID,
			SubjectPrincipalID: middleID,
			Scope: delegation.CapabilityCeiling{
				AllowedCapabilities: []string{"http.get"},
				RateLimitPerMinute:  60,
			},
			ValidFrom:  now.Add(-1 * time.Hour),
			ValidUntil: now.Add(5 * time.Hour),
			CreatedAt:  now.Add(-1 * time.Hour),
		}
		_ = rootRecord.Sign(rootKey.priv)

		childRecord := &delegation.DelegationRecord{
			DelegationID:       domain.ID("del_child_rate"),
			ParentDelegationID: rootRecord.DelegationID,
			IssuerPrincipalID:  middleID,
			SubjectPrincipalID: domain.ID("foreign_leaf"),
			Scope: delegation.CapabilityCeiling{
				AllowedCapabilities: []string{"http.get"},
				RateLimitPerMinute:  120, // Exceeds parent's 60 rpm!
			},
			ValidFrom:  now.Add(-1 * time.Hour),
			ValidUntil: now.Add(4 * time.Hour),
			CreatedAt:  now.Add(-1 * time.Hour),
		}
		_ = childRecord.Sign(middleKey.priv)

		_, err := v.ValidateChain(ctx, []*delegation.DelegationRecord{rootRecord, childRecord}, "http.get", 0, now)
		if err == nil {
			t.Fatalf("expected error when child RateLimit > parent RateLimit, got nil")
		}
	})

	// E: MaxChainDepth limit
	t.Run("MaxChainDepth_Exceeded", func(t *testing.T) {
		var longChain []*delegation.DelegationRecord
		for i := 0; i <= delegation.MaxChainDepth; i++ {
			rec := &delegation.DelegationRecord{
				DelegationID: domain.ID("del"),
			}
			longChain = append(longChain, rec)
		}
		_, err := v.ValidateChain(ctx, longChain, "http.get", 0, now)
		if err == nil {
			t.Fatalf("expected error when chain exceeds MaxChainDepth")
		}
	})
}
