package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/capacity"
	"github.com/SofiaFlux/summa42/internal/casereport"
	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/purpose"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

// seedReportCase leaves one case that the Owner blocked by hand, citing
// evidence written through the real evidence store, and a model call ledger
// with calls for that case and for another one.
func seedReportCase(t *testing.T, cfg localconfig.Config) workflowcase.Case {
	t.Helper()
	active, other := seedCases(t, cfg.DatabasePath)
	ctx := context.Background()
	store, err := state.Open(ctx, cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	evidenceStore, err := evidence.New(store, cfg.EvidencePath, clock.System{})
	if err != nil {
		t.Fatal(err)
	}
	object, err := evidenceStore.Put(ctx, bytes.NewReader([]byte(`{"reason":"hold for the release"}`)),
		evidence.Metadata{MediaType: "application/json", Kind: "summa42.owner.case_block"})
	if err != nil {
		t.Fatal(err)
	}
	cases := workflowcase.New(store, clock.System{}, purpose.New(store, clock.System{}))
	if _, err := cases.Block(ctx, active.ID, "hold for the release", object.ID); err != nil {
		t.Fatal(err)
	}
	tokens := func(n int) *int { return &n }
	started := time.Now().UTC()
	for _, call := range []capacity.Call{
		{Stage: "plan", Model: "model-a", CaseID: string(active.ID), Duration: 1500 * time.Millisecond, Outcome: capacity.OutcomeTimeout},
		{Stage: "plan", Model: "model-a", CaseID: string(active.ID), Duration: 2500 * time.Millisecond, Outcome: capacity.OutcomeOK, InputTokens: tokens(1000), OutputTokens: tokens(200)},
		{Stage: "plan-review", Model: "model-b", CaseID: string(active.ID), Duration: 4 * time.Second, Outcome: capacity.OutcomeOK, InputTokens: tokens(3000), OutputTokens: tokens(50)},
		{Stage: "plan", Model: "model-a", CaseID: string(other.ID), Duration: time.Second, Outcome: capacity.OutcomeOK, InputTokens: tokens(7), OutputTokens: tokens(7)},
	} {
		call.Pool, call.Harness, call.StartedAt = "pool", "harness", started
		if err := store.RecordCall(ctx, call); err != nil {
			t.Fatal(err)
		}
	}
	return active
}

func TestCaseReportJSON(t *testing.T) {
	home, cfg := emergencyCollective(t)
	c := seedReportCase(t, cfg)

	output, err := runCLIOutput(t, "--json", "case", "report", string(c.ID), "--home", home)
	if err != nil {
		t.Fatalf("case report: %v\n%s", err, output)
	}
	var report casereport.Report
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, output)
	}
	if report.CaseID != string(c.ID) || report.Object != "SofiaFlux/summa42#101" || report.State != "BLOCKED" {
		t.Errorf("case header: %+v", report)
	}
	if len(report.Stages) != 1 {
		t.Fatalf("%d stages, want the one assessment", len(report.Stages))
	}
	stage := report.Stages[0]
	if stage.WorkID != string(c.CurrentWorkID) || stage.Verdict != "UNKNOWN" || stage.Outcome != "BLOCKED" || stage.Reason != "hold for the release" {
		t.Errorf("stage: %+v", stage)
	}
	if len(stage.Evidence) != 1 || stage.Evidence[0].Kind != "summa42.owner.case_block" || len(stage.Evidence[0].ContentHash) == 0 {
		t.Errorf("stage evidence: %+v", stage.Evidence)
	}
	if report.Interventions.Total != 1 || report.Interventions.OwnerEvidence["summa42.owner.case_block"] != 1 {
		t.Errorf("interventions: %+v", report.Interventions)
	}
	total := report.ModelTotal
	if total.Calls != 3 || total.OK != 2 || total.Failed != 1 || total.Retries != 1 || total.InputTokens != 4000 || total.OutputTokens != 250 || total.CallsWithoutTokens != 1 || total.DurationSeconds != 8 {
		t.Errorf("model total (the other case's call must not be counted): %+v", total)
	}
	if strings.Join(total.Models, ",") != "model-a,model-b" {
		t.Errorf("models: %v", total.Models)
	}
	if len(report.ModelCalls) != 2 || report.ModelCalls[0].Stage != "plan" || report.ModelCalls[0].Calls != 2 || report.ModelCalls[0].Retries != 1 || report.ModelCalls[1].Stage != "plan-review" || report.ModelCalls[1].InputTokens != 3000 {
		t.Errorf("model calls per stage: %+v", report.ModelCalls)
	}
	if report.WallSeconds < 0 || report.UpdatedAt.Before(report.CreatedAt) {
		t.Errorf("times: created %v updated %v", report.CreatedAt, report.UpdatedAt)
	}
}

func TestCaseReportTextSaysWhatItDoesNotCount(t *testing.T) {
	home, cfg := emergencyCollective(t)
	c := seedReportCase(t, cfg)

	output, err := runCLIOutput(t, "case", "report", string(c.ID), "--home", home)
	if err != nil {
		t.Fatalf("case report: %v\n%s", err, output)
	}
	for _, want := range []string{
		string(c.ID), "SofiaFlux/summa42#101", "BLOCKED", "hold for the release", "summa42.owner.case_block",
		"plan-review", "model-b", "4000", "250",
		"approvals: 0", "Cancels, restarts and anything the Owner did outside the Box are not counted",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output does not contain %q:\n%s", want, output)
		}
	}
}

func TestCaseReportUnknownCase(t *testing.T) {
	home, _ := emergencyCollective(t)
	if output, err := runCLIOutput(t, "case", "report", "case-does-not-exist", "--home", home); err == nil {
		t.Fatalf("a report for an unknown case succeeded:\n%s", output)
	}
}
