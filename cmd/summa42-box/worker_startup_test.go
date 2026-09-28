package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/bootstrap"
	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

// initializedCollective points SUMMA42_HOME at a bootstrapped Collective, the
// same way `summa42 init` leaves it, so runWorker can be driven end to end, and
// returns the local config that Collective is reachable by, which is what a
// test needs to open a Box at all. The only executor configured is copilot: no
// ADO provider and no triage model, so this is a box whose work is not triage
// work.
func initializedCollective(t *testing.T) localconfig.Config {
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
	config := localconfig.Config{
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
	}
	if err := localconfig.Write(home, config); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUMMA42_HOME", home)
	clearPublishEnv(t)
	t.Setenv("SUMMA42_ADO_MCP_COMMAND", "")
	t.Setenv("SUMMA42_COPILOT_PATH", "/bin/true")
	t.Setenv("SUMMA42_COPILOT_MCP_SERVER", "ado")
	t.Setenv("SUMMA42_COPILOT_TOOLS", "")
	t.Setenv("SUMMA42_COPILOT_TIMEOUT", "")
	return config
}

// workerRuntimeConfigFor builds the configuration a run-worker box opens with,
// over a bootstrapped Collective: the state, evidence and principals the local
// config names, the policy set the Collective was initialized with, and the
// copilot executor the environment offers. copilot matters: it is the kind that
// sorts before the triage kind, so a registry that holds it is a registry the
// alphabetical baseline would hand a triage Task to.
func workerRuntimeConfigFor(t *testing.T, ctx context.Context, config localconfig.Config) summa42runtime.Config {
	t.Helper()
	material, err := loadStartupMaterial(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	copilot, err := buildCopilotExecutorFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	runtimeCfg := summa42runtime.Config{
		StatePath:        config.DatabasePath,
		EvidencePath:     config.EvidencePath,
		CollectiveID:     config.CollectiveID,
		OwnerPrincipalID: config.OwnerPrincipalID,
		PolicyEngine:     material.policyEngine,
	}
	if len(copilot) > 0 {
		runtimeCfg.Executors = copilot
	}
	return runtimeCfg
}

// captureStderr points the process's os.Stderr at a pipe for the rest of the
// test and returns the reader for it. run-worker reports every executor kind it
// could not register on os.Stderr, which is the only place the box's notices go,
// so counting them means capturing that stream; the package runs no parallel
// tests, and the original stream is restored through t.Cleanup. A test that adds
// t.Parallel() must not use this: the swap is process-wide.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = writer
	t.Cleanup(func() {
		os.Stderr = original
		_ = writer.Close()
		_ = reader.Close()
	})
	var captured bytes.Buffer
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(&captured, reader)
	}()
	return func() string {
		_ = writer.Close()
		<-drained
		return captured.String()
	}
}

// triageNotices counts the "executor kind github-issue-triage is not registered"
// lines in a captured stderr. ado-publish and copilot are reported in the same
// shape, so counting the triage line is what isolates the one-notice clause.
func triageNotices(captured string) int {
	return strings.Count(captured, fmt.Sprintf("executor kind %s is not registered", ghtriage.ExecutorKind))
}

// A usable model binary has to reach the opened box in all three places it
// matters: the registry key workerCapacity reads, the capability that key
// derives, and the TaskClassRouting the Box consumed before its scheduler was
// built. Missing any of them is one of the two defects this composition exists
// to remove, and none of them is visible from the outside of a started box.
func TestOpenWorkerBoxRegistersTriageAndRoutesTheTaskClass(t *testing.T) {
	ctx := context.Background()
	config := initializedCollective(t)
	var stderr bytes.Buffer
	box, verdict, err := openWorkerBox(ctx, workerRuntimeConfigFor(t, ctx, config), triageModelConfigFor(t,
		"--model-binary="+usableModelBinary(t), "--model-timeout=5s"), &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()

	if _, registered := box.Executors[ghtriage.ExecutorKind]; !registered {
		t.Fatalf("registry = %v, want the %s key: without it no triage Task is leasable", box.Executors, ghtriage.ExecutorKind)
	}
	capacity, err := workerCapacity(box)
	if err != nil {
		t.Fatal(err)
	}
	available, advertised := capacity.Capabilities[ghtriage.RequiredCapability]
	if !advertised || !available.Accessible || available.Enforcement != domain.EnforcementEnforced {
		t.Fatalf("capacity[%q] = %+v (advertised %t), want enforced accessible capability",
			ghtriage.RequiredCapability, available, advertised)
	}
	if kind := verdict.routing[ghtriage.TaskClass]; kind != ghtriage.ExecutorKind {
		t.Fatalf("routing[%q] = %q, want %q: the Box consumed this before its scheduler was built, and copilot is the alphabetical fallback",
			ghtriage.TaskClass, kind, ghtriage.ExecutorKind)
	}
	if !verdict.usable {
		t.Fatal("verdict.usable = false with a usable model binary")
	}
	if notices := triageNotices(stderr.String()); notices != 0 {
		t.Fatalf("stderr = %q, want no triage notice when the model is usable", stderr.String())
	}
}

// The degraded half of the same composition, and the state the human decided a
// box with no usable model binary degrades into: no triage executor, no
// github.issue.read advertised, and no routing entry, so a triage Task stays
// ELIGIBLE instead of being handed to copilot. A routing entry without the
// executor is the fail-open this pins against in the other direction.
func TestOpenWorkerBoxLeavesADegradedBoxWithoutTriage(t *testing.T) {
	ctx := context.Background()
	config := initializedCollective(t)
	var stderr bytes.Buffer
	box, verdict, err := openWorkerBox(ctx, workerRuntimeConfigFor(t, ctx, config), triageModelConfigFor(t,
		"--model-binary="+filepath.Join(t.TempDir(), "absent")), &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()

	if executor, registered := box.Executors[ghtriage.ExecutorKind]; registered {
		t.Fatalf("registry[%q] = %#v, want no triage executor: one built with a nil classifier fails every attempt",
			ghtriage.ExecutorKind, executor)
	}
	capacity, err := workerCapacity(box)
	if err != nil {
		t.Fatal(err)
	}
	if available, advertised := capacity.Capabilities[ghtriage.RequiredCapability]; advertised {
		t.Fatalf("capacity advertises %q = %+v with no %s executor registered",
			ghtriage.RequiredCapability, available, ghtriage.ExecutorKind)
	}
	if kind, routed := verdict.routing[ghtriage.TaskClass]; routed {
		t.Fatalf("routing[%q] = %q, want no entry: routing to a kind that is not registered hands the Task to the alphabetical baseline",
			ghtriage.TaskClass, kind)
	}
	if verdict.usable {
		t.Fatal("verdict.usable = true with a model binary that is not on PATH")
	}
	notice := stderr.String()
	if notices := triageNotices(notice); notices != 1 {
		t.Fatalf("triage notice count = %d in %q, want exactly one", notices, notice)
	}
	for _, fragment := range []string{ghtriage.ExecutorKind, "is not registered", "absent"} {
		if !strings.Contains(notice, fragment) {
			t.Fatalf("stderr = %q, want a notice naming %q and why it is unusable", notice, fragment)
		}
	}
}

// A model binary that is not on PATH is a missing optional dependency: the box
// still starts. runWorker polls until its context ends, so the deadline below
// is what stops it: the box has to open, advertise and idle first, and the
// fresh database holds no Task for it to claim. The deadline is generous
// against a slow machine; it bounds the test, not the assertion.
func TestRunWorkerStartsWithoutAUsableModelBinary(t *testing.T) {
	initializedCollective(t)
	readStderr := captureStderr(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := runWorker(ctx, []string{
		"--workspace-root=" + t.TempDir(),
		"--model-binary=" + filepath.Join(t.TempDir(), "absent"),
	})
	if err != nil {
		t.Fatalf("runWorker with no usable model binary: %v", err)
	}
	// The degradation clause is one notice, not none and not one per attempt:
	// the command resolves the model once and reports it once.
	if notices := triageNotices(readStderr()); notices != 1 {
		t.Fatalf("triage notice count = %d, want exactly one notice from a degraded run-worker", notices)
	}
}

func TestRunWorkerStartsWithAUsableModelBinary(t *testing.T) {
	initializedCollective(t)
	readStderr := captureStderr(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := runWorker(ctx, []string{
		"--workspace-root=" + t.TempDir(),
		"--model-binary=" + usableModelBinary(t),
		"--model-timeout=5s",
	}); err != nil {
		t.Fatalf("runWorker with a usable model binary: %v", err)
	}
	if notices := triageNotices(readStderr()); notices != 0 {
		t.Fatalf("triage notice count = %d, want none: a usable model registers its executor", notices)
	}
}

// A model flag that cannot be honoured is a misconfiguration, not a missing
// dependency, so it still stops the box from starting. The degradation covers a
// model that cannot be invoked and must not swallow this. The deadline bounds
// the test the way it bounds the two above: a regression that stops parsing
// --model-timeout would otherwise run this to the package timeout.
func TestRunWorkerStillRejectsAMalformedModelTimeout(t *testing.T) {
	initializedCollective(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
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
