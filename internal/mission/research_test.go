package mission_test

import (
	"context"
	"strings"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/mission"
)

func TestMissionResearcher_BasicCompilation(t *testing.T) {
	ctx := context.Background()
	researcher := mission.NewResearcher()

	input := mission.ResearchInput{
		OwnerStatement: "Develop a high-throughput transaction processing engine with zero double-spending and strict GDPR data protection.",
		OrgContext:     "Fintech enterprise environment subject to strict regulatory compliance and audit logs.",
		Policies: []string{
			"All financial state modifications must be strictly audited",
			"GDPR article 17 right to erasure must be respected",
		},
		EvidenceRefs: []string{"policy-doc-v2.1", "compliance-checklist-2026"},
		AuthorPrincipal: mission.Principal{
			ID:   "researcher-agent-01",
			Kind: mission.PrincipalKindWorker,
		},
	}

	proposal, err := researcher.Research(ctx, input)
	if err != nil {
		t.Fatalf("unexpected error during research: %v", err)
	}

	if proposal == nil {
		t.Fatal("expected non-nil proposal")
	}

	// Non-authoritative draft state
	if proposal.ProposedMission.State != mission.MissionStateDraft {
		t.Errorf("expected proposal state DRAFT, got %s", proposal.ProposedMission.State)
	}
	if proposal.ProposedMission.Version != 1 {
		t.Errorf("expected version 1, got %d", proposal.ProposedMission.Version)
	}

	// Completeness validation: all 10 governance dimensions should be populated
	if err := proposal.ProposedMission.ValidateCompleteness(); err != nil {
		t.Errorf("expected complete mission schema in proposal, got validation error: %v", err)
	}

	// Provenance checks
	if proposal.ProposedMission.Provenance.AuthorPrincipal != "researcher-agent-01" {
		t.Errorf("expected author researcher-agent-01, got %s", proposal.ProposedMission.Provenance.AuthorPrincipal)
	}
	if len(proposal.Evidence.InputHash) == 0 {
		t.Error("expected deterministic evidence input hash")
	}
	if len(proposal.Evidence.SourceInputs) == 0 {
		t.Error("expected captured source inputs in evidence")
	}

	// Determinism check: identical inputs yield identical findings and proposal hash
	proposal2, err2 := researcher.Research(ctx, input)
	if err2 != nil {
		t.Fatalf("unexpected error on second run: %v", err2)
	}
	if proposal.Evidence.InputHash != proposal2.Evidence.InputHash {
		t.Errorf("expected deterministic input hash, got %s vs %s", proposal.Evidence.InputHash, proposal2.Evidence.InputHash)
	}
	if len(proposal.Conflicts) != len(proposal2.Conflicts) {
		t.Errorf("conflicts count mismatch between deterministic runs")
	}
}

func TestMissionResearcher_AmbiguityDetection(t *testing.T) {
	ctx := context.Background()
	researcher := mission.NewResearcher()

	input := mission.ResearchInput{
		OwnerStatement: "Build the fastest and best cloud platform with maximum uptime and lowest possible cost as soon as possible.",
		AuthorPrincipal: mission.Principal{
			ID:   "human-owner",
			Kind: mission.PrincipalKindHuman,
		},
	}

	proposal, err := researcher.Research(ctx, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !proposal.RequiresClarification {
		t.Error("expected RequiresClarification=true for highly ambiguous statement")
	}

	if len(proposal.Ambiguities) < 3 {
		t.Fatalf("expected at least 3 ambiguity findings, got %d", len(proposal.Ambiguities))
	}

	var foundFastest, foundBest, foundAsap bool
	for _, amb := range proposal.Ambiguities {
		termLower := strings.ToLower(amb.Term)
		if strings.Contains(termLower, "fastest") {
			foundFastest = true
		}
		if strings.Contains(termLower, "best") {
			foundBest = true
		}
		if strings.Contains(termLower, "as soon as possible") || strings.Contains(termLower, "lowest possible") {
			foundAsap = true
		}
		if amb.Recommendation == "" {
			t.Errorf("ambiguity finding for term %q missing recommendation", amb.Term)
		}
	}

	if !foundFastest || !foundBest || !foundAsap {
		t.Errorf("missing expected ambiguity terms: fastest=%v, best=%v, asap/lowest=%v",
			foundFastest, foundBest, foundAsap)
	}
}

func TestMissionResearcher_AdversarialProxyGaming_RevenueVsTrust(t *testing.T) {
	ctx := context.Background()
	researcher := mission.NewResearcher()

	input := mission.ResearchInput{
		OwnerStatement: "Maximize short-term subscription revenue and user signups at all costs, regardless of user friction or cancellation difficulty.",
		AuthorPrincipal: mission.Principal{
			ID:   "product-lead",
			Kind: mission.PrincipalKindHuman,
		},
	}

	proposal, err := researcher.Research(ctx, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !proposal.IsDangerousProxyDetected {
		t.Error("expected IsDangerousProxyDetected=true for revenue at all costs")
	}

	var foundRevenueTrustConflict bool
	for _, c := range proposal.Conflicts {
		if (c.CompetingValues[0] == "revenue" || c.CompetingValues[1] == "revenue") &&
			(c.CompetingValues[0] == "trust" || c.CompetingValues[1] == "trust" || c.CompetingValues[0] == "ethics" || c.CompetingValues[1] == "ethics") {
			foundRevenueTrustConflict = true
			if c.ProxyRisk == "" {
				t.Errorf("expected detailed ProxyRisk description in conflict finding")
			}
			if c.MitigatingConstraint == "" {
				t.Errorf("expected MitigatingConstraint in conflict finding")
			}
		}
	}

	if !foundRevenueTrustConflict {
		t.Fatalf("expected conflict between revenue and trust/ethics, found: %+v", proposal.Conflicts)
	}

	// Adversarial scenario generation check
	if len(proposal.AdversarialScenarios) == 0 {
		t.Fatal("expected at least one adversarial scenario generated")
	}

	var foundDarkPatternScenario bool
	for _, scenario := range proposal.AdversarialScenarios {
		if strings.Contains(strings.ToLower(scenario.GamingVector), "retention") ||
			strings.Contains(strings.ToLower(scenario.GamingVector), "friction") ||
			strings.Contains(strings.ToLower(scenario.GamingVector), "cancellation") ||
			strings.Contains(strings.ToLower(scenario.GamingVector), "churn") {
			foundDarkPatternScenario = true
		}
	}
	if !foundDarkPatternScenario {
		t.Errorf("expected adversarial scenario describing dark pattern / cancellation obstruction")
	}

	// Check that the proposed Mission contains counter-measures
	var foundTrustTradeOff bool
	for _, rule := range proposal.ProposedMission.TradeOffRules {
		if strings.Contains(strings.ToLower(rule.Priority), "trust") &&
			strings.Contains(strings.ToLower(rule.Subordinate), "revenue") {
			foundTrustTradeOff = true
			break
		}
	}
	if !foundTrustTradeOff {
		t.Errorf("expected proposed TradeOffRules to prioritize trust over short-term revenue")
	}
}

func TestMissionResearcher_AdversarialProxyGaming_ErrorMinimizationVsAvailability(t *testing.T) {
	ctx := context.Background()
	researcher := mission.NewResearcher()

	input := mission.ResearchInput{
		OwnerStatement: "Achieve zero errors across all microservices with 100% success rate on handled requests.",
		AuthorPrincipal: mission.Principal{
			ID:   "qa-lead",
			Kind: mission.PrincipalKindHuman,
		},
	}

	proposal, err := researcher.Research(ctx, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var foundAvailabilityConflict bool
	for _, c := range proposal.Conflicts {
		if (c.CompetingValues[0] == "error_rate" || c.CompetingValues[1] == "error_rate") &&
			(c.CompetingValues[0] == "availability" || c.CompetingValues[1] == "availability" || c.CompetingValues[0] == "throughput" || c.CompetingValues[1] == "throughput") {
			foundAvailabilityConflict = true
		}
	}
	if !foundAvailabilityConflict {
		t.Errorf("expected error rate vs availability conflict, found: %+v", proposal.Conflicts)
	}

	// Check adversarial exploit vector: refusing requests to keep error rate at zero
	var foundRefusalGaming bool
	for _, scenario := range proposal.AdversarialScenarios {
		if strings.Contains(strings.ToLower(scenario.GamingVector), "drop") ||
			strings.Contains(strings.ToLower(scenario.GamingVector), "reject") ||
			strings.Contains(strings.ToLower(scenario.GamingVector), "refus") {
			foundRefusalGaming = true
		}
	}
	if !foundRefusalGaming {
		t.Errorf("expected adversarial scenario highlighting traffic rejection / refusal gaming")
	}
}

func TestMissionResearcher_OrganizationalPolicyConflict(t *testing.T) {
	ctx := context.Background()
	researcher := mission.NewResearcher()

	input := mission.ResearchInput{
		OwnerStatement: "Fully automate high-volume wire transfers up to $50,000 directly from AI agents without human intervention.",
		Policies: []string{
			"No automated financial transfers exceeding $5,000 without multi-party human approval.",
		},
		AuthorPrincipal: mission.Principal{
			ID:   "growth-bot",
			Kind: mission.PrincipalKindWorker,
		},
	}

	proposal, err := researcher.Research(ctx, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var foundPolicyConflict bool
	for _, c := range proposal.Conflicts {
		if c.Severity == mission.FindingSeverityCritical && strings.Contains(strings.ToLower(c.Description), "human approval") {
			foundPolicyConflict = true
		}
	}
	if !foundPolicyConflict {
		t.Errorf("expected CRITICAL policy conflict regarding human approval for transfers, got: %+v", proposal.Conflicts)
	}

	// The proposed mission must enforce human trust boundaries
	var foundBoundary bool
	for _, b := range proposal.ProposedMission.HumanTrustBoundaries {
		if strings.Contains(strings.ToLower(b), "human approval") || strings.Contains(strings.ToLower(b), "$5,000") {
			foundBoundary = true
		}
	}
	if !foundBoundary {
		t.Errorf("expected human trust boundary to be maintained in proposed mission")
	}
}

func TestMissionResearcher_ExtractsAssumptionsUnknownsAndTriggers(t *testing.T) {
	ctx := context.Background()
	researcher := mission.NewResearcher()

	input := mission.ResearchInput{
		OwnerStatement: "Migrate legacy CRM to distributed event-driven microservices assuming cloud infra is stable and budget is adequate.",
		AuthorPrincipal: mission.Principal{
			ID:   "architect",
			Kind: mission.PrincipalKindHuman,
		},
	}

	proposal, err := researcher.Research(ctx, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(proposal.ExtractedAssumptions) == 0 {
		t.Error("expected extracted assumptions")
	}
	if len(proposal.IdentifiedUnknowns) == 0 {
		t.Error("expected identified unknowns")
	}
	if len(proposal.SuggestedRevisionTriggers) == 0 {
		t.Error("expected suggested revision triggers")
	}

	// Check draft mission contains these as well
	if len(proposal.ProposedMission.Assumptions) == 0 ||
		len(proposal.ProposedMission.Unknowns) == 0 ||
		len(proposal.ProposedMission.RevisionTriggers) == 0 {
		t.Errorf("expected proposed mission to have assumptions, unknowns, and revision triggers populated")
	}
}

func TestMissionResearcher_NonAuthoritativeInvariant(t *testing.T) {
	ctx := context.Background()
	store := mission.NewInMemoryMissionStore()
	svc := mission.NewService(store)
	researcher := mission.NewResearcher()

	worker := mission.Principal{
		ID:   "researcher-worker-01",
		Kind: mission.PrincipalKindWorker,
	}

	input := mission.ResearchInput{
		OwnerStatement:  "Provide automated customer support response drafting.",
		AuthorPrincipal: worker,
	}

	proposal, err := researcher.Research(ctx, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 1. In-memory store should have NOT had any missions saved
	_, err = store.Get(ctx, proposal.ProposedMission.ID, 1)
	if err != mission.ErrMissionNotFound {
		t.Errorf("expected store to NOT contain proposed mission before explicit save, got %v", err)
	}

	// 2. Worker attempting to approve proposal via Service MUST fail
	_, err = svc.Approve(ctx, worker, proposal.ProposedMission.ID, 1)
	// Because not even in store or if in store, worker is unauthorized
	if err == nil {
		t.Error("expected error when worker attempts approval")
	}
}

func TestMissionResearcher_DeterministicErrorHandling(t *testing.T) {
	ctx := context.Background()
	researcher := mission.NewResearcher()

	// Empty statement
	_, err := researcher.Research(ctx, mission.ResearchInput{
		OwnerStatement: "   ",
	})
	if err != mission.ErrEmptyStatement {
		t.Errorf("expected ErrEmptyStatement, got %v", err)
	}

	// Canceled context
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()

	_, err = researcher.Research(canceledCtx, mission.ResearchInput{
		OwnerStatement: "Valid statement but canceled context",
	})
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestMissionResearcher_EvolutionAndVelocityConflict(t *testing.T) {
	ctx := context.Background()
	researcher := mission.NewResearcher()

	baseID := domain.NewID("mission")
	baseMission := &mission.Mission{
		ID:      baseID,
		Version: 3,
		Purpose: "Initial baseline mission",
	}

	input := mission.ResearchInput{
		OwnerStatement: "Ship features immediately to production bypassing test suites to move faster.",
		BaseMission:    baseMission,
		AuthorPrincipal: mission.Principal{
			ID:   "eng-lead",
			Kind: mission.PrincipalKindHuman,
		},
	}

	proposal, err := researcher.Research(ctx, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Evolution version increment and ID continuity
	if proposal.ProposedMission.ID != baseID {
		t.Errorf("expected proposed mission ID %s to match base mission ID %s", proposal.ProposedMission.ID, baseID)
	}
	if proposal.ProposedMission.Version != 4 {
		t.Errorf("expected version 4, got %d", proposal.ProposedMission.Version)
	}
	if proposal.ProposedMission.Provenance.BaseVersion != 3 {
		t.Errorf("expected base version 3, got %d", proposal.ProposedMission.Provenance.BaseVersion)
	}

	// Velocity vs Verification conflict check
	var foundVelocityConflict bool
	for _, c := range proposal.Conflicts {
		if c.Severity == mission.FindingSeverityCritical &&
			(c.CompetingValues[0] == "velocity" || c.CompetingValues[1] == "velocity") {
			foundVelocityConflict = true
		}
	}
	if !foundVelocityConflict {
		t.Errorf("expected critical velocity vs correctness conflict, got: %+v", proposal.Conflicts)
	}

	// Proposed mission completeness
	if err := proposal.ProposedMission.ValidateCompleteness(); err != nil {
		t.Errorf("expected valid completeness on proposal, got: %v", err)
	}
}
