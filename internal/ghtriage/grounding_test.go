package ghtriage_test

import (
	"encoding/json"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/repoworkspace"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func groundingFixture(t *testing.T) ghtriage.GroundingConfig {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		raw, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git: %s %v", raw, err)
		}
		return strings.TrimSpace(string(raw))
	}
	run("init", "-q")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "code.go"), []byte("package test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "code.go")
	run("commit", "-qm", "base")
	return ghtriage.GroundingConfig{Source: repoworkspace.Config{LocalPath: dir, Repository: "o/r", Commit: run("rev-parse", "HEAD"), Paths: []string{"code.go"}}, AllowedPaths: []string{"code.go", "code_test.go"}, ValidationCommands: [][]string{{"go", "test", "./..."}}}
}
func storedPlan(t *testing.T, f *driverFixture) (ghtriage.Plan, domain.ID) {
	t.Helper()
	c := f.casesByRev[fixtureRevision]
	records, err := f.cases.ListAssessments(f.ctx, c.ID)
	if err != nil || len(records) != 1 {
		t.Fatalf("records %+v %v", records, err)
	}
	var req workflowcase.AssessmentRequest
	if err := json.Unmarshal([]byte(records[0].RequestJSON), &req); err != nil {
		t.Fatal(err)
	}
	id := domain.ID(req.Assessment.EvidenceIDs[0])
	_, raw, err := f.evidenceStore.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var p ghtriage.Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p, id
}
func TestGroundedPlannerStoresPinnedContextAndOwnerContract(t *testing.T) {
	f, model := readyPlannerFixture(t)
	cfg := groundingFixture(t)
	planner := ghtriage.NewPlanner(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model)
	if err := planner.SetGrounding(cfg); err != nil {
		t.Fatal(err)
	}
	model.onCall = func() {
		model.input.Grounding.Contract.AllowedPaths[0] = "model-expanded"
		model.input.Grounding.Contract.ValidationCommands[0][0] = "model-command"
	}
	cfg.AllowedPaths[0] = "escape"
	cfg.ValidationCommands[0][0] = "evil"
	result, err := planner.Tick(f.ctx, f.missionID)
	if err != nil || result.Planned != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	p, _ := storedPlan(t, f)
	if p.Schema != ghtriage.PlanSchemaV2 || p.Source == nil || p.Source.AllowedPaths[0] != "code.go" || p.Source.ValidationCommands[0][0] != "go" {
		t.Fatalf("source contract changed: %+v", p)
	}
	object, raw, err := f.evidenceStore.Get(f.ctx, p.Source.ContextEvidenceID)
	if err != nil || object.ContentHash != p.Source.ContextHash {
		t.Fatalf("context citation %v %v", object, err)
	}
	var snapshot repoworkspace.Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Commit != p.Source.BaseSHA || snapshot.Files[0].Content != "package test\n" || model.input.Grounding == nil {
		t.Fatalf("context missing %+v", snapshot)
	}
}
func TestGroundingWrongRepositoryStopsBeforeModel(t *testing.T) {
	f, model := readyPlannerFixture(t)
	cfg := groundingFixture(t)
	cfg.Source.Repository = "another/repo"
	planner := ghtriage.NewPlanner(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model)
	if err := planner.SetGrounding(cfg); err != nil {
		t.Fatal(err)
	}
	result, err := planner.Tick(f.ctx, f.missionID)
	if err != nil || len(result.Failures) != 1 || model.calls != 0 {
		t.Fatalf("wrong repo planned: %+v calls=%d err=%v", result, model.calls, err)
	}
}
