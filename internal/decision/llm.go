package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

var (
	ErrNilLLMClient = errors.New("nil LLM client")
)

// LLMCompletionRequest defines the prompt and constraints passed to an LLM completion backend.
type LLMCompletionRequest struct {
	SystemPrompt string `json:"system_prompt"`
	UserPrompt   string `json:"user_prompt"`
	SchemaName   string `json:"schema_name,omitempty"`
}

// LLMCompletionResponse encapsulates the raw response and consumption telemetry from an LLM.
type LLMCompletionResponse struct {
	RawContent     string `json:"raw_content"`
	TokensIn       int    `json:"tokens_in,omitempty"`
	TokensOut      int    `json:"tokens_out,omitempty"`
	CostMicroCents int64  `json:"cost_micro_cents,omitempty"`
}

// LLMClient is the interface to invoke a constrained structured-output LLM backend.
// Implementations MUST be concurrency-safe.
type LLMClient interface {
	Complete(ctx context.Context, req LLMCompletionRequest) (LLMCompletionResponse, error)
}

// LLMProviderConfig contains tuning parameters for the constrained LLM provider.
type LLMProviderConfig struct {
	MinConfidenceThreshold float64
}

// ConstrainedLLMDecisionProvider implements DecisionProvider using a structured-output LLM.
// It is stateless after construction and safe for concurrent use.
type ConstrainedLLMDecisionProvider struct {
	name    string
	version string
	client  LLMClient
	config  LLMProviderConfig
}

// NewConstrainedLLMDecisionProvider constructs a new ConstrainedLLMDecisionProvider.
func NewConstrainedLLMDecisionProvider(name, version string, client LLMClient, config LLMProviderConfig) *ConstrainedLLMDecisionProvider {
	if config.MinConfidenceThreshold < 0 || math.IsNaN(config.MinConfidenceThreshold) {
		config.MinConfidenceThreshold = 0
	} else if config.MinConfidenceThreshold > 1.0 {
		config.MinConfidenceThreshold = 1.0
	}
	return &ConstrainedLLMDecisionProvider{
		name:    name,
		version: version,
		client:  client,
		config:  config,
	}
}

func (p *ConstrainedLLMDecisionProvider) Info() ProviderInfo {
	return ProviderInfo{
		Kind:    ProviderKindConstrainedLLM,
		Name:    p.name,
		Version: p.version,
	}
}

type modelOutputJSON struct {
	SelectedOutcome string             `json:"selected_outcome"`
	Confidence      float64            `json:"confidence"`
	Abstain         bool               `json:"abstain"`
	Reason          string             `json:"reason"`
	OutcomeScores   map[string]float64 `json:"outcome_scores,omitempty"`
}

func (p *ConstrainedLLMDecisionProvider) abstainResult(req DecisionRequest, reason string, latency time.Duration, usage DecisionUsage) DecisionResult {
	return DecisionResult{
		DecisionID:      req.DecisionID,
		SelectedOutcome: OutcomeAbstain,
		Abstained:       true,
		AbstainReason:   reason,
		Confidence:      0.0,
		ConfidenceKind:  ConfidenceNone,
		Provider:        p.Info(),
		Latency:         latency,
		Usage:           usage,
		DecidedAt:       time.Now().UTC(),
	}
}

func (p *ConstrainedLLMDecisionProvider) Resolve(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
	if p.client == nil {
		return DecisionResult{}, ErrNilLLMClient
	}

	// 1. Eligibility pre-checks
	if !req.Eligibility.AllowRemoteLLM {
		return p.abstainResult(req, "remote LLM not permitted by request eligibility", 0, DecisionUsage{}), nil
	}

	if req.Eligibility.RequireCalibratedConfidence {
		// LLMs produce self-reported guesses, not calibrated probabilities.
		// Abstain honestly to avoid masquerading uncalibrated floats.
		return p.abstainResult(req, "constrained LLM produces self-reported confidence, not calibrated probabilities", 0, DecisionUsage{}), nil
	}

	// 2. Format constrained prompts
	systemPrompt := p.buildSystemPrompt(req)
	userPrompt, err := p.buildUserPrompt(req)
	if err != nil {
		return DecisionResult{}, fmt.Errorf("failed to build user prompt: %w", err)
	}

	start := time.Now()
	resp, err := p.client.Complete(ctx, LLMCompletionRequest{
		SystemPrompt: systemPrompt,
		UserPrompt:   userPrompt,
		SchemaName:   "summa42_micro_decision_v1",
	})
	latency := time.Since(start)
	if err != nil {
		return DecisionResult{}, err
	}

	usage := DecisionUsage{
		TokensIn:       resp.TokensIn,
		TokensOut:      resp.TokensOut,
		CostMicroCents: resp.CostMicroCents,
	}

	// 3. Parse and strictly validate structured model output
	res, err := p.parseModelOutput(req, resp.RawContent)
	if err != nil {
		// Parsing or format failure: abstain honestly rather than hallucinating
		return p.abstainResult(req, fmt.Sprintf("unparseable or invalid model output: %v", err), latency, usage), nil
	}

	res.Latency = latency
	res.Usage = usage

	return res, nil
}

func (p *ConstrainedLLMDecisionProvider) buildSystemPrompt(req DecisionRequest) string {
	var b strings.Builder
	b.WriteString("You are a deterministic micro-decision classifier for Summa42.\n")
	b.WriteString("You MUST choose EXACTLY ONE outcome from the following allowed outcomes:\n")
	for _, outcome := range req.AllowedOutcomes {
		fmt.Fprintf(&b, "- %s\n", outcome)
	}
	b.WriteString("- ABSTAIN (if the evidence is ambiguous, incomplete, or none apply)\n\n")
	b.WriteString("Return ONLY valid JSON matching this schema:\n")
	b.WriteString("{\n")
	b.WriteString(`  "selected_outcome": "<one of the allowed outcomes or ABSTAIN>",` + "\n")
	b.WriteString(`  "confidence": <float between 0.0 and 1.0>,` + "\n")
	b.WriteString(`  "abstain": <true if abstaining, false otherwise>,` + "\n")
	b.WriteString(`  "reason": "<short factual explanation>",` + "\n")
	b.WriteString(`  "outcome_scores": {"<outcome>": <score>}` + "\n")
	b.WriteString("}\n")
	return b.String()
}

func (p *ConstrainedLLMDecisionProvider) buildUserPrompt(req DecisionRequest) (string, error) {
	projBytes, err := json.MarshalIndent(req.ContextProjection, "", "  ")
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("Decision ID: %s\nDecision Type: %s\nSubject: %s (ID: %s)\nRisk Profile: %s\nContext:\n%s\n",
		req.DecisionID,
		req.DecisionType,
		req.SubjectRef.Kind,
		req.SubjectRef.ID,
		req.RiskProfile,
		string(projBytes),
	), nil
}

func (p *ConstrainedLLMDecisionProvider) parseModelOutput(req DecisionRequest, raw string) (DecisionResult, error) {
	cleaned := strings.TrimSpace(raw)
	// Strip markdown code fences if wrapped by model
	if strings.HasPrefix(cleaned, "```") {
		lines := strings.Split(cleaned, "\n")
		if len(lines) >= 2 && strings.HasPrefix(lines[0], "```") && strings.HasSuffix(lines[len(lines)-1], "```") {
			cleaned = strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
		}
	}

	var parsed modelOutputJSON
	dec := json.NewDecoder(bytes.NewReader([]byte(cleaned)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&parsed); err != nil {
		return DecisionResult{}, fmt.Errorf("malformed json: %w", err)
	}

	// Ensure no trailing tokens after JSON object
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return DecisionResult{}, fmt.Errorf("unexpected trailing data after JSON object")
	}

	// 1. Explicit abstention
	if parsed.Abstain || parsed.SelectedOutcome == OutcomeAbstain {
		reason := parsed.Reason
		if reason == "" {
			reason = "model explicitly abstained"
		}
		return p.abstainResult(req, reason, 0, DecisionUsage{}), nil
	}

	// 2. Validate confidence bounds [0.0, 1.0] and non-NaN/Inf
	if math.IsNaN(parsed.Confidence) || math.IsInf(parsed.Confidence, 0) || parsed.Confidence < 0.0 || parsed.Confidence > 1.0 {
		return p.abstainResult(req, fmt.Sprintf("model reported confidence out of bounds [0.0, 1.0]: %f", parsed.Confidence), 0, DecisionUsage{}), nil
	}

	// 3. Low-confidence threshold check
	if p.config.MinConfidenceThreshold > 0 && parsed.Confidence < p.config.MinConfidenceThreshold {
		reason := parsed.Reason
		if reason == "" {
			reason = "insufficient confidence"
		}
		return p.abstainResult(req, fmt.Sprintf("confidence %f below threshold %f: %s", parsed.Confidence, p.config.MinConfidenceThreshold, reason), 0, DecisionUsage{}), nil
	}

	// 4. Allowed outcome check: reject hallucinations
	allowedSet := make(map[string]struct{}, len(req.AllowedOutcomes))
	for _, ao := range req.AllowedOutcomes {
		allowedSet[ao] = struct{}{}
	}

	if _, ok := allowedSet[parsed.SelectedOutcome]; !ok {
		return p.abstainResult(req, fmt.Sprintf("model proposed outcome %q not in allowed list %v", parsed.SelectedOutcome, req.AllowedOutcomes), 0, DecisionUsage{}), nil
	}

	// 5. Sanitize outcome scores to only allowed outcomes
	var sanitizedScores map[string]float64
	if len(parsed.OutcomeScores) > 0 {
		sanitizedScores = make(map[string]float64)
		for outcome, score := range parsed.OutcomeScores {
			if math.IsNaN(score) || math.IsInf(score, 0) {
				continue
			}
			if _, ok := allowedSet[outcome]; ok {
				sanitizedScores[outcome] = score
			}
		}
	}

	return DecisionResult{
		DecisionID:      req.DecisionID,
		SelectedOutcome: parsed.SelectedOutcome,
		OutcomeScores:   sanitizedScores,
		Confidence:      parsed.Confidence,
		ConfidenceKind:  ConfidenceSelfReported,
		Abstained:       false,
		AbstainReason:   parsed.Reason,
		Provider:        p.Info(),
		DecidedAt:       time.Now().UTC(),
	}, nil
}
