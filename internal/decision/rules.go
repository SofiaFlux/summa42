package decision

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Rule represents a deterministic decision rule evaluated against a request projection.
type Rule interface {
	Name() string
	Evaluate(req DecisionRequest) (outcome string, reason string, matched bool)
}

// CustomRule executes an arbitrary evaluation function.
type CustomRule struct {
	name string
	fn   func(req DecisionRequest) (string, string, bool)
}

// NewCustomRule constructs a custom evaluation rule.
func NewCustomRule(name string, fn func(req DecisionRequest) (string, string, bool)) *CustomRule {
	return &CustomRule{name: name, fn: fn}
}

func (r *CustomRule) Name() string {
	return r.name
}

func (r *CustomRule) Evaluate(req DecisionRequest) (string, string, bool) {
	if r.fn == nil {
		return "", "", false
	}
	return r.fn(req)
}

// ExactMatchRule matches when a projection key equals an expected value.
type ExactMatchRule struct {
	name          string
	projectionKey string
	expectedValue any
	outcome       string
	reason        string
}

// NewExactMatchRule constructs an exact match rule.
func NewExactMatchRule(name string, key string, expected any, outcome string, reason string) *ExactMatchRule {
	return &ExactMatchRule{
		name:          name,
		projectionKey: key,
		expectedValue: expected,
		outcome:       outcome,
		reason:        reason,
	}
}

func (r *ExactMatchRule) Name() string {
	return r.name
}

func (r *ExactMatchRule) Evaluate(req DecisionRequest) (string, string, bool) {
	if req.ContextProjection == nil {
		return "", "", false
	}
	val, ok := req.ContextProjection[r.projectionKey]
	if !ok {
		return "", "", false
	}
	if fmt.Sprintf("%v", val) == fmt.Sprintf("%v", r.expectedValue) {
		return r.outcome, r.reason, true
	}
	return "", "", false
}

// DenylistRule matches when a projection key's string representation is in a forbidden list.
type DenylistRule struct {
	name          string
	projectionKey string
	denylist      map[string]struct{}
	outcome       string
	reason        string
}

// NewDenylistRule constructs a denylist rule.
func NewDenylistRule(name string, key string, denylist []string, outcome string, reason string) *DenylistRule {
	set := make(map[string]struct{}, len(denylist))
	for _, item := range denylist {
		set[item] = struct{}{}
	}
	return &DenylistRule{
		name:          name,
		projectionKey: key,
		denylist:      set,
		outcome:       outcome,
		reason:        reason,
	}
}

func (r *DenylistRule) Name() string {
	return r.name
}

func (r *DenylistRule) Evaluate(req DecisionRequest) (string, string, bool) {
	if req.ContextProjection == nil {
		return "", "", false
	}
	val, ok := req.ContextProjection[r.projectionKey]
	if !ok {
		return "", "", false
	}
	strVal := fmt.Sprintf("%v", val)
	if _, found := r.denylist[strVal]; found {
		return r.outcome, r.reason, true
	}
	return "", "", false
}

// ThresholdOp defines numeric comparison operations.
type ThresholdOp string

const (
	ThresholdOpGreaterThan        ThresholdOp = ">"
	ThresholdOpGreaterThanOrEqual ThresholdOp = ">="
	ThresholdOpLessThan           ThresholdOp = "<"
	ThresholdOpLessThanOrEqual    ThresholdOp = "<="
	ThresholdOpEqual              ThresholdOp = "=="
)

// ThresholdRule matches when a numeric projection field satisfies a threshold condition.
type ThresholdRule struct {
	name          string
	projectionKey string
	op            ThresholdOp
	threshold     float64
	outcome       string
	reason        string
}

// NewThresholdRule constructs a threshold rule.
func NewThresholdRule(name string, key string, op ThresholdOp, threshold float64, outcome string, reason string) *ThresholdRule {
	return &ThresholdRule{
		name:          name,
		projectionKey: key,
		op:            op,
		threshold:     threshold,
		outcome:       outcome,
		reason:        reason,
	}
}

func (r *ThresholdRule) Name() string {
	return r.name
}

func (r *ThresholdRule) Evaluate(req DecisionRequest) (string, string, bool) {
	if req.ContextProjection == nil {
		return "", "", false
	}
	val, ok := req.ContextProjection[r.projectionKey]
	if !ok {
		return "", "", false
	}
	num, ok := toFloat64(val)
	if !ok {
		return "", "", false
	}

	var matched bool
	switch r.op {
	case ThresholdOpGreaterThan:
		matched = num > r.threshold
	case ThresholdOpGreaterThanOrEqual:
		matched = num >= r.threshold
	case ThresholdOpLessThan:
		matched = num < r.threshold
	case ThresholdOpLessThanOrEqual:
		matched = num <= r.threshold
	case ThresholdOpEqual:
		matched = num == r.threshold
	}

	if matched {
		return r.outcome, r.reason, true
	}
	return "", "", false
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint64:
		return float64(n), true
	case uint32:
		return float64(n), true
	default:
		return 0, false
	}
}

// RuleDecisionProvider is a thread-safe deterministic decision provider executing ordered rules.
type RuleDecisionProvider struct {
	mu    sync.RWMutex
	name  string
	rules []Rule
}

// NewRuleDecisionProvider creates a new RuleDecisionProvider.
func NewRuleDecisionProvider(name string) *RuleDecisionProvider {
	return &RuleDecisionProvider{
		name:  name,
		rules: make([]Rule, 0),
	}
}

func (p *RuleDecisionProvider) Info() ProviderInfo {
	return ProviderInfo{
		Kind:    ProviderKindRule,
		Name:    p.name,
		Version: "1.0",
	}
}

// AddRule registers a rule into the provider in a thread-safe manner.
func (p *RuleDecisionProvider) AddRule(rule Rule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = append(p.rules, rule)
}

// Resolve evaluates registered rules against the request.
// Only matches whose outcomes are in req.AllowedOutcomes are accepted,
// preventing earlier disallowed rules from shadowing valid later rules.
func (p *RuleDecisionProvider) Resolve(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
	p.mu.RLock()
	rules := make([]Rule, len(p.rules))
	copy(rules, p.rules)
	p.mu.RUnlock()

	allowedSet := make(map[string]struct{}, len(req.AllowedOutcomes))
	for _, ao := range req.AllowedOutcomes {
		allowedSet[ao] = struct{}{}
	}

	for _, rule := range rules {
		outcome, reason, matched := safeEvaluate(rule, req)
		if matched {
			if _, ok := allowedSet[outcome]; !ok {
				// Outcome not permitted for this request; continue checking subsequent rules
				continue
			}

			confKind := ConfidenceHeuristicScore
			var scores map[string]float64
			if req.Eligibility.RequireCalibratedConfidence {
				confKind = ConfidenceCalibratedProbability
				scores = map[string]float64{outcome: 1.0}
			}

			return DecisionResult{
				DecisionID:      req.DecisionID,
				SelectedOutcome: outcome,
				OutcomeScores:   scores,
				Confidence:      1.0,
				ConfidenceKind:  confKind,
				Abstained:       false,
				AbstainReason:   reason,
				Provider:        p.Info(),
				DecidedAt:       time.Now().UTC(),
			}, nil
		}
	}

	// No allowed rule matched: abstain honestly
	return DecisionResult{
		DecisionID:      req.DecisionID,
		SelectedOutcome: OutcomeAbstain,
		Confidence:      0.0,
		ConfidenceKind:  ConfidenceNone,
		Abstained:       true,
		AbstainReason:   fmt.Sprintf("provider %s: no matching rule found", p.name),
		Provider:        p.Info(),
		DecidedAt:       time.Now().UTC(),
	}, nil
}

func safeEvaluate(r Rule, req DecisionRequest) (outcome string, reason string, matched bool) {
	defer func() {
		if rec := recover(); rec != nil {
			matched = false
		}
	}()
	return r.Evaluate(req)
}
