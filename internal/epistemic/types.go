package epistemic

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

// SourceKind categorizes the epistemic origin of observations and sources.
type SourceKind string

const (
	SourceKindAttempt   SourceKind = "ATTEMPT"
	SourceKindTelemetry SourceKind = "TELEMETRY"
	SourceKindAudit     SourceKind = "AUDIT"
	SourceKindHuman     SourceKind = "HUMAN"
	SourceKindDTU       SourceKind = "DTU"
)

// SourceRef encapsulates provenance details and source independence grouping.
type SourceRef struct {
	ID                string     `json:"id"`
	Kind              SourceKind `json:"kind"`
	URI               string     `json:"uri"`
	AuthorID          string     `json:"author_id"`
	CapturedAt        time.Time  `json:"captured_at"`
	IndependenceGroup string     `json:"independence_group"`
}

// Observation represents an empirical atomic finding recorded by an agent or system.
type Observation struct {
	ID          domain.ID `json:"id"`
	ContentHash string    `json:"content_hash"`
	Source      SourceRef `json:"source"`
	Subject     string    `json:"subject"`
	Predicate   string    `json:"predicate"`
	Value       string    `json:"value"`
	EvidenceIDs []string  `json:"evidence_ids"`
	ObservedAt  time.Time `json:"observed_at"`
	Sealed      bool      `json:"sealed"`
}

// NewObservation constructs a sealed Observation with deterministic cryptographic hashing.
func NewObservation(
	source SourceRef,
	subject, predicate, value string,
	evidenceIDs []string,
	observedAt time.Time,
) Observation {
	h := sha256.New()
	fmt.Fprintf(h, "source:%s:%s:%s\n", source.Kind, source.URI, source.IndependenceGroup)
	fmt.Fprintf(h, "subject:%s\npredicate:%s\nvalue:%s\n", subject, predicate, value)
	cleanEvidence := cloneStringSlice(evidenceIDs)
	sort.Strings(cleanEvidence)
	for _, ev := range cleanEvidence {
		fmt.Fprintf(h, "evidence:%s\n", ev)
	}

	return Observation{
		ID:          domain.NewID("obs"),
		ContentHash: hex.EncodeToString(h.Sum(nil)),
		Source:      source,
		Subject:     subject,
		Predicate:   predicate,
		Value:       value,
		EvidenceIDs: cleanEvidence,
		ObservedAt:  observedAt.UTC(),
		Sealed:      true,
	}
}

// ClaimStatus categorizes the lifecycle state of an asserted claim.
type ClaimStatus string

const (
	ClaimStatusProposed     ClaimStatus = "PROPOSED"
	ClaimStatusActive       ClaimStatus = "ACTIVE"
	ClaimStatusSuperseded   ClaimStatus = "SUPERSEDED"
	ClaimStatusContradicted ClaimStatus = "CONTRADICTED"
	ClaimStatusDisputed     ClaimStatus = "DISPUTED"
)

// Claim represents a structured assertion supported by observations and evidence.
type Claim struct {
	ID                       domain.ID   `json:"id"`
	ContentHash              string      `json:"content_hash"`
	Subject                  string      `json:"subject"`
	Predicate                string      `json:"predicate"`
	Object                   string      `json:"object"`
	Confidence               float64     `json:"confidence"`
	Status                   ClaimStatus `json:"status"`
	ValidFrom                time.Time   `json:"valid_from"`
	ValidTo                  *time.Time  `json:"valid_to,omitempty"`
	FirstObservedAt          time.Time   `json:"first_observed_at"`
	LastConfirmedAt          time.Time   `json:"last_confirmed_at"`
	SupportingObservations   []domain.ID `json:"supporting_observations"`
	EvidenceIDs              []string    `json:"evidence_ids"`
	Sources                  []SourceRef `json:"sources"`
	IndependentSourceCount   int         `json:"independent_source_count"`
	SupersededBy             *domain.ID  `json:"superseded_by,omitempty"`
	ContradictingClaimIDs    []domain.ID `json:"contradicting_claim_ids,omitempty"`
	ContradictingEvidenceIDs []string    `json:"contradicting_evidence_ids,omitempty"`
	CreatedAt                time.Time   `json:"created_at"`
	Sealed                   bool        `json:"sealed"`
}

// NewClaim constructs a sealed Claim with cryptographic content hash and independent source count.
func NewClaim(
	subject, predicate, object string,
	confidence float64,
	validFrom time.Time,
	supportingObs []domain.ID,
	evidenceIDs []string,
	sources []SourceRef,
) Claim {
	h := sha256.New()
	fmt.Fprintf(h, "claim:%s:%s:%s\n", subject, predicate, object)
	cleanObs := cloneIDSlice(supportingObs)
	for _, o := range cleanObs {
		fmt.Fprintf(h, "obs:%s\n", o)
	}
	cleanEv := cloneStringSlice(evidenceIDs)
	sort.Strings(cleanEv)
	for _, ev := range cleanEv {
		fmt.Fprintf(h, "ev:%s\n", ev)
	}

	// Calculate independent sources
	groups := make(map[string]bool)
	for _, s := range sources {
		grp := strings.TrimSpace(s.IndependenceGroup)
		if grp == "" {
			grp = s.ID
		}
		groups[grp] = true
	}

	now := time.Now().UTC()
	vFrom := validFrom.UTC()

	return Claim{
		ID:                     domain.NewID("claim"),
		ContentHash:            hex.EncodeToString(h.Sum(nil)),
		Subject:                subject,
		Predicate:              predicate,
		Object:                 object,
		Confidence:             confidence,
		Status:                 ClaimStatusActive,
		ValidFrom:              vFrom,
		FirstObservedAt:        vFrom,
		LastConfirmedAt:        vFrom,
		SupportingObservations: cleanObs,
		EvidenceIDs:            cleanEv,
		Sources:                sources,
		IndependentSourceCount: len(groups),
		CreatedAt:              now,
		Sealed:                 true,
	}
}

// ContradictionRecord captures the audit linkage between mutually conflicting claims.
type ContradictionRecord struct {
	ExistingClaimID         domain.ID `json:"existing_claim_id"`
	OpposingClaimID         domain.ID `json:"opposing_claim_id"`
	ContradictingEvidenceID string    `json:"contradicting_evidence_id"`
	Reason                  string    `json:"reason"`
	RecordedAt              time.Time `json:"recorded_at"`
}

// SupersessionRecord captures the non-destructive temporal replacement of a prior claim.
type SupersessionRecord struct {
	PriorClaimID       domain.ID `json:"prior_claim_id"`
	SupersedingClaimID domain.ID `json:"superseding_claim_id"`
	Reason             string    `json:"reason"`
	EffectiveFrom      time.Time `json:"effective_from"`
}

// KnowledgeDelta represents the delta bundle of epistemic state produced by an attempt.
type KnowledgeDelta struct {
	ID                     domain.ID             `json:"id"`
	SourceAttemptID        domain.ID             `json:"source_attempt_id"`
	Observations           []Observation         `json:"observations,omitempty"`
	ProposedClaims         []Claim               `json:"proposed_claims,omitempty"`
	ContradictionsRecorded []ContradictionRecord `json:"contradictions_recorded,omitempty"`
	SupersessionsRecorded  []SupersessionRecord  `json:"supersessions_recorded,omitempty"`
	IsNoMaterialLearning   bool                  `json:"is_no_material_learning"`
	DeltaHash              string                `json:"delta_hash"`
	CreatedAt              time.Time             `json:"created_at"`
}

// NewNoMaterialLearningDelta constructs a clean delta signifying no novel learning occurred.
func NewNoMaterialLearningDelta(sourceAttemptID domain.ID, at time.Time) KnowledgeDelta {
	now := at.UTC()
	return KnowledgeDelta{
		ID:                   domain.NewID("kdelta"),
		SourceAttemptID:      sourceAttemptID,
		IsNoMaterialLearning: true,
		DeltaHash:            fmt.Sprintf("no_material_learning:%s", sourceAttemptID),
		CreatedAt:            now,
	}
}

func cloneStringSlice(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

func cloneIDSlice(s []domain.ID) []domain.ID {
	if s == nil {
		return nil
	}
	out := make([]domain.ID, len(s))
	copy(out, s)
	return out
}
