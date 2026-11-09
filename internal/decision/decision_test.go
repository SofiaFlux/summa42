package decision_test

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/decision"
	"github.com/SofiaFlux/summa42/internal/domain"
)

// 1. Acceptance Criteria 1: JSON Serialization and Deserialization of DecisionRequest & DecisionResult
func TestDecisionTypes_JSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	req := decision.DecisionRequest{
		DecisionID:   domain.ID("dec_req_1"),
		DecisionType: decision.DecisionTypeAcceptanceTriage,
		SubjectRef: decision.SubjectRef{
			Kind: "TASK",
			ID:   domain.ID("task_123"),
		},
		AllowedOutcomes: []string{"ACCEPT", "RETRY", "ESCALATE"},
		ContextProjection: map[string]any{
			"attempt_count": float64(2),
			"error_rate":    0.05,
		},
		RiskProfile:   decision.RiskProfileMedium,
		CostBudgetMax: 500, // micro-cents
		LatencyBudget: 250 * time.Millisecond,
		Eligibility: decision.ProviderEligibility{
			AllowProbabilistic:          true,
			AllowRemoteLLM:              true,
			RequireCalibratedConfidence: false,
		},
		CreatedAt: now,
	}

	rawReq, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal DecisionRequest: %v", err)
	}

	var reqUnmarshaled decision.DecisionRequest
	if err := json.Unmarshal(rawReq, &reqUnmarshaled); err != nil {
		t.Fatalf("unmarshal DecisionRequest: %v", err)
	}

	if reqUnmarshaled.DecisionID != req.DecisionID || len(reqUnmarshaled.AllowedOutcomes) != 3 {
		t.Fatalf("round-trip mismatch: got %+v", reqUnmarshaled)
	}

	res := decision.DecisionResult{
		DecisionID:      req.DecisionID,
		SelectedOutcome: "ACCEPT",
		OutcomeScores: map[string]float64{
			"ACCEPT":   0.92,
			"RETRY":    0.05,
			"ESCALATE": 0.03,
		},
		Confidence:     0.92,
		ConfidenceKind: decision.ConfidenceCalibratedProbability,
		Abstained:      false,
		Provider: decision.ProviderInfo{
			Kind:    decision.ProviderKindSpecialistModel,
			Name:    "triage-classifier-v2",
			Version: "2.1.0",
		},
		Latency: 45 * time.Millisecond,
		Usage: decision.DecisionUsage{
			TokensIn:       0,
			TokensOut:      0,
			CostMicroCents: 5,
		},
		ProjectionHash: "a1b2c3d4e5f6",
		DecidedAt:      now.Add(45 * time.Millisecond),
	}

	rawRes, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal DecisionResult: %v", err)
	}

	var resUnmarshaled decision.DecisionResult
	if err := json.Unmarshal(rawRes, &resUnmarshaled); err != nil {
		t.Fatalf("unmarshal DecisionResult: %v", err)
	}

	if resUnmarshaled.SelectedOutcome != "ACCEPT" || resUnmarshaled.ConfidenceKind != decision.ConfidenceCalibratedProbability {
		t.Fatalf("round-trip mismatch: got %+v", resUnmarshaled)
	}
}

// 2. Acceptance Criteria 2 & 3: Semantic Envelope Enforcement - Unauthorized Outcomes
func TestDecisionValidation_EnforcesAllowedOutcomes(t *testing.T) {
	req := decision.DecisionRequest{
		DecisionID:      domain.ID("dec_req_envelope"),
		DecisionType:    decision.DecisionTypeRouting,
		AllowedOutcomes: []string{"FAST_WORKER", "STRONG_WORKER"},
		ContextProjection: map[string]any{
			"complexity": "low",
		},
		CreatedAt: time.Now().UTC(),
	}

	// Valid result within envelope
	validRes := decision.DecisionResult{
		DecisionID:      req.DecisionID,
		SelectedOutcome: "FAST_WORKER",
		Confidence:      0.85,
		ConfidenceKind:  decision.ConfidenceSelfReported,
		DecidedAt:       time.Now().UTC(),
	}
	if err := decision.ValidateEnvelope(req, validRes); err != nil {
		t.Fatalf("expected valid result to pass envelope validation, got: %v", err)
	}

	// Invalid result with unapproved outcome invented by provider
	unauthorizedRes := decision.DecisionResult{
		DecisionID:      req.DecisionID,
		SelectedOutcome: "GOD_MODE_EXECUTE", // NOT in AllowedOutcomes!
		Confidence:      0.99,
		ConfidenceKind:  decision.ConfidenceSelfReported,
		DecidedAt:       time.Now().UTC(),
	}
	err := decision.ValidateEnvelope(req, unauthorizedRes)
	if err == nil {
		t.Fatalf("expected validation error for unauthorized outcome, got nil")
	}

	// Invalid result with extra unapproved outcome score keys
	unauthorizedScoreRes := decision.DecisionResult{
		DecisionID:      req.DecisionID,
		SelectedOutcome: "FAST_WORKER",
		OutcomeScores: map[string]float64{
			"FAST_WORKER":    0.8,
			"UNKNOWN_ACTION": 0.2, // Unapproved option
		},
		Confidence:     0.8,
		ConfidenceKind: decision.ConfidenceSelfReported,
		DecidedAt:      time.Now().UTC(),
	}
	err = decision.ValidateEnvelope(req, unauthorizedScoreRes)
	if err == nil {
		t.Fatalf("expected validation error for unauthorized score key, got nil")
	}

	// Decision ID mismatch
	mismatchRes := validRes
	mismatchRes.DecisionID = "different_id"
	if err := decision.ValidateEnvelope(req, mismatchRes); err == nil {
		t.Fatalf("expected error on decision ID mismatch, got nil")
	}

	// Empty result ID when request ID is present
	emptyIDRes := validRes
	emptyIDRes.DecisionID = ""
	if err := decision.ValidateEnvelope(req, emptyIDRes); err == nil {
		t.Fatalf("expected error on empty result ID, got nil")
	}
}

// 3. Acceptance Criteria 2 & 3: Abstention Invariants
func TestDecisionValidation_AbstentionInvariants(t *testing.T) {
	req := decision.DecisionRequest{
		DecisionID:      domain.ID("dec_abstain"),
		DecisionType:    decision.DecisionTypeAcceptanceTriage,
		AllowedOutcomes: []string{"ACCEPT", "REJECT"},
		Eligibility: decision.ProviderEligibility{
			RequireCalibratedConfidence: true,
		},
		CreatedAt: time.Now().UTC(),
	}

	// Valid abstention with reason and calibrated/none confidence
	validAbstain := decision.DecisionResult{
		DecisionID:     req.DecisionID,
		Abstained:      true,
		AbstainReason:  "confidence below threshold for ambiguous result",
		Confidence:     0.0,
		ConfidenceKind: decision.ConfidenceNone,
		DecidedAt:      time.Now().UTC(),
	}
	if err := decision.ValidateEnvelope(req, validAbstain); err != nil {
		t.Fatalf("expected valid abstention to pass, got: %v", err)
	}

	// Invalid abstention without reason
	invalidAbstain := decision.DecisionResult{
		DecisionID:     req.DecisionID,
		Abstained:      true,
		AbstainReason:  "", // Missing reason
		ConfidenceKind: decision.ConfidenceNone,
		DecidedAt:      time.Now().UTC(),
	}
	if err := decision.ValidateEnvelope(req, invalidAbstain); err == nil {
		t.Fatalf("expected error for abstention missing reason, got nil")
	}

	// Contradictory result: abstained=true but selected non-empty outcome
	contradictoryAbstain := decision.DecisionResult{
		DecisionID:      req.DecisionID,
		Abstained:       true,
		AbstainReason:   "too uncertain",
		SelectedOutcome: "ACCEPT",
		ConfidenceKind:  decision.ConfidenceNone,
		DecidedAt:       time.Now().UTC(),
	}
	if err := decision.ValidateEnvelope(req, contradictoryAbstain); err == nil {
		t.Fatalf("expected error for contradictory abstention with outcome, got nil")
	}

	// Abstaining with uncalibrated confidence when calibrated is required
	uncalibratedAbstain := decision.DecisionResult{
		DecisionID:     req.DecisionID,
		Abstained:      true,
		AbstainReason:  "uncalibrated guess",
		Confidence:     0.4,
		ConfidenceKind: decision.ConfidenceSelfReported,
		DecidedAt:      time.Now().UTC(),
	}
	if err := decision.ValidateEnvelope(req, uncalibratedAbstain); err == nil {
		t.Fatalf("expected error when abstaining with uncalibrated confidence, got nil")
	}

	// Abstaining with unauthorized outcome score keys
	unauthorizedScoreAbstain := decision.DecisionResult{
		DecisionID:    req.DecisionID,
		Abstained:     true,
		AbstainReason: "unauthorized score key present",
		OutcomeScores: map[string]float64{
			"FORBIDDEN_KEY": 0.5,
		},
		ConfidenceKind: decision.ConfidenceNone,
		DecidedAt:      time.Now().UTC(),
	}
	if err := decision.ValidateEnvelope(req, unauthorizedScoreAbstain); err == nil {
		t.Fatalf("expected error when abstention contains unauthorized score key, got nil")
	}
}

// 4. Acceptance Criteria 2: Confidence Calibration Rigor & NaN/Inf Protection
func TestDecisionValidation_ConfidenceCalibrationRigor(t *testing.T) {
	req := decision.DecisionRequest{
		DecisionID:      domain.ID("dec_calibration"),
		DecisionType:    decision.DecisionTypeAcceptanceTriage,
		AllowedOutcomes: []string{"ACCEPT", "RETRY"},
		Eligibility: decision.ProviderEligibility{
			RequireCalibratedConfidence: true,
		},
		CreatedAt: time.Now().UTC(),
	}

	// A: Self-reported LLM confidence rejected when calibrated confidence is required
	uncalibratedRes := decision.DecisionResult{
		DecisionID:      req.DecisionID,
		SelectedOutcome: "ACCEPT",
		Confidence:      0.95,
		ConfidenceKind:  decision.ConfidenceSelfReported,
		DecidedAt:       time.Now().UTC(),
	}
	err := decision.ValidateEnvelope(req, uncalibratedRes)
	if err == nil {
		t.Fatalf("expected rejection of self-reported confidence when calibrated is required, got nil")
	}

	// B: Calibrated probability out of bounds [0.0, 1.0] rejected
	outOfBoundsRes := decision.DecisionResult{
		DecisionID:      req.DecisionID,
		SelectedOutcome: "ACCEPT",
		Confidence:      1.45,
		ConfidenceKind:  decision.ConfidenceCalibratedProbability,
		DecidedAt:       time.Now().UTC(),
	}
	if err := decision.ValidateEnvelope(req, outOfBoundsRes); err == nil {
		t.Fatalf("expected error for calibrated probability > 1.0, got nil")
	}

	// C: NaN and Inf rejected
	nanRes := decision.DecisionResult{
		DecisionID:      req.DecisionID,
		SelectedOutcome: "ACCEPT",
		Confidence:      math.NaN(),
		ConfidenceKind:  decision.ConfidenceCalibratedProbability,
		DecidedAt:       time.Now().UTC(),
	}
	if err := decision.ValidateEnvelope(req, nanRes); err == nil {
		t.Fatalf("expected error for NaN confidence, got nil")
	}

	infRes := decision.DecisionResult{
		DecisionID:      req.DecisionID,
		SelectedOutcome: "ACCEPT",
		Confidence:      math.Inf(1),
		ConfidenceKind:  decision.ConfidenceCalibratedProbability,
		DecidedAt:       time.Now().UTC(),
	}
	if err := decision.ValidateEnvelope(req, infRes); err == nil {
		t.Fatalf("expected error for Inf confidence, got nil")
	}

	// D: Calibrated probability with invalid score distribution (does not sum to ~1.0)
	invalidDistRes := decision.DecisionResult{
		DecisionID:      req.DecisionID,
		SelectedOutcome: "ACCEPT",
		OutcomeScores: map[string]float64{
			"ACCEPT": 0.2,
			"RETRY":  0.2, // Sums to 0.4
		},
		Confidence:     0.2,
		ConfidenceKind: decision.ConfidenceCalibratedProbability,
		DecidedAt:      time.Now().UTC(),
	}
	if err := decision.ValidateEnvelope(req, invalidDistRes); err == nil {
		t.Fatalf("expected error for calibrated probability distribution not summing to 1.0, got nil")
	}

	// E: Valid calibrated probability distribution
	validCalibrated := decision.DecisionResult{
		DecisionID:      req.DecisionID,
		SelectedOutcome: "ACCEPT",
		OutcomeScores: map[string]float64{
			"ACCEPT": 0.85,
			"RETRY":  0.15,
		},
		Confidence:     0.85,
		ConfidenceKind: decision.ConfidenceCalibratedProbability,
		DecidedAt:      time.Now().UTC(),
	}
	if err := decision.ValidateEnvelope(req, validCalibrated); err != nil {
		t.Fatalf("expected valid calibrated probability to pass, got: %v", err)
	}
}

// 5. Deterministic Projection Hash Invariant
func TestDecisionValidation_DeterministicProjectionHash(t *testing.T) {
	proj := map[string]any{
		"task_kind": "compute",
		"items":     []any{"item_b", "item_a"},
		"attempts":  float64(3),
	}

	hash1, err := decision.ComputeProjectionHash(proj)
	if err != nil {
		t.Fatalf("compute hash 1: %v", err)
	}

	proj2 := map[string]any{
		"attempts":  float64(3),
		"items":     []any{"item_b", "item_a"},
		"task_kind": "compute",
	}

	hash2, err := decision.ComputeProjectionHash(proj2)
	if err != nil {
		t.Fatalf("compute hash 2: %v", err)
	}

	if hash1 != hash2 {
		t.Fatalf("expected projection hash to be canonical and deterministic: %s != %s", hash1, hash2)
	}

	// Nil projection returns empty string without error
	nilHash, err := decision.ComputeProjectionHash(nil)
	if err != nil || nilHash != "" {
		t.Fatalf("expected empty hash for nil projection, got %q, err=%v", nilHash, err)
	}
}
