package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/adoreview"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/resources"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
	"github.com/SofiaFlux/summa42/internal/scheduler"
	"github.com/SofiaFlux/summa42/internal/testutil"
	"github.com/SofiaFlux/summa42/internal/verification"
)

func clearPublishEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"SUMMA42_ADO_MCP_COMMAND",
		"SUMMA42_ADO_ORGANIZATION",
		"SUMMA42_PUBLISH_MODE",
		"SUMMA42_PUBLISH_APPROVERS",
		"SUMMA42_PUBLISH_RISK_COMMENT",
		"SUMMA42_PUBLISH_RISK_APPROVE",
	} {
		t.Setenv(key, "")
	}
}

func TestPublishEnvAbsentRegistersNothing(t *testing.T) {
	clearPublishEnv(t)
	settings, providers, err := buildPublishFromEnv(nil)
	if err != nil || settings.mode != "" || len(settings.approvers) != 0 || settings.riskComment != "" || settings.riskApprove != "" || providers != nil {
		t.Fatalf("got %#v, %v, %v; want nil registration", settings, providers, err)
	}
}

func TestPublishEnvRejectsBadMode(t *testing.T) {
	clearPublishEnv(t)
	t.Setenv("SUMMA42_PUBLISH_MODE", "everything")
	if _, _, err := buildPublishFromEnv(nil); err == nil {
		t.Fatal("accepted bad mode")
	}
}

func TestPublishEnvRequiresApproveRiskInAllMode(t *testing.T) {
	clearPublishEnv(t)
	t.Setenv("SUMMA42_PUBLISH_MODE", "all")
	t.Setenv("SUMMA42_PUBLISH_RISK_APPROVE", "")
	if _, _, err := buildPublishFromEnv(nil); err == nil {
		t.Fatal("accepted all mode without approve risk")
	}
}

func TestPublishEnvBuildsProvidersAndSettings(t *testing.T) {
	clearPublishEnv(t)
	t.Setenv("SUMMA42_ADO_MCP_COMMAND", "/bin/true")
	t.Setenv("SUMMA42_ADO_ORGANIZATION", "Contoso")
	t.Setenv("SUMMA42_PUBLISH_MODE", "comments")
	t.Setenv("SUMMA42_PUBLISH_APPROVERS", "owner-1, owner-2")
	t.Setenv("SUMMA42_PUBLISH_RISK_COMMENT", "MEDIUM")
	t.Setenv("SUMMA42_PUBLISH_RISK_APPROVE", "HIGH")
	provider, err := buildADOProviderFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	settings, providers, err := buildPublishFromEnv(provider)
	if err != nil {
		t.Fatal(err)
	}
	if settings.mode != adoreview.PublishComments || !reflect.DeepEqual(settings.approvers, []domain.ID{"owner-1", "owner-2"}) || settings.riskComment != "MEDIUM" || settings.riskApprove != "HIGH" {
		t.Fatalf("settings = %#v", settings)
	}
	if len(providers) != 2 || providers[0].Name() != "ado-pr-comment" || providers[1].Name() != "ado-pr-vote" {
		t.Fatalf("providers = %v", providers)
	}
}

type publishCapacityExecutor struct{}

func (publishCapacityExecutor) ExecutionContract() executors.Contract {
	return executors.Contract{Capabilities: []string{"ado.pr.comment", "ado.pr.approve"}, Enforcement: domain.EnforcementEnforced}
}

func (publishCapacityExecutor) Start(context.Context, executors.AttemptEnvelope) (executors.ExecutionResult, error) {
	return executors.ExecutionResult{Evidence: []executors.Evidence{{Kind: executors.EvidenceAgentMessage, Content: "published"}}}, nil
}

func TestWorkerCapacityAdvertisesPublishEffectsAndLeasesWork2(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC))
	purposes := purpose.New(store, clk)
	executionSvc := execution.New(store, clk, purposes)
	resourcesSvc := resources.New(store, clk)
	evidenceStore, err := evidence.New(store, t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	verificationSvc := verification.New(store, clk, executionSvc)
	schedulerSvc := scheduler.New(store, clk, purposes, executionSvc, resourcesSvc, time.Minute)

	envelopeID := domain.NewID("envelope")
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, ?, ?)`,
		envelopeID, 100, clk.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	task, err := executionSvc.CreateTask(ctx, execution.TaskRequest{
		Purpose:              domain.PurposeRef{Kind: domain.PurposeOwnerDirective, ID: "owner-publish"},
		Objective:            "publish review",
		PayloadJSON:          json.RawMessage(`{}`),
		AcceptanceCriteria:   []string{"published"},
		RequiredCapabilities: []string{"ado.pr.comment"},
		RequiredEnforcement:  domain.EnforcementEnforced,
		AuthorityCeiling:     []string{"ado.pr.comment"},
		ResourceEnvelopeID:   envelopeID,
	})
	if err != nil {
		t.Fatal(err)
	}
	capacity, err := workerCapacity(&summa42runtime.Box{Executors: map[string]executors.Executor{
		"ado-publish": publishCapacityExecutor{},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range []string{"ado-publish", "ado.pr.comment", "ado.pr.approve"} {
		available := capacity.Capabilities[capability]
		if !available.Accessible || available.Enforcement != domain.EnforcementEnforced {
			t.Fatalf("capacity[%q] = %+v, want enforced accessible capability", capability, available)
		}
	}
	worker, err := scheduler.NewWorker(schedulerSvc, executionSvc, evidenceStore, verificationSvc,
		map[string]executors.Executor{"ado-publish": publishCapacityExecutor{}}, clk, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	step, err := worker.StepOnce(ctx, capacity)
	if err != nil {
		t.Fatal(err)
	}
	if step.Outcome != scheduler.StepCompleted || step.TaskID != task.ID || step.AttemptID == "" {
		t.Fatalf("step = %+v, want completed leased task %q", step, task.ID)
	}
}
