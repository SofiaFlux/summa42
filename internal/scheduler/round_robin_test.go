package scheduler_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/bootstrap"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/project"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/resources"
	"github.com/SofiaFlux/summa42/internal/scheduler"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

func TestSchedulerRoundRobinPreventsBusyProjectStarvation(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC))
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
	initRes, err := bootstrap.New(store, clk).Init(ctx, bootstrap.InitRequest{
		Constitution: []byte(bootstrap.DefaultConstitution), Owner: owner, Cube: cube, ConstitutionalRoot: rootKey,
	})
	if err != nil {
		t.Fatal(err)
	}

	purp := purpose.New(store, clk)
	mID, err := purp.CreateMission(ctx, "Fair multi-project work")
	if err != nil {
		t.Fatal(err)
	}
	projSvc := project.New(store, clk)
	resSvc := resources.New(store, clk)
	execSvc := execution.New(store, clk, purp)
	svc := scheduler.New(store, clk, purp, execSvc, resSvc, time.Minute)

	gRoot, _ := resources.NewOwnerGrant(owner, initRes.CollectiveID, resources.GrantSetLimit, project.CollectiveRootEnvelopeID, 100, "root", clk.Now())
	if _, err := resSvc.ApplyOwnerGrant(ctx, gRoot); err != nil {
		t.Fatal(err)
	}

	makeProject := func(slug string) project.Project {
		t.Helper()
		cCmd, err := project.NewCreateCommand(owner, initRes.CollectiveID, slug, slug, "create "+slug, clk.Now())
		if err != nil {
			t.Fatal(err)
		}
		p, err := projSvc.CreateSigned(ctx, cCmd)
		if err != nil {
			t.Fatal(err)
		}
		fg, err := resources.NewOwnerGrant(owner, initRes.CollectiveID, resources.GrantSetLimit, p.EnvelopeID, 30, "fund "+slug, clk.Now())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := resSvc.ApplyOwnerGrant(ctx, fg); err != nil {
			t.Fatal(err)
		}
		return p
	}

	busy := makeProject("busy")
	quiet := makeProject("quiet")
	paused := makeProject("paused")

	// Queue 4 tasks on busy earlier, then 2 on quiet later, plus 1 on paused.
	for i := 0; i < 4; i++ {
		if _, err := execSvc.CreateTask(ctx, execution.TaskRequest{
			Purpose:   domain.PurposeRef{Kind: domain.PurposeMission, ID: mID},
			Objective: "busy task", AcceptanceCriteria: []string{"ok"},
			ResourceEnvelopeID: busy.EnvelopeID, ProjectID: busy.ID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := execSvc.CreateTask(ctx, execution.TaskRequest{
			Purpose:   domain.PurposeRef{Kind: domain.PurposeMission, ID: mID},
			Objective: "quiet task", AcceptanceCriteria: []string{"ok"},
			ResourceEnvelopeID: quiet.EnvelopeID, ProjectID: quiet.ID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := execSvc.CreateTask(ctx, execution.TaskRequest{
		Purpose:   domain.PurposeRef{Kind: domain.PurposeMission, ID: mID},
		Objective: "paused task", AcceptanceCriteria: []string{"ok"},
		ResourceEnvelopeID: paused.EnvelopeID, ProjectID: paused.ID,
	}); err != nil {
		t.Fatal(err)
	}
	pCmd, err := project.NewTransitionCommand(owner, initRes.CollectiveID, paused.ID, paused.Slug, project.ActionPause, "pause", clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := projSvc.TransitionSigned(ctx, pCmd); err != nil {
		t.Fatal(err)
	}

	// Lease first 4 picks: paused must NEVER be picked; busy and quiet must alternate.
	var picked []domain.ID
	for i := 0; i < 4; i++ {
		cand, err := svc.Next(ctx, scheduler.CapacitySnapshot{Capabilities: map[string]scheduler.CapabilityCapacity{}})
		if err != nil {
			t.Fatalf("Next(%d): %v", i, err)
		}
		if cand == nil {
			t.Fatalf("Next(%d) returned nil unexpectedly", i)
		}
		picked = append(picked, cand.Task.ProjectID)
		if _, err := svc.Lease(ctx, cand.Task.ID, "codex"); err != nil {
			t.Fatalf("Lease(%d): %v", i, err)
		}
	}
	want := []domain.ID{busy.ID, quiet.ID, busy.ID, quiet.ID}
	if !slices.Equal(picked, want) {
		t.Fatalf("scheduler round-robin projects = %v, want %v (busy must not starve quiet; paused must be skipped)", picked, want)
	}
}
