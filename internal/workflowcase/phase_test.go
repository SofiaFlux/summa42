package workflowcase

import (
	"context"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/workflow"
)

func setupPhaseFixture(t *testing.T) (*Service, domain.ID, context.Context) {
	t.Helper()
	svc, _, missionID, ctx := setupEnsure(t)
	return svc, missionID, ctx
}

func TestWorkflowCaseInitialPhase(t *testing.T) {
	svc, missionID, ctx := setupPhaseFixture(t)

	// Incompatible initial phase (e.g. CLOSED on active case) must be rejected
	_, err := svc.Ensure(ctx, Observation{
		MissionID:       missionID,
		Source:          "github",
		ObjectID:        "issue-invalid-phase",
		RevisionID:      "rev-1",
		EvidenceID:      "evidence",
		FirstWork:       workflow.WorkProposal{Kind: "github.issue.triage", RequiredCapabilities: []string{"read"}, AuthorityCeiling: []string{"read"}},
		Grant:           workflow.Grant{Capabilities: []string{"read"}},
		MaxSteps:        5,
		RemainingBudget: 100,
		Phase:           PhaseClosed,
	})
	if err == nil {
		t.Fatalf("expected error ensuring case with incompatible PhaseClosed, got nil")
	}

	// Standard case: FirstWork is planning/triage -> PLANNING_PENDING
	c1, err := svc.Ensure(ctx, Observation{
		MissionID:       missionID,
		Source:          "github",
		ObjectID:        "issue-1",
		RevisionID:      "rev-1",
		EvidenceID:      "evidence",
		FirstWork:       workflow.WorkProposal{Kind: "github.issue.triage", RequiredCapabilities: []string{"read"}, AuthorityCeiling: []string{"read"}},
		Grant:           workflow.Grant{Capabilities: []string{"read", "workspace.repo.write", "workspace.test"}},
		MaxSteps:        5,
		RemainingBudget: 100,
	})
	if err != nil {
		t.Fatalf("ensure case 1: %v", err)
	}
	if c1.Phase != PhasePlanningPending {
		t.Fatalf("expected phase %q, got %q", PhasePlanningPending, c1.Phase)
	}
	if c1.State != Active {
		t.Fatalf("expected state %q, got %q", Active, c1.State)
	}

	// Implementation case: FirstWork is implementation -> IMPLEMENTATION_PENDING
	c2, err := svc.Ensure(ctx, Observation{
		MissionID:       missionID,
		Source:          "github",
		ObjectID:        "issue-2",
		RevisionID:      "rev-1",
		EvidenceID:      "evidence",
		FirstWork:       workflow.WorkProposal{Kind: "github.issue.implement", RequiredCapabilities: []string{"read"}, AuthorityCeiling: []string{"read"}},
		Grant:           workflow.Grant{Capabilities: []string{"read"}},
		MaxSteps:        5,
		RemainingBudget: 100,
	})
	if err != nil {
		t.Fatalf("ensure case 2: %v", err)
	}
	if c2.Phase != PhaseImplementationPending {
		t.Fatalf("expected phase %q, got %q", PhaseImplementationPending, c2.Phase)
	}
}

func TestWorkflowCaseSetPhase(t *testing.T) {
	svc, missionID, ctx := setupPhaseFixture(t)

	c, err := svc.Ensure(ctx, Observation{
		MissionID:       missionID,
		Source:          "github",
		ObjectID:        "issue-setphase",
		RevisionID:      "rev-1",
		EvidenceID:      "evidence",
		FirstWork:       workflow.WorkProposal{Kind: "github.issue.triage", RequiredCapabilities: []string{"read"}, AuthorityCeiling: []string{"read"}},
		Grant:           workflow.Grant{Capabilities: []string{"read"}},
		MaxSteps:        5,
		RemainingBudget: 100,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Transition PLANNING_PENDING -> PLANNING_IN_PROGRESS
	if err := svc.SetPhase(ctx, c.ID, PhasePlanningInProgress); err != nil {
		t.Fatalf("set phase to PLANNING_IN_PROGRESS: %v", err)
	}
	updated, err := svc.Get(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Phase != PhasePlanningInProgress {
		t.Fatalf("expected phase %q, got %q", PhasePlanningInProgress, updated.Phase)
	}

	// Setting an incompatible phase for ACTIVE state (e.g. CLOSED) must fail
	if err := svc.SetPhase(ctx, c.ID, PhaseClosed); err == nil {
		t.Fatalf("expected error setting CLOSED phase on ACTIVE case, got nil")
	}
}

func TestWorkflowCaseAssessPhaseTransitions(t *testing.T) {
	svc, missionID, ctx := setupPhaseFixture(t)

	c, err := svc.Ensure(ctx, Observation{
		MissionID:       missionID,
		Source:          "github",
		ObjectID:        "issue-assess",
		RevisionID:      "rev-1",
		EvidenceID:      "evidence",
		FirstWork:       workflow.WorkProposal{Kind: "github.issue.triage", RequiredCapabilities: []string{"read"}, AuthorityCeiling: []string{"read"}},
		Grant:           workflow.Grant{Capabilities: []string{"read", "workspace.repo.write", "workspace.test"}},
		MaxSteps:        10,
		RemainingBudget: 100,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Assess with incompatible phase override (e.g. PhaseClosed on OutcomeContinue) must fail
	_, err = svc.Assess(ctx, AssessmentRequest{
		CaseID:            c.ID,
		WorkID:            c.CurrentWorkID,
		RemainingBudget:   90,
		ProgressSignature: "plan-hash",
		Phase:             PhaseClosed,
		Assessment: workflow.Assessment{
			Verdict:     workflow.Continue,
			Reason:      "plan accepted",
			EvidenceIDs: []string{"review-evidence"},
			Next: &workflow.WorkProposal{
				Kind:                 "github.issue.implement",
				RequiredCapabilities: []string{"workspace.repo.write", "workspace.test"},
				AuthorityCeiling:     []string{"workspace.repo.write", "workspace.test"},
			},
		},
	})
	if err == nil {
		t.Fatalf("expected error on incompatible PhaseClosed override in Assess, got nil")
	}

	// Plan approved -> transitions to IMPLEMENTATION_PENDING
	res1, err := svc.Assess(ctx, AssessmentRequest{
		CaseID:            c.ID,
		WorkID:            c.CurrentWorkID,
		RemainingBudget:   90,
		ProgressSignature: "plan-hash",
		Assessment: workflow.Assessment{
			Verdict:     workflow.Continue,
			Reason:      "plan accepted",
			EvidenceIDs: []string{"review-evidence"},
			Next: &workflow.WorkProposal{
				Kind:                 "github.issue.implement",
				RequiredCapabilities: []string{"workspace.repo.write", "workspace.test"},
				AuthorityCeiling:     []string{"workspace.repo.write", "workspace.test"},
			},
		},
	})
	if err != nil {
		t.Fatalf("assess planning to implementation: %v", err)
	}
	if res1.Case.Phase != PhaseImplementationPending {
		t.Fatalf("expected phase %q, got %q", PhaseImplementationPending, res1.Case.Phase)
	}

	// Start implementation
	if err := svc.SetPhase(ctx, c.ID, PhaseImplementationInProgress); err != nil {
		t.Fatalf("set phase implementation in progress: %v", err)
	}

	// Implementation completes -> READY_FOR_VERIFICATION
	res2, err := svc.Assess(ctx, AssessmentRequest{
		CaseID:            c.ID,
		WorkID:            res1.Case.CurrentWorkID,
		RemainingBudget:   80,
		ProgressSignature: "impl-hash",
		Assessment: workflow.Assessment{
			Verdict:     workflow.Ready,
			Reason:      "ready for verification",
			EvidenceIDs: []string{"ready-evidence"},
		},
	})
	if err != nil {
		t.Fatalf("assess ready: %v", err)
	}
	if res2.Case.Phase != PhaseReadyForVerification {
		t.Fatalf("expected phase %q, got %q", PhaseReadyForVerification, res2.Case.Phase)
	}
	if res2.Case.State != ReadyForVerification {
		t.Fatalf("expected state %q, got %q", ReadyForVerification, res2.Case.State)
	}

	// Close case -> CLOSED
	_, err = svc.Close(ctx, VerificationRequest{
		CaseID:       c.ID,
		VerifierID:   "verifier-1",
		VerifierType: "automated-test",
		SnapshotHash: "snap-hash",
		SnapshotJSON: `{"status":"ok"}`,
		EvidenceIDs:  []domain.ID{"publication-evidence"},
	})
	if err != nil {
		t.Fatalf("close case: %v", err)
	}
	closedCase, err := svc.Get(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closedCase.Phase != PhaseClosed {
		t.Fatalf("expected phase %q, got %q", PhaseClosed, closedCase.Phase)
	}
	if closedCase.State != Closed {
		t.Fatalf("expected state %q, got %q", Closed, closedCase.State)
	}
}

func TestWorkflowCaseBlockedPhases(t *testing.T) {
	svc, missionID, ctx := setupPhaseFixture(t)

	// 1. Manual Block -> BLOCKED_WAITING_DIRECTIVE
	c1, err := svc.Ensure(ctx, Observation{
		MissionID:       missionID,
		Source:          "github",
		ObjectID:        "issue-block-dir",
		RevisionID:      "rev-1",
		EvidenceID:      "evidence",
		FirstWork:       workflow.WorkProposal{Kind: "github.issue.triage", RequiredCapabilities: []string{"read"}, AuthorityCeiling: []string{"read"}},
		Grant:           workflow.Grant{Capabilities: []string{"read"}},
		MaxSteps:        5,
		RemainingBudget: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	b1, err := svc.Block(ctx, c1.ID, "manual operator stop", domain.ID("blocked-evidence"))
	if err != nil {
		t.Fatalf("block case 1: %v", err)
	}
	if b1.Phase != PhaseBlockedWaitingDirective {
		t.Fatalf("expected phase %q, got %q", PhaseBlockedWaitingDirective, b1.Phase)
	}

	// 2. Blocked due to context missing -> BLOCKED_WAITING_CONTEXT
	c2, err := svc.Ensure(ctx, Observation{
		MissionID:       missionID,
		Source:          "github",
		ObjectID:        "issue-block-ctx",
		RevisionID:      "rev-1",
		EvidenceID:      "evidence",
		FirstWork:       workflow.WorkProposal{Kind: "github.issue.triage", RequiredCapabilities: []string{"read"}, AuthorityCeiling: []string{"read"}},
		Grant:           workflow.Grant{Capabilities: []string{"read"}},
		MaxSteps:        5,
		RemainingBudget: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	res2, err := svc.Assess(ctx, AssessmentRequest{
		CaseID:            c2.ID,
		WorkID:            c2.CurrentWorkID,
		RemainingBudget:   90,
		ProgressSignature: "sig",
		Assessment: workflow.Assessment{
			Verdict:     workflow.Unknown,
			Reason:      "missing prompt context for task",
			EvidenceIDs: []string{"blocked-evidence"},
		},
	})
	if err != nil {
		t.Fatalf("assess block context: %v", err)
	}
	if res2.Case.Phase != PhaseBlockedWaitingContext {
		t.Fatalf("expected phase %q, got %q", PhaseBlockedWaitingContext, res2.Case.Phase)
	}
}

func TestWorkflowCaseListByPhaseAndMetrics(t *testing.T) {
	svc, missionID, ctx := setupPhaseFixture(t)

	// Create 2 planning cases and 1 implementation case
	_, err := svc.Ensure(ctx, Observation{
		MissionID:       missionID,
		Source:          "github",
		ObjectID:        "issue-m1",
		RevisionID:      "rev-1",
		EvidenceID:      "evidence",
		FirstWork:       workflow.WorkProposal{Kind: "github.issue.triage", RequiredCapabilities: []string{"read"}, AuthorityCeiling: []string{"read"}},
		Grant:           workflow.Grant{Capabilities: []string{"read"}},
		MaxSteps:        5,
		RemainingBudget: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	c2, err := svc.Ensure(ctx, Observation{
		MissionID:       missionID,
		Source:          "github",
		ObjectID:        "issue-m2",
		RevisionID:      "rev-1",
		EvidenceID:      "evidence",
		FirstWork:       workflow.WorkProposal{Kind: "github.issue.triage", RequiredCapabilities: []string{"read"}, AuthorityCeiling: []string{"read"}},
		Grant:           workflow.Grant{Capabilities: []string{"read"}},
		MaxSteps:        5,
		RemainingBudget: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Move c2 to PLANNING_IN_PROGRESS
	if err := svc.SetPhase(ctx, c2.ID, PhasePlanningInProgress); err != nil {
		t.Fatal(err)
	}

	_, err = svc.Ensure(ctx, Observation{
		MissionID:       missionID,
		Source:          "github",
		ObjectID:        "issue-m3",
		RevisionID:      "rev-1",
		EvidenceID:      "evidence",
		FirstWork:       workflow.WorkProposal{Kind: "github.issue.implement", RequiredCapabilities: []string{"read"}, AuthorityCeiling: []string{"read"}},
		Grant:           workflow.Grant{Capabilities: []string{"read"}},
		MaxSteps:        5,
		RemainingBudget: 100,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Query ListByPhase
	pending, err := svc.ListByPhase(ctx, missionID, PhasePlanningPending)
	if err != nil {
		t.Fatalf("list by phase: %v", err)
	}
	if len(pending) != 1 || pending[0].ObjectID != "issue-m1" {
		t.Fatalf("expected 1 PLANNING_PENDING case (issue-m1), got %d", len(pending))
	}

	inProg, err := svc.ListByPhase(ctx, missionID, PhasePlanningInProgress)
	if err != nil {
		t.Fatalf("list by phase in progress: %v", err)
	}
	if len(inProg) != 1 || inProg[0].ObjectID != "issue-m2" {
		t.Fatalf("expected 1 PLANNING_IN_PROGRESS case (issue-m2), got %d", len(inProg))
	}

	// Query Metrics
	metrics, err := svc.Metrics(ctx, missionID)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	if metrics.Total != 3 {
		t.Fatalf("expected total 3, got %d", metrics.Total)
	}
	if metrics.ByState[Active] != 3 {
		t.Fatalf("expected 3 ACTIVE cases, got %d", metrics.ByState[Active])
	}
	if metrics.ByPhase[PhasePlanningPending] != 1 {
		t.Fatalf("expected 1 PLANNING_PENDING, got %d", metrics.ByPhase[PhasePlanningPending])
	}
	if metrics.ByPhase[PhasePlanningInProgress] != 1 {
		t.Fatalf("expected 1 PLANNING_IN_PROGRESS, got %d", metrics.ByPhase[PhasePlanningInProgress])
	}
	if metrics.ByPhase[PhaseImplementationPending] != 1 {
		t.Fatalf("expected 1 IMPLEMENTATION_PENDING, got %d", metrics.ByPhase[PhaseImplementationPending])
	}
}
