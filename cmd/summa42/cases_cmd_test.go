package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/purpose"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/version"
	"github.com/SofiaFlux/summa42/internal/workflow"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

// seedCases writes two cases through the real case service into the store of
// an initialized Collective: one still active, one blocked by an assessment.
func seedCases(t *testing.T, databasePath string) (active, blocked workflowcase.Case) {
	t.Helper()
	ctx := context.Background()
	store, err := state.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	purposes := purpose.New(store, clock.System{})
	missionID, err := purposes.CreateMission(ctx, "Maintain this repository safely")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO evidence_objects(evidence_id,content_hash,media_type,kind,size_bytes,created_at) VALUES ('evidence','hash','application/json','TEST',1,?)`,
		domain.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	cases := workflowcase.New(store, clock.System{}, purposes)
	ensure := func(object string) workflowcase.Case {
		c, err := cases.Ensure(ctx, workflowcase.Observation{
			MissionID: missionID, Source: "github.issue", ObjectID: object, RevisionID: "r1", EvidenceID: "evidence",
			FirstWork: workflow.WorkProposal{Kind: "github.issue.triage", RequiredCapabilities: []string{"read"}, AuthorityCeiling: []string{"read"}},
			Grant:     workflow.Grant{Capabilities: []string{"read"}}, MaxSteps: 6, RemainingBudget: 5,
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	active = ensure("SofiaFlux/summa42#101")
	blocked = ensure("SofiaFlux/summa42#102")
	if _, err := cases.Assess(ctx, workflowcase.AssessmentRequest{
		CaseID: blocked.ID, WorkID: blocked.CurrentWorkID,
		Assessment:      workflow.Assessment{Verdict: workflow.Unknown, Reason: "the issue does not say which flag", EvidenceIDs: []string{"evidence"}},
		RemainingBudget: blocked.RemainingBudget, ProgressSignature: "blocked",
	}); err != nil {
		t.Fatal(err)
	}
	return active, blocked
}

func TestCasesListsEveryCaseWithItsLastAssessment(t *testing.T) {
	home, cfg := emergencyCollective(t)
	active, blocked := seedCases(t, cfg.DatabasePath)

	output, err := runCLIOutput(t, "cases", "--home", home)
	if err != nil {
		t.Fatalf("cases: %v\n%s", err, output)
	}
	for _, want := range []string{
		string(active.ID), string(blocked.ID), "github.issue", "SofiaFlux/summa42#101", "SofiaFlux/summa42#102",
		"ACTIVE", "BLOCKED", "github.issue.triage", "the issue does not say which flag",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output does not contain %q:\n%s", want, output)
		}
	}
}

func TestCasesFiltersByState(t *testing.T) {
	home, cfg := emergencyCollective(t)
	active, blocked := seedCases(t, cfg.DatabasePath)

	output, err := runCLIOutput(t, "cases", "--home", home, "--state", "blocked")
	if err != nil {
		t.Fatalf("cases: %v\n%s", err, output)
	}
	if !strings.Contains(output, string(blocked.ID)) || strings.Contains(output, string(active.ID)) {
		t.Fatalf("--state blocked did not select only the blocked case:\n%s", output)
	}
	if output, err := runCLIOutput(t, "cases", "--home", home, "--state", "DONE"); err == nil {
		t.Fatalf("an unknown state was accepted:\n%s", output)
	}
}

func TestCasesJSON(t *testing.T) {
	home, cfg := emergencyCollective(t)
	active, blocked := seedCases(t, cfg.DatabasePath)

	output, err := runCLIOutput(t, "--json", "cases", "--home", home)
	if err != nil {
		t.Fatalf("cases: %v\n%s", err, output)
	}
	var rows []struct {
		CaseID               string `json:"case_id"`
		Source               string `json:"source"`
		Object               string `json:"object"`
		State                string `json:"state"`
		CurrentWorkID        string `json:"current_work_id"`
		CurrentWorkKind      string `json:"current_work_kind"`
		CompletedSteps       int    `json:"completed_steps"`
		MaxSteps             int    `json:"max_steps"`
		LastAssessmentReason string `json:"last_assessment_reason"`
		UpdatedAt            string `json:"updated_at"`
	}
	if err := json.Unmarshal([]byte(output), &rows); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, output)
	}
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2:\n%s", len(rows), output)
	}
	byID := map[string]int{rows[0].CaseID: 0, rows[1].CaseID: 1}
	a, b := rows[byID[string(active.ID)]], rows[byID[string(blocked.ID)]]
	if a.State != "ACTIVE" || a.CurrentWorkID != string(active.CurrentWorkID) || a.CurrentWorkKind != "github.issue.triage" || a.MaxSteps != 6 || a.LastAssessmentReason != "" {
		t.Errorf("active row: %+v", a)
	}
	if b.State != "BLOCKED" || b.CurrentWorkID != "" || b.LastAssessmentReason != "the issue does not say which flag" {
		t.Errorf("blocked row: %+v", b)
	}
	for _, row := range rows {
		if _, err := time.Parse(time.RFC3339Nano, row.UpdatedAt); err != nil {
			t.Errorf("updated_at %q: %v", row.UpdatedAt, err)
		}
	}
}

func TestCasesOnAnEmptyCollective(t *testing.T) {
	home, _ := emergencyCollective(t)
	output, err := runCLIOutput(t, "cases", "--home", home)
	if err != nil || !strings.Contains(output, "No cases") {
		t.Fatalf("cases on an empty Collective: %v\n%s", err, output)
	}
}

func TestVersionFlagPrintsTheVersion(t *testing.T) {
	output, err := runCLIOutput(t, "--version")
	if err != nil || !strings.Contains(output, version.Version) {
		t.Fatalf("--version: %v\n%s", err, output)
	}
}
