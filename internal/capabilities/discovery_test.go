package capabilities_test

import (
	"context"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/approvals"
	"github.com/SofiaFlux/summa42/internal/capabilities"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

type mockCandidateProvider struct {
	name        string
	semantic    string
	version     string
	enforcement domain.EnforcementLevel
	cost        float64
	health      capabilities.Health
	available   bool
	evidence    []string
}

func (m *mockCandidateProvider) Name() string { return m.name }

func (m *mockCandidateProvider) Call(_ context.Context, capability string, request any) (any, error) {
	return map[string]any{"provider": m.name, "capability": capability, "result": "ok", "request": request}, nil
}

func (m *mockCandidateProvider) Advertise(_ context.Context) ([]capabilities.Definition, error) {
	return []capabilities.Definition{{
		ID: domain.ID("cap_" + m.name + "_" + m.semantic),
		Skill: capabilities.Skill{
			Name:    m.semantic,
			Version: m.version,
		},
		Access: capabilities.Access{
			Provider: m.name,
			Context:  "sandbox",
		},
		Authority: capabilities.AuthorityRequirement{
			Capabilities: []string{m.semantic},
		},
		Environment: capabilities.EnvironmentRequirement{
			MinimumEnforcement: m.enforcement,
		},
	}}, nil
}

func (m *mockCandidateProvider) Probe(_ context.Context, _ capabilities.Definition) (capabilities.ProbeResult, error) {
	ev := m.evidence
	if len(ev) == 0 {
		ev = []string{m.name + ":probe:ok"}
	}
	return capabilities.ProbeResult{
		Enforcement: m.enforcement,
		Evidence:    ev,
		CostMetadata: map[string]any{
			"unit_cost": m.cost,
		},
		Health:    m.health,
		Available: m.available,
	}, nil
}

type staticDiscoverySource struct {
	name       string
	candidates []capabilities.CandidateProvider
}

func (s *staticDiscoverySource) Name() string { return s.name }
func (s *staticDiscoverySource) Discover(_ context.Context, _ capabilities.CapabilityGap) ([]capabilities.CandidateProvider, error) {
	return s.candidates, nil
}

func setupDiscoveryFixture(t *testing.T) (
	context.Context,
	*capabilities.Registry,
	*capabilities.DiscoveryEngine,
	*approvals.Service,
	domain.ID,
) {
	t.Helper()
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC))
	purposes := purpose.New(store, clk)
	execSvc := execution.New(store, clk, purposes)
	approvalsSvc := approvals.New(store, clk)
	ownerID := domain.ID("owner-alice")

	registry := capabilities.NewRegistry(store, clk, execSvc)
	engine := capabilities.NewDiscoveryEngine(store, clk, registry, approvalsSvc, ownerID)

	return ctx, registry, engine, approvalsSvc, ownerID
}

func TestDiscoveryDetectGapMissingCapability(t *testing.T) {
	ctx, _, engine, _, _ := setupDiscoveryFixture(t)

	gap, err := engine.DetectGap(ctx, "audio.tts", capabilities.GapRequirements{
		MinimumEnforcement: domain.EnforcementEnforced,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gap == nil {
		t.Fatal("expected gap for missing capability, got nil")
	}
	if gap.Reason != capabilities.GapReasonMissingCapability {
		t.Fatalf("gap reason = %s, want %s", gap.Reason, capabilities.GapReasonMissingCapability)
	}
	if gap.SemanticName != "audio.tts" {
		t.Fatalf("gap semantic name = %s, want audio.tts", gap.SemanticName)
	}
	if gap.RequiredEnforcement != domain.EnforcementEnforced {
		t.Fatalf("required enforcement = %s, want %s", gap.RequiredEnforcement, domain.EnforcementEnforced)
	}
}

func TestDiscoveryDetectGapSuboptimalIncumbent(t *testing.T) {
	ctx, registry, engine, _, _ := setupDiscoveryFixture(t)

	// Register incumbent provider: nano-banana
	incumbent := &mockCandidateProvider{
		name:        "nano-banana",
		semantic:    "image.generate",
		version:     "v1",
		enforcement: domain.EnforcementPartial,
		cost:        1.00,
		health:      capabilities.HealthHealthy,
		available:   true,
	}
	if err := registry.RegisterProvider(incumbent); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.AssessProvider(ctx, incumbent.Name()); err != nil {
		t.Fatal(err)
	}

	// 1. Minimum enforcement ENFORCED vs incumbent PARTIAL
	gapEnforce, err := engine.DetectGap(ctx, "image.generate", capabilities.GapRequirements{
		MinimumEnforcement: domain.EnforcementEnforced,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gapEnforce == nil || gapEnforce.Reason != capabilities.GapReasonInsufficientEnforce {
		t.Fatalf("expected InsufficientEnforce gap, got %#v", gapEnforce)
	}
	if gapEnforce.CurrentIncumbent != "nano-banana" {
		t.Fatalf("incumbent = %s, want nano-banana", gapEnforce.CurrentIncumbent)
	}

	// 2. MaxAcceptableCost 0.50 vs incumbent 1.00
	gapCost, err := engine.DetectGap(ctx, "image.generate", capabilities.GapRequirements{
		MinimumEnforcement: domain.EnforcementPartial,
		MaxAcceptableCost:  0.50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gapCost == nil || gapCost.Reason != capabilities.GapReasonSuboptimalCost {
		t.Fatalf("expected SuboptimalCost gap, got %#v", gapCost)
	}
	if gapCost.IncumbentCost != 1.00 {
		t.Fatalf("incumbent cost = %f, want 1.00", gapCost.IncumbentCost)
	}

	// 3. Requirements met -> no gap
	gapNone, err := engine.DetectGap(ctx, "image.generate", capabilities.GapRequirements{
		MinimumEnforcement: domain.EnforcementPartial,
		MaxAcceptableCost:  1.50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gapNone != nil {
		t.Fatalf("expected no gap, got %#v", gapNone)
	}
}

func TestDiscoveryCandidateLifecycleAndApprovalGate(t *testing.T) {
	ctx, registry, engine, approvalsSvc, ownerID := setupDiscoveryFixture(t)

	// 1. Detect gap for missing NPC voice-over capability
	gap, err := engine.DetectGap(ctx, "audio.tts", capabilities.GapRequirements{
		MinimumEnforcement: domain.EnforcementEnforced,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 2. Register discovery source that finds ElevenLabs
	elevenLabsMock := &mockCandidateProvider{
		name:        "elevenlabs",
		semantic:    "audio.tts",
		version:     "v2",
		enforcement: domain.EnforcementEnforced,
		cost:        0.05,
		health:      capabilities.HealthHealthy,
		available:   true,
		evidence:    []string{"elevenlabs:voice_synthesis:verified"},
	}

	source := &staticDiscoverySource{
		name: "catalog-discovery",
		candidates: []capabilities.CandidateProvider{{
			ProviderName:          "elevenlabs",
			SemanticName:          "audio.tts",
			SemanticVersion:       "v2",
			AccessContext:         "cloud-api",
			AuthorityRequirements: []string{"audio.tts", "network.external"},
			MinimumEnforcement:    domain.EnforcementEnforced,
			Description:           "ElevenLabs realistic neural TTS",
			EstimatedCost:         0.05,
			QualityScore:          0.95,
			License:               "Proprietary-Approved",
			Factory: func() (capabilities.AssessableProvider, error) {
				return elevenLabsMock, nil
			},
		}},
	}
	if err := engine.RegisterSource(source); err != nil {
		t.Fatal(err)
	}

	// 3. Discover candidates
	candidates, err := engine.DiscoverCandidates(ctx, *gap)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("discovered candidates = %d, want 1", len(candidates))
	}
	cand := candidates[0]
	if cand.ProviderName != "elevenlabs" {
		t.Fatalf("candidate name = %s, want elevenlabs", cand.ProviderName)
	}

	// 4. Evaluate candidate in sandboxed probe
	eval, err := engine.EvaluateCandidate(ctx, cand, *gap)
	if err != nil {
		t.Fatal(err)
	}
	if !eval.Recommended {
		t.Fatalf("expected candidate to be recommended, got false; eval = %#v", eval)
	}
	if !eval.MeetsEnforcement || !eval.LicenseAcceptable {
		t.Fatalf("eval check failed: meets_enforce=%v, license_ok=%v", eval.MeetsEnforcement, eval.LicenseAcceptable)
	}

	// 5. Propose acquisition (requires Owner approval because of network.external and cost)
	proposal, err := engine.ProposeAcquisition(ctx, eval, *gap, capabilities.ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !proposal.RequiresApproval {
		t.Fatal("expected proposal to require approval")
	}
	if proposal.State != capabilities.ProposalApprovalPending {
		t.Fatalf("proposal state = %s, want %s", proposal.State, capabilities.ProposalApprovalPending)
	}
	if proposal.ApprovalID == "" {
		t.Fatal("expected approval id to be populated")
	}

	// 6. Activating before approval must fail
	err = engine.ActivateAcquisition(ctx, proposal.ID, cand.Factory)
	if err == nil {
		t.Fatal("activating unapproved acquisition must fail, but succeeded")
	}

	// 7. Owner reviews and approves
	approvalRec, err := approvalsSvc.Get(ctx, proposal.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	if err := approvalsSvc.Approve(ctx, proposal.ApprovalID, ownerID, approvalRec.RequestDigest); err != nil {
		t.Fatal(err)
	}

	// 8. Now activation succeeds
	if err := engine.ActivateAcquisition(ctx, proposal.ID, cand.Factory); err != nil {
		t.Fatalf("activation failed after approval: %v", err)
	}

	// Verify proposal state in DB is ACTIVATED
	updatedProposal, err := engine.GetProposal(ctx, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedProposal.State != capabilities.ProposalActivated {
		t.Fatalf("proposal state = %s, want %s", updatedProposal.State, capabilities.ProposalActivated)
	}

	// 9. Verify capability is now live in Registry with active assessment
	assessment, err := registry.LatestAssessment(ctx, "audio.tts")
	if err != nil {
		t.Fatalf("latest assessment error: %v", err)
	}
	if assessment.Provider != "elevenlabs" || !assessment.Available || assessment.Enforcement != domain.EnforcementEnforced {
		t.Fatalf("unexpected live assessment: %#v", assessment)
	}
}

func TestDiscoveryZeroAuthorityCandidatePreapproved(t *testing.T) {
	ctx, registry, engine, _, _ := setupDiscoveryFixture(t)

	gap := capabilities.CapabilityGap{
		SemanticName:        "local.hash",
		RequiredEnforcement: domain.EnforcementUnenforced,
		Reason:              capabilities.GapReasonMissingCapability,
	}

	localMock := &mockCandidateProvider{
		name:        "local-sha256",
		semantic:    "local.hash",
		version:     "v1",
		enforcement: domain.EnforcementUnenforced,
		cost:        0,
		health:      capabilities.HealthHealthy,
		available:   true,
	}

	cand := capabilities.CandidateProvider{
		ProviderName:          "local-sha256",
		SemanticName:          "local.hash",
		SemanticVersion:       "v1",
		AccessContext:         "in-process",
		AuthorityRequirements: nil, // Zero authority expansion
		MinimumEnforcement:    domain.EnforcementUnenforced,
		EstimatedCost:         0,
		QualityScore:          1.0,
		License:               "MIT",
		Factory: func() (capabilities.AssessableProvider, error) {
			return localMock, nil
		},
	}

	eval, err := engine.EvaluateCandidate(ctx, cand, gap)
	if err != nil {
		t.Fatal(err)
	}
	if !eval.Recommended {
		t.Fatalf("eval not recommended: %#v", eval)
	}

	proposal, err := engine.ProposeAcquisition(ctx, eval, gap, capabilities.ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.RequiresApproval {
		t.Fatal("expected zero-authority local candidate to NOT require approval")
	}
	if proposal.State != capabilities.ProposalApproved {
		t.Fatalf("proposal state = %s, want APPROVED", proposal.State)
	}

	// Can activate directly
	if err := engine.ActivateAcquisition(ctx, proposal.ID, cand.Factory); err != nil {
		t.Fatalf("direct activation error: %v", err)
	}

	assessment, err := registry.LatestAssessment(ctx, "local.hash")
	if err != nil {
		t.Fatal(err)
	}
	if assessment.Provider != "local-sha256" {
		t.Fatalf("provider = %s, want local-sha256", assessment.Provider)
	}
}

func TestDiscoveryRejectProposal(t *testing.T) {
	ctx, _, engine, _, ownerID := setupDiscoveryFixture(t)

	gap := capabilities.CapabilityGap{
		SemanticName: "sketchy.cap",
		Reason:       capabilities.GapReasonMissingCapability,
	}
	mock := &mockCandidateProvider{
		name:        "sketchy",
		semantic:    "sketchy.cap",
		version:     "v1",
		enforcement: domain.EnforcementUnenforced,
		health:      capabilities.HealthHealthy,
		available:   true,
	}
	cand := capabilities.CandidateProvider{
		ProviderName:          "sketchy",
		SemanticName:          "sketchy.cap",
		SemanticVersion:       "v1",
		AccessContext:         "unknown",
		AuthorityRequirements: []string{"root"},
		MinimumEnforcement:    domain.EnforcementUnenforced,
		License:               "MIT",
		Factory: func() (capabilities.AssessableProvider, error) {
			return mock, nil
		},
	}

	eval, err := engine.EvaluateCandidate(ctx, cand, gap)
	if err != nil {
		t.Fatal(err)
	}

	proposal, err := engine.ProposeAcquisition(ctx, eval, gap, capabilities.ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if err := engine.RejectProposal(ctx, proposal.ID, ownerID, "rejected untrusted provider"); err != nil {
		t.Fatal(err)
	}

	updated, err := engine.GetProposal(ctx, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != capabilities.ProposalRejected {
		t.Fatalf("proposal state = %s, want REJECTED", updated.State)
	}

	// Activation must fail
	if err := engine.ActivateAcquisition(ctx, proposal.ID, cand.Factory); err == nil {
		t.Fatal("activation of rejected proposal must fail, but succeeded")
	}
}
