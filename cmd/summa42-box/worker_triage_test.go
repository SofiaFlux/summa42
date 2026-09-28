package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
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
