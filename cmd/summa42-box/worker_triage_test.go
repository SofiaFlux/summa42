package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/ghtriage/climodel"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/resources"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
	"github.com/SofiaFlux/summa42/internal/scheduler"
	"github.com/SofiaFlux/summa42/internal/testutil"
	"github.com/SofiaFlux/summa42/internal/verification"
)

// triageWorkload is the slice of a worker a triage Task travels through: the
// services, the routing preference runWorker installs, and the Task intake
// materializes. The Task requires exactly what the advertisement covers, so a
// claim proves the advertisement and not something else.
type triageWorkload struct {
	execution *execution.Service
	evidence  *evidence.Store
	verify    *verification.Service
	scheduler *scheduler.Service
	clock     *testutil.Clock
	task      domain.Task
}

func newTriageWorkload(t *testing.T) triageWorkload {
	t.Helper()
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	purposes := purpose.New(store, clk)
	execSvc := execution.New(store, clk, purposes)
	resourcesSvc := resources.New(store, clk)
	evidenceStore, err := evidence.New(store, t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	verificationSvc := verification.New(store, clk, execSvc)
	schedulerSvc := scheduler.New(store, clk, purposes, execSvc, resourcesSvc, time.Minute,
		scheduler.TaskClassRouting{ghtriage.TaskClass: ghtriage.ExecutorKind})

	envelopeID := domain.NewID("envelope")
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, ?, ?)`,
		envelopeID, 100, clk.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	task, err := execSvc.CreateTask(ctx, execution.TaskRequest{
		Purpose:              domain.PurposeRef{Kind: domain.PurposeOwnerDirective, ID: "owner-intake"},
		TaskClass:            ghtriage.TaskClass,
		Objective:            "Triage GitHub issue o/r#7",
		PayloadJSON:          json.RawMessage(`{"repo":"o/r","issue":7,"revision":"2026-09-24T10:00:00Z","issueSnapshot":"ev-snapshot"}`),
		AcceptanceCriteria:   []string{"triage decision recorded for 2026-09-24T10:00:00Z"},
		RequiredCapabilities: []string{ghtriage.RequiredCapability},
		RequiredEnforcement:  domain.EnforcementEnforced,
		AuthorityCeiling:     []string{ghtriage.RequiredCapability},
		ResourceEnvelopeID:   envelopeID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return triageWorkload{
		execution: execSvc, evidence: evidenceStore, verify: verificationSvc,
		scheduler: schedulerSvc, clock: clk, task: task,
	}
}

type triageCapacityExecutor struct{}

func (triageCapacityExecutor) Start(context.Context, executors.AttemptEnvelope) (executors.ExecutionResult, error) {
	return executors.ExecutionResult{
		Evidence: []executors.Evidence{{Kind: executors.EvidenceAgentMessage, Content: "triaged"}},
	}, nil
}

// The capability this task exists to advertise, and the routing that stops the
// alphabetical baseline from claiming the work: copilot sorts before
// github-issue-triage, so an attempt landing on copilot is the defect.
func TestWorkerCapacityAdvertisesTriageReadAndRoutesToTheTriageExecutor(t *testing.T) {
	ctx := context.Background()
	work := newTriageWorkload(t)
	registry := map[string]executors.Executor{
		"copilot":             triageCapacityExecutor{},
		ghtriage.ExecutorKind: triageCapacityExecutor{},
	}
	capacity, err := workerCapacity(&summa42runtime.Box{Executors: registry})
	if err != nil {
		t.Fatal(err)
	}
	available, advertised := capacity.Capabilities[ghtriage.RequiredCapability]
	if !advertised || !available.Accessible || available.Enforcement != domain.EnforcementEnforced {
		t.Fatalf("capacity[%q] = %+v (advertised %t), want enforced accessible capability",
			ghtriage.RequiredCapability, available, advertised)
	}
	worker, err := scheduler.NewWorker(work.scheduler, work.execution, work.evidence, work.verify, registry, work.clock, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	step, err := worker.StepOnce(ctx, capacity)
	if err != nil {
		t.Fatal(err)
	}
	if step.Outcome != scheduler.StepCompleted || step.TaskID != work.task.ID || step.AttemptID == "" {
		t.Fatalf("step = %+v, want completed leased task %q", step, work.task.ID)
	}
	attempt, err := work.execution.Attempt(ctx, step.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutorKind != ghtriage.ExecutorKind {
		t.Fatalf("attempt executor kind = %q, want %q: the alphabetical baseline picks copilot and routing exists to prevent exactly that",
			attempt.ExecutorKind, ghtriage.ExecutorKind)
	}
}

// A box that degraded at startup holds no triage executor, so it advertises no
// github.issue.read and the triage Task stays ELIGIBLE and unclaimed instead of
// falling to whatever sorts first.
func TestWorkerCapacityWithoutTriageExecutorLeavesTriageWorkUnclaimed(t *testing.T) {
	ctx := context.Background()
	work := newTriageWorkload(t)
	registry := map[string]executors.Executor{"copilot": triageCapacityExecutor{}}
	capacity, err := workerCapacity(&summa42runtime.Box{Executors: registry})
	if err != nil {
		t.Fatal(err)
	}
	if available, advertised := capacity.Capabilities[ghtriage.RequiredCapability]; advertised {
		t.Fatalf("capacity advertises %q = %+v with no %s executor registered",
			ghtriage.RequiredCapability, available, ghtriage.ExecutorKind)
	}
	worker, err := scheduler.NewWorker(work.scheduler, work.execution, work.evidence, work.verify, registry, work.clock, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	step, err := worker.StepOnce(ctx, capacity)
	if err != nil {
		t.Fatal(err)
	}
	if step.Outcome != scheduler.StepIdle {
		t.Fatalf("step = %+v, want IDLE: without the triage executor there is nothing to run the work", step)
	}
	stored, err := work.execution.Task(ctx, work.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != domain.TaskEligible {
		t.Fatalf("state = %q, want ELIGIBLE (eligible but unclaimed)", stored.State)
	}
}

// usableModelBinary is the smallest thing climodel.New accepts: the binary
// resolves on PATH. Nothing is invoked at registration time.
func usableModelBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "model")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// A missing model binary degrades to "not registered" the way a missing copilot
// path does, so a box that only runs ADO work still starts.
func TestTriageModelAdapterDegradesWhenTheModelBinaryIsMissing(t *testing.T) {
	_, _, modelConfig, err := parseWorkerFlags([]string{
		"--model-binary=" + filepath.Join(t.TempDir(), "absent"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	adapter, registered := triageModelAdapter(modelConfig, &stderr)
	if registered {
		t.Fatal("registered the triage executor with a model binary that is not on PATH")
	}
	if adapter != nil {
		t.Fatalf("adapter = %#v, want nil", adapter)
	}
	notice := stderr.String()
	for _, fragment := range []string{ghtriage.ExecutorKind, "is not registered", "absent"} {
		if !strings.Contains(notice, fragment) {
			t.Fatalf("stderr = %q, want a notice naming %q and why it is unusable", notice, fragment)
		}
	}
}

func TestTriageModelAdapterRegistersAUsableModelBinary(t *testing.T) {
	_, _, modelConfig, err := parseWorkerFlags([]string{
		"--model-binary=" + usableModelBinary(t), "--model-timeout=5s",
	})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	adapter, registered := triageModelAdapter(modelConfig, &stderr)
	if !registered || adapter == nil {
		t.Fatalf("registered = %t, adapter = %#v, want the triage executor registered", registered, adapter)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want no notice when the model is usable", stderr.String())
	}
}

// triageModelConfigFor reads the model flags the way runWorker does, so the
// installation tests cover the flag path and not only the config literal.
func triageModelConfigFor(t *testing.T, args ...string) climodel.Config {
	t.Helper()
	_, _, modelConfig, err := parseWorkerFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	return modelConfig
}

// The model binary is exec'd with the prompt on stdin and nothing else, so a
// provider CLI that needs a subcommand cannot be used at all without arguments -
// and Config.Args was reachable only from a hand-built Config, which no
// production path builds. The default is --model-binary codex, so a deployment
// needing arguments had no way to say so and every attempt ran the full timeout
// twice and then blocked the case. The flag is repeatable because a subcommand
// and its own options have to arrive in order, and it is on both the worker's
// and the reviewer's flag set because both invoke the model.
func TestModelArgsReachTheModelConfigurationInOrder(t *testing.T) {
	args := []string{"exec", "--model", "gpt-5"}
	flags := make([]string, 0, len(args))
	for _, arg := range args {
		flags = append(flags, "--model-arg="+arg)
	}
	got := triageModelConfigFor(t, append(
		[]string{"--model-binary=" + usableModelBinary(t), "--model-timeout=5s"}, flags...)...)
	if len(got.Args) != len(args) {
		t.Fatalf("args = %q, want %q", got.Args, args)
	}
	for i, want := range args {
		if got.Args[i] != want {
			t.Fatalf("args = %q, want %q in the order they were written", got.Args, args)
		}
	}
	// And the reviewer's flag set, which is the other one that calls a model: a
	// flag registered on run-worker alone would leave the reviewer invoking a
	// binary configured one way and the worker another.
	mission, config, err := parseGHTriageReviewFlags(append(
		[]string{"--mission", "mission-1", "--model-binary", usableModelBinary(t),
			"--model-timeout", "5s"}, flags...))
	if err != nil {
		t.Fatal(err)
	}
	if mission != "mission-1" || len(config.Args) != len(args) {
		t.Fatalf("review model config = %+v, want the same arguments in order", config)
	}
	for i, want := range args {
		if config.Args[i] != want {
			t.Fatalf("review args = %q, want %q in the order they were written", config.Args, args)
		}
	}
	// Omitting the flag is not an error; for a binary that is not codex it
	// means no arguments, which is the documented contract of the binary: it
	// reads the prompt on stdin. (The codex default is ["exec"]; that binary
	// here is a stub whose base name is not codex, so no default applies.)
	if got := triageModelConfigFor(t, "--model-binary="+usableModelBinary(t), "--model-timeout=5s"); len(got.Args) != 0 {
		t.Fatalf("args = %q, want none when the flag is omitted", got.Args)
	}
}

// The default invocation is codex + exec: bare codex with a piped stdin exits
// immediately demanding a terminal, while codex exec with no prompt argument
// reads the instructions from stdin, so the adapter's stdin contract reaches a
// working invocation only with the exec default in place. The reviewer shares
// the default because both commands build their config through
// triageModelConfig.
func TestTriageModelConfigDefaultsCodexToExec(t *testing.T) {
	got := triageModelConfigFor(t)
	if got.Binary != "codex" {
		t.Fatalf("binary = %q, want codex", got.Binary)
	}
	if !slices.Equal(got.Args, []string{"exec"}) {
		t.Fatalf("args = %q, want [exec] when no --model-arg was passed", got.Args)
	}
	if _, config, err := parseGHTriageReviewFlags([]string{"--mission", "mission-1"}); err != nil {
		t.Fatal(err)
	} else if !slices.Equal(config.Args, []string{"exec"}) {
		t.Fatalf("review args = %q, want [exec] when no --model-arg was passed", config.Args)
	}
}

// An explicitly passed argument list is used exactly as passed, with nothing
// prepended: a user pointing --model-binary at codex who needs different
// arguments must be able to say so.
func TestTriageModelConfigKeepsExplicitArgsWithoutPrependingExec(t *testing.T) {
	got := triageModelConfigFor(t,
		"--model-binary=codex", "--model-arg=foo", "--model-arg=bar")
	if !slices.Equal(got.Args, []string{"foo", "bar"}) {
		t.Fatalf("args = %q, want exactly [foo bar] with no exec prepended", got.Args)
	}
}

// A binary that is not codex must not receive a stray exec: the default is
// codex-shaped, and anyone pointing --model-binary elsewhere sets --model-arg
// explicitly or gets no arguments.
func TestTriageModelConfigInjectsNoExecForABinaryThatIsNotCodex(t *testing.T) {
	got := triageModelConfigFor(t, "--model-binary=/usr/bin/other")
	if len(got.Args) != 0 {
		t.Fatalf("args = %q, want none for a binary that is not codex", got.Args)
	}
}

// A model binary that is not on PATH must leave the registry holding no triage
// executor, because that registry is the only capability source workerCapacity
// reads: no key, no github.issue.read, no lease. An executor registered with a
// nil classifier would advertise the capability and fail every attempt.
func TestInstallTriageExecutorLeavesADegradedBoxUnregistered(t *testing.T) {
	ctx := context.Background()
	work := newTriageWorkload(t)
	registry := map[string]executors.Executor{"copilot": triageCapacityExecutor{}}
	var stderr bytes.Buffer
	adapter, usable := triageModelAdapter(triageModelConfigFor(t,
		"--model-binary="+filepath.Join(t.TempDir(), "absent")), &stderr)
	installed := installTriageExecutor(registry, work.evidence, adapter)
	if installed || usable {
		t.Fatalf("installed = %t, usable = %t, want no triage executor: the model binary is not on PATH", installed, usable)
	}
	if executor, registered := registry[ghtriage.ExecutorKind]; registered {
		t.Fatalf("registry[%q] = %#v, want no triage executor: one built with a nil classifier fails every attempt",
			ghtriage.ExecutorKind, executor)
	}
	capacity, err := workerCapacity(&summa42runtime.Box{Executors: registry})
	if err != nil {
		t.Fatal(err)
	}
	if available, advertised := capacity.Capabilities[ghtriage.RequiredCapability]; advertised {
		t.Fatalf("capacity advertises %q = %+v with no %s executor registered",
			ghtriage.RequiredCapability, available, ghtriage.ExecutorKind)
	}
	worker, err := scheduler.NewWorker(work.scheduler, work.execution, work.evidence, work.verify, registry, work.clock, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	step, err := worker.StepOnce(ctx, capacity)
	if err != nil {
		t.Fatal(err)
	}
	if step.Outcome != scheduler.StepIdle {
		t.Fatalf("step = %+v, want IDLE: a degraded box must not lease triage work", step)
	}
	notice := stderr.String()
	for _, fragment := range []string{ghtriage.ExecutorKind, "is not registered", "absent"} {
		if !strings.Contains(notice, fragment) {
			t.Fatalf("stderr = %q, want a notice naming %q and why it is unusable", notice, fragment)
		}
	}
}

// The usable half of the same seam: the key lands in the registry and the
// capacity derived from that registry advertises the read capability the triage
// Task requires.
func TestInstallTriageExecutorRegistersAUsableModelAndAdvertisesTheReadCapability(t *testing.T) {
	work := newTriageWorkload(t)
	registry := map[string]executors.Executor{"copilot": triageCapacityExecutor{}}
	var stderr bytes.Buffer
	adapter, usable := triageModelAdapter(triageModelConfigFor(t,
		"--model-binary="+usableModelBinary(t), "--model-timeout=5s"), &stderr)
	installed := installTriageExecutor(registry, work.evidence, adapter)
	if !installed || !usable {
		t.Fatalf("installed = %t, usable = %t, want the %s executor registered", installed, usable, ghtriage.ExecutorKind)
	}
	if _, registered := registry[ghtriage.ExecutorKind]; !registered {
		t.Fatalf("registry = %v, want the %s key", registry, ghtriage.ExecutorKind)
	}
	capacity, err := workerCapacity(&summa42runtime.Box{Executors: registry})
	if err != nil {
		t.Fatal(err)
	}
	available, advertised := capacity.Capabilities[ghtriage.RequiredCapability]
	if !advertised || !available.Accessible || available.Enforcement != domain.EnforcementEnforced {
		t.Fatalf("capacity[%q] = %+v (advertised %t), want enforced accessible capability",
			ghtriage.RequiredCapability, available, advertised)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want no notice when the model is usable", stderr.String())
	}
}

// triageModelConfig is documented as reusable by run-gh-triage-driver and
// run-gh-triage-review, which bring their own flag sets. Reading a flag that
// was never registered has to say so instead of dereferencing nil.
func TestTriageModelConfigReportsAFlagSetThatNeverRegisteredTheModelFlags(t *testing.T) {
	bare := flag.NewFlagSet("run-gh-triage-driver", flag.ContinueOnError)
	if _, err := triageModelConfig(bare); err == nil {
		t.Fatal("triageModelConfig accepted a flag set with no model flags")
	} else if !strings.Contains(err.Error(), "--model-binary") {
		t.Fatalf("error = %q, want it to name the missing --model-binary", err)
	}
	partial := flag.NewFlagSet("run-gh-triage-review", flag.ContinueOnError)
	partial.String("model-binary", "codex", "model binary classifying a triage issue")
	if _, err := triageModelConfig(partial); err == nil {
		t.Fatal("triageModelConfig accepted a flag set with no --model-timeout")
	} else if !strings.Contains(err.Error(), "--model-timeout") {
		t.Fatalf("error = %q, want it to name --model-timeout", err)
	}
	// Every flag has to be there, in the order they are checked, and one that
	// does not collect a list is refused rather than read as "no arguments":
	// silently dropping the arguments is the failure this flag exists to remove.
	for _, tc := range []struct {
		name  string
		setup func(*flag.FlagSet)
		want  string
	}{
		{"no --model-arg", func(f *flag.FlagSet) {
			f.String("model-binary", "codex", "b")
			f.String("model-timeout", "60s", "t")
		}, "--model-arg"},
		{"a --model-arg that keeps only one value", func(f *flag.FlagSet) {
			f.String("model-binary", "codex", "b")
			f.String("model-timeout", "60s", "t")
			f.String("model-arg", "", "a")
		}, "--model-arg does not collect"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set := flag.NewFlagSet("partial", flag.ContinueOnError)
			tc.setup(set)
			if _, err := triageModelConfig(set); err == nil {
				t.Fatal("triageModelConfig accepted the flag set")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}
