package evidence

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

// TrustTier categorizes the trust relationship with an external agent or collective.
type TrustTier string

const (
	TrustTierUntrusted                TrustTier = "UNTRUSTED"
	TrustTierProvisional              TrustTier = "PROVISIONAL"
	TrustTierVerifiedPartner          TrustTier = "VERIFIED_PARTNER"
	TrustTierConstitutionalCollective TrustTier = "CONSTITUTIONAL_COLLECTIVE"
)

// Rank returns a numeric rank for comparing trust levels.
func (t TrustTier) Rank() int {
	switch t {
	case TrustTierConstitutionalCollective:
		return 4
	case TrustTierVerifiedPartner:
		return 3
	case TrustTierProvisional:
		return 2
	case TrustTierUntrusted:
		return 1
	default:
		return 0
	}
}

// CorroborationLevel indicates the degree of independent corroboration for external evidence.
type CorroborationLevel string

const (
	CorroborationNone               CorroborationLevel = "NONE"
	CorroborationSelfAttested       CorroborationLevel = "SELF_ATTESTED"
	CorroborationIndependentWitness CorroborationLevel = "INDEPENDENT_WITNESS"
	CorroborationMultiPartyAttested CorroborationLevel = "MULTI_PARTY_ATTESTED"
)

// Rank returns a numeric rank for comparing corroboration levels.
func (c CorroborationLevel) Rank() int {
	switch c {
	case CorroborationMultiPartyAttested:
		return 4
	case CorroborationIndependentWitness:
		return 3
	case CorroborationSelfAttested:
		return 2
	case CorroborationNone:
		return 1
	default:
		return 0
	}
}

var (
	ErrInvalidEvidenceSignature = errors.New("invalid foreign evidence signature")
	ErrInsufficientTrustTier    = errors.New("insufficient trust tier for foreign evidence")
	ErrUncorroboratedEvidence   = errors.New("foreign evidence lacks required corroboration")
)

// ForeignEvidenceRecord tracks provenance, delegation, cryptographic signatures,
// and corroboration level for an incoming foreign artifact.
type ForeignEvidenceRecord struct {
	EvidenceID          domain.ID          `json:"evidence_id"`
	ContentHash         string             `json:"content_hash"`
	OriginCollectiveID  domain.ID          `json:"origin_collective_id"`
	ProducerPrincipalID domain.ID          `json:"producer_principal_id"`
	DelegationID        domain.ID          `json:"delegation_id,omitempty"`
	Signature           []byte             `json:"signature"`
	TrustTier           TrustTier          `json:"trust_tier"`
	CorroborationLevel  CorroborationLevel `json:"corroboration_level"`
	Witnesses           []domain.ID        `json:"witnesses,omitempty"`
	UpstreamLineage     []string           `json:"upstream_lineage,omitempty"`
	CreatedAt           time.Time          `json:"created_at"`
	VerifiedAt          *time.Time         `json:"verified_at,omitempty"`
}

// CanonicalBytes returns the deterministic serialization for signing.
func (r *ForeignEvidenceRecord) CanonicalBytes() []byte {
	witnesses := make([]string, 0, len(r.Witnesses))
	for _, w := range r.Witnesses {
		witnesses = append(witnesses, string(w))
	}
	sort.Strings(witnesses)

	upstream := make([]string, len(r.UpstreamLineage))
	copy(upstream, r.UpstreamLineage)
	sort.Strings(upstream)

	type canonicalPayload struct {
		DomainTag           string   `json:"_domain"`
		EvidenceID          string   `json:"evidence_id"`
		ContentHash         string   `json:"content_hash"`
		OriginCollectiveID  string   `json:"origin_collective_id"`
		ProducerPrincipalID string   `json:"producer_principal_id"`
		DelegationID        string   `json:"delegation_id,omitempty"`
		TrustTier           string   `json:"trust_tier"`
		CorroborationLevel  string   `json:"corroboration_level"`
		Witnesses           []string `json:"witnesses"`
		UpstreamLineage     []string `json:"upstream_lineage"`
		CreatedAtRFC3339    string   `json:"created_at"`
	}

	payload := canonicalPayload{
		DomainTag:           "summa42:evidence:v1",
		EvidenceID:          string(r.EvidenceID),
		ContentHash:         r.ContentHash,
		OriginCollectiveID:  string(r.OriginCollectiveID),
		ProducerPrincipalID: string(r.ProducerPrincipalID),
		DelegationID:        string(r.DelegationID),
		TrustTier:           string(r.TrustTier),
		CorroborationLevel:  string(r.CorroborationLevel),
		Witnesses:           witnesses,
		UpstreamLineage:     upstream,
		CreatedAtRFC3339:    r.CreatedAt.UTC().Format(time.RFC3339Nano),
	}

	raw, _ := json.Marshal(payload)
	return raw
}

// Sign signs the canonical bytes with the producer's private key.
func (r *ForeignEvidenceRecord) Sign(privKey ed25519.PrivateKey) error {
	if len(privKey) == 0 {
		return errors.New("private key is empty")
	}
	canonical := r.CanonicalBytes()
	r.Signature = ed25519.Sign(privKey, canonical)
	return nil
}

// VerifySignature verifies the signature against the producer's public key.
func (r *ForeignEvidenceRecord) VerifySignature(pubKey ed25519.PublicKey) error {
	if len(pubKey) == 0 {
		return errors.New("public key is empty")
	}
	if len(r.Signature) == 0 {
		return ErrInvalidEvidenceSignature
	}
	canonical := r.CanonicalBytes()
	if !ed25519.Verify(pubKey, canonical, r.Signature) {
		return ErrInvalidEvidenceSignature
	}
	return nil
}

// MeetsRequirements verifies that the evidence satisfies minimum trust tier and corroboration level,
// and enforces that independent witnesses are distinct from the producer.
func (r *ForeignEvidenceRecord) MeetsRequirements(minTier TrustTier, minLevel CorroborationLevel, requireIndependentWitness bool) error {
	if r.TrustTier.Rank() < minTier.Rank() {
		return fmt.Errorf("%w: evidence has %s, requires %s", ErrInsufficientTrustTier, r.TrustTier, minTier)
	}
	if r.CorroborationLevel.Rank() < minLevel.Rank() {
		return fmt.Errorf("%w: evidence has %s, requires %s", ErrUncorroboratedEvidence, r.CorroborationLevel, minLevel)
	}
	if requireIndependentWitness && r.CorroborationLevel.Rank() < CorroborationIndependentWitness.Rank() {
		return fmt.Errorf("%w: independent witness required, got %s", ErrUncorroboratedEvidence, r.CorroborationLevel)
	}

	// Validate witness independence
	if r.CorroborationLevel == CorroborationIndependentWitness || r.CorroborationLevel == CorroborationMultiPartyAttested {
		if len(r.Witnesses) == 0 {
			return fmt.Errorf("%w: %s requires at least one witness", ErrUncorroboratedEvidence, r.CorroborationLevel)
		}
		if r.CorroborationLevel == CorroborationMultiPartyAttested && len(r.Witnesses) < 2 {
			return fmt.Errorf("%w: multi-party attestation requires at least 2 distinct witnesses", ErrUncorroboratedEvidence)
		}
		seen := make(map[domain.ID]struct{}, len(r.Witnesses))
		for _, w := range r.Witnesses {
			if w == "" {
				return fmt.Errorf("%w: empty witness identifier", ErrUncorroboratedEvidence)
			}
			if w == r.ProducerPrincipalID {
				return fmt.Errorf("%w: witness %s cannot be the producer (self-corroboration forbidden)", ErrUncorroboratedEvidence, w)
			}
			if _, exists := seen[w]; exists {
				return fmt.Errorf("%w: duplicate witness %s", ErrUncorroboratedEvidence, w)
			}
			seen[w] = struct{}{}
		}
	}

	return nil
}
