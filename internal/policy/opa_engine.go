package policy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/rego"
)

const defaultEvaluationTimeout = 250 * time.Millisecond

type OPAConfig struct {
	ModuleName    string
	Module        string
	PolicySetID   domain.ID
	PolicySetHash string
	Timeout       time.Duration
}

type OPAEngine struct {
	prepared               rego.PreparedEvalQuery
	prepareErr             error
	policySetID            domain.ID
	policySetHash          string
	policyCapabilitiesHash string
	timeout                time.Duration
}

type OPAMetadata struct {
	PolicySetID            domain.ID
	PolicySetHash          string
	PolicyCapabilitiesHash string
}

type rawDecision struct {
	Outcome           domain.PolicyOutcome `json:"outcome"`
	Limits            map[string]any       `json:"limits,omitempty"`
	RequiredApprovals []domain.ID          `json:"required_approvals,omitempty"`
	ReasonCodes       []string             `json:"reason_codes"`
}

func NewOPAEngine(cfg OPAConfig) *OPAEngine {
	if cfg.ModuleName == "" {
		cfg.ModuleName = "policy.rego"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultEvaluationTimeout
	}

	engine := &OPAEngine{
		policySetID: cfg.PolicySetID,
		timeout:     cfg.Timeout,
	}
	if cfg.PolicySetID == "" {
		engine.prepareErr = errors.New("policy set id is required")
		return engine
	}
	if cfg.Module == "" {
		engine.prepareErr = errors.New("policy module is required")
		return engine
	}

	moduleHash := digestBytes([]byte(cfg.Module))
	if cfg.PolicySetHash != "" && cfg.PolicySetHash != moduleHash {
		engine.prepareErr = fmt.Errorf("configured policy set hash does not match module content")
		return engine
	}
	engine.policySetHash = moduleHash

	caps := safeCapabilities()
	capsHash, err := capabilityProfileHash(caps)
	if err != nil {
		engine.prepareErr = fmt.Errorf("hash capability profile: %w", err)
		return engine
	}
	engine.policyCapabilitiesHash = capsHash

	prepareCtx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	prepared, err := rego.New(
		rego.Query("data.summa42.decision"),
		rego.Module(cfg.ModuleName, cfg.Module),
		rego.SetRegoVersion(ast.RegoV1),
		rego.Capabilities(caps),
		rego.StrictBuiltinErrors(true),
	).PrepareForEval(prepareCtx)
	if err != nil {
		engine.prepareErr = fmt.Errorf("prepare policy: %w", err)
		return engine
	}
	engine.prepared = prepared
	return engine
}

func (e *OPAEngine) Metadata() (OPAMetadata, error) {
	if e == nil {
		return OPAMetadata{}, errors.New("OPA engine is not configured")
	}
	if e.prepareErr != nil {
		return OPAMetadata{}, e.prepareErr
	}
	return OPAMetadata{
		PolicySetID:            e.policySetID,
		PolicySetHash:          e.policySetHash,
		PolicyCapabilitiesHash: e.policyCapabilitiesHash,
	}, nil
}

func (e *OPAEngine) Evaluate(ctx context.Context, in PolicyInput) (domain.PolicyDecision, error) {
	if e.prepareErr != nil {
		return domain.PolicyDecision{}, e.prepareErr
	}
	if in.Now.IsZero() {
		return domain.PolicyDecision{}, errors.New("policy input trusted time is required")
	}
	if err := ctx.Err(); err != nil {
		return domain.PolicyDecision{}, fmt.Errorf("policy evaluation context: %w", err)
	}

	in.Now = in.Now.UTC()
	inputJSON, err := json.Marshal(in)
	if err != nil {
		return domain.PolicyDecision{}, fmt.Errorf("marshal policy input: %w", err)
	}
	inputDigest := digestBytes(inputJSON)

	evalCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	results, err := e.prepared.Eval(evalCtx, rego.EvalInput(in), rego.EvalTime(in.Now))
	if err != nil {
		return domain.PolicyDecision{}, fmt.Errorf("evaluate policy: %w", err)
	}
	if len(results) != 1 || len(results[0].Expressions) != 1 {
		return domain.PolicyDecision{}, fmt.Errorf("policy decision must produce exactly one result and one expression; got %d results", len(results))
	}

	encoded, err := json.Marshal(results[0].Expressions[0].Value)
	if err != nil {
		return domain.PolicyDecision{}, fmt.Errorf("marshal policy decision: %w", err)
	}
	var raw rawDecision
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return domain.PolicyDecision{}, fmt.Errorf("decode policy decision: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return domain.PolicyDecision{}, err
	}
	if err := validateRawDecision(raw); err != nil {
		return domain.PolicyDecision{}, err
	}

	return domain.PolicyDecision{
		ID:                     domain.NewID("decision"),
		Outcome:                raw.Outcome,
		Limits:                 raw.Limits,
		RequiredApprovals:      raw.RequiredApprovals,
		ReasonCodes:            raw.ReasonCodes,
		PolicySetID:            e.policySetID,
		PolicySetHash:          e.policySetHash,
		PolicyCapabilitiesHash: e.policyCapabilitiesHash,
		InputDigest:            inputDigest,
		EvaluatedAt:            in.Now,
	}, nil
}

func validateRawDecision(raw rawDecision) error {
	switch raw.Outcome {
	case domain.PolicyAllow, domain.PolicyDeny:
	case domain.PolicyAllowWithLimit:
		if len(raw.Limits) == 0 {
			return errors.New("ALLOW_WITH_LIMIT requires non-empty limits")
		}
	case domain.PolicyRequireApproval:
		if len(raw.RequiredApprovals) == 0 {
			return errors.New("REQUIRE_APPROVAL requires at least one approval")
		}
	default:
		return fmt.Errorf("invalid policy outcome %q", raw.Outcome)
	}
	if len(raw.ReasonCodes) == 0 {
		return errors.New("policy decision requires at least one reason code")
	}
	for _, code := range raw.ReasonCodes {
		if code == "" {
			return errors.New("policy decision reason code cannot be empty")
		}
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("policy decision contains trailing JSON")
		}
		return fmt.Errorf("decode policy decision trailer: %w", err)
	}
	return nil
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
