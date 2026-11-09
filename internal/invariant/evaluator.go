package invariant

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/company"
)

type EvaluationResult struct {
	Status        EvaluationStatus
	ObservedValue map[string]any
	IsSevere      bool
	Reason        string
}

// EvaluateEntity deterministically evaluates an invariant against a specific company entity.
func EvaluateEntity(inv *BusinessInvariant, entity *company.Entity, now time.Time) (EvaluationResult, error) {
	switch inv.RuleType {
	case RuleExpiration:
		return evaluateExpiration(inv, entity.Attributes, now)

	case RuleRedundancy:
		return evaluateRedundancy(inv, entity.Attributes)

	case RuleExposureLimit:
		return evaluateExposure(inv, float64(entity.ExposureAmount))

	case RuleThreshold:
		return evaluateThresholdOnEntity(inv, entity)

	default:
		return EvaluationResult{Status: StatusNominal}, nil
	}
}

// EvaluateCompanySummary deterministically evaluates an invariant against the overall company summary.
func EvaluateCompanySummary(inv *BusinessInvariant, summary *company.CompanySummary) (EvaluationResult, error) {
	switch inv.RuleType {
	case RuleExposureLimit:
		return evaluateExposure(inv, float64(summary.TotalExposureAmount))

	case RuleThreshold:
		var val float64
		switch strings.ToLower(inv.RuleConfig.MetricPath) {
		case "total_exposure_amount", "exposure":
			val = float64(summary.TotalExposureAmount)
		case "total_entities":
			val = float64(summary.TotalEntities)
		case "active_commitments":
			val = float64(summary.ActiveCommitments)
		case "active_contracts":
			val = float64(summary.ActiveContracts)
		case "active_risks":
			val = float64(summary.ActiveRisks)
		default:
			return EvaluationResult{Status: StatusNominal}, nil
		}
		return evaluateComparison(inv, val, inv.RuleConfig.MetricPath)

	default:
		return EvaluationResult{Status: StatusNominal}, nil
	}
}

func evaluateExpiration(inv *BusinessInvariant, attrs map[string]any, now time.Time) (EvaluationResult, error) {
	var expiryStr string
	for _, key := range []string{"expires_at", "valid_until", "deadline", "expiration_date"} {
		if v, ok := attrs[key]; ok && v != nil {
			s := strings.TrimSpace(fmt.Sprintf("%v", v))
			if s != "" {
				expiryStr = s
				break
			}
		}
	}
	if expiryStr == "" {
		return EvaluationResult{Status: StatusNominal, ObservedValue: map[string]any{"note": "no expiration field found"}}, nil
	}

	expiryTime, err := time.Parse(time.RFC3339, expiryStr)
	if err != nil {
		// Attempt alternate ISO parse
		t2, err2 := time.Parse("2006-01-02", expiryStr)
		if err2 != nil {
			return EvaluationResult{
				Status:        StatusBreached,
				ObservedValue: map[string]any{"error": "unparseable expiry date", "raw_value": expiryStr},
				IsSevere:      true,
				Reason:        fmt.Sprintf("unparseable expiration date: %s", expiryStr),
			}, nil
		}
		expiryTime = t2
	}

	timeToExpiry := expiryTime.Sub(now).Seconds()
	leadWindow := float64(inv.RuleConfig.LeadWindowSeconds)
	if leadWindow <= 0 {
		leadWindow = 86400 // default 1 day
	}

	observed := map[string]any{
		"time_to_expiry_seconds": timeToExpiry,
		"expires_at":             expiryTime.Format(time.RFC3339),
		"lead_window_seconds":    leadWindow,
	}

	if timeToExpiry <= 0 {
		return EvaluationResult{
			Status:        StatusBreached,
			ObservedValue: observed,
			IsSevere:      inv.Severity == SeverityCritical,
			Reason:        fmt.Sprintf("expired by %.0f seconds", math.Abs(timeToExpiry)),
		}, nil
	}

	if timeToExpiry <= leadWindow {
		return EvaluationResult{
			Status:        StatusApproachingBreach,
			ObservedValue: observed,
			IsSevere:      false,
			Reason:        fmt.Sprintf("expires in %.0f seconds (within lead window %.0f s)", timeToExpiry, leadWindow),
		}, nil
	}

	return EvaluationResult{Status: StatusNominal, ObservedValue: observed}, nil
}

func evaluateRedundancy(inv *BusinessInvariant, attrs map[string]any) (EvaluationResult, error) {
	var count float64
	found := false
	for _, key := range []string{"redundancy_count", "backup_count", "replicas", "redundancy"} {
		if v, ok := attrs[key]; ok && v != nil {
			f, err := toFloat64(v)
			if err == nil {
				count = f
				found = true
				break
			}
		}
	}
	if !found {
		return EvaluationResult{Status: StatusNominal, ObservedValue: map[string]any{"note": "no redundancy field found"}}, nil
	}

	minRequired := float64(inv.RuleConfig.MinRedundancy)
	observed := map[string]any{
		"observed_redundancy": count,
		"min_required":        minRequired,
	}

	if count < minRequired {
		return EvaluationResult{
			Status:        StatusBreached,
			ObservedValue: observed,
			IsSevere:      count == 0 || inv.Severity == SeverityCritical,
			Reason:        fmt.Sprintf("redundancy %v below required minimum %v", count, minRequired),
		}, nil
	}

	return EvaluationResult{Status: StatusNominal, ObservedValue: observed}, nil
}

func evaluateExposure(inv *BusinessInvariant, exposure float64) (EvaluationResult, error) {
	threshold := inv.RuleConfig.Threshold
	observed := map[string]any{
		"exposure_amount": exposure,
		"threshold":       threshold,
	}

	isSevere := false
	if inv.RuleConfig.SevereExposureThreshold > 0 && int64(exposure) >= inv.RuleConfig.SevereExposureThreshold {
		isSevere = true
	}

	if exposure > threshold {
		return EvaluationResult{
			Status:        StatusBreached,
			ObservedValue: observed,
			IsSevere:      isSevere || inv.Severity == SeverityCritical,
			Reason:        fmt.Sprintf("exposure amount %.0f exceeds threshold %.0f", exposure, threshold),
		}, nil
	}

	// Approaching breach if within 10% of threshold
	if threshold > 0 && exposure >= threshold*0.9 {
		return EvaluationResult{
			Status:        StatusApproachingBreach,
			ObservedValue: observed,
			IsSevere:      false,
			Reason:        fmt.Sprintf("exposure amount %.0f approaching threshold %.0f", exposure, threshold),
		}, nil
	}

	return EvaluationResult{Status: StatusNominal, ObservedValue: observed}, nil
}

func evaluateThresholdOnEntity(inv *BusinessInvariant, entity *company.Entity) (EvaluationResult, error) {
	metric := strings.TrimSpace(inv.RuleConfig.MetricPath)
	var val float64
	var err error

	if metric == "exposure_amount" || metric == "exposure" {
		val = float64(entity.ExposureAmount)
	} else {
		// Look up in attributes
		attrKey := metric
		if strings.HasPrefix(metric, "attributes.") {
			attrKey = strings.TrimPrefix(metric, "attributes.")
		}
		raw, ok := entity.Attributes[attrKey]
		if !ok || raw == nil {
			return EvaluationResult{Status: StatusNominal, ObservedValue: map[string]any{"note": "attribute not found"}}, nil
		}
		val, err = toFloat64(raw)
		if err != nil {
			return EvaluationResult{Status: StatusNominal, ObservedValue: map[string]any{"error": "attribute not a number"}}, nil
		}
	}

	return evaluateComparison(inv, val, metric)
}

func evaluateComparison(inv *BusinessInvariant, val float64, metricName string) (EvaluationResult, error) {
	threshold := inv.RuleConfig.Threshold
	op := inv.RuleConfig.Operator
	if op == "" {
		op = "<="
	}

	observed := map[string]any{
		"metric":    metricName,
		"value":     val,
		"threshold": threshold,
		"operator":  op,
	}

	violated := false
	switch op {
	case "<":
		violated = val < threshold
	case "<=":
		violated = val <= threshold
	case ">":
		violated = val > threshold
	case ">=":
		violated = val >= threshold
	case "==":
		violated = val == threshold
	case "!=":
		violated = val != threshold
	default:
		return EvaluationResult{Status: StatusNominal}, ErrInvalidOperator
	}

	if violated {
		isSevere := false
		if inv.RuleConfig.SevereExposureThreshold > 0 && int64(val) >= inv.RuleConfig.SevereExposureThreshold {
			isSevere = true
		}
		return EvaluationResult{
			Status:        StatusBreached,
			ObservedValue: observed,
			IsSevere:      isSevere || inv.Severity == SeverityCritical,
			Reason:        fmt.Sprintf("metric %s value %v violated condition %s %v", metricName, val, op, threshold),
		}, nil
	}

	return EvaluationResult{Status: StatusNominal, ObservedValue: observed}, nil
}

func toFloat64(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case float32:
		return float64(n), nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case int32:
		return float64(n), nil
	case string:
		return strconv.ParseFloat(strings.TrimSpace(n), 64)
	default:
		return 0, fmt.Errorf("cannot convert %T to float64", v)
	}
}
