package epistemic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

// ReflectionInput encapsulates the structured reflection signals emitted by a Summand after an Attempt.
type ReflectionInput struct {
	AttemptID             domain.ID `json:"attempt_id"`
	SummandID             string    `json:"summand_id"`
	Expected              string    `json:"expected"`
	Observed              string    `json:"observed"`
	Surprise              string    `json:"surprise,omitempty"`
	PlanDeviation         string    `json:"plan_deviation,omitempty"`
	Adaptation            string    `json:"adaptation,omitempty"`
	Outcome               string    `json:"outcome"`
	CandidateLesson       string    `json:"candidate_lesson,omitempty"`
	SupportingEvidenceIDs []string  `json:"supporting_evidence_ids,omitempty"`
	Timestamp             time.Time `json:"timestamp,omitempty"`
}

// PostAttemptReflector evaluates post-attempt reflections in a read-only context and produces candidate KnowledgeDeltas.
type PostAttemptReflector struct{}

// NewPostAttemptReflector creates a new PostAttemptReflector instance.
func NewPostAttemptReflector() *PostAttemptReflector {
	return &PostAttemptReflector{}
}

// Reflect processes the post-attempt reflection. It establishes NO_MATERIAL_LEARNING as the clean default.
func (r *PostAttemptReflector) Reflect(ctx context.Context, input ReflectionInput) (KnowledgeDelta, error) {
	if err := ctx.Err(); err != nil {
		return KnowledgeDelta{}, err
	}

	ts := input.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	} else {
		ts = ts.UTC()
	}

	// Graceful handling: empty/malformed inputs default to clean NO_MATERIAL_LEARNING
	if strings.TrimSpace(string(input.AttemptID)) == "" || strings.TrimSpace(input.SummandID) == "" {
		return NewNoMaterialLearningDelta(input.AttemptID, ts), nil
	}

	surprise := strings.TrimSpace(input.Surprise)
	deviation := strings.TrimSpace(input.PlanDeviation)
	adaptation := strings.TrimSpace(input.Adaptation)
	lesson := strings.TrimSpace(input.CandidateLesson)

	// If nominal execution (no surprise, no deviation, no adaptation, and no candidate lesson)
	if surprise == "" && deviation == "" && adaptation == "" && lesson == "" {
		return NewNoMaterialLearningDelta(input.AttemptID, ts), nil
	}

	src := SourceRef{
		ID:                fmt.Sprintf("src-reflection-%s", input.AttemptID),
		Kind:              SourceKindAttempt,
		URI:               fmt.Sprintf("attempt://%s", input.AttemptID),
		AuthorID:          input.SummandID,
		CapturedAt:        ts,
		IndependenceGroup: input.SummandID,
	}

	var observations []Observation
	obsVal := fmt.Sprintf("Observed: %s. Surprise: %s. Adaptation: %s.", input.Observed, surprise, adaptation)
	obs := NewObservation(
		src,
		"attempt_execution",
		"post_attempt_finding",
		obsVal,
		input.SupportingEvidenceIDs,
		ts,
	)
	// Bind observation ID deterministically to attempt and content hash for reproducible deltas
	obs.ID = domain.ID(fmt.Sprintf("obs_%s_%s", input.AttemptID, obs.ContentHash[:16]))
	observations = append(observations, obs)

	var proposedClaims []Claim
	if lesson != "" {
		claim := NewClaim(
			"process_learning",
			"reusable_lesson",
			lesson,
			0.80, // initial proposed confidence
			ts,
			[]domain.ID{obs.ID},
			input.SupportingEvidenceIDs,
			[]SourceRef{src},
		)
		claim.Status = ClaimStatusProposed
		claim.ID = domain.ID(fmt.Sprintf("claim_%s_%s", input.AttemptID, claim.ContentHash[:16]))
		proposedClaims = append(proposedClaims, claim)
	}

	// Compute deterministic delta hash
	h := sha256.New()
	fmt.Fprintf(h, "attempt:%s\n", input.AttemptID)
	for _, o := range observations {
		fmt.Fprintf(h, "obs:%s\n", o.ContentHash)
	}
	for _, c := range proposedClaims {
		fmt.Fprintf(h, "claim:%s\n", c.ContentHash)
	}
	deltaHash := hex.EncodeToString(h.Sum(nil))

	return KnowledgeDelta{
		ID:                   domain.NewID("kdelta"),
		SourceAttemptID:      input.AttemptID,
		Observations:         observations,
		ProposedClaims:       proposedClaims,
		IsNoMaterialLearning: false,
		DeltaHash:            deltaHash,
		CreatedAt:            ts,
	}, nil
}
