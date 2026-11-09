package decision_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/decision"
)

type mockLLMClient struct {
	mu         sync.Mutex
	completeFn func(ctx context.Context, req decision.LLMCompletionRequest) (decision.LLMCompletionResponse, error)
	callsCount int
}

func (m *mockLLMClient) Complete(ctx context.Context, req decision.LLMCompletionRequest) (decision.LLMCompletionResponse, error) {
	m.mu.Lock()
	m.callsCount++
	m.mu.Unlock()
	if m.completeFn != nil {
		return m.completeFn(ctx, req)
	}
	return decision.LLMCompletionResponse{}, errors.New("completeFn not defined")
}

func (m *mockLLMClient) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.callsCount
}

func TestConstrainedLLMProvider_SuccessfulChoice(t *testing.T) {
	client := &mockLLMClient{
		completeFn: func(ctx context.Context, req decision.LLMCompletionRequest) (decision.LLMCompletionResponse, error) {
			// Verify prompt includes allowed outcomes
			if !strings.Contains(req.SystemPrompt, "FAST_PATH") || !strings.Contains(req.SystemPrompt, "STANDARD_PATH") {
				return decision.LLMCompletionResponse{}, fmt.Errorf("prompt missing allowed outcomes: %s", req.SystemPrompt)
			}
			return decision.LLMCompletionResponse{
				RawContent: "```json\n" + `{
					"selected_outcome": "STANDARD_PATH",
					"confidence": 0.92,
					"abstain": false,
					"reason": "complexity indicates standard path",
					"outcome_scores": {
						"STANDARD_PATH": 0.92,
						"FAST_PATH": 0.08
					}
				}` + "\n```",
				TokensIn:       120,
				TokensOut:      35,
				CostMicroCents: 450,
			}, nil
		},
	}

	provider := decision.NewConstrainedLLMDecisionProvider("cheap-llm", "v1.0", client, decision.LLMProviderConfig{
		MinConfidenceThreshold: 0.70,
	})

	req := baseTestRequest()
	res, err := provider.Resolve(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.Abstained {
		t.Fatalf("expected valid choice, got abstained: %s", res.AbstainReason)
	}
	if res.SelectedOutcome != "STANDARD_PATH" {
		t.Errorf("expected outcome STANDARD_PATH, got %s", res.SelectedOutcome)
	}
	if res.ConfidenceKind != decision.ConfidenceSelfReported {
		t.Errorf("expected ConfidenceSelfReported, got %s", res.ConfidenceKind)
	}
	if res.Confidence != 0.92 {
		t.Errorf("expected confidence 0.92, got %f", res.Confidence)
	}
	if res.Usage.TokensIn != 120 || res.Usage.TokensOut != 35 || res.Usage.CostMicroCents != 450 {
		t.Errorf("unexpected usage: %+v", res.Usage)
	}

	// Verify outcome validates through envelope
	if err := decision.ValidateEnvelope(req, res); err != nil {
		t.Errorf("envelope validation failed: %v", err)
	}
}

func TestConstrainedLLMProvider_ConfidenceRangeValidation(t *testing.T) {
	testCases := []struct {
		name       string
		confidence string
	}{
		{"greater than 1.0", "1.5"},
		{"negative confidence", "-0.1"},
		{"extreme positive", "100.0"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			client := &mockLLMClient{
				completeFn: func(ctx context.Context, req decision.LLMCompletionRequest) (decision.LLMCompletionResponse, error) {
					return decision.LLMCompletionResponse{
						RawContent: fmt.Sprintf(`{
							"selected_outcome": "FAST_PATH",
							"confidence": %s,
							"abstain": false,
							"reason": "out of bounds confidence"
						}`, tc.confidence),
					}, nil
				},
			}

			provider := decision.NewConstrainedLLMDecisionProvider("cheap-llm", "v1.0", client, decision.LLMProviderConfig{})
			req := baseTestRequest()
			res, err := provider.Resolve(context.Background(), req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !res.Abstained {
				t.Errorf("expected abstained for out of bounds confidence %s, got: %+v", tc.confidence, res)
			}
			if !strings.Contains(res.AbstainReason, "bounds") {
				t.Errorf("expected bounds in abstain reason, got: %s", res.AbstainReason)
			}
		})
	}
}

func TestConstrainedLLMProvider_TrailingJSONRejection(t *testing.T) {
	client := &mockLLMClient{
		completeFn: func(ctx context.Context, req decision.LLMCompletionRequest) (decision.LLMCompletionResponse, error) {
			return decision.LLMCompletionResponse{
				RawContent: `{"selected_outcome":"FAST_PATH","confidence":0.9,"abstain":false,"reason":"ok"} {"extra":"garbage"}`,
			}, nil
		},
	}

	provider := decision.NewConstrainedLLMDecisionProvider("cheap-llm", "v1.0", client, decision.LLMProviderConfig{})
	req := baseTestRequest()
	res, err := provider.Resolve(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !res.Abstained {
		t.Fatalf("expected abstained on trailing json, got outcome: %s", res.SelectedOutcome)
	}
	if !strings.Contains(res.AbstainReason, "trailing") {
		t.Errorf("expected reason to mention trailing, got: %s", res.AbstainReason)
	}
}

func TestConstrainedLLMProvider_UnknownFieldsRejection(t *testing.T) {
	client := &mockLLMClient{
		completeFn: func(ctx context.Context, req decision.LLMCompletionRequest) (decision.LLMCompletionResponse, error) {
			return decision.LLMCompletionResponse{
				RawContent: `{"selected_outcome":"FAST_PATH","confidence":0.9,"abstain":false,"reason":"ok","hallucinated_field":"dangerous"}`,
			}, nil
		},
	}

	provider := decision.NewConstrainedLLMDecisionProvider("cheap-llm", "v1.0", client, decision.LLMProviderConfig{})
	req := baseTestRequest()
	res, err := provider.Resolve(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !res.Abstained {
		t.Fatalf("expected abstained on unknown fields")
	}
}

func TestConstrainedLLMProvider_SanitizeOutcomeScores(t *testing.T) {
	client := &mockLLMClient{
		completeFn: func(ctx context.Context, req decision.LLMCompletionRequest) (decision.LLMCompletionResponse, error) {
			return decision.LLMCompletionResponse{
				RawContent: `{
					"selected_outcome": "FAST_PATH",
					"confidence": 0.85,
					"abstain": false,
					"reason": "good signal",
					"outcome_scores": {
						"FAST_PATH": 0.85,
						"ROGUE_OUTCOME": 0.15
					}
				}`,
			}, nil
		},
	}

	provider := decision.NewConstrainedLLMDecisionProvider("cheap-llm", "v1.0", client, decision.LLMProviderConfig{})
	req := baseTestRequest()
	res, err := provider.Resolve(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.Abstained {
		t.Fatalf("expected valid choice, got abstained: %s", res.AbstainReason)
	}
	if _, ok := res.OutcomeScores["ROGUE_OUTCOME"]; ok {
		t.Errorf("expected ROGUE_OUTCOME score to be sanitized, but was present")
	}
	if res.OutcomeScores["FAST_PATH"] != 0.85 {
		t.Errorf("expected FAST_PATH score 0.85, got %f", res.OutcomeScores["FAST_PATH"])
	}
}

func TestConstrainedLLMProvider_LowConfidenceAbstention(t *testing.T) {
	client := &mockLLMClient{
		completeFn: func(ctx context.Context, req decision.LLMCompletionRequest) (decision.LLMCompletionResponse, error) {
			return decision.LLMCompletionResponse{
				RawContent: `{
					"selected_outcome": "FAST_PATH",
					"confidence": 0.45,
					"abstain": false,
					"reason": "insufficient signal"
				}`,
			}, nil
		},
	}

	provider := decision.NewConstrainedLLMDecisionProvider("cheap-llm", "v1.0", client, decision.LLMProviderConfig{
		MinConfidenceThreshold: 0.80,
	})

	req := baseTestRequest()
	res, err := provider.Resolve(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !res.Abstained {
		t.Fatalf("expected provider to abstain due to low confidence (0.45 < 0.80), got outcome: %s", res.SelectedOutcome)
	}
	if res.SelectedOutcome != decision.OutcomeAbstain {
		t.Errorf("expected outcome ABSTAIN, got %s", res.SelectedOutcome)
	}
	if !strings.Contains(res.AbstainReason, "confidence") {
		t.Errorf("expected reason to mention confidence, got: %s", res.AbstainReason)
	}

	if err := decision.ValidateEnvelope(req, res); err != nil {
		t.Errorf("abstention failed envelope validation: %v", err)
	}
}

func TestConstrainedLLMProvider_HallucinatedOutcomeAbstention(t *testing.T) {
	client := &mockLLMClient{
		completeFn: func(ctx context.Context, req decision.LLMCompletionRequest) (decision.LLMCompletionResponse, error) {
			return decision.LLMCompletionResponse{
				RawContent: `{
					"selected_outcome": "DESTROY_WORLD",
					"confidence": 0.99,
					"abstain": false,
					"reason": "hallucinated action"
				}`,
			}, nil
		},
	}

	provider := decision.NewConstrainedLLMDecisionProvider("cheap-llm", "v1.0", client, decision.LLMProviderConfig{
		MinConfidenceThreshold: 0.70,
	})

	req := baseTestRequest()
	res, err := provider.Resolve(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Must abstain instead of returning an unauthorized outcome
	if !res.Abstained {
		t.Fatalf("expected provider to abstain on unauthorized outcome, got %s", res.SelectedOutcome)
	}
	if res.SelectedOutcome != decision.OutcomeAbstain {
		t.Errorf("expected outcome ABSTAIN, got %s", res.SelectedOutcome)
	}
	if !strings.Contains(res.AbstainReason, "DESTROY_WORLD") {
		t.Errorf("expected reason to mention hallucinated outcome, got: %s", res.AbstainReason)
	}
}

func TestConstrainedLLMProvider_UnparseableOutputAbstention(t *testing.T) {
	client := &mockLLMClient{
		completeFn: func(ctx context.Context, req decision.LLMCompletionRequest) (decision.LLMCompletionResponse, error) {
			return decision.LLMCompletionResponse{
				RawContent: "I think the answer is FAST_PATH but I am not returning JSON.",
			}, nil
		},
	}

	provider := decision.NewConstrainedLLMDecisionProvider("cheap-llm", "v1.0", client, decision.LLMProviderConfig{})

	req := baseTestRequest()
	res, err := provider.Resolve(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !res.Abstained {
		t.Fatalf("expected provider to abstain on unparseable output")
	}
	if res.SelectedOutcome != decision.OutcomeAbstain {
		t.Errorf("expected ABSTAIN outcome, got %s", res.SelectedOutcome)
	}
}

func TestConstrainedLLMProvider_ExplicitModelAbstain(t *testing.T) {
	client := &mockLLMClient{
		completeFn: func(ctx context.Context, req decision.LLMCompletionRequest) (decision.LLMCompletionResponse, error) {
			return decision.LLMCompletionResponse{
				RawContent: `{
					"selected_outcome": "ABSTAIN",
					"confidence": 0.0,
					"abstain": true,
					"reason": "conflicting signals in context"
				}`,
			}, nil
		},
	}

	provider := decision.NewConstrainedLLMDecisionProvider("cheap-llm", "v1.0", client, decision.LLMProviderConfig{})

	req := baseTestRequest()
	res, err := provider.Resolve(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !res.Abstained {
		t.Fatalf("expected explicit abstention")
	}
	if res.SelectedOutcome != decision.OutcomeAbstain {
		t.Errorf("expected SelectedOutcome ABSTAIN, got %s", res.SelectedOutcome)
	}
	if res.AbstainReason != "conflicting signals in context" {
		t.Errorf("unexpected reason: %s", res.AbstainReason)
	}
}

func TestConstrainedLLMProvider_EligibilityGating(t *testing.T) {
	client := &mockLLMClient{}
	provider := decision.NewConstrainedLLMDecisionProvider("cheap-llm", "v1.0", client, decision.LLMProviderConfig{})

	// 1. AllowRemoteLLM = false
	req1 := baseTestRequest()
	req1.Eligibility.AllowRemoteLLM = false
	res1, err := provider.Resolve(context.Background(), req1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res1.Abstained || !strings.Contains(res1.AbstainReason, "remote LLM not permitted") {
		t.Errorf("expected abstention on AllowRemoteLLM=false, got: %+v", res1)
	}

	// 2. RequireCalibratedConfidence = true
	req2 := baseTestRequest()
	req2.Eligibility.RequireCalibratedConfidence = true
	res2, err := provider.Resolve(context.Background(), req2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res2.Abstained || !strings.Contains(res2.AbstainReason, "calibrated") {
		t.Errorf("expected abstention on RequireCalibratedConfidence=true, got: %+v", res2)
	}

	if client.Calls() != 0 {
		t.Errorf("expected zero client calls under gating, got %d", client.Calls())
	}
}

func TestConstrainedLLMProvider_NilClient(t *testing.T) {
	provider := decision.NewConstrainedLLMDecisionProvider("cheap-llm", "v1.0", nil, decision.LLMProviderConfig{})
	req := baseTestRequest()
	_, err := provider.Resolve(context.Background(), req)
	if !errors.Is(err, decision.ErrNilLLMClient) {
		t.Errorf("expected ErrNilLLMClient, got %v", err)
	}
}

func TestConstrainedLLMProvider_TimeoutBehavior(t *testing.T) {
	client := &mockLLMClient{
		completeFn: func(ctx context.Context, req decision.LLMCompletionRequest) (decision.LLMCompletionResponse, error) {
			select {
			case <-ctx.Done():
				return decision.LLMCompletionResponse{}, ctx.Err()
			case <-time.After(100 * time.Millisecond):
				return decision.LLMCompletionResponse{}, errors.New("timeout")
			}
		},
	}

	provider := decision.NewConstrainedLLMDecisionProvider("cheap-llm", "v1.0", client, decision.LLMProviderConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	req := baseTestRequest()
	_, err := provider.Resolve(ctx, req)
	if err == nil {
		t.Fatalf("expected context error on timeout, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Errorf("expected DeadlineExceeded/Canceled, got %v", err)
	}
}

func TestConstrainedLLMProvider_IntegrationWithEngine(t *testing.T) {
	engine := decision.NewEngine()

	// 1. First provider: Rule provider that abstains
	ruleProvider := decision.NewRuleDecisionProvider("rule-tier")
	engine.RegisterProvider(decision.DecisionTypeRouting, ruleProvider)

	// 2. Second provider: LLM provider that selects outcome
	llmClient := &mockLLMClient{
		completeFn: func(ctx context.Context, req decision.LLMCompletionRequest) (decision.LLMCompletionResponse, error) {
			return decision.LLMCompletionResponse{
				RawContent: `{
					"selected_outcome": "FAST_PATH",
					"confidence": 0.88,
					"abstain": false,
					"reason": "suitable for fast path"
				}`,
				TokensIn:  50,
				TokensOut: 20,
			}, nil
		},
	}
	llmProvider := decision.NewConstrainedLLMDecisionProvider("llm-tier", "1.0", llmClient, decision.LLMProviderConfig{})
	engine.RegisterProvider(decision.DecisionTypeRouting, llmProvider)

	req := baseTestRequest()
	// RiskProfileLow allows SELF_REPORTED confidence
	req.RiskProfile = decision.RiskProfileLow
	res, err := engine.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.SelectedOutcome != "FAST_PATH" {
		t.Errorf("expected FAST_PATH, got %s", res.SelectedOutcome)
	}
	if res.Provider.Name != "llm-tier" {
		t.Errorf("expected provider llm-tier, got %s", res.Provider.Name)
	}
	if res.ConfidenceKind != decision.ConfidenceSelfReported {
		t.Errorf("expected ConfidenceSelfReported, got %s", res.ConfidenceKind)
	}
}
