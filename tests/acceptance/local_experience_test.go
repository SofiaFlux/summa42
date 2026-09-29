package acceptance_test

import (
	"context"
	"path/filepath"
	"testing"
	"net/http/httptest"
	"time"

	"github.com/SofiaFlux/summa42/internal/control"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/experience"
	"github.com/SofiaFlux/summa42/internal/fieldfeedback"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/scheduler"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
	"github.com/SofiaFlux/summa42/internal/testutil"
	"github.com/SofiaFlux/summa42/internal/verification"
)

type experienceAcceptanceFixture struct {
	ctx        context.Context
	box        *summa42runtime.Box
	client     *control.Client
	clock      *testutil.Clock
	owner      *identity.LocalEd25519
	envelopeID domain.ID
	grant      domain.AdaptationGrant
	proposal   domain.ExperienceProposal
}

func newExperienceAcceptanceFixture(t *testing.T, withGrant bool) *experienceAcceptanceFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	clk := testutil.NewClock(time.Date(2026, 9, 19, 14, 0, 0, 0, time.UTC))
	owner, err := identity.NewLocalEd25519(filepath.Join(root, "owner.key"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	pol := &feedbackAcceptancePolicy{outcome: domain.PolicyAllow, hash: "policy-experience-v1"}
	box, err := summa42runtime.Open(ctx, summa42runtime.Config{
		StatePath: filepath.Join(root, "state.db"), EvidencePath: filepath.Join(root, "evidence"),
		Clock: clk, CollectiveID: "collective-experience-acceptance", OwnerPrincipalID: owner.PrincipalID(),
		PolicyEngine: pol, LeaseDuration: time.Hour,
		FieldFeedback: localconfig.FieldFeedbackConfig{Enabled: false, Mode: localconfig.FeedbackModeLocalOnly},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = box.Close() })
	envelopeID := domain.ID("experience-work")
	if _, err := box.Store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, 1000, ?)`,
		envelopeID, clk.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	server, err := control.NewServer(control.ServerConfig{
		AuthToken:"experience-product-path",OwnerPrincipalID:owner.PrincipalID(),
		OwnerPublicKey:owner.PublicKey(),ChallengeTTL:time.Minute,Now:clk.Now,
	},control.Dependencies{
		Status:feedbackStatusProvider{box:box},Tasks:box.Execution,Missions:box.Purpose,Approvals:box.Approvals,
		Feedback:box.Feedback,Sanitizer:box.Sanitizer,FieldObserver:box.FieldObserver,Experience:box.Experience,
		Attempts:box.Execution,Operations:box.Operations,Shutdown:box,
	})
	if err!=nil{t.Fatal(err)}
	httpServer:=httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	client:=control.NewClient(httpServer.URL,"experience-product-path",httpServer.Client())
	fixture := &experienceAcceptanceFixture{ctx: ctx, box: box, client: client, clock: clk, owner: owner, envelopeID: envelopeID}
	if !withGrant {
		return fixture
	}
	observation, err := client.FeedbackObserve(ctx, control.FeedbackObserveRequest{
		Category:"EXECUTOR_SELECTION_FRICTION",
		SummaryLocal:"codex performed this local review class reliably",
		Enforcement:domain.EnforcementEnforced,
	})
	if err!=nil{t.Fatal(err)}
	request, err := client.ExperienceGrantRequest(ctx, control.ExperienceGrantCreateRequest{
		ScopeKey:"repo.review",AllowedExecutors:[]string{"claude","codex"},MinVerifiedSamples:3,
		MaxAcceptanceRegressionBps:0,MaxCostRegressionBps:0,ExpiresAt:clk.Now().Add(7*24*time.Hour),
	})
	if err!=nil{t.Fatal(err)}
	signedApproveExperience(t,fixture,request.ApprovalID)
	grantDTO,err:=client.ExperienceGrantActivate(ctx,request.RequestID)
	if err!=nil{t.Fatal(err)}
	grant,err:=box.Experience.Grant(ctx,grantDTO.ID)
	if err!=nil{t.Fatal(err)}
	proposalDTO,err:=client.ExperienceProposalCreate(ctx,control.ExperienceProposalCreateRequest{
		GrantID:grant.ID,PreferredExecutor:"codex",EvidenceObservationIDs:[]domain.ID{observation.ID},
	})
	if err!=nil{t.Fatal(err)}
	fixture.grant=grant
	fixture.proposal=domain.ExperienceProposal{
		ID:proposalDTO.ID,GrantID:proposalDTO.GrantID,GenericTaskClass:proposalDTO.GenericTaskClass,
		ScopeKey:proposalDTO.ScopeKey,PreferredExecutor:proposalDTO.PreferredExecutor,
		EvidenceObservationIDs:append([]domain.ID(nil),proposalDTO.EvidenceObservationIDs...),State:proposalDTO.State,
	}
	return fixture
}

func signedApproveExperience(t *testing.T, f *experienceAcceptanceFixture, approvalID domain.ID) {
	t.Helper()
	if _,err:=f.client.Approve(f.ctx,approvalID,f.owner);err!=nil{t.Fatal(err)}
}

func (f *experienceAcceptanceFixture) verifiedSuccess(t *testing.T, name, executor string) domain.Task {
	t.Helper()
	taskDTO,err:=f.client.CreateTask(f.ctx,control.CreateTaskRequest{
		Purpose:domain.PurposeRef{Kind:domain.PurposeOwnerDirective,ID:domain.ID("experience-"+name)},
		TaskClass:"repo.review",AcceptanceCriteria:[]string{"review accepted"},
		RequiredCapabilities:[]string{"repo.read"},RequiredEnforcement:domain.EnforcementEnforced,
		AuthorityCeiling:[]string{"repo.read"},ResourceEnvelopeID:f.envelopeID,
	})
	if err!=nil{t.Fatal(err)}
	task:=domain.Task{ID:taskDTO.ID,TaskClass:taskDTO.TaskClass}
	attempt, err := f.box.Execution.StartAttempt(f.ctx, task.ID, executor, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ev := putTextEvidence(t, f.ctx, f.box, "verified "+name, "EXPERIENCE_VERIFICATION")
	if _, err := f.box.Verification.CompleteAttempt(f.ctx, attempt.ID, verification.CompletionManifest{EvidenceIDs: []domain.ID{ev.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.box.Verification.AcceptTask(f.ctx, task.ID, verification.AcceptanceRequest{
		VerifierID: f.owner.PrincipalID(), VerifierType: "OWNER_ACCEPTANCE",
		CriteriaMet: true, EvidenceIDs: []domain.ID{ev.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if _,err:=f.client.ExperienceOutcomeCreate(f.ctx,control.ExperienceOutcomeCreateRequest{
		TaskID:task.ID,Accepted:true,RetryCount:0,
	});err!=nil{t.Fatal(err)}
	return task
}

func TestLocalExperienceEarnsAndLosesAutonomyFromVerifiedResults(t *testing.T) {
	f := newExperienceAcceptanceFixture(t, true)
	first := f.verifiedSuccess(t, "one", "codex")
	_ = first
	f.verifiedSuccess(t, "two", "codex")
	rule, err := f.client.ExperienceEvaluate(f.ctx, f.proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rule.State != domain.ExperienceShadow || rule.VerifiedSamples != 2 {
		t.Fatalf("two-sample rule=%+v", rule)
	}

	third := f.verifiedSuccess(t, "three", "codex")
	rule, err = f.client.ExperienceEvaluate(f.ctx, f.proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rule.State != domain.ExperienceActive || rule.VerifiedSamples != 3 {
		t.Fatalf("three-sample rule=%+v", rule)
	}
	chosen, err := f.box.Scheduler.ChooseExecutor(f.ctx, domain.Task{TaskClass: "repo.review"}, []string{"claude", "codex"})
	if err != nil || chosen != "codex" {
		t.Fatalf("active preference chosen=%q err=%v", chosen, err)
	}

	f.clock.Advance(time.Second)
	if err := f.box.Execution.ChallengeTask(f.ctx, third.ID, domain.ChallengeTask, "verified regression after field use", nil); err != nil {
		t.Fatal(err)
	}
	if _,err:=f.client.ExperienceOutcomeCreate(f.ctx,control.ExperienceOutcomeCreateRequest{
		TaskID:third.ID,Accepted:false,
	});err!=nil{t.Fatal(err)}
	rule, err = f.client.ExperienceEvaluate(f.ctx, f.proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rule.State != domain.ExperienceRolledBack {
		t.Fatalf("regressed rule=%+v", rule)
	}
	chosen, err = f.box.Scheduler.ChooseExecutor(f.ctx, domain.Task{TaskClass: "repo.review"}, []string{"codex", "claude"})
	if err != nil || chosen != "claude" {
		t.Fatalf("rollback baseline chosen=%q err=%v", chosen, err)
	}

	var linkedOutcomes int
	if err := f.box.Store.DB().QueryRowContext(f.ctx,
		`SELECT count(*) FROM experience_rule_outcomes WHERE rule_id = ?`, rule.ID,
	).Scan(&linkedOutcomes); err != nil {
		t.Fatal(err)
	}
	if linkedOutcomes < 3 {
		t.Fatalf("rollback rule linked outcomes=%d", linkedOutcomes)
	}
}

func TestLocalExperienceNegativeAcceptance(t *testing.T) {
	t.Run("no_grant_no_automatic_adaptation", func(t *testing.T) {
		f := newExperienceAcceptanceFixture(t, false)
		chosen, err := f.box.Scheduler.ChooseExecutor(f.ctx, domain.Task{TaskClass: "repo.review"}, []string{"codex", "claude"})
		if err != nil || chosen != "claude" {
			t.Fatalf("baseline chosen=%q err=%v", chosen, err)
		}
		if _, found, err := f.box.Experience.Preference(f.ctx, experience.PreferenceQuery{ScopeKey: "repo.review"}); err != nil || found {
			t.Fatalf("implicit preference found=%v err=%v", found, err)
		}
	})

	t.Run("unverified_outcome_does_not_count", func(t *testing.T) {
		f := newExperienceAcceptanceFixture(t, true)
		task, err := f.box.Execution.CreateTask(f.ctx, execution.TaskRequest{
			Purpose: domain.PurposeRef{Kind: domain.PurposeOwnerDirective, ID: "unverified"},
			TaskClass: "repo.review", AcceptanceCriteria: []string{"not verified"},
			RequiredCapabilities: []string{"repo.read"}, RequiredEnforcement: domain.EnforcementEnforced,
			AuthorityCeiling: []string{"repo.read"}, ResourceEnvelopeID: f.envelopeID,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.box.Execution.StartAttempt(f.ctx, task.ID, "codex", time.Hour); err != nil {
			t.Fatal(err)
		}
		err = f.box.Experience.ObserveVerifiedOutcome(f.ctx, experience.VerifiedOutcome{
			TaskID: task.ID, GenericTaskClass: domain.GenericTaskReview, ScopeKey: "repo.review",
			ExecutorKind: "codex", Accepted: true,
		})
		if err == nil {
			t.Fatal("unverified outcome counted")
		}
	})

	t.Run("preference_cannot_make_missing_capability_eligible", func(t *testing.T) {
		f := activeExperienceFixture(t)
		task := createPreferenceGuardTask(t, f, "missing-capability", []string{"gpu"}, []string{"gpu"}, domain.EnforcementEnforced)
		capacity := scheduler.CapacitySnapshot{Capabilities: map[string]scheduler.CapabilityCapacity{
			"repo.read": {Accessible: true, Enforcement: domain.EnforcementEnforced},
		}}
		next, err := f.box.Scheduler.Next(f.ctx, capacity)
		if err != nil {
			t.Fatal(err)
		}
		if next != nil && next.Task.ID == task.ID {
			t.Fatal("learned preference made missing capability eligible")
		}
	})

	t.Run("preference_cannot_lower_enforcement", func(t *testing.T) {
		f := activeExperienceFixture(t)
		task := createPreferenceGuardTask(t, f, "weak-enforcement", []string{"repo.read"}, []string{"repo.read"}, domain.EnforcementEnforced)
		capacity := scheduler.CapacitySnapshot{Capabilities: map[string]scheduler.CapabilityCapacity{
			"repo.read": {Accessible: true, Enforcement: domain.EnforcementPartial},
		}}
		next, err := f.box.Scheduler.Next(f.ctx, capacity)
		if err != nil {
			t.Fatal(err)
		}
		if next != nil && next.Task.ID == task.ID {
			t.Fatal("learned preference lowered enforcement requirement")
		}
	})

	t.Run("preference_cannot_expand_authority", func(t *testing.T) {
		f := activeExperienceFixture(t)
		_, err := f.box.Execution.CreateTask(f.ctx, execution.TaskRequest{
			Purpose: domain.PurposeRef{Kind: domain.PurposeOwnerDirective, ID: "authority-expansion"},
			TaskClass: "repo.review", AcceptanceCriteria: []string{"must not exist"},
			RequiredCapabilities: []string{"admin"}, RequiredEnforcement: domain.EnforcementEnforced,
			AuthorityCeiling: []string{"repo.read"}, ResourceEnvelopeID: f.envelopeID,
		})
		if err == nil {
			t.Fatal("task widened authority before preference evaluation")
		}
	})

	t.Run("discovered_independent_problem_creates_new_work_not_scope_mutation", func(t *testing.T) {
		f := activeExperienceFixture(t)
		before, err := f.box.Experience.Grant(f.ctx, f.grant.ID)
		if err != nil {
			t.Fatal(err)
		}
		observation, err := f.box.FieldObserver.Record(f.ctx, fieldfeedback.ObservationInput{
			Category: "INDEPENDENT_PROBLEM", BasisClass: "FIELD_DISCOVERY", SourceKind: "OPERATOR",
			SummaryLocal: "another problem discovered while working", Metrics: map[string]any{},
			Enforcement: domain.EnforcementEnforced,
		})
		if err != nil {
			t.Fatal(err)
		}
		task, err := f.box.Execution.CreateTask(f.ctx, execution.TaskRequest{
			Purpose: domain.PurposeRef{Kind: domain.PurposeCollectiveMaintenance, ID: observation.ID},
			TaskClass: "collective.discovery.followup", AcceptanceCriteria: []string{"independent problem assessed"},
			RequiredEnforcement: domain.EnforcementEnforced, ResourceEnvelopeID: f.envelopeID,
		})
		if err != nil {
			t.Fatal(err)
		}
		after, err := f.box.Experience.Grant(f.ctx, f.grant.ID)
		if err != nil {
			t.Fatal(err)
		}
		if task.ID == "" || before.ScopeKey != after.ScopeKey || before.Kind != after.Kind {
			t.Fatalf("discovery mutated grant: before=%+v after=%+v task=%+v", before, after, task)
		}
	})
}

func activeExperienceFixture(t *testing.T) *experienceAcceptanceFixture {
	t.Helper()
	f := newExperienceAcceptanceFixture(t, true)
	f.verifiedSuccess(t, "active-1", "codex")
	f.verifiedSuccess(t, "active-2", "codex")
	f.verifiedSuccess(t, "active-3", "codex")
	rule, err := f.client.ExperienceEvaluate(f.ctx, f.proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rule.State != domain.ExperienceActive {
		t.Fatalf("fixture rule=%+v", rule)
	}
	return f
}

func createPreferenceGuardTask(t *testing.T, f *experienceAcceptanceFixture, name string, caps, authority []string, enforcement domain.EnforcementLevel) domain.Task {
	t.Helper()
	task, err := f.box.Execution.CreateTask(f.ctx, execution.TaskRequest{
		Purpose: domain.PurposeRef{Kind: domain.PurposeOwnerDirective, ID: domain.ID(name)},
		TaskClass: "repo.review", AcceptanceCriteria: []string{"guard remains enforced"},
		RequiredCapabilities: caps, RequiredEnforcement: enforcement,
		AuthorityCeiling: authority, ResourceEnvelopeID: f.envelopeID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}
