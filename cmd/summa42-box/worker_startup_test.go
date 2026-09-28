package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/bootstrap"
	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

// initializedCollective points SUMMA42_HOME at a bootstrapped Collective, the
// same way `summa42 init` leaves it, so runWorker can be driven end to end. The
// only executor configured is copilot: no ADO provider and no triage model, so
// this is a box whose work is not triage work.
func initializedCollective(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	store, err := state.Open(context.Background(), filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()

	owner, err := identity.NewLocalEd25519(filepath.Join(root, "keys", "owner.key"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	cube, err := identity.NewLocalEd25519(filepath.Join(root, "keys", "cube.key"), "cube")
	if err != nil {
		t.Fatal(err)
	}
	constitutionalRoot, err := identity.NewConstitutionalRootForCeremony(filepath.Join(root, "ceremony", "root.key"))
	if err != nil {
		t.Fatal(err)
	}
	initialized, err := bootstrap.New(store, clock.System{}).Init(context.Background(), bootstrap.InitRequest{
		Constitution:       []byte(bootstrap.DefaultConstitution),
		Owner:              owner,
		Cube:               cube,
		ConstitutionalRoot: constitutionalRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := localconfig.Write(home, localconfig.Config{
		Version:                       localconfig.CurrentVersion,
		CollectiveID:                  initialized.CollectiveID,
		OwnerPrincipalID:              initialized.OwnerPrincipalID,
		CubePrincipalID:               initialized.CubePrincipalID,
		ConstitutionalRootPrincipalID: initialized.ConstitutionalRootPrincipalID,
		ConstitutionHash:              initialized.ConstitutionHash,
		ActivePolicySetID:             initialized.PolicySetID,
		DatabasePath:                  filepath.Join(root, "state.db"),
		EvidencePath:                  filepath.Join(root, "evidence"),
		ControlToken:                  strings.Repeat("t", 32),
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUMMA42_HOME", home)
	clearPublishEnv(t)
	t.Setenv("SUMMA42_ADO_MCP_COMMAND", "")
	t.Setenv("SUMMA42_COPILOT_PATH", "/bin/true")
	t.Setenv("SUMMA42_COPILOT_MCP_SERVER", "ado")
	t.Setenv("SUMMA42_COPILOT_TOOLS", "")
	t.Setenv("SUMMA42_COPILOT_TIMEOUT", "")
	return home
}

// A model binary that is not on PATH is a missing optional dependency: the box
// still starts. runWorker polls until its context ends, so the deadline below
// is what stops it: the box has to open, advertise and idle first, and the
// fresh database holds no Task for it to claim. The deadline is generous
// against a slow machine; it bounds the test, not the assertion.
func TestRunWorkerStartsWithoutAUsableModelBinary(t *testing.T) {
	initializedCollective(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := runWorker(ctx, []string{
		"--workspace-root=" + t.TempDir(),
		"--model-binary=" + filepath.Join(t.TempDir(), "absent"),
	})
	if err != nil {
		t.Fatalf("runWorker with no usable model binary: %v", err)
	}
}

func TestRunWorkerStartsWithAUsableModelBinary(t *testing.T) {
	initializedCollective(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := runWorker(ctx, []string{
		"--workspace-root=" + t.TempDir(),
		"--model-binary=" + usableModelBinary(t),
		"--model-timeout=5s",
	}); err != nil {
		t.Fatalf("runWorker with a usable model binary: %v", err)
	}
}

// A model flag that cannot be honoured is a misconfiguration, not a missing
// dependency, so it still stops the box from starting. The degradation covers a
// model that cannot be invoked and must not swallow this. The deadline bounds
// the test the way it bounds the two above: a regression that stops parsing
// --model-timeout would otherwise run this to the package timeout.
func TestRunWorkerStillRejectsAMalformedModelTimeout(t *testing.T) {
	initializedCollective(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := runWorker(ctx, []string{
		"--workspace-root=" + t.TempDir(),
		"--model-timeout=nope",
	})
	if err == nil {
		t.Fatal("run-worker started with an unparseable --model-timeout")
	}
	if !strings.Contains(err.Error(), "--model-timeout") {
		t.Fatalf("error = %q, want it to name --model-timeout", err)
	}
	if strings.Contains(err.Error(), ghtriage.ExecutorKind) {
		t.Fatalf("error = %q, want a configuration error and not a degraded registration", err)
	}
}
