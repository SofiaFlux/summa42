package delegation

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

var (
	ErrInvalidPrincipal         = errors.New("invalid foreign principal")
	ErrInvalidDelegation        = errors.New("invalid delegation record")
	ErrExpiredDelegation        = errors.New("delegation is expired or not yet valid")
	ErrRevokedDelegation        = errors.New("delegation has been revoked")
	ErrInvalidSignature         = errors.New("delegation cryptographic signature verification failed")
	ErrCeilingExceeded          = errors.New("delegated authority exceeds parent ceiling")
	ErrCapabilityNotGranted     = errors.New("requested capability is not within delegated capability scope")
	ErrExposureCeilingExceeded  = errors.New("requested financial exposure exceeds delegated ceiling")
	ErrInternalIdentityAssigned = errors.New("foreign principal must not assume internal summand or ambient box authority")
	ErrBrokenDelegationChain    = errors.New("delegation chain is broken or discontinuous")
)

type TrustTier string

const (
	TrustUnknown     TrustTier = "UNKNOWN"
	TrustProvisional TrustTier = "PROVISIONAL"
	TrustVerified    TrustTier = "VERIFIED"
	TrustFederated   TrustTier = "FEDERATED"
)

type ForeignPrincipal struct {
	PrincipalID        domain.ID         `json:"principal_id"`
	OriginCollectiveID domain.ID         `json:"origin_collective_id"`
	RepresentedOrg     string            `json:"represented_org"`
	PublicKey          ed25519.PublicKey `json:"public_key"`
	TrustTier          TrustTier         `json:"trust_tier"`
	RegisteredAt       time.Time         `json:"registered_at"`
	Metadata           map[string]string `json:"metadata,omitempty"`
}

type CapabilityCeiling struct {
	AllowedCapabilities []string `json:"allowed_capabilities"`
	MaxExposureAmount   int64    `json:"max_exposure_amount"`
	MaxResourceCredits  int64    `json:"max_resource_credits"`
	RateLimitPerMinute  int      `json:"rate_limit_per_minute,omitempty"`
}

// AllowsCapability returns true if cap is covered by the allowed capabilities.
func (c CapabilityCeiling) AllowsCapability(cap string) bool {
	cap = strings.TrimSpace(cap)
	for _, allowed := range c.AllowedCapabilities {
		allowed = strings.TrimSpace(allowed)
		if allowed == "*" || allowed == cap {
			return true
		}
		// Prefix matching for scoped capabilities, e.g. "github.issue.*"
		if strings.HasSuffix(allowed, ".*") {
			prefix := strings.TrimSuffix(allowed, ".*")
			if strings.HasPrefix(cap, prefix+".") {
				return true
			}
		}
	}
	return false
}

// Subsumes returns true if child ceiling is strictly equal to or attenuated from parent ceiling.
func (parent CapabilityCeiling) Subsumes(child CapabilityCeiling) bool {
	if child.MaxExposureAmount > parent.MaxExposureAmount {
		return false
	}
	if child.MaxResourceCredits > parent.MaxResourceCredits {
		return false
	}
	if parent.RateLimitPerMinute > 0 && child.RateLimitPerMinute > parent.RateLimitPerMinute {
		return false
	}
	for _, childCap := range child.AllowedCapabilities {
		if !parent.AllowsCapability(childCap) {
			return false
		}
	}
	return true
}

type DelegationRecord struct {
	DelegationID       domain.ID         `json:"delegation_id"`
	ParentDelegationID domain.ID         `json:"parent_delegation_id,omitempty"`
	IssuerPrincipalID  domain.ID         `json:"issuer_principal_id"`
	IssuerCollectiveID domain.ID         `json:"issuer_collective_id"`
	SubjectPrincipalID domain.ID         `json:"subject_principal_id"`
	RepresentedOrg     string            `json:"represented_org"`
	Scope              CapabilityCeiling `json:"scope"`
	ValidFrom          time.Time         `json:"valid_from"`
	ValidUntil         time.Time         `json:"valid_until"`
	Revoked            bool              `json:"revoked"`
	RevokedAt          *time.Time        `json:"revoked_at,omitempty"`
	RevocationReason   string            `json:"revocation_reason,omitempty"`
	Signature          []byte            `json:"signature,omitempty"`
	CreatedAt          time.Time         `json:"created_at"`
}

// CanonicalBytes returns deterministic canonical byte payload for signing and signature verification.
func (d *DelegationRecord) CanonicalBytes() []byte {
	caps := make([]string, len(d.Scope.AllowedCapabilities))
	copy(caps, d.Scope.AllowedCapabilities)
	sort.Strings(caps)

	type canonicalPayload struct {
		DomainTag          string   `json:"_domain"`
		DelegationID       string   `json:"delegation_id"`
		ParentDelegationID string   `json:"parent_delegation_id,omitempty"`
		IssuerPrincipalID  string   `json:"issuer_principal_id"`
		IssuerCollectiveID string   `json:"issuer_collective_id"`
		SubjectPrincipalID string   `json:"subject_principal_id"`
		RepresentedOrg     string   `json:"represented_org"`
		AllowedCaps        []string `json:"allowed_caps"`
		MaxExposure        int64    `json:"max_exposure"`
		MaxCredits         int64    `json:"max_credits"`
		RateLimitPerMinute int      `json:"rate_limit_per_minute,omitempty"`
		ValidFromRFC3339   string   `json:"valid_from"`
		ValidUntilRFC3339  string   `json:"valid_until"`
		CreatedAtRFC3339   string   `json:"created_at"`
	}

	payload := canonicalPayload{
		DomainTag:          "summa42:delegation:v1",
		DelegationID:       string(d.DelegationID),
		ParentDelegationID: string(d.ParentDelegationID),
		IssuerPrincipalID:  string(d.IssuerPrincipalID),
		IssuerCollectiveID: string(d.IssuerCollectiveID),
		SubjectPrincipalID: string(d.SubjectPrincipalID),
		RepresentedOrg:     d.RepresentedOrg,
		AllowedCaps:        caps,
		MaxExposure:        d.Scope.MaxExposureAmount,
		MaxCredits:         d.Scope.MaxResourceCredits,
		RateLimitPerMinute: d.Scope.RateLimitPerMinute,
		ValidFromRFC3339:   d.ValidFrom.UTC().Format(time.RFC3339Nano),
		ValidUntilRFC3339:  d.ValidUntil.UTC().Format(time.RFC3339Nano),
		CreatedAtRFC3339:   d.CreatedAt.UTC().Format(time.RFC3339Nano),
	}

	raw, _ := json.Marshal(payload)
	return raw
}

// Sign signs the canonical delegation payload with issuer's private key.
func (d *DelegationRecord) Sign(priv ed25519.PrivateKey) error {
	if len(priv) != ed25519.PrivateKeySize {
		return errors.New("invalid private key size")
	}
	d.Signature = ed25519.Sign(priv, d.CanonicalBytes())
	return nil
}

// VerifySignature cryptographically verifies the delegation against the issuer's public key.
func (d *DelegationRecord) VerifySignature(pub ed25519.PublicKey) bool {
	if len(pub) != ed25519.PublicKeySize || len(d.Signature) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, d.CanonicalBytes(), d.Signature)
}

// Digest returns the SHA-256 hash of the signed delegation.
func (d *DelegationRecord) Digest() [32]byte {
	var buf bytes.Buffer
	buf.Write(d.CanonicalBytes())
	buf.Write(d.Signature)
	return sha256.Sum256(buf.Bytes())
}

// DelegationContext represents an isolated, verified delegation scope without ambient authority.
type DelegationContext struct {
	SubjectPrincipalID domain.ID
	IssuerPrincipalID  domain.ID
	RepresentedOrg     string
	EffectiveScope     CapabilityCeiling
	ValidUntil         time.Time
	ChainLength        int
}
