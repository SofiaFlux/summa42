package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/bootstrap"
	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/project"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

func setupBoxManifestHome(t *testing.T) (home string, collectiveID, missionID string, owner identity.Signer, manifestJSON []byte) {
	t.Helper()
	ctx := context.Background()
	home = t.TempDir()
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
	defer store.DB().Close()

	owner, err = identity.CreateLocalEd25519(filepath.Join(keysDir, "owner.key"), "owner")
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
	if err := localconfig.Write(home, localconfig.Config{
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
	}); err != nil {
		t.Fatal(err)
	}

	repoDir := filepath.Join(home, "repos", "summa42")
	wsDir := filepath.Join(home, "ws")
	credFile := filepath.Join(home, "github_token")
	for _, d := range []string{repoDir, wsDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(credFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	valUser := "bin"
	manifest := map[string]any{
		"schema_version":    1,
		"project":           "summa42",
		"mission_statement": "Maintain summa42",
		"data_class":        "public",
		"workspace_root":    wsDir,
		"repositories": []map[string]any{{
			"id":               "main",
			"host":             "github",
			"repository":       "SofiaFlux/summa42",
			"local_path":       repoDir,
			"base_branch":      "main",
			"credential_file":  credFile,
			"data_class":       "public",
			"allowed_prefixes": []string{"internal/"},
			"context_files":    []string{"AGENTS.md"},
			"validation": map[string]any{
				"commands": [][]string{{"go", "test", "./..."}},
				"user":     valUser,
				"timeout":  "10m",
			},
			"intake": map[string]any{
				"maintainers":      []string{"owner-login"},
				"bot_login":        "summa42-bot",
				"auto_grant_label": "summa42-pilot",
				"poll_interval":    "45s",
			},
			"case_grants": map[string]any{
				"capabilities":      []string{"github.issue.read", "workspace.repo.write", "workspace.test"},
				"actions":           []string{"github.issue.read", "workspace.repo.write", "workspace.test"},
				"work_capabilities": []string{"workspace.repo.write", "workspace.test"},
			},
		}},
		"models": map[string]any{
			"producer": map[string]any{
				"binary": "codex", "user": "nobody", "timeout": "20m",
			},
			"reviewer": map[string]any{
				"binary": "claude", "user": "daemon", "timeout": "15m",
			},
		},
		"limits": map[string]any{
			"max_steps":          4,
			"remaining_budget":   12,
			"max_repairs":        2,
			"lease_duration":     "6m",
			"heartbeat_interval": "1m",
			"max_wall_time":      "2h",
		},
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "projects", "summa42"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "projects", "summa42", "project.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	projSvc := project.New(store, clock.System{})
	cCmd, err := project.NewCreateCommand(owner, initRes.CollectiveID, "summa42", "Summa42", "Create", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := projSvc.CreateSigned(ctx, cCmd); err != nil {
		t.Fatal(err)
	}
	mID := "mission_test_1"
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO missions(mission_id, statement, active, created_at) VALUES (?, 'Maintain', 1, '2026-11-27T12:00:00Z')`, mID); err != nil {
		t.Fatal(err)
	}
	return home, string(initRes.CollectiveID), mID, owner, raw
}

func TestUnsignedManifestRefused(t *testing.T) {
	setupBoxManifestHome(t)
	err := runGHIntake(context.Background(), []string{"--project", "summa42", "--once"})
	if !errors.Is(err, project.ErrManifestUnsigned) {
		t.Fatalf("runGHIntake unsigned manifest err = %v, want ErrManifestUnsigned", err)
	}
	err = runServe(context.Background(), []string{"--project", "summa42"})
	if !errors.Is(err, project.ErrManifestUnsigned) {
		t.Fatalf("runServe unsigned manifest err = %v, want ErrManifestUnsigned", err)
	}
}

func TestEditedManifestRefusedUntilReapplied(t *testing.T) {
	home, colID, _, owner, raw := setupBoxManifestHome(t)
	cfg, err := localconfig.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(context.Background(), cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	projSvc := project.New(store, clock.System{})
	proj, err := projSvc.Get(context.Background(), "summa42")
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := project.LoadManifestFile(home, "summa42")
	if err != nil {
		t.Fatal(err)
	}
	applyCmd, err := project.NewApplyCommand(owner, domain.ID(colID), proj.ID, "summa42", digest, "apply", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := projSvc.ApplyManifestSigned(context.Background(), applyCmd); err != nil {
		t.Fatal(err)
	}
	_ = store.DB().Close()

	// Edit project.json without reapplying.
	edited := strings.Replace(string(raw), `"max_steps": 4`, `"max_steps": 9`, 1)
	if err := os.WriteFile(filepath.Join(home, "projects", "summa42", "project.json"), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	err = runGHIntake(context.Background(), []string{"--project", "summa42", "--once"})
	if !errors.Is(err, project.ErrManifestMismatch) {
		t.Fatalf("runGHIntake edited manifest err = %v, want ErrManifestMismatch", err)
	}
}

func TestPipelineFlagsAndManifestAreEquivalent(t *testing.T) {
	home, colID, mID, owner, _ := setupBoxManifestHome(t)
	cfg, err := localconfig.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(context.Background(), cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	projSvc := project.New(store, clock.System{})
	proj, err := projSvc.Get(context.Background(), "summa42")
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := project.LoadManifestFile(home, "summa42")
	if err != nil {
		t.Fatal(err)
	}
	applyCmd, err := project.NewApplyCommand(owner, domain.ID(colID), proj.ID, "summa42", digest, "apply", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := projSvc.ApplyManifestSigned(context.Background(), applyCmd); err != nil {
		t.Fatal(err)
	}
	_ = store.DB().Close()

	intakeArgs, pipelineArgs, workerArgs, err := manifestArgsForProject(context.Background(), home, cfg, "summa42", true)
	if err != nil {
		t.Fatalf("manifestArgsForProject failed: %v", err)
	}

	gotIntake, gotPoll, err := parseGHIntakeFlags(intakeArgs)
	if err != nil {
		t.Fatalf("parseGHIntakeFlags(manifest): %v", err)
	}
	explicitIntake, explicitPoll, err := parseGHIntakeFlags([]string{
		"--mission", mID,
		"--envelope", string(proj.EnvelopeID),
		"--repo", "SofiaFlux/summa42",
		"--maintainer", "owner-login",
		"--grant-capability", "github.issue.read",
		"--grant-capability", "workspace.repo.write",
		"--grant-capability", "workspace.test",
		"--grant-action", "github.issue.read",
		"--grant-action", "workspace.repo.write",
		"--grant-action", "workspace.test",
		"--work-capability", "workspace.repo.write",
		"--work-capability", "workspace.test",
		"--bot-login", "summa42-bot",
		"--auto-grant-label", "summa42-pilot",
		"--credential-file", filepath.Join(home, "github_token"),
		"--max-steps", "4",
		"--remaining-budget", "12",
		"--poll-interval", "45s",
		"--once",
	})
	if err != nil {
		t.Fatalf("parseGHIntakeFlags(explicit): %v", err)
	}
	if !reflect.DeepEqual(gotIntake, explicitIntake) || gotPoll != explicitPoll {
		t.Fatalf("intake mismatch:\ngot = %+v (%s)\nwant = %+v (%s)", gotIntake, gotPoll, explicitIntake, explicitPoll)
	}

	gotWorker, err := parseWorkerFlagsCtx(context.Background(), workerArgs)
	if err != nil {
		t.Fatalf("parseWorkerFlagsCtx(manifest): %v", err)
	}
	explicitWorker, err := parseWorkerFlagsCtx(context.Background(), []string{
		"--workspace-root", filepath.Join(home, "ws"),
		"--poll-interval", "45s",
		"--lease-duration", "6m",
		"--heartbeat-interval", "1m",
		"--max-wall-time", "2h",
		"--credential-file", filepath.Join(home, "github_token"),
		"--model-binary", "codex",
		"--model-user", "nobody",
		"--model-timeout", "20m",
		"--once",
	})
	if err != nil {
		t.Fatalf("parseWorkerFlagsCtx(explicit): %v", err)
	}
	if !reflect.DeepEqual(gotWorker, explicitWorker) {
		t.Fatalf("worker flags mismatch:\ngot = %+v\nwant = %+v", gotWorker, explicitWorker)
	}

	gotPipe, gotInterval, err := parsePipelineFlags([]string{"--project", "summa42", "--once"})
	if err != nil {
		t.Fatalf("parsePipelineFlags(--project): %v", err)
	}
	valUser := "bin"
	explicitPipe, explicitInterval, err := parsePipelineFlags([]string{
		"--mission", mID,
		"--source-repo", filepath.Join(home, "repos", "summa42"),
		"--source-repository", "SofiaFlux/summa42",
		"--repository", "SofiaFlux/summa42",
		"--base-branch", "main",
		"--workspace-root", filepath.Join(home, "ws"),
		"--credential-file", filepath.Join(home, "github_token"),
		"--max-repairs", "2",
		"--model-binary", "codex", "--model-user", "nobody", "--model-timeout", "20m",
		"--review-model-binary", "claude", "--review-model-user", "daemon", "--review-model-timeout", "15m",
		"--allowed-prefix", "internal/",
		"--context-file", "AGENTS.md",
		"--validation-command-json", `["go","test","./..."]`,
		"--validation-user", valUser,
		"--validation-timeout", "10m",
		"--once",
	})
	if err != nil {
		t.Fatalf("parsePipelineFlags(explicit): %v", err)
	}
	if gotInterval != explicitInterval ||
		gotPipe.producerUser != explicitPipe.producerUser ||
		gotPipe.reviewerUser != explicitPipe.reviewerUser ||
		gotPipe.reviewerPool != explicitPipe.reviewerPool ||
		!reflect.DeepEqual(gotPipe.reviewArgv, explicitPipe.reviewArgv) {
		t.Fatalf("pipeline mismatch:\ngot=(%s,%+v)\nwant=(%s,%+v)", gotInterval, gotPipe, explicitInterval, explicitPipe)
	}
	if _, _, err := parsePipelineFlags([]string{"--project", "summa42", "--once", "--base-branch", "main"}); err == nil {
		t.Fatal("expected --project + conflicting pipeline flag to fail")
	}
	if len(pipelineArgs) == 0 {
		t.Fatal("pipelineArgs empty")
	}
}
