package operations_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/SofiaFlux/summa42/internal/resources"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

type fakeProvider struct {
	mu                  sync.Mutex
	name                string
	loseAcknowledgement bool
	dispatchCount       int
	lookupCount         int
	outcomes            map[domain.ID]operations.ProviderOutcome
	onDispatch          func()
	lookupNoEffect      bool
	settleWindow        time.Duration
	minimumRisk         string
}

func newFakeProvider(name string) *fakeProvider {
	return &fakeProvider{name: name, outcomes: make(map[domain.ID]operations.ProviderOutcome)}
}

func (p *fakeProvider) Name() string                              { return p.name }
func (p *fakeProvider) Capability() string                        { return "external:" + p.name }
func (p *fakeProvider) EnforcementLevel() domain.EnforcementLevel { return domain.EnforcementEnforced }
func (p *fakeProvider) AdapterVersion() string                    { return "fake-v1" }
func (p *fakeProvider) AdapterVersionSemanticallyRelevant() bool  { return false }
func (p *fakeProvider) SettleWindow() time.Duration              { return p.settleWindow }
func (p *fakeProvider) MinimumRisk() string {
	if p.minimumRisk == "" {
		return "LOW"
	}
	return p.minimumRisk
}

func (p *fakeProvider) CanonicalIntent(descriptor operations.IntentDescriptor) ([]byte, error) {
	intent, ok := descriptor.(testutil.PurchaseIntent)
	if !ok {
		return nil, fmt.Errorf("fake provider does not own descriptor %T", descriptor)
	}
	if intent.SKU == "" || intent.Quantity <= 0 {
		return nil, errors.New("purchase requires sku and positive quantity")
	}
	return json.Marshal(intent)
}

func (p *fakeProvider) CostProfile(descriptor operations.IntentDescriptor) (operations.CostProfile, error) {
	intent, ok := descriptor.(testutil.PurchaseIntent)
	if !ok {
		return operations.CostProfile{}, fmt.Errorf("fake provider does not own descriptor %T", descriptor)
	}
	if intent.Quantity <= 0 {
		return operations.CostProfile{}, errors.New("purchase requires positive quantity")
	}
	return operations.CostProfile{
		MaxExposure: int64(intent.Quantity * 10),
		Enforceability: resources.Enforceability{
			CostControl:    resources.CostTechnicallyCapped,
			RequireHardCap: true,
			Source:         "fake-provider-technical-ceiling",
		},
	}, nil
}

func (p *fakeProvider) Dispatch(_ context.Context, request operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dispatchCount++
	if p.onDispatch != nil {
		p.onDispatch()
	}
	actualCost, err := fakePurchaseCost(request.CanonicalIntent)
	if err != nil {
		return operations.ProviderOutcome{}, err
	}
	outcome := operations.ProviderOutcome{
		State:             domain.OperationConfirmedEffect,
		ProviderReference: "fake-ref-" + string(request.OperationID),
		ActualCost:        actualCost,
	}
	p.outcomes[request.OperationID] = outcome
	if p.loseAcknowledgement {
		return operations.ProviderOutcome{}, domain.ErrOutcomeUnknown
	}
	return outcome, nil
}

func (p *fakeProvider) LookupOutcome(_ context.Context, request operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lookupCount++
	if p.lookupNoEffect {
		return operations.ProviderOutcome{State: domain.OperationConfirmedNoEffect}, nil
	}
	outcome, ok := p.outcomes[request.OperationID]
	if !ok {
		return operations.ProviderOutcome{State: domain.OperationOutcomeUnknown}, domain.ErrOutcomeUnknown
	}
	return outcome, nil
}

func (p *fakeProvider) SetLoseAcknowledgement(value bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loseAcknowledgement = value
}

func (p *fakeProvider) DispatchCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dispatchCount
}

func (p *fakeProvider) LookupCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lookupCount
}

func fakePurchaseCost(raw []byte) (int64, error) {
	var intent testutil.PurchaseIntent
	if err := json.Unmarshal(raw, &intent); err != nil {
		return 0, err
	}
	if intent.Quantity <= 0 {
		return 0, errors.New("invalid canonical purchase quantity")
	}
	return int64(intent.Quantity * 10), nil
}
