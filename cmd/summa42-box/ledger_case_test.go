package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SofiaFlux/summa42/internal/casereport"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/ghissue"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/modelrun/modelruntest"
	"github.com/SofiaFlux/summa42/internal/workflow"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

// The worker has one model adapter for every case, so the case a triage call
// belongs to has to come from the Task it runs for. A real worker Box, a real
// case and Task, and a fake model CLI: the ledger row must carry the case ID,
// and the case report must count the call.
func TestWorkerTriageModelCallsAreLedgeredUnderTheirCase(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SUMMA42_MODEL_USER", modelruntest.User)
	_, _, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	config := initializedCollective(t)
	f := newTriageDriverFixture(t, config)
	revision := "2026-09-28T10:00:00Z"
	raw, err := json.Marshal(ghtriage.Snapshot{
		Repo: "o/r", Issue: 42, Title: "Crash on save", Body: "it crashes",
		Author: "maintainer", Labels: []string{"bug"}, Triage: "bug", UpdatedAt: revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.evidence.Put(ctx, strings.NewReader(string(raw)), evidence.Metadata{MediaType: "application/json", Kind: ghissue.SnapshotKind})
	if err != nil {
		t.Fatal(err)
	}
	c, task, err := f.cases.EnsureAndMaterialize(ctx, f.execSvc, workflowcase.Observation{
		MissionID: f.mission, Source: ghtriage.SourceGitHub, ObjectID: "o/r#42", RevisionID: revision, EvidenceID: string(snapshot.ID),
		FirstWork: workflow.WorkProposal{
			Kind: ghtriage.TaskClass, RequiredCapabilities: []string{ghtriage.RequiredCapability},
			AuthorityCeiling: []string{ghtriage.RequiredCapability}, ProposedActions: []string{"github.issue.read"},
		},
		Grant:    workflow.Grant{Capabilities: []string{ghtriage.RequiredCapability}, Actions: []string{"github.issue.read"}},
		MaxSteps: 3, RemainingBudget: 10,
	}, execution.TaskRequest{
		Purpose: domain.PurposeRef{Kind: domain.PurposeMission, ID: f.mission}, TaskClass: ghtriage.TaskClass,
		Objective: "Triage o/r#42", AcceptanceCriteria: []string{"triage decision recorded for " + revision},
		RequiredCapabilities: []string{ghtriage.RequiredCapability}, RequiredEnforcement: domain.EnforcementEnforced,
		AuthorityCeiling: []string{ghtriage.RequiredCapability}, ResourceEnvelopeID: f.envelope,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	answer := `{"is_actionable":true,"needs_repro":false,"scope":"small","suggested_type":"bug","rationale":"a crash"}`
	binary := filepath.Join(t.TempDir(), "cmd")
	script := "#!/bin/sh\ncat > /dev/null\ncat <<'NDJSON'\n" +
		`{"type":"result","subtype":"success","finalText":` + string(mustJSON(t, answer)) + `,"error":"","usage":{"inputTokens":7,"outputTokens":3}}` + "\nNDJSON\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	box, _, err := openWorkerBox(ctx, workerRuntimeConfigFor(t, ctx, config), triageModelConfigFor(t,
		"--model-binary="+binary, "--model-commandcode=reflection/beam:free", "--model-timeout=10s"), &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()

	payload, _ := json.Marshal(map[string]any{"repo": "o/r", "issue": 42, "revision": revision, "issueSnapshot": snapshot.ID})
	if _, err := box.Executors[ghtriage.ExecutorKind].Start(ctx, executors.AttemptEnvelope{TaskID: task.ID, PayloadJSON: payload}); err != nil {
		t.Fatalf("triage attempt: %v (stderr %q)", err, stderr.String())
	}

	var rows int
	if err := box.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM model_calls WHERE case_id = ? AND stage = 'worker'`, string(c.ID)).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("model_calls rows for case %s = %d, want 1", c.ID, rows)
	}
	report, err := casereport.Build(ctx, box.Store, workflowcase.New(box.Store, box.Clock, box.Purpose), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.ModelTotal.Calls != 1 || report.ModelTotal.InputTokens != 7 {
		t.Fatalf("report model total = %+v, want the one call with its tokens", report.ModelTotal)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
