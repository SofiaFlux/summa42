package epistemic

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

var (
	// ErrClaimNotFound is returned when a requested claim cannot be located.
	ErrClaimNotFound = errors.New("claim not found")

	// ErrObservationNotFound is returned when a requested observation is missing.
	ErrObservationNotFound = errors.New("observation not found")
)

// LineageResult answers "Why do we believe Claim X?" with complete empirical lineage.
type LineageResult struct {
	Claim                  Claim         `json:"claim"`
	ValidityState          string        `json:"validity_state"`
	SupportingEvidence     []string      `json:"supporting_evidence"`
	SupportingObservations []Observation `json:"supporting_observations"`
	SupportingSources      []SourceRef   `json:"supporting_sources"`
	IndependentSourceCount int           `json:"independent_source_count"`
	ContradictingEvidence  []string      `json:"contradicting_evidence,omitempty"`
	ContradictingClaims    []Claim       `json:"contradicting_claims,omitempty"`
	ConfidenceExplanation  string        `json:"confidence_explanation"`
}

// Engine manages immutable epistemic observations, claims, temporal validity, and lineage queries.
type Engine struct {
	mu           sync.RWMutex
	observations map[domain.ID]Observation
	claims       map[domain.ID]Claim
	deltas       map[domain.ID]KnowledgeDelta
}

// NewEngine initializes an in-memory epistemic engine.
func NewEngine() *Engine {
	return &Engine{
		observations: make(map[domain.ID]Observation),
		claims:       make(map[domain.ID]Claim),
		deltas:       make(map[domain.ID]KnowledgeDelta),
	}
}

// RecordObservation stores an empirical observation.
func (e *Engine) RecordObservation(ctx context.Context, obs Observation) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.observations[obs.ID] = obs
	return nil
}

// RecordClaim stores a structured epistemic claim.
func (e *Engine) RecordClaim(ctx context.Context, claim Claim) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.claims[claim.ID] = claim
	return nil
}

// RecordDelta integrates a complete knowledge delta bundle.
func (e *Engine) RecordDelta(ctx context.Context, delta KnowledgeDelta) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.deltas[delta.ID] = delta
	if delta.IsNoMaterialLearning {
		return nil
	}

	for _, obs := range delta.Observations {
		e.observations[obs.ID] = obs
	}
	for _, claim := range delta.ProposedClaims {
		e.claims[claim.ID] = claim
	}
	return nil
}

// GetClaim retrieves a claim by its unique ID.
func (e *Engine) GetClaim(ctx context.Context, id domain.ID) (Claim, error) {
	if err := ctx.Err(); err != nil {
		return Claim{}, err
	}

	e.mu.RLock()
	defer e.mu.RUnlock()

	c, ok := e.claims[id]
	if !ok {
		return Claim{}, ErrClaimNotFound
	}
	return c, nil
}

// SupersedeClaim executes non-destructive temporal supersession.
func (e *Engine) SupersedeClaim(
	ctx context.Context,
	priorID domain.ID,
	supersedingID domain.ID,
	reason string,
	effectiveFrom time.Time,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	prior, ok := e.claims[priorID]
	if !ok {
		return fmt.Errorf("prior %w: %s", ErrClaimNotFound, priorID)
	}
	if _, ok := e.claims[supersedingID]; !ok {
		return fmt.Errorf("superseding %w: %s", ErrClaimNotFound, supersedingID)
	}

	eff := effectiveFrom.UTC()
	prior.Status = ClaimStatusSuperseded
	prior.SupersededBy = &supersedingID
	prior.ValidTo = &eff

	e.claims[priorID] = prior
	return nil
}

// RecordContradiction records non-destructive mutual contradiction between competing claims.
func (e *Engine) RecordContradiction(
	ctx context.Context,
	existingID domain.ID,
	opposingID domain.ID,
	evidenceID string,
	reason string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	existing, ok := e.claims[existingID]
	if !ok {
		return fmt.Errorf("existing %w: %s", ErrClaimNotFound, existingID)
	}
	opposing, ok := e.claims[opposingID]
	if !ok {
		return fmt.Errorf("opposing %w: %s", ErrClaimNotFound, opposingID)
	}

	existing.Status = ClaimStatusDisputed
	existing.ContradictingClaimIDs = appendUnique(existing.ContradictingClaimIDs, opposingID)
	if evidenceID != "" {
		existing.ContradictingEvidenceIDs = appendUniqueString(existing.ContradictingEvidenceIDs, evidenceID)
	}

	opposing.Status = ClaimStatusDisputed
	opposing.ContradictingClaimIDs = appendUnique(opposing.ContradictingClaimIDs, existingID)
	if evidenceID != "" {
		opposing.ContradictingEvidenceIDs = appendUniqueString(opposing.ContradictingEvidenceIDs, evidenceID)
	}

	e.claims[existingID] = existing
	e.claims[opposingID] = opposing
	return nil
}

// ListClaimsBySubject returns claims for a subject, optionally filtered by historical timestamp.
func (e *Engine) ListClaimsBySubject(ctx context.Context, subject string, asOf *time.Time) ([]Claim, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	e.mu.RLock()
	defer e.mu.RUnlock()

	var results []Claim
	for _, c := range e.claims {
		if c.Subject != subject {
			continue
		}
		if asOf != nil {
			target := asOf.UTC()
			if c.ValidFrom.After(target) {
				continue
			}
			if c.ValidTo != nil && !c.ValidTo.After(target) {
				continue
			}
		}
		results = append(results, c)
	}
	return results, nil
}

// WhyDoWeBelieve traverses empirical evidence, observations, and source independence to explain belief lineage.
func (e *Engine) WhyDoWeBelieve(ctx context.Context, claimID domain.ID) (LineageResult, error) {
	if err := ctx.Err(); err != nil {
		return LineageResult{}, err
	}

	e.mu.RLock()
	defer e.mu.RUnlock()

	claim, ok := e.claims[claimID]
	if !ok {
		return LineageResult{}, ErrClaimNotFound
	}

	var observations []Observation
	for _, obsID := range claim.SupportingObservations {
		if obs, found := e.observations[obsID]; found {
			observations = append(observations, obs)
		}
	}

	var contradictingClaims []Claim
	for _, cID := range claim.ContradictingClaimIDs {
		if c, found := e.claims[cID]; found {
			contradictingClaims = append(contradictingClaims, c)
		}
	}

	validityState := "VALID"
	if claim.Status == ClaimStatusSuperseded {
		validityState = "SUPERSEDED"
	} else if claim.Status == ClaimStatusDisputed || len(contradictingClaims) > 0 {
		validityState = "DISPUTED"
	}

	confidenceExplanation := fmt.Sprintf(
		"Claim supported by %d empirical observation(s) across %d independent sources (confidence: %.2f). Contradicting claims: %d.",
		len(observations),
		claim.IndependentSourceCount,
		claim.Confidence,
		len(contradictingClaims),
	)

	return LineageResult{
		Claim:                  claim,
		ValidityState:          validityState,
		SupportingEvidence:     cloneStringSlice(claim.EvidenceIDs),
		SupportingObservations: observations,
		SupportingSources:      claim.Sources,
		IndependentSourceCount: claim.IndependentSourceCount,
		ContradictingEvidence:  cloneStringSlice(claim.ContradictingEvidenceIDs),
		ContradictingClaims:    contradictingClaims,
		ConfidenceExplanation:  confidenceExplanation,
	}, nil
}

func appendUnique(slice []domain.ID, item domain.ID) []domain.ID {
	for _, s := range slice {
		if s == item {
			return slice
		}
	}
	return append(slice, item)
}

func appendUniqueString(slice []string, item string) []string {
	for _, s := range slice {
		if s == item {
			return slice
		}
	}
	return append(slice, item)
}
