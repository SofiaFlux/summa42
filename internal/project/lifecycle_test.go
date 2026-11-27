package project

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/approvals"
	"github.com/SofiaFlux/summa42/internal/bootstrap"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/emergency"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/SofiaFlux/summa42/internal/policy"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/resources"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

type stubOpProvider struct{ name string }

func (p stubOpProvider) Name() string                             { return p.name }
func (p stubOpProvider) AdapterVersion() string                   { return "v1" }
func (p stubOpProvider) AdapterVersionSemanticallyRelevant() bool { return false }
func (p stubOpProvider) Capability() string                       { return "workspace.repo.write" }
func (p stubOpProvider) EnforcementLevel() domain.EnforcementLevel {
	return domain.EnforcementUnenforced
}
func (p stubOpProvider) SettleWindow() time.Duration { return 0 }
func (p stubOpProvider) MinimumRisk() string         { return "LOW" }
func (p stubOpProvider) CanonicalIntent(d operations.IntentDescriptor) ([]byte, error) {
	return d.(stubIntent).payload, nil
}
func (p stubOpProvider) CostProfile(operations.IntentDescriptor) (operations.CostProfile, error) {
	return operations.CostProfile{
		MaxExposure: 1,
		Enforceability: resources.Enforceability{
			CostControl: resources.CostTechnicallyCapped, RequireHardCap: true, Source: "stub",
		},
	}, nil
}
func (p stubOpProvider) Dispatch(context.Context, operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	return operations.ProviderOutcome{State: domain.OperationConfirmedEffect, ProviderReference: "ref-1"}, nil
}
func (p stubOpProvider) LookupOutcome(context.Context, operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	return operations.ProviderOutcome{State: domain.OperationConfirmedNoEffect}, nil
}

type lifecycleFixture struct {
	svc          *Service
	clk          *testutil.Clock
	owner        identity.Signer
	collectiveID domain.ID
	missionID    domain.ID
	exec         *execution.Service
	ops          *operations.Service
	emerg        *emergency.Service
	res          *resources.Service
}

func newLifecycleFixture(t *testing.T) lifecycleFixture {
	t.Helper()
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC))
	keyDir := t.TempDir()
	owner, err := identity.CreateLocalEd25519(filepath.Join(keyDir, "owner.key"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	cube, err := identity.CreateLocalEd25519(filepath.Join(keyDir, "cube.key"), "cube")
	if err != nil {
		t.Fatal(err)
	}
	rootKey, err := identity.CreateLocalEd25519(filepath.Join(keyDir, "root.key"), "root")
	if err != nil {
		t.Fatal(err)
	}
	resInit, err := bootstrap.New(store, clk).Init(ctx, bootstrap.InitRequest{
		Constitution:       []byte(bootstrap.DefaultConstitution),
		Owner:              owner,
		Cube:               cube,
		ConstitutionalRoot: rootKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	purp := purpose.New(store, clk)
	mID, err := purp.CreateMission(ctx, "Operate multiple projects")
	if err != nil {
		t.Fatal(err)
	}
	emerg := emergency.NewService(store, clk, owner.PrincipalID())
	if err := emerg.EnsureDefaults(ctx, 0); err != nil {
		t.Fatal(err)
	}
	resSvc := resources.New(store, clk)
	execSvc := execution.New(store, clk, purp)
	execSvc.SetLeaseGate(emerg)
	execSvc.SetEnvelopeChecker(resSvc)

	prof, err := policy.DefaultProfile()
	if err != nil {
		t.Fatal(err)
	}
	pol := policy.NewOPAEngine(policy.OPAConfig{
		ModuleName: prof.ModuleName, Module: prof.Module, PolicySetID: prof.PolicySetID, PolicySetHash: prof.PolicyHash,
	})
	appr := approvals.New(store, clk)
	opsSvc := operations.New(store, clk, execSvc, pol, resSvc, appr, resInit.CollectiveID, stubOpProvider{name: "stub"})
	opsSvc.SetEmergency(emerg)

	// Fund root envelope with 100 units.
	gRoot, err := resources.NewOwnerGrant(owner, resInit.CollectiveID, resources.GrantSetLimit, CollectiveRootEnvelopeID, 100, "fund root", clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resSvc.ApplyOwnerGrant(ctx, gRoot); err != nil {
		t.Fatal(err)
	}

	return lifecycleFixture{
		svc: New(store, clk), clk: clk, owner: owner, collectiveID: resInit.CollectiveID,
		missionID: mID, exec: execSvc, ops: opsSvc, emerg: emerg, res: resSvc,
	}
}

func (f lifecycleFixture) createAndFundProject(t *testing.T, slug string, limit int64) Project {
	t.Helper()
	ctx := context.Background()
	cmd, err := NewCreateCommand(f.owner, f.collectiveID, slug, slug, "create "+slug, f.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	proj, err := f.svc.CreateSigned(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := resources.NewOwnerGrant(f.owner, f.collectiveID, resources.GrantSetLimit, proj.EnvelopeID, limit, "fund "+slug, f.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.FundSigned(ctx, f.res, proj.Slug, grant); err != nil {
		t.Fatal(err)
	}
	return proj
}

func (f lifecycleFixture) createTaskForProject(t *testing.T, proj Project, key string) domain.Task {
	t.Helper()
	task, err := f.exec.CreateTask(context.Background(), execution.TaskRequest{
		Purpose:              domain.PurposeRef{Kind: domain.PurposeMission, ID: f.missionID},
		TaskClass:            "github.issue.implement",
		Objective:            "Task for " + proj.Slug,
		IdempotencyKey:       key,
		AcceptanceCriteria:   []string{"done"},
		RequiredCapabilities: []string{"workspace.repo.write"},
		AuthorityCeiling:     []string{"workspace.repo.write"},
		ResourceEnvelopeID:   proj.EnvelopeID,
		ProjectID:            proj.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestPausedProjectRefusesLeaseOtherProjectContinues(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()
	alpha := f.createAndFundProject(t, "alpha", 20)
	beta := f.createAndFundProject(t, "beta", 20)

	tAlpha := f.createTaskForProject(t, alpha, "work-alpha")
	tBeta := f.createTaskForProject(t, beta, "work-beta")

	// Start an Attempt + Prepare on alpha before pausing alpha to verify Dispatch gate too.
	tAlphaPrep := f.createTaskForProject(t, alpha, "work-alpha-op")
	attAlphaOp, err := f.exec.StartAttempt(ctx, tAlphaPrep.ID, "codex", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	prepOp, err := f.ops.Prepare(ctx, operations.PrepareRequest{
		AttemptID: attAlphaOp.ID, TrustedSlotKey: "slot-1", Provider: "stub",
		Intent: stubIntent{desc: "low_action", payload: []byte(`{"x":1}`)}, Risk: "LOW",
	})
	if err != nil {
		t.Fatalf("prepare before pause: %v", err)
	}

	// Pause alpha (also revokes active lease attAlphaOp).
	pauseCmd, err := NewTransitionCommand(f.owner, f.collectiveID, alpha.ID, alpha.Slug, ActionPause, "incident on alpha", f.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	pausedAlpha, err := f.svc.TransitionSigned(ctx, pauseCmd)
	if err != nil {
		t.Fatalf("pause alpha: %v", err)
	}
	if pausedAlpha.State != StatePaused {
		t.Fatalf("alpha state = %s, want PAUSED", pausedAlpha.State)
	}

	// Leasing new attempt on paused alpha must refuse inside tx.
	if _, err := f.exec.StartAttempt(ctx, tAlpha.ID, "codex", time.Minute); !errors.Is(err, domain.ErrProjectPaused) {
		t.Fatalf("StartAttempt on paused alpha = %v, want ErrProjectPaused", err)
	}
	// Preparing or dispatching on paused alpha refuses too (either ErrLeaseInactive from revoked lease or ErrProjectPaused).
	if _, err := f.ops.Dispatch(ctx, prepOp.ID, attAlphaOp.ID); err == nil {
		t.Fatal("Dispatch on paused alpha must refuse")
	}

	// Verify in-tx Dispatch gate rejects PAUSED project even if an active lease existed (no pause-vs-dispatch interleaving).
	var errPaused = f.ops.SetEmergency // keep compile check trivial; directly test CheckProjectExecutionAllowedTx
	_ = errPaused
	tx, err := f.svc.store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := f.emerg.CheckProjectExecutionAllowedTx(ctx, tx, alpha.ID, "external_operation"); !errors.Is(err, domain.ErrProjectPaused) {
		t.Fatalf("CheckProjectExecutionAllowedTx on paused alpha = %v, want ErrProjectPaused", err)
	}
	if err := f.emerg.CheckProjectExecutionAllowedTx(ctx, tx, beta.ID, "external_operation"); err != nil {
		t.Fatalf("CheckProjectExecutionAllowedTx on active beta = %v", err)
	}
	_ = tx.Rollback()

	// Beta (still ACTIVE) continues leasing and executing normally.
	attBeta, err := f.exec.StartAttempt(ctx, tBeta.ID, "codex", time.Minute)
	if err != nil {
		t.Fatalf("StartAttempt on active beta failed: %v", err)
	}
	if err := f.exec.RenewLease(ctx, attBeta.ID, time.Minute); err != nil {
		t.Fatalf("RenewLease on active beta failed: %v", err)
	}
	betaOp, err := f.ops.Prepare(ctx, operations.PrepareRequest{
		AttemptID: attBeta.ID, TrustedSlotKey: "slot-beta", Provider: "stub",
		Intent: stubIntent{desc: "low_action", payload: []byte(`{"x":2}`)}, Risk: "LOW",
	})
	if err != nil {
		t.Fatalf("Prepare on active beta failed: %v", err)
	}
	if _, err := f.ops.Dispatch(ctx, betaOp.ID, attBeta.ID); err != nil {
		t.Fatalf("Dispatch on active beta failed: %v", err)
	}
}

func TestCollectiveFreezeStillStopsAllProjects(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()
	alpha := f.createAndFundProject(t, "alpha", 20)
	tAlpha := f.createTaskForProject(t, alpha, "work-freeze")
	tAlphaOp := f.createTaskForProject(t, alpha, "work-freeze-op")
	attOp, err := f.exec.StartAttempt(ctx, tAlphaOp.ID, "codex", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	prepOp, err := f.ops.Prepare(ctx, operations.PrepareRequest{
		AttemptID: attOp.ID, TrustedSlotKey: "slot-freeze", Provider: "stub",
		Intent: stubIntent{desc: "low_action", payload: []byte(`{"x":3}`)}, Risk: "LOW",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Collective Freeze overrides ACTIVE projects.
	freezeCmd, err := emergency.NewOwnerCommand(f.owner, f.collectiveID, emergency.ActionFreeze, "global freeze", 0, f.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.emerg.ApplyOwnerCommand(ctx, freezeCmd); err != nil {
		t.Fatal(err)
	}
	if _, err := f.exec.StartAttempt(ctx, tAlpha.ID, "codex", time.Minute); !errors.Is(err, emergency.ErrBreakGlassFrozen) {
		t.Fatalf("StartAttempt during freeze = %v, want ErrBreakGlassFrozen", err)
	}
	if _, err := f.ops.Dispatch(ctx, prepOp.ID, attOp.ID); !errors.Is(err, emergency.ErrBreakGlassFrozen) {
		t.Fatalf("Dispatch during freeze = %v, want ErrBreakGlassFrozen", err)
	}
}

func TestResumeDoesNotRestoreRevokedLeases(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()
	alpha := f.createAndFundProject(t, "alpha", 20)
	task := f.createTaskForProject(t, alpha, "work-revoke")
	att, err := f.exec.StartAttempt(ctx, task.ID, "codex", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// Pause revokes active Attempt leases on alpha inside the same transaction.
	pauseCmd, err := NewTransitionCommand(f.owner, f.collectiveID, alpha.ID, alpha.Slug, ActionPause, "pause revokes leases", f.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.TransitionSigned(ctx, pauseCmd); err != nil {
		t.Fatal(err)
	}

	var leaseState, attState string
	if err := f.svc.store.DB().QueryRowContext(ctx, `SELECT lease_state, state FROM attempts WHERE attempt_id = ?`, att.ID).Scan(&leaseState, &attState); err != nil {
		t.Fatal(err)
	}
	if leaseState != string(domain.LeaseRevoked) || attState != string(domain.AttemptCancelled) {
		t.Fatalf("after pause attempt=(%s,%s), want (REVOKED,CANCELLED)", leaseState, attState)
	}

	// Resume alpha.
	f.clk.Advance(time.Second)
	resumeCmd, err := NewTransitionCommand(f.owner, f.collectiveID, alpha.ID, alpha.Slug, ActionResume, "resume alpha", f.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := f.svc.TransitionSigned(ctx, resumeCmd)
	if err != nil || resumed.State != StateActive {
		t.Fatalf("resume = (%+v, %v)", resumed, err)
	}

	// Revoked attempt MUST stay revoked and cannot renew; task gets a brand-new Attempt instead.
	if err := f.svc.store.DB().QueryRowContext(ctx, `SELECT lease_state FROM attempts WHERE attempt_id = ?`, att.ID).Scan(&leaseState); err != nil {
		t.Fatal(err)
	}
	if leaseState != string(domain.LeaseRevoked) {
		t.Fatalf("after resume old attempt lease_state = %s, want REVOKED", leaseState)
	}
	if err := f.exec.RenewLease(ctx, att.ID, time.Minute); err == nil {
		t.Fatal("revoked attempt must not renew after resume")
	}
	att2, err := f.exec.StartAttempt(ctx, task.ID, "codex", time.Minute)
	if err != nil || att2.ID == att.ID {
		t.Fatalf("expected fresh attempt after resume, got (%+v, %v)", att2, err)
	}

	// Archive while active lease exists must refuse, then succeed once lease is released/revoked.
	archCmd, err := NewTransitionCommand(f.owner, f.collectiveID, alpha.ID, alpha.Slug, ActionArchive, "retire alpha", f.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.TransitionSigned(ctx, archCmd); err == nil {
		t.Fatal("expected archive while active lease exists to refuse")
	}
	if err := f.exec.RevokeLease(ctx, att2.ID); err != nil {
		t.Fatal(err)
	}
	archCmd2, err := NewTransitionCommand(f.owner, f.collectiveID, alpha.ID, alpha.Slug, ActionArchive, "retire alpha clean", f.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	archived, err := f.svc.TransitionSigned(ctx, archCmd2)
	if err != nil || archived.State != StateArchived || archived.ArchivedAt == nil {
		t.Fatalf("archive = (%+v, %v)", archived, err)
	}
	// Funding an archived project must refuse.
	gArch, err := resources.NewOwnerGrant(f.owner, f.collectiveID, resources.GrantSetLimit, alpha.EnvelopeID, 30, "fund archived", f.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.FundSigned(ctx, f.res, alpha.Slug, gArch); !errors.Is(err, domain.ErrProjectArchived) {
		t.Fatalf("FundSigned on archived project = %v, want ErrProjectArchived", err)
	}
}

type stubIntent struct {
	desc    string
	payload []byte
}

func (i stubIntent) DescriptorType() string { return i.desc }
