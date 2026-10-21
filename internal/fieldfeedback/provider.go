package fieldfeedback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/SofiaFlux/summa42/internal/resources"
)

const emitDescriptorType = "feedback.issue.emit.v1"

type EmitIntent struct {
	SanitizedFeedbackID domain.ID `json:"sanitized_feedback_id"`
	Destination         string    `json:"destination"`
}

func (EmitIntent) DescriptorType() string { return emitDescriptorType }

type FeedbackReader interface {
	SanitizedFeedback(context.Context, domain.ID) (domain.SanitizedFeedback, error)
}

type Sink interface {
	Create(context.Context, IssuePayload) (reference string, actualCost int64, err error)
	FindByMarker(context.Context, string) (reference string, found bool, err error)
}

type IssuePayload struct {
	Title  string
	Body   string
	Marker string
}

type Provider struct {
	name        string
	destination string
	feedback    FeedbackReader
	sink        Sink
}

func NewProvider(name, destination string, feedback FeedbackReader, sink Sink) (*Provider, error) {
	name = strings.TrimSpace(name)
	destination = strings.TrimSpace(destination)
	if name == "" || destination == "" || feedback == nil || sink == nil {
		return nil, errors.New("feedback provider requires name, destination, feedback reader, and sink")
	}
	return &Provider{name: name, destination: destination, feedback: feedback, sink: sink}, nil
}

func (p *Provider) Name() string { return p.name }

func (p *Provider) Capability() string {
	return "feedback." + p.name + ".issue.create"
}

// EnforcementLevel and the CostProfile below are provider-declared constants,
// not values measured at runtime.
func (p *Provider) EnforcementLevel() domain.EnforcementLevel {
	return domain.EnforcementEnforced
}

func (p *Provider) AdapterVersion() string { return "feedback-provider-v1" }

func (p *Provider) AdapterVersionSemanticallyRelevant() bool { return false }

// SettleWindow: the lookup only searches for the marker; an absent marker does
// not prove a delayed issue-create cannot still land.
func (p *Provider) SettleWindow() time.Duration { return providerSettleWindow }

// MinimumRisk: a feedback issue carries only redacted, Owner-configured content.
func (p *Provider) MinimumRisk() string { return "LOW" }

const providerSettleWindow = 10 * time.Minute

func (p *Provider) CanonicalIntent(descriptor operations.IntentDescriptor) ([]byte, error) {
	var intent EmitIntent
	switch typed := descriptor.(type) {
	case EmitIntent:
		intent = typed
	case *EmitIntent:
		if typed == nil {
			return nil, errors.New("feedback emit intent is nil")
		}
		intent = *typed
	default:
		return nil, fmt.Errorf("feedback provider rejects descriptor type %T", descriptor)
	}
	intent.SanitizedFeedbackID = domain.ID(strings.TrimSpace(string(intent.SanitizedFeedbackID)))
	intent.Destination = strings.TrimSpace(intent.Destination)
	if intent.SanitizedFeedbackID == "" || intent.Destination == "" {
		return nil, errors.New("sanitized feedback id and destination are required")
	}
	if intent.Destination != p.destination {
		return nil, fmt.Errorf("feedback destination %q is not bound provider destination", intent.Destination)
	}
	return json.Marshal(intent)
}

// CostProfile: CostTechnicallyCapped is provider-declared, not verified.
func (p *Provider) CostProfile(descriptor operations.IntentDescriptor) (operations.CostProfile, error) {
	if _, err := p.CanonicalIntent(descriptor); err != nil {
		return operations.CostProfile{}, err
	}
	return operations.CostProfile{
		MaxExposure: 1,
		Enforceability: resources.Enforceability{
			CostControl:    resources.CostTechnicallyCapped,
			RequireHardCap: false,
			Source:         "feedback issue emission",
		},
	}, nil
}

func (p *Provider) Dispatch(ctx context.Context, request operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	intent, artifact, err := p.resolve(ctx, request.CanonicalIntent)
	if err != nil {
		return operations.ProviderOutcome{}, err
	}
	payload, err := issuePayload(artifact)
	if err != nil {
		return operations.ProviderOutcome{}, err
	}
	if intent.Destination != p.destination {
		return operations.ProviderOutcome{}, errors.New("canonical feedback destination changed")
	}
	reference, actualCost, err := p.sink.Create(ctx, payload)
	if err != nil {
		return operations.ProviderOutcome{}, err
	}
	if strings.TrimSpace(reference) == "" {
		return operations.ProviderOutcome{}, errors.New("feedback sink confirmed create without a provider reference")
	}
	if actualCost < 0 {
		return operations.ProviderOutcome{}, errors.New("feedback sink returned negative actual cost")
	}
	return operations.ProviderOutcome{
		State:             domain.OperationConfirmedEffect,
		ProviderReference: strings.TrimSpace(reference),
		ActualCost:        actualCost,
	}, nil
}

func (p *Provider) LookupOutcome(ctx context.Context, request operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	_, artifact, err := p.resolve(ctx, request.CanonicalIntent)
	if err != nil {
		return operations.ProviderOutcome{}, err
	}
	payload, err := issuePayload(artifact)
	if err != nil {
		return operations.ProviderOutcome{}, err
	}
	reference, found, err := p.sink.FindByMarker(ctx, payload.Marker)
	if err != nil {
		return operations.ProviderOutcome{}, err
	}
	if !found {
		return operations.ProviderOutcome{State: domain.OperationOutcomeUnknown}, nil
	}
	if strings.TrimSpace(reference) == "" {
		return operations.ProviderOutcome{}, errors.New("feedback sink found marker without provider reference")
	}
	return operations.ProviderOutcome{
		State:             domain.OperationConfirmedEffect,
		ProviderReference: strings.TrimSpace(reference),
		ActualCost:        0,
	}, nil
}

func (p *Provider) resolve(ctx context.Context, canonical []byte) (EmitIntent, domain.SanitizedFeedback, error) {
	var intent EmitIntent
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&intent); err != nil {
		return EmitIntent{}, domain.SanitizedFeedback{}, fmt.Errorf("decode feedback canonical intent: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return EmitIntent{}, domain.SanitizedFeedback{}, errors.New("feedback canonical intent contains trailing JSON")
		}
		return EmitIntent{}, domain.SanitizedFeedback{}, fmt.Errorf("decode feedback canonical intent trailer: %w", err)
	}
	intent.SanitizedFeedbackID = domain.ID(strings.TrimSpace(string(intent.SanitizedFeedbackID)))
	intent.Destination = strings.TrimSpace(intent.Destination)
	if intent.SanitizedFeedbackID == "" || intent.Destination == "" || intent.Destination != p.destination {
		return EmitIntent{}, domain.SanitizedFeedback{}, errors.New("feedback canonical intent is outside provider binding")
	}
	artifact, err := p.feedback.SanitizedFeedback(ctx, intent.SanitizedFeedbackID)
	if err != nil {
		return EmitIntent{}, domain.SanitizedFeedback{}, fmt.Errorf("load immutable sanitized feedback: %w", err)
	}
	if artifact.ID != intent.SanitizedFeedbackID || strings.TrimSpace(artifact.ContentJSON) == "" ||
		strings.TrimSpace(artifact.Fingerprint) == "" {
		return EmitIntent{}, domain.SanitizedFeedback{}, errors.New("sanitized feedback artifact is incomplete")
	}
	return intent, artifact, nil
}

func issuePayload(artifact domain.SanitizedFeedback) (IssuePayload, error) {
	var projection struct {
		CategoryText string `json:"category"`
	}
	if err := json.Unmarshal([]byte(artifact.ContentJSON), &projection); err != nil {
		return IssuePayload{}, fmt.Errorf("decode sanitized feedback projection: %w", err)
	}
	category := strings.TrimSpace(projection.CategoryText)
	if category == "" {
		category = "FIELD_FEEDBACK"
	}
	return IssuePayload{
		Title:  "[Summa42] " + category,
		Body:   artifact.ContentJSON,
		Marker: "summa42-feedback:" + artifact.Fingerprint,
	}, nil
}
