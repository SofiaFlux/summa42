package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/bootstrap"
	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/control"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/policy"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type runtimeStatusProvider struct {
	collectiveID domain.ID
}

func (p runtimeStatusProvider) Status(context.Context) (control.StatusDTO, error) {
	return control.StatusDTO{CollectiveID: p.collectiveID, State: "DORMANT"}, nil
}

func TestProjectAndMissionSwitchEndToEndThroughRuntimeBox(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	keyDir := filepath.Join(dir, "keys")
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

	initStore, err := state.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	initRes, err := bootstrap.New(initStore, clock.System{}).Init(ctx, bootstrap.InitRequest{
		Constitution:       []byte(bootstrap.DefaultConstitution),
		Owner:              owner,
		Cube:               cube,
		ConstitutionalRoot: rootKey,
	})
	_ = initStore.DB().Close()
	if err != nil {
		t.Fatal(err)
	}

	const mod = `package summa42` + "\n" + `default decision := {"outcome": "DENY", "reason_codes": ["deny"]}`
	modSum := sha256.Sum256([]byte(mod))
	box, err := summa42runtime.Open(ctx, summa42runtime.Config{
		StatePath:        dbPath,
		EvidencePath:     filepath.Join(dir, "evidence"),
		Clock:            clock.System{},
		CollectiveID:     initRes.CollectiveID,
		OwnerPrincipalID: owner.PrincipalID(),
		CubePrincipalID:  cube.PrincipalID(),
		PolicyEngine: policy.NewOPAEngine(policy.OPAConfig{
			ModuleName: "summa42.rego", Module: mod, PolicySetID: initRes.PolicySetID, PolicySetHash: hex.EncodeToString(modSum[:]),
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()

	srv, err := control.NewServer(control.ServerConfig{
		AuthToken:        "test-token",
		OwnerPrincipalID: owner.PrincipalID(),
		OwnerPublicKey:   owner.PublicKey(),
		ChallengeTTL:     time.Minute,
	}, control.Dependencies{
		Status:     runtimeStatusProvider{collectiveID: initRes.CollectiveID},
		Projects:   box.Projects,
		Tasks:      box.Execution,
		Missions:   box.Purpose,
		Approvals:  box.Approvals,
		Attempts:   box.Execution,
		Operations: box.Operations,
		Shutdown:   box,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	api := control.NewClient(httpSrv.URL, "test-token", httpSrv.Client())
	ownerKeyPath := filepath.Join(keyDir, "owner.key")

	out := executeCommand(t, newRootCommandWithClient(api), "--json", "project", "create", "daas", "--name", "DaaS Product", "--owner-key", ownerKeyPath)
	if !strings.Contains(out, `"slug":"daas"`) {
		t.Fatalf("project create output=%s", out)
	}
	// Prepare valid project.json and test `summa42 project apply`.
	repoDir := filepath.Join(dir, "repo-daas")
	wsDir := filepath.Join(dir, "ws-daas")
	credFile := filepath.Join(dir, "gh-token")
	for _, p := range []string{repoDir, wsDir, filepath.Join(dir, "projects", "daas")} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(credFile, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestJSON := `{"schema_version":1,"project":"daas","mission_statement":"Launch DaaS","data_class":"public","workspace_root":"` + wsDir + `","repositories":[{"id":"main","host":"github","repository":"SofiaFlux/daas","local_path":"` + repoDir + `","base_branch":"main","credential_file":"` + credFile + `","data_class":"public","allowed_prefixes":["src/"],"intake":{"maintainers":["alice"]},"case_grants":{"capabilities":["github.issue.read"],"actions":["github.issue.read"]}}],"models":{"producer":{"binary":"codex","user":"nobody","timeout":"10m"},"reviewer":{"binary":"claude","user":"daemon","timeout":"10m"}},"limits":{"max_steps":3,"remaining_budget":10,"max_repairs":1}}`
	if err := os.WriteFile(filepath.Join(dir, "projects", "daas", "project.json"), []byte(manifestJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	// Off-TTY without SUMMA42_APPROVE_NONINTERACTIVE=1 and --yes must be refused.
	prevTTY := stdinIsTTY
	stdinIsTTY = func() bool { return false }
	t.Cleanup(func() { stdinIsTTY = prevTTY })
	rootNoYes := newRootCommandWithClient(api)
	rootNoYes.SetArgs([]string{"project", "apply", "daas", "--home", dir, "--owner-key", ownerKeyPath})
	if err := rootNoYes.Execute(); err == nil {
		t.Fatal("expected project apply without --yes off-TTY to fail")
	}
	t.Setenv("SUMMA42_APPROVE_NONINTERACTIVE", "1")
	applyOut := executeCommand(t, newRootCommandWithClient(api), "--json", "project", "apply", "daas", "--home", dir, "--owner-key", ownerKeyPath, "--yes", "--reason", "apply daas")
	if !strings.Contains(applyOut, `"manifest_sha256":"`) {
		t.Fatalf("project apply output=%s", applyOut)
	}
	listOut := executeCommand(t, newRootCommandWithClient(api), "project", "list")
	if !strings.Contains(listOut, "default") || !strings.Contains(listOut, "daas") {
		t.Fatalf("project list = %s", listOut)
	}

	m1, err := box.Purpose.CreateMission(ctx, "Core systems")
	if err != nil {
		t.Fatal(err)
	}
	m2, err := box.Purpose.CreateStandbyMission(ctx, "DaaS launch")
	if err != nil {
		t.Fatal(err)
	}
	swOut := executeCommand(t, newRootCommandWithClient(api), "--json", "mission", "switch", string(m2), "--owner-key", ownerKeyPath)
	if !strings.Contains(swOut, string(m2)) {
		t.Fatalf("mission switch output=%s", swOut)
	}
	activeOut := executeCommand(t, newRootCommandWithClient(api), "mission", "show")
	if !strings.Contains(activeOut, "DaaS launch") {
		t.Fatalf("mission show = %s", activeOut)
	}
	missionsOut := executeCommand(t, newRootCommandWithClient(api), "mission", "list")
	if !strings.Contains(missionsOut, string(m1)) || !strings.Contains(missionsOut, string(m2)) {
		t.Fatalf("mission list = %s", missionsOut)
	}
}
