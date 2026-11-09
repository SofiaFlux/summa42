package decision

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrInvalidRequest = errors.New("invalid decision request")
)

// DecisionEngine coordinates deterministic L0 pre-checks, provider dispatch, and multi-tier escalation.
type DecisionEngine struct {
	mu        sync.RWMutex
	preChecks []Rule
	providers map[DecisionType][]DecisionProvider
	store     ProvenanceStore
	emitter   TelemetryEmitter
}

// NewEngine creates a new DecisionEngine.
func NewEngine() *DecisionEngine {
	return &DecisionEngine{
		preChecks: make([]Rule, 0),
		providers: make(map[DecisionType][]DecisionProvider),
	}
}

// SetProvenanceStore configures a provenance store for tracking decision lineage.
func (e *DecisionEngine) SetProvenanceStore(store ProvenanceStore) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.store = store
}

// SetTelemetryEmitter configures an emitter for publishing decision telemetry.
func (e *DecisionEngine) SetTelemetryEmitter(emitter TelemetryEmitter) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.emitter = emitter
}

// RegisterPreCheck adds a deterministic pre-check rule executed at L0 before provider dispatch.
func (e *DecisionEngine) RegisterPreCheck(rule Rule) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.preChecks = append(e.preChecks, rule)
}

// RegisterProvider registers a provider into the escalation chain for a given DecisionType.
// Providers are invoked in registration order when earlier providers abstain or fail.
func (e *DecisionEngine) RegisterProvider(dt DecisionType, provider DecisionProvider) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.providers == nil {
		e.providers = make(map[DecisionType][]DecisionProvider)
	}
	e.providers[dt] = append(e.providers[dt], provider)
}

// Decide dispatches a micro-decision through deterministic L0 pre-checks and multi-tier escalation.
func (e *DecisionEngine) Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
	if req.DecisionID == "" {
		return DecisionResult{}, fmt.Errorf("%w: decision_id cannot be empty", ErrInvalidRequest)
	}
	if !req.DecisionType.Valid() {
		return DecisionResult{}, fmt.Errorf("%w: invalid decision_type %q", ErrInvalidRequest, req.DecisionType)
	}
	if len(req.AllowedOutcomes) == 0 {
		return DecisionResult{}, fmt.Errorf("%w: allowed_outcomes cannot be empty", ErrInvalidRequest)
	}
	if !req.RiskProfile.Valid() {
		return DecisionResult{}, fmt.Errorf("%w: invalid risk_profile %q", ErrInvalidRequest, req.RiskProfile)
	}

	projHash, err := ComputeProjectionHash(req.ContextProjection)
	if err != nil {
		return DecisionResult{}, fmt.Errorf("failed to compute projection hash: %w", err)
	}

	e.mu.RLock()
	preChecks := make([]Rule, len(e.preChecks))
	copy(preChecks, e.preChecks)

	var chain []DecisionProvider
	if e.providers != nil {
		chain = make([]DecisionProvider, len(e.providers[req.DecisionType]))
		copy(chain, e.providers[req.DecisionType])
	}
	e.mu.RUnlock()

	// 1. L0 Deterministic Pre-checks
	for _, preCheck := range preChecks {
		outcome, reason, matched := safeEvaluateRule(preCheck, req)
		if matched {
			// Check if outcome is in AllowedOutcomes
			allowed := false
			for _, ao := range req.AllowedOutcomes {
				if ao == outcome {
					allowed = true
					break
				}
			}
			if !allowed {
				continue
			}

			confKind := ConfidenceHeuristicScore
			var scores map[string]float64
			if req.Eligibility.RequireCalibratedConfidence {
				confKind = ConfidenceCalibratedProbability
				scores = map[string]float64{outcome: 1.0}
			}

			res := DecisionResult{
				DecisionID:      req.DecisionID,
				SelectedOutcome: outcome,
				OutcomeScores:   scores,
				Confidence:      1.0,
				ConfidenceKind:  confKind,
				Abstained:       false,
				AbstainReason:   reason,
				Provider: ProviderInfo{
					Kind:    ProviderKindRule,
					Name:    "summa42:l0-precheck:" + preCheck.Name(),
					Version: "1.0",
				},
				ProjectionHash: projHash,
				DecidedAt:      time.Now().UTC(),
			}

			if err := ValidateEnvelope(req, res); err == nil {
				e.recordAndEmit(ctx, req, res)
				return res, nil
			}
		}
	}

	// 2. Multi-tier Provider Escalation Chain
	// Derive single chain-wide context budget so N tiers don't consume N× the request budget
	chainCtx := ctx
	var cancel context.CancelFunc
	if req.LatencyBudget > 0 {
		chainCtx, cancel = context.WithTimeout(ctx, req.LatencyBudget)
		defer cancel()
	}

	for _, provider := range chain {
		if chainCtx.Err() != nil {
			break
		}

		info := provider.Info()

		// Eligibility Gating
		if info.Kind == ProviderKindConstrainedLLM && !req.Eligibility.AllowRemoteLLM {
			continue
		}
		if (info.Kind == ProviderKindClassicalML || info.Kind == ProviderKindSpecialistModel) && !req.Eligibility.AllowProbabilistic {
			continue
		}

		start := time.Now()
		res, err := safeResolve(chainCtx, provider, req)
		latency := time.Since(start)

		if err != nil {
			// Provider failed or panicked; escalate deterministically to next provider in chain
			continue
		}

		if res.Latency == 0 {
			res.Latency = latency
		}
		// DecisionEngine authoritatively seals the canonical projection hash
		res.ProjectionHash = projHash

		// Ensure provider provenance is valid
		if res.Provider.Name == "" || !res.Provider.Kind.Valid() {
			res.Provider = info
		}

		// Provider abstained; escalate deterministically to next provider
		if res.Abstained {
			continue
		}

		// Validate result against semantic envelope
		if err := ValidateEnvelope(req, res); err != nil {
			// Envelope violation; reject invalid result and escalate
			continue
		}

		e.recordAndEmit(ctx, req, res)
		return res, nil
	}

	// 3. Fail-safe Honest Degradation
	failSafe := DecisionResult{
		DecisionID:      req.DecisionID,
		SelectedOutcome: OutcomeAbstain,
		Abstained:       true,
		AbstainReason:   "all decision providers abstained, failed, or were ineligible",
		Confidence:      0.0,
		ConfidenceKind:  ConfidenceNone,
		Provider: ProviderInfo{
			Kind:    ProviderKindRule,
			Name:    "summa42:fail-safe",
			Version: "1.0",
		},
		ProjectionHash: projHash,
		DecidedAt:      time.Now().UTC(),
	}

	if err := ValidateEnvelope(req, failSafe); err != nil {
		return failSafe, fmt.Errorf("fail-safe validation error: %w", err)
	}

	e.recordAndEmit(ctx, req, failSafe)
	return failSafe, nil
}

func (e *DecisionEngine) recordAndEmit(ctx context.Context, req DecisionRequest, res DecisionResult) {
	e.mu.RLock()
	store := e.store
	emitter := e.emitter
	e.mu.RUnlock()

	if store != nil {
		_ = store.RecordDecision(ctx, req, res)
	}
	if emitter != nil {
		decidedAt := res.DecidedAt
		if decidedAt.IsZero() {
			decidedAt = time.Now().UTC()
		}
		emitter.EmitDecision(ctx, ProvenanceRecord{
			DecisionID:      req.DecisionID,
			DecisionType:    req.DecisionType,
			SubjectRef:      req.SubjectRef,
			Provider:        res.Provider,
			SelectedOutcome: res.SelectedOutcome,
			Confidence:      res.Confidence,
			ConfidenceKind:  res.ConfidenceKind,
			Abstained:       res.Abstained,
			AbstainReason:   res.AbstainReason,
			ProjectionHash:  res.ProjectionHash,
			Latency:         res.Latency,
			Usage:           res.Usage,
			DecidedAt:       decidedAt,
		})
	}
}

// safeResolve invokes provider.Resolve with panic recovery for robust isolation.
func safeResolve(ctx context.Context, p DecisionProvider, req DecisionRequest) (res DecisionResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("provider panicked: %v", r)
		}
	}()
	return p.Resolve(ctx, req)
}

// safeEvaluateRule invokes rule.Evaluate with panic recovery.
func safeEvaluateRule(r Rule, req DecisionRequest) (outcome string, reason string, matched bool) {
	defer func() {
		if rec := recover(); rec != nil {
			matched = false
		}
	}()
	return r.Evaluate(req)
}
