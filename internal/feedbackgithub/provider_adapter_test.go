package feedbackgithub

import (
	"context"
	"errors"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/fieldfeedback"
	"github.com/SofiaFlux/summa42/internal/operations"
)

// Test-only adapter wiring the GitHub sink into a fieldfeedback.Provider.

type Provider struct {
	inner *fieldfeedback.Provider
}

func New(cfg Config, feedback fieldfeedback.FeedbackReader) (*Provider, error) {
	if feedback == nil {
		return nil, errors.New("GitHub feedback provider requires feedback reader")
	}
	client, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	inner, err := fieldfeedback.NewProvider("github", client.repository, feedback, githubSink{client: client})
	if err != nil {
		return nil, err
	}
	return &Provider{inner: inner}, nil
}

func (p *Provider) Name() string                              { return p.inner.Name() }
func (p *Provider) Capability() string                        { return p.inner.Capability() }
func (p *Provider) EnforcementLevel() domain.EnforcementLevel { return p.inner.EnforcementLevel() }
func (p *Provider) AdapterVersion() string                    { return p.inner.AdapterVersion() }
func (p *Provider) AdapterVersionSemanticallyRelevant() bool {
	return p.inner.AdapterVersionSemanticallyRelevant()
}
func (p *Provider) SettleWindow() time.Duration { return p.inner.SettleWindow() }
func (p *Provider) MinimumRisk() string         { return p.inner.MinimumRisk() }
func (p *Provider) CanonicalIntent(intent operations.IntentDescriptor) ([]byte, error) {
	return p.inner.CanonicalIntent(intent)
}
func (p *Provider) CostProfile(intent operations.IntentDescriptor) (operations.CostProfile, error) {
	return p.inner.CostProfile(intent)
}
func (p *Provider) Dispatch(ctx context.Context, req operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	return p.inner.Dispatch(ctx, req)
}
func (p *Provider) LookupOutcome(ctx context.Context, req operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	return p.inner.LookupOutcome(ctx, req)
}
