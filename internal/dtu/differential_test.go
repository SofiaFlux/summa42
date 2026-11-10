package dtu_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SofiaFlux/summa42/internal/dtu"
	"github.com/SofiaFlux/summa42/internal/replay"
)

func TestDifferentialHarness_IdenticalProviders(t *testing.T) {
	ctx := context.Background()

	liveMeta := dtu.ProviderMetadata{
		ServiceID:  "mock-payment",
		ProviderID: "live-sandbox",
		Version:    "1.0.0",
		Fidelity:   dtu.FidelityT1Behavioral,
	}
	twinMeta := dtu.ProviderMetadata{
		ServiceID:  "mock-payment",
		ProviderID: "twin-payment",
		Version:    "1.0.0",
		Fidelity:   dtu.FidelityT1Behavioral,
	}

	liveProv := dtu.NewMockEnvironmentProvider(liveMeta, dtu.EnvModeLive)
	twinProv := dtu.NewMockEnvironmentProvider(twinMeta, dtu.EnvModeTwin)

	resp := dtu.InvocationResponse{
		Success:    true,
		StatusCode: 200,
		Data: map[string]any{
			"status": "APPROVED",
			"amount": 1000,
		},
	}
	liveProv.SetResponse("charge", resp)
	twinProv.SetResponse("charge", resp)

	harness := dtu.NewDifferentialHarness(liveProv, twinProv)

	requests := []dtu.InvocationRequest{
		{
			Action:         "charge",
			IdempotencyKey: "charge_1",
			Payload:        map[string]any{"amount": 1000},
		},
	}

	report, err := harness.Compare(ctx, requests, dtu.DifferentialOptions{})
	if err != nil {
		t.Fatalf("differential compare failed: %v", err)
	}

	if !report.Conforms {
		t.Errorf("expected report to conform when responses match")
	}
	if report.UnexpectedDivergences != 0 {
		t.Errorf("expected 0 unexpected divergences, got %d", report.UnexpectedDivergences)
	}
	if report.Matches != 1 {
		t.Errorf("expected 1 match, got %d", report.Matches)
	}
}

func TestDifferentialHarness_ValidationErrors(t *testing.T) {
	ctx := context.Background()
	liveProv := dtu.NewMockEnvironmentProvider(dtu.ProviderMetadata{ProviderID: "p1"}, dtu.EnvModeLive)

	// 1. Nil provider
	harnessNil := dtu.NewDifferentialHarness(liveProv, nil)
	_, err := harnessNil.Compare(ctx, []dtu.InvocationRequest{{Action: "a"}}, dtu.DifferentialOptions{})
	if !errors.Is(err, dtu.ErrNilProvider) {
		t.Errorf("expected ErrNilProvider, got %v", err)
	}

	// 2. Empty requests
	harness := dtu.NewDifferentialHarness(liveProv, liveProv)
	_, err = harness.Compare(ctx, nil, dtu.DifferentialOptions{})
	if !errors.Is(err, dtu.ErrEmptyRequests) {
		t.Errorf("expected ErrEmptyRequests, got %v", err)
	}

	// 3. Context cancellation
	cancCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = harness.Compare(cancCtx, []dtu.InvocationRequest{{Action: "a"}}, dtu.DifferentialOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestDifferentialHarness_DivergenceDetectionAndKnownFiltering(t *testing.T) {
	ctx := context.Background()

	liveMeta := dtu.ProviderMetadata{
		ServiceID:        "auth-service",
		ProviderID:       "live-auth-staging",
		Version:          "1.0.0",
		Fidelity:         dtu.FidelityT1Behavioral,
		KnownDivergences: []string{"rate_limiting_granularity"},
	}
	twinMeta := dtu.ProviderMetadata{
		ServiceID:        "auth-service",
		ProviderID:       "twin-auth",
		Version:          "1.0.0",
		Fidelity:         dtu.FidelityT1Behavioral,
		KnownDivergences: []string{"rate_limiting_granularity"},
	}

	liveProv := dtu.NewMockEnvironmentProvider(liveMeta, dtu.EnvModeLive)
	twinProv := dtu.NewMockEnvironmentProvider(twinMeta, dtu.EnvModeTwin)

	// Action 1: Matches
	liveProv.SetResponse("login", dtu.InvocationResponse{
		Success:    true,
		StatusCode: 200,
		Data:       map[string]any{"token": "tok_123"},
	})
	twinProv.SetResponse("login", dtu.InvocationResponse{
		Success:    true,
		StatusCode: 200,
		Data:       map[string]any{"token": "tok_123"},
	})

	// Action 2: Known divergence (status code difference in rate limiting)
	liveProv.SetResponse("throttle_check", dtu.InvocationResponse{
		Success:    false,
		StatusCode: 429,
		Error:      "Too Many Requests",
	})
	twinProv.SetResponse("throttle_check", dtu.InvocationResponse{
		Success:    true,
		StatusCode: 200,
		Data:       map[string]any{"rate_limit_bypass": true},
	})

	// Action 3: Unexpected divergence
	liveProv.SetResponse("refresh", dtu.InvocationResponse{
		Success:    true,
		StatusCode: 200,
		Data:       map[string]any{"refreshed": true},
	})
	twinProv.SetResponse("refresh", dtu.InvocationResponse{
		Success:    false,
		StatusCode: 500,
		Error:      "internal twin bug",
	})

	harness := dtu.NewDifferentialHarness(liveProv, twinProv)

	requests := []dtu.InvocationRequest{
		{Action: "login", IdempotencyKey: "k1"},
		{Action: "throttle_check", IdempotencyKey: "k2"},
		{Action: "refresh", IdempotencyKey: "k3"},
	}

	opts := dtu.DifferentialOptions{
		KnownDivergenceActions: map[string]string{
			"throttle_check": "rate_limiting_granularity",
		},
	}

	report, err := harness.Compare(ctx, requests, opts)
	if err != nil {
		t.Fatalf("compare failed: %v", err)
	}

	if report.Conforms {
		t.Errorf("expected report not to conform due to unexpected divergence")
	}
	if report.Matches != 1 {
		t.Errorf("expected 1 match, got %d", report.Matches)
	}
	if report.KnownDivergences != 1 {
		t.Errorf("expected 1 known divergence, got %d", report.KnownDivergences)
	}
	if report.UnexpectedDivergences != 1 {
		t.Errorf("expected 1 unexpected divergence, got %d", report.UnexpectedDivergences)
	}

	// Verify export of unexpected divergence to replay fixture
	var unexpDiv *dtu.DivergenceRecord
	for i := range report.Divergences {
		if !report.Divergences[i].Known {
			unexpDiv = &report.Divergences[i]
			break
		}
	}
	if unexpDiv == nil {
		t.Fatalf("expected to find unexpected divergence in report")
	}

	fixture, err := report.ExportReplayFixture(*unexpDiv, "regression_divergence")
	if err != nil {
		t.Fatalf("export replay fixture failed: %v", err)
	}
	if err := fixture.Validate(); err != nil {
		t.Fatalf("exported fixture validation failed: %v", err)
	}
	if fixture.Version != replay.FixtureVersion {
		t.Errorf("expected fixture version %d, got %d", replay.FixtureVersion, fixture.Version)
	}
	if !fixture.Invariants.RequireAuthorityCeilingHeld {
		t.Errorf("expected declared invariant on exported fixture")
	}
}

func TestDifferentialHarness_HostileAckLossIntegrationAndSanitization(t *testing.T) {
	ctx := context.Background()

	liveMeta := dtu.ProviderMetadata{
		ServiceID:  "ledger",
		ProviderID: "live-ledger",
		Version:    "1.0.0",
		Fidelity:   dtu.FidelityT1Behavioral,
	}
	twinMeta := dtu.ProviderMetadata{
		ServiceID:  "ledger",
		ProviderID: "hostile-twin-ledger",
		Version:    "1.0.0",
		Fidelity:   dtu.FidelityT2Adversarial,
	}

	liveProv := dtu.NewMockEnvironmentProvider(liveMeta, dtu.EnvModeLive)
	twinProv := dtu.NewHostileAckLossTwin(twinMeta, dtu.HostileAckLossConfig{
		DropAckActions: map[string]bool{"transfer": true},
	})

	liveProv.SetResponse("transfer", dtu.InvocationResponse{
		Success:    true,
		StatusCode: 200,
		Data: map[string]any{
			"status": "COMMITTED",
		},
	})

	harness := dtu.NewDifferentialHarness(liveProv, twinProv)

	requests := []dtu.InvocationRequest{
		{
			Action:         "transfer",
			IdempotencyKey: "tx_123",
			Payload: map[string]any{
				"amount": 500,
				"secret": "Bearer super_secret_key_12345",
			},
		},
	}

	report, err := harness.Compare(ctx, requests, dtu.DifferentialOptions{})
	if err != nil {
		t.Fatalf("compare failed: %v", err)
	}

	if report.Conforms {
		t.Errorf("expected divergence due to hostile ACK loss in twin")
	}
	if report.UnexpectedDivergences != 1 {
		t.Fatalf("expected 1 unexpected divergence, got %d", report.UnexpectedDivergences)
	}

	div := report.Divergences[0]
	if div.DivergenceType != "ERROR_DISPARITY" {
		t.Errorf("expected ERROR_DISPARITY, got %s", div.DivergenceType)
	}

	// Verify that secret in details is sanitized in replay fixture
	div.Details = "failed with token: Bearer super_secret_token_abc"
	fix, err := report.ExportReplayFixture(div, "hostile_regression")
	if err != nil {
		t.Fatalf("export fixture failed: %v", err)
	}
	if strings.Contains(fix.Task.Objective, "super_secret_token_abc") {
		t.Errorf("secret token leaked into fixture objective: %s", fix.Task.Objective)
	}
	if !strings.Contains(fix.Task.Objective, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in fixture objective: %s", fix.Task.Objective)
	}
}
