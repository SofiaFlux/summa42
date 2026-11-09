package decision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

var (
	ErrUnauthorizedOutcome              = errors.New("selected outcome is not within the allowed outcomes envelope")
	ErrEmptyOutcome                     = errors.New("selected outcome cannot be empty when not abstaining")
	ErrInvalidAbstention                = errors.New("invalid abstention: reason required and outcome must be empty or ABSTAIN")
	ErrUncalibratedConfidenceNotAllowed = errors.New("calibrated confidence is required by request eligibility")
	ErrInvalidProbabilityBounds         = errors.New("calibrated probability must be finite and in range [0.0, 1.0]")
	ErrInvalidProbabilityDistribution   = errors.New("calibrated probability distribution across outcomes must sum to 1.0")
	ErrDecisionIDMismatch               = errors.New("decision result ID does not match request ID")
	ErrInvalidConfidenceKind            = errors.New("unknown or invalid confidence kind")
	ErrInvalidDecisionType              = errors.New("unknown or invalid decision type")
)

// ValidateEnvelope strictly enforces semantic envelope invariants between request and result.
func ValidateEnvelope(req DecisionRequest, res DecisionResult) error {
	if req.DecisionID != "" && res.DecisionID != req.DecisionID {
		return fmt.Errorf("%w: req=%s res=%s", ErrDecisionIDMismatch, req.DecisionID, res.DecisionID)
	}

	if req.DecisionType != "" && !req.DecisionType.Valid() {
		return fmt.Errorf("%w: %s", ErrInvalidDecisionType, req.DecisionType)
	}

	if !res.ConfidenceKind.Valid() {
		return fmt.Errorf("%w: %s", ErrInvalidConfidenceKind, res.ConfidenceKind)
	}

	// NaN/Inf guard on confidence
	if math.IsNaN(res.Confidence) || math.IsInf(res.Confidence, 0) {
		return fmt.Errorf("%w: confidence cannot be NaN or Inf", ErrInvalidProbabilityBounds)
	}

	allowedSet := make(map[string]struct{}, len(req.AllowedOutcomes))
	for _, outcome := range req.AllowedOutcomes {
		allowedSet[outcome] = struct{}{}
	}

	// Validate outcome scores keys regardless of abstention
	if len(res.OutcomeScores) > 0 {
		var sum float64
		for outcome, score := range res.OutcomeScores {
			if math.IsNaN(score) || math.IsInf(score, 0) {
				return fmt.Errorf("%w: score for %q cannot be NaN or Inf", ErrInvalidProbabilityBounds, outcome)
			}
			if _, ok := allowedSet[outcome]; !ok {
				return fmt.Errorf("%w: score outcome %q is not in allowed list %v", ErrUnauthorizedOutcome, outcome, req.AllowedOutcomes)
			}
			if res.ConfidenceKind == ConfidenceCalibratedProbability {
				if score < 0.0 || score > 1.0 {
					return fmt.Errorf("%w: score %f for outcome %q", ErrInvalidProbabilityBounds, score, outcome)
				}
				sum += score
			}
		}

		if res.ConfidenceKind == ConfidenceCalibratedProbability && !res.Abstained {
			// Probability distribution across outcomes must sum to approximately 1.0 (with 1e-6 tolerance)
			if math.Abs(sum-1.0) > 1e-6 {
				return fmt.Errorf("%w: sum of outcome scores is %f, expected 1.0", ErrInvalidProbabilityDistribution, sum)
			}
		}
	}

	// 1. Abstention rules
	if res.Abstained {
		if res.AbstainReason == "" {
			return fmt.Errorf("%w: abstain_reason cannot be empty", ErrInvalidAbstention)
		}
		if res.SelectedOutcome != "" && res.SelectedOutcome != "ABSTAIN" {
			return fmt.Errorf("%w: cannot specify outcome %q when abstained=true", ErrInvalidAbstention, res.SelectedOutcome)
		}
		// When calibrated confidence is required, abstention must not report self-reported or uncalibrated confidence
		if req.Eligibility.RequireCalibratedConfidence && res.ConfidenceKind != ConfidenceCalibratedProbability && res.ConfidenceKind != ConfidenceNone {
			return fmt.Errorf("%w: abstaining with %s violates calibrated confidence requirement", ErrUncalibratedConfidenceNotAllowed, res.ConfidenceKind)
		}
		return nil
	}

	// 2. Allowed outcomes enforcement
	if res.SelectedOutcome == "" {
		return ErrEmptyOutcome
	}
	if _, ok := allowedSet[res.SelectedOutcome]; !ok {
		return fmt.Errorf("%w: %q is not in %v", ErrUnauthorizedOutcome, res.SelectedOutcome, req.AllowedOutcomes)
	}

	// 3. Calibration checks
	if req.Eligibility.RequireCalibratedConfidence && res.ConfidenceKind != ConfidenceCalibratedProbability {
		return fmt.Errorf("%w: received %s", ErrUncalibratedConfidenceNotAllowed, res.ConfidenceKind)
	}

	if res.ConfidenceKind == ConfidenceCalibratedProbability {
		if res.Confidence < 0.0 || res.Confidence > 1.0 {
			return fmt.Errorf("%w: confidence %f", ErrInvalidProbabilityBounds, res.Confidence)
		}
	}

	return nil
}

// ComputeProjectionHash calculates a deterministic SHA-256 digest of the context projection.
// encoding/json deterministically sorts map keys recursively.
func ComputeProjectionHash(proj map[string]any) (string, error) {
	if proj == nil {
		return "", nil
	}

	raw, err := json.Marshal(proj)
	if err != nil {
		return "", err
	}

	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}
