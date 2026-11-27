//go:build unix

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/bootstrap"
	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/project"
	"github.com/SofiaFlux/summa42/internal/purpose"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type multiProjectFixture struct {
	home         string
	cfg          localconfig.Config
	collectiveID string
	missionID    string
	owner        identity.Signer
	projects     *project.Service
}

func setupMultiProjectServeFixture(t *testing.T) multiProjectFixture {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("SUMMA42_HOME", home)
	stateDir := filepath.Join(home, "state")
	evDir := filepath.Join(home, "evidence")
	keysDir := filepath.Join(home, "keys")
	for _, d := range []string{stateDir, evDir, keysDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	dbPath := filepath.Join(stateDir, "summa42.db")
	store, err := state.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.DB().Close() })

	owner, err := identity.CreateLocalEd25519(filepath.Join(keysDir, "owner.key"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	cube, err := identity.CreateLocalEd25519(filepath.Join(keysDir, "cube.key"), "cube")
	if err != nil {
		t.Fatal(err)
	}
	rootKey, err := identity.CreateLocalEd25519(filepath.Join(keysDir, "root.key"), "root")
	if err != nil {
		t.Fatal(err)
	}
	initRes, err := bootstrap.New(store, clock.System{}).Init(ctx, bootstrap.InitRequest{
		Constitution: []byte(bootstrap.DefaultConstitution), Owner: owner, Cube: cube, ConstitutionalRoot: rootKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := localconfig.Config{
		Version:                       localconfig.CurrentVersion,
		CollectiveID:                  initRes.CollectiveID,
		OwnerPrincipalID:              initRes.OwnerPrincipalID,
		CubePrincipalID:               initRes.CubePrincipalID,
		ConstitutionalRootPrincipalID: initRes.ConstitutionalRootPrincipalID,
		ConstitutionHash:              initRes.ConstitutionHash,
		ActivePolicySetID:             initRes.PolicySetID,
		DatabasePath:                  dbPath,
		EvidencePath:                  evDir,
		ControlToken:                  "0123456789abcdef0123456789abcdef",
	}
	if err := localconfig.Write(home, cfg); err != nil {
		t.Fatal(err)
	}
	mID, err := purpose.New(store, clock.System{}).CreateMission(ctx, "Multi-project mission")
	if err != nil {
		t.Fatal(err)
	}
	return multiProjectFixture{
		home: home, cfg: cfg, collectiveID: string(initRes.CollectiveID),
		missionID: string(mID), owner: owner, projects: project.New(store, clock.System{}),
	}
}

func (f multiProjectFixture) addProjectWithManifest(t *testing.T, slug string, apply bool) project.Project {
	t.Helper()
	ctx := context.Background()
	cmd, err := project.NewCreateCommand(f.owner, domain.ID(f.collectiveID), slug, slug, "create "+slug, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	proj, err := f.projects.CreateSigned(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(f.home, "repos", slug)
	wsDir := filepath.Join(f.home, "ws", slug)
	credFile := filepath.Join(f.home, "cred_"+slug)
	for _, d := range []string{repoDir, wsDir, filepath.Join(f.home, "projects", slug)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(credFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(map[string]any{
		"schema_version":    1,
		"project":           slug,
		"mission_statement": "Serve " + slug,
		"data_class":        "public",
		"workspace_root":    wsDir,
		"repositories": []map[string]any{{
			"id":               "main",
			"host":             "github",
			"repository":       "SofiaFlux/" + slug,
			"local_path":       repoDir,
			"base_branch":      "main",
			"credential_file":  credFile,
			"data_class":       "public",
			"allowed_prefixes": []string{"internal/"},
			"intake":           map[string]any{"maintainers": []string{"owner-login"}, "poll_interval": "15s"},
			"case_grants": map[string]any{
				"capabilities": []string{"github.issue.read", "workspace.repo.write"},
				"actions":      []string{"github.issue.read", "workspace.repo.write"},
			},
		}},
		"models": map[string]any{
			"producer": map[string]any{"binary": "codex", "user": "nobody", "timeout": "10m"},
			"reviewer": map[string]any{"binary": "claude", "user": "daemon", "timeout": "10m"},
		},
		"limits": map[string]any{"max_steps": 3, "remaining_budget": 10, "max_repairs": 1},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.home, "projects", slug, "project.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if apply {
		_, digest, err := project.LoadManifestFile(f.home, slug)
		if err != nil {
			t.Fatal(err)
		}
		aCmd, err := project.NewApplyCommand(f.owner, domain.ID(f.collectiveID), proj.ID, slug, digest, "apply "+slug, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if proj, err = f.projects.ApplyManifestSigned(ctx, aCmd); err != nil {
			t.Fatal(err)
		}
	}
	return proj
}

func TestTwoProjectsBothAdvance(t *testing.T) {
	f := setupMultiProjectServeFixture(t)
	f.addProjectWithManifest(t, "alpha", true)
	f.addProjectWithManifest(t, "beta", true)

	var mu sync.Mutex
	var order []string
	coord := &multiProjectCoordinator{
		home: f.home, cfg: f.cfg,
		runner: func(_ context.Context, slug string, intake, pipeline, worker []string) error {
			if !slices.Contains(intake, "--envelope=envelope_project_"+slug) {
				t.Errorf("%s intake missing project envelope: %v", slug, intake)
			}
			if len(pipeline) == 0 || len(worker) == 0 {
				t.Errorf("%s empty pipeline/worker args", slug)
			}
			mu.Lock()
			order = append(order, slug)
			mu.Unlock()
			return nil
		},
	}

	if err := coord.runRound(context.Background()); err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if err := coord.runRound(context.Background()); err != nil {
		t.Fatalf("round 2: %v", err)
	}
	want := []string{"alpha", "beta", "alpha", "beta"}
	if !slices.Equal(order, want) {
		t.Fatalf("round-robin order = %v, want %v", order, want)
	}
}

func TestBusyProjectDoesNotStarveQuietOne(t *testing.T) {
	f := setupMultiProjectServeFixture(t)
	f.addProjectWithManifest(t, "busy", true)
	f.addProjectWithManifest(t, "quiet", true)

	var mu sync.Mutex
	counts := map[string]int{}
	var order []string
	coord := &multiProjectCoordinator{
		home: f.home, cfg: f.cfg, passTimeout: 20 * time.Millisecond,
		runner: func(ctx context.Context, slug string, _, _, _ []string) error {
			mu.Lock()
			counts[slug]++
			order = append(order, slug)
			n := counts[slug]
			mu.Unlock()
			if slug == "busy" {
				if n == 2 {
					// Simulate a hung stage: per-project timeout cancels only busy's passCtx.
					<-ctx.Done()
					return ctx.Err()
				}
				return errors.New("busy upstream error")
			}
			return nil
		},
	}

	for i := 0; i < 3; i++ {
		if err := coord.runRound(context.Background()); err != nil {
			t.Fatalf("round %d unexpected err: %v", i, err)
		}
	}
	if counts["busy"] != 3 || counts["quiet"] != 3 {
		t.Fatalf("counts = %v, want busy=3 and quiet=3", counts)
	}
	want := []string{"busy", "quiet", "busy", "quiet", "busy", "quiet"}
	if !slices.Equal(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

func TestArchivedProjectIsNotServed(t *testing.T) {
	f := setupMultiProjectServeFixture(t)
	ctx := context.Background()
	f.addProjectWithManifest(t, "active-proj", true)
	paused := f.addProjectWithManifest(t, "paused-proj", true)
	archived := f.addProjectWithManifest(t, "archived-proj", true)
	f.addProjectWithManifest(t, "unapplied-proj", false)

	pCmd, err := project.NewTransitionCommand(f.owner, domain.ID(f.collectiveID), paused.ID, paused.Slug, project.ActionPause, "pause", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.projects.TransitionSigned(ctx, pCmd); err != nil {
		t.Fatal(err)
	}
	aCmd, err := project.NewTransitionCommand(f.owner, domain.ID(f.collectiveID), archived.ID, archived.Slug, project.ActionArchive, "archive", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.projects.TransitionSigned(ctx, aCmd); err != nil {
		t.Fatal(err)
	}

	var served []string
	coord := &multiProjectCoordinator{
		home: f.home, cfg: f.cfg,
		runner: func(_ context.Context, slug string, _, _, _ []string) error {
			served = append(served, slug)
			return nil
		},
	}
	if err := coord.runRound(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(served, []string{"active-proj"}) {
		t.Fatalf("served = %v, want only [active-proj] (paused, archived, and unapplied must not run)", served)
	}
}
