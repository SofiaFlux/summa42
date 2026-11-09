package decision_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/decision"
	"github.com/SofiaFlux/summa42/internal/domain"
)

// mockProvider implements decision.DecisionProvider for test scenarios.
type mockProvider struct {
	info       decision.ProviderInfo
	resolveFn  func(ctx context.Context, req decision.DecisionRequest) (decision.DecisionResult, error)
	callsCount int
	mu         sync.Mutex
}

func (m *mockProvider) Info() decision.ProviderInfo {
	return m.info
}

func (m *mockProvider) Resolve(ctx context.Context, req decision.DecisionRequest) (decision.DecisionResult, error) {
	m.mu.Lock()
	m.callsCount++
	m.mu.Unlock()
	if m.resolveFn != nil {
		return m.resolveFn(ctx, req)
	}
	return decision.DecisionResult{
		DecisionID:      req.DecisionID,
		SelectedOutcome: req.AllowedOutcomes[0],
		Confidence:      1.0,
		ConfidenceKind:  decision.ConfidenceCalibratedProbability,
		Provider:        m.info,
		DecidedAt:       time.Now().UTC(),
	}, nil
}

func (m *mockProvider) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.callsCount
}

func baseTestRequest() decision.DecisionRequest {
	return decision.DecisionRequest{
		DecisionID:   domain.NewID("dec"),
		DecisionType: decision.DecisionTypeRouting,
		SubjectRef: decision.SubjectRef{
			Kind: "task",
			ID:   domain.NewID("task"),
		},
		AllowedOutcomes: []string{"FAST_PATH", "STANDARD_PATH", "HUMAN_ESCALATION"},
		ContextProjection: map[string]any{
			"complexity": "low",
			"score":      85,
			"tag":        "safe",
		},
		RiskProfile: decision.RiskProfileLow,
		Eligibility: decision.ProviderEligibility{
			AllowProbabilistic: true,
			AllowRemoteLLM:     true,
		},
		CreatedAt: time.Now().UTC(),
	}
}

func TestRuleDecisionProvider_ExactMatchAndDenylist(t *testing.T) {
	ruleProvider := decision.NewRuleDecisionProvider("test-rules")

	// 1. Exact match rule
	ruleProvider.AddRule(decision.NewExactMatchRule(
		"fast-route-rule",
		"complexity",
		"low",
		"FAST_PATH",
		"complexity is low, selecting fast path",
	))

	// 2. Denylist rule
	ruleProvider.AddRule(decision.NewDenylistRule(
		"blocked-tag-rule",
		"tag",
		[]string{"malicious", "prohibited"},
		"HUMAN_ESCALATION",
		"tag is prohibited, escalating",
	))

	ctx := context.Background()

	// Should match exact match rule
	req1 := baseTestRequest()
	res1, err := ruleProvider.Resolve(ctx, req1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res1.Abstained {
		t.Fatalf("expected rule to match, got abstained: %s", res1.AbstainReason)
	}
	if res1.SelectedOutcome != "FAST_PATH" {
		t.Errorf("expected outcome FAST_PATH, got %s", res1.SelectedOutcome)
	}

	// Should match denylist rule
	req2 := baseTestRequest()
	req2.ContextProjection["tag"] = "malicious"
	// Set complexity to high so exact match doesn't fire first
	req2.ContextProjection["complexity"] = "high"
	res2, err := ruleProvider.Resolve(ctx, req2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res2.SelectedOutcome != "HUMAN_ESCALATION" {
		t.Errorf("expected outcome HUMAN_ESCALATION, got %s", res2.SelectedOutcome)
	}

	// Should abstain when no rule matches
	req3 := baseTestRequest()
	req3.ContextProjection["complexity"] = "high"
	req3.ContextProjection["tag"] = "safe"
	res3, err := ruleProvider.Resolve(ctx, req3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res3.Abstained {
		t.Errorf("expected abstained when no rule matches, got outcome: %s", res3.SelectedOutcome)
	}
	if res3.SelectedOutcome != decision.OutcomeAbstain {
		t.Errorf("expected SelectedOutcome ABSTAIN, got %s", res3.SelectedOutcome)
	}
}

func TestRuleDecisionProvider_ThresholdRule(t *testing.T) {
	ruleProvider := decision.NewRuleDecisionProvider("threshold-rules")

	ruleProvider.AddRule(decision.NewThresholdRule(
		"score-threshold",
		"score",
		decision.ThresholdOpGreaterThanOrEqual,
		90.0,
		"FAST_PATH",
		"score >= 90 qualifies for fast path",
	))

	ctx := context.Background()

	// Score 85 -> below threshold, should abstain
	req1 := baseTestRequest()
	req1.ContextProjection["score"] = 85
	res1, err := ruleProvider.Resolve(ctx, req1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res1.Abstained {
		t.Fatalf("expected score 85 to abstain")
	}

	// Score 95 -> >= 90, should match
	req2 := baseTestRequest()
	req2.ContextProjection["score"] = 95
	res2, err := ruleProvider.Resolve(ctx, req2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res2.Abstained {
		t.Fatalf("expected score 95 to match, got abstained: %s", res2.AbstainReason)
	}
	if res2.SelectedOutcome != "FAST_PATH" {
		t.Errorf("expected FAST_PATH, got %s", res2.SelectedOutcome)
	}
}

func TestDecisionEngine_L0PreChecks(t *testing.T) {
	engine := decision.NewEngine()

	// Register L0 pre-check rule
	engine.RegisterPreCheck(decision.NewCustomRule(
		"emergency-halt",
		func(req decision.DecisionRequest) (string, string, bool) {
			if req.ContextProjection["emergency"] == true {
				return "HUMAN_ESCALATION", "emergency flag present", true
			}
			return "", "", false
		},
	))

	// Register a mock provider for ROUTING that should NOT be called when pre-check matches
	mock := &mockProvider{
		info: decision.ProviderInfo{
			Kind: decision.ProviderKindSpecialistModel,
			Name: "specialist-router",
		},
	}
	engine.RegisterProvider(decision.DecisionTypeRouting, mock)

	ctx := context.Background()

	// 1. With emergency=true, L0 pre-check triggers immediately
	reqEmergency := baseTestRequest()
	reqEmergency.ContextProjection["emergency"] = true
	res, err := engine.Decide(ctx, reqEmergency)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.SelectedOutcome != "HUMAN_ESCALATION" {
		t.Errorf("expected HUMAN_ESCALATION, got %s", res.SelectedOutcome)
	}
	if mock.Calls() != 0 {
		t.Errorf("expected provider not to be called, got %d calls", mock.Calls())
	}
	if res.Provider.Kind != decision.ProviderKindRule {
		t.Errorf("expected ProviderKindRule, got %s", res.Provider.Kind)
	}

	// 2. Without emergency, falls through to mock provider
	reqNormal := baseTestRequest()
	res2, err := engine.Decide(ctx, reqNormal)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res2.SelectedOutcome != "FAST_PATH" {
		t.Errorf("expected FAST_PATH, got %s", res2.SelectedOutcome)
	}
	if mock.Calls() != 1 {
		t.Errorf("expected provider to be called once, got %d calls", mock.Calls())
	}
}

func TestDecisionEngine_EscalationPolicy(t *testing.T) {
	engine := decision.NewEngine()

	// Tier 1: Abstained
	p1 := &mockProvider{
		info: decision.ProviderInfo{Kind: decision.ProviderKindRule, Name: "tier1-rule"},
		resolveFn: func(ctx context.Context, req decision.DecisionRequest) (decision.DecisionResult, error) {
			return decision.DecisionResult{
				DecisionID:      req.DecisionID,
				SelectedOutcome: decision.OutcomeAbstain,
				Abstained:       true,
				AbstainReason:   "rule didn't match",
				Provider:        decision.ProviderInfo{Kind: decision.ProviderKindRule, Name: "tier1-rule"},
				DecidedAt:       time.Now().UTC(),
			}, nil
		},
	}

	// Tier 2: Error
	p2 := &mockProvider{
		info: decision.ProviderInfo{Kind: decision.ProviderKindClassicalML, Name: "tier2-ml"},
		resolveFn: func(ctx context.Context, req decision.DecisionRequest) (decision.DecisionResult, error) {
			return decision.DecisionResult{}, errors.New("ml model service unavailable")
		},
	}

	// Tier 3: Success
	p3 := &mockProvider{
		info: decision.ProviderInfo{Kind: decision.ProviderKindConstrainedLLM, Name: "tier3-llm"},
		resolveFn: func(ctx context.Context, req decision.DecisionRequest) (decision.DecisionResult, error) {
			return decision.DecisionResult{
				DecisionID:      req.DecisionID,
				SelectedOutcome: "STANDARD_PATH",
				Confidence:      0.95,
				ConfidenceKind:  decision.ConfidenceCalibratedProbability,
				OutcomeScores: map[string]float64{
					"STANDARD_PATH":    0.95,
					"FAST_PATH":        0.03,
					"HUMAN_ESCALATION": 0.02,
				},
				Provider:  decision.ProviderInfo{Kind: decision.ProviderKindConstrainedLLM, Name: "tier3-llm"},
				DecidedAt: time.Now().UTC(),
			}, nil
		},
	}

	engine.RegisterProvider(decision.DecisionTypeRouting, p1)
	engine.RegisterProvider(decision.DecisionTypeRouting, p2)
	engine.RegisterProvider(decision.DecisionTypeRouting, p3)

	req := baseTestRequest()
	res, err := engine.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if p1.Calls() != 1 {
		t.Errorf("expected p1 to be called once, got %d", p1.Calls())
	}
	if p2.Calls() != 1 {
		t.Errorf("expected p2 to be called once, got %d", p2.Calls())
	}
	if p3.Calls() != 1 {
		t.Errorf("expected p3 to be called once, got %d", p3.Calls())
	}

	if res.SelectedOutcome != "STANDARD_PATH" {
		t.Errorf("expected outcome STANDARD_PATH, got %s", res.SelectedOutcome)
	}
	if res.Provider.Name != "tier3-llm" {
		t.Errorf("expected provider tier3-llm, got %s", res.Provider.Name)
	}
}

func TestDecisionEngine_EligibilityGating(t *testing.T) {
	engine := decision.NewEngine()

	llmProvider := &mockProvider{
		info: decision.ProviderInfo{Kind: decision.ProviderKindConstrainedLLM, Name: "remote-llm"},
	}
	engine.RegisterProvider(decision.DecisionTypeRouting, llmProvider)

	// When AllowRemoteLLM is false, LLM provider must be skipped
	req := baseTestRequest()
	req.Eligibility.AllowRemoteLLM = false

	res, err := engine.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if llmProvider.Calls() != 0 {
		t.Errorf("expected LLM provider to be skipped, but was called %d times", llmProvider.Calls())
	}

	// Should degrade safely to fail-safe abstention
	if !res.Abstained {
		t.Fatalf("expected fail-safe abstention when all providers ineligible, got %s", res.SelectedOutcome)
	}
	if res.SelectedOutcome != decision.OutcomeAbstain {
		t.Errorf("expected ABSTAIN outcome, got %s", res.SelectedOutcome)
	}
}

func TestDecisionEngine_EnvelopeViolationRejection(t *testing.T) {
	engine := decision.NewEngine()

	// Provider returns outcome not in AllowedOutcomes
	badProvider := &mockProvider{
		info: decision.ProviderInfo{Kind: decision.ProviderKindSpecialistModel, Name: "rogue-provider"},
		resolveFn: func(ctx context.Context, req decision.DecisionRequest) (decision.DecisionResult, error) {
			return decision.DecisionResult{
				DecisionID:      req.DecisionID,
				SelectedOutcome: "UNAUTHORIZED_OUTCOME",
				Confidence:      1.0,
				ConfidenceKind:  decision.ConfidenceCalibratedProbability,
				Provider:        decision.ProviderInfo{Kind: decision.ProviderKindSpecialistModel, Name: "rogue-provider"},
				DecidedAt:       time.Now().UTC(),
			}, nil
		},
	}

	fallbackProvider := &mockProvider{
		info: decision.ProviderInfo{Kind: decision.ProviderKindRule, Name: "fallback-rule"},
		resolveFn: func(ctx context.Context, req decision.DecisionRequest) (decision.DecisionResult, error) {
			return decision.DecisionResult{
				DecisionID:      req.DecisionID,
				SelectedOutcome: "FAST_PATH",
				Confidence:      1.0,
				ConfidenceKind:  decision.ConfidenceCalibratedProbability,
				Provider:        decision.ProviderInfo{Kind: decision.ProviderKindRule, Name: "fallback-rule"},
				DecidedAt:       time.Now().UTC(),
			}, nil
		},
	}

	engine.RegisterProvider(decision.DecisionTypeRouting, badProvider)
	engine.RegisterProvider(decision.DecisionTypeRouting, fallbackProvider)

	req := baseTestRequest()
	res, err := engine.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Bad provider was called but its result violated envelope, so engine escalated to fallback
	if badProvider.Calls() != 1 {
		t.Errorf("expected badProvider to be called once, got %d", badProvider.Calls())
	}
	if fallbackProvider.Calls() != 1 {
		t.Errorf("expected fallbackProvider to be called once, got %d", fallbackProvider.Calls())
	}
	if res.SelectedOutcome != "FAST_PATH" {
		t.Errorf("expected FAST_PATH from fallback, got %s", res.SelectedOutcome)
	}
}

func TestDecisionEngine_FailSafeDegradation(t *testing.T) {
	engine := decision.NewEngine()

	req := baseTestRequest()
	// No providers registered for ROUTING
	res, err := engine.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !res.Abstained {
		t.Fatalf("expected fail-safe abstention, got outcome: %s", res.SelectedOutcome)
	}
	if res.SelectedOutcome != decision.OutcomeAbstain {
		t.Errorf("expected SelectedOutcome ABSTAIN, got %s", res.SelectedOutcome)
	}
	if res.ConfidenceKind != decision.ConfidenceNone {
		t.Errorf("expected ConfidenceNone, got %s", res.ConfidenceKind)
	}
	if res.Provider.Name == "" {
		t.Errorf("expected non-empty fail-safe provider name")
	}

	// Verify projection hash is preserved
	expectedHash, err := decision.ComputeProjectionHash(req.ContextProjection)
	if err != nil {
		t.Fatalf("hash err: %v", err)
	}
	if res.ProjectionHash != expectedHash {
		t.Errorf("expected projection hash %s, got %s", expectedHash, res.ProjectionHash)
	}
}

func TestDecisionEngine_ConcurrentThreadSafety(t *testing.T) {
	engine := decision.NewEngine()

	ruleProvider := decision.NewRuleDecisionProvider("concurrent-rules")
	ruleProvider.AddRule(decision.NewExactMatchRule("r1", "tag", "safe", "FAST_PATH", "safe tag"))
	engine.RegisterProvider(decision.DecisionTypeRouting, ruleProvider)

	const concurrency = 25
	const iterations = 50
	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				req := baseTestRequest()
				req.ContextProjection["tag"] = fmt.Sprintf("worker-%d-%d", workerID, j)
				if j%2 == 0 {
					req.ContextProjection["tag"] = "safe"
				}

				res, err := engine.Decide(context.Background(), req)
				if err != nil {
					t.Errorf("concurrent Decide error: %v", err)
					return
				}
				if j%2 == 0 {
					if res.SelectedOutcome != "FAST_PATH" {
						t.Errorf("expected FAST_PATH, got %s", res.SelectedOutcome)
					}
				} else {
					if !res.Abstained {
						t.Errorf("expected abstained, got %s", res.SelectedOutcome)
					}
				}
			}
		}(i)
	}

	wg.Wait()
}

func TestRuleDecisionProvider_AllowedOutcomesFiltering(t *testing.T) {
	ruleProvider := decision.NewRuleDecisionProvider("shadowing-test")

	// Rule 1 matches, but its outcome is NOT in AllowedOutcomes
	ruleProvider.AddRule(decision.NewExactMatchRule(
		"rule-disallowed",
		"tier",
		"gold",
		"UNAUTHORIZED_ACTION",
		"gold tier rule",
	))

	// Rule 2 matches and its outcome IS in AllowedOutcomes
	ruleProvider.AddRule(decision.NewExactMatchRule(
		"rule-allowed",
		"tier",
		"gold",
		"FAST_PATH",
		"gold tier allowed fast path",
	))

	req := baseTestRequest()
	req.ContextProjection["tier"] = "gold"

	res, err := ruleProvider.Resolve(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.Abstained {
		t.Fatalf("expected rule 2 to match, but provider abstained")
	}
	if res.SelectedOutcome != "FAST_PATH" {
		t.Errorf("expected FAST_PATH, got %s", res.SelectedOutcome)
	}
}

func TestDecisionEngine_PanicRecovery(t *testing.T) {
	engine := decision.NewEngine()

	panickingProvider := &mockProvider{
		info: decision.ProviderInfo{Kind: decision.ProviderKindSpecialistModel, Name: "panicking-model"},
		resolveFn: func(ctx context.Context, req decision.DecisionRequest) (decision.DecisionResult, error) {
			panic("unexpected nil pointer in model runtime")
		},
	}

	fallbackProvider := &mockProvider{
		info: decision.ProviderInfo{Kind: decision.ProviderKindRule, Name: "fallback-rule"},
		resolveFn: func(ctx context.Context, req decision.DecisionRequest) (decision.DecisionResult, error) {
			return decision.DecisionResult{
				DecisionID:      req.DecisionID,
				SelectedOutcome: "STANDARD_PATH",
				Confidence:      1.0,
				ConfidenceKind:  decision.ConfidenceCalibratedProbability,
				DecidedAt:       time.Now().UTC(),
			}, nil
		},
	}

	engine.RegisterProvider(decision.DecisionTypeRouting, panickingProvider)
	engine.RegisterProvider(decision.DecisionTypeRouting, fallbackProvider)

	req := baseTestRequest()
	res, err := engine.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.SelectedOutcome != "STANDARD_PATH" {
		t.Errorf("expected STANDARD_PATH from fallback, got %s", res.SelectedOutcome)
	}
	if panickingProvider.Calls() != 1 {
		t.Errorf("expected panickingProvider to be called once, got %d", panickingProvider.Calls())
	}
	if fallbackProvider.Calls() != 1 {
		t.Errorf("expected fallbackProvider to be called once, got %d", fallbackProvider.Calls())
	}
}

func TestDecisionEngine_AuthoritativeProjectionHash(t *testing.T) {
	engine := decision.NewEngine()

	tamperingProvider := &mockProvider{
		info: decision.ProviderInfo{Kind: decision.ProviderKindRule, Name: "tampering-rule"},
		resolveFn: func(ctx context.Context, req decision.DecisionRequest) (decision.DecisionResult, error) {
			return decision.DecisionResult{
				DecisionID:      req.DecisionID,
				SelectedOutcome: "FAST_PATH",
				Confidence:      1.0,
				ConfidenceKind:  decision.ConfidenceCalibratedProbability,
				ProjectionHash:  "bogus_forged_hash",
				DecidedAt:       time.Now().UTC(),
			}, nil
		},
	}
	engine.RegisterProvider(decision.DecisionTypeRouting, tamperingProvider)

	req := baseTestRequest()
	res, err := engine.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedHash, err := decision.ComputeProjectionHash(req.ContextProjection)
	if err != nil {
		t.Fatalf("hash computation error: %v", err)
	}

	if res.ProjectionHash != expectedHash {
		t.Errorf("expected projection hash %s, got %s", expectedHash, res.ProjectionHash)
	}
}

func TestDecisionEngine_ZeroValueEngine(t *testing.T) {
	var zeroEngine decision.DecisionEngine

	req := baseTestRequest()
	res, err := zeroEngine.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error on zero engine: %v", err)
	}
	if !res.Abstained {
		t.Errorf("expected abstained on zero engine, got %s", res.SelectedOutcome)
	}

	// Should not panic on RegisterProvider
	zeroEngine.RegisterProvider(decision.DecisionTypeRouting, &mockProvider{
		info: decision.ProviderInfo{Kind: decision.ProviderKindRule, Name: "registered-on-zero"},
	})
}
