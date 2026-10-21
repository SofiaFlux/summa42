package ghtriage_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestGroundingReusesIdenticalContextEvidence(t *testing.T) {
	f, model := readyPlannerFixture(t)
	planner := ghtriage.NewPlanner(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model)
	if err := planner.SetGrounding(groundingFixture(t)); err != nil {
		t.Fatal(err)
	}
	model.err = errors.New("model unavailable")
	if _, err := planner.Tick(f.ctx, f.missionID); err != nil {
		t.Fatal(err)
	}
	model.err = nil
	if result, err := planner.Tick(f.ctx, f.missionID); err != nil || result.Planned != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	var n int
	if err := f.store.DB().QueryRowContext(f.ctx, `SELECT count(*) FROM evidence_objects WHERE kind = ?`, ghtriage.KindSourceContext).Scan(&n); err != nil || n != 1 {
		t.Fatalf("source context rows = %d, %v", n, err)
	}
}

type selectingModel struct {
	*plannerModel
	selection ghtriage.FileSelection
	selInput  ghtriage.FileSelectionInput
	selCalls  int
}

func (m *selectingModel) SelectFiles(_ context.Context, input ghtriage.FileSelectionInput) (ghtriage.FileSelection, error) {
	m.selCalls++
	m.selInput = input
	return m.selection, nil
}

// prefixFixture pins a repository with files inside and outside the owner prefix internal/.
func prefixFixture(t *testing.T) ghtriage.GroundingConfig {
	t.Helper()
	cfg := groundingFixture(t)
	dir := cfg.Source.LocalPath
	if err := os.MkdirAll(filepath.Join(dir, "internal", "purpose"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "purpose", "research.go"), []byte("package purpose\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		raw, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git: %s %v", raw, err)
		}
		return strings.TrimSpace(string(raw))
	}
	git("add", "internal/purpose/research.go")
	git("commit", "-qm", "purpose")
	cfg.Source.Commit = git("rev-parse", "HEAD")
	cfg.Source.Paths = nil
	cfg.AllowedPaths = nil
	cfg.AllowedPrefixes = []string{"internal/"}
	return cfg
}

func prefixPlanner(t *testing.T, selection ghtriage.FileSelection) (*driverFixture, *selectingModel, *ghtriage.Planner) {
	t.Helper()
	f, inner := readyPlannerFixture(t)
	model := &selectingModel{plannerModel: inner, selection: selection}
	planner := ghtriage.NewPlanner(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model)
	if err := planner.SetGrounding(prefixFixture(t)); err != nil {
		t.Fatal(err)
	}
	return f, model, planner
}

// rejectedSelection asserts the tick failed before the plan call and recorded a counted validation failure.
func rejectedSelection(t *testing.T, selection ghtriage.FileSelection) {
	t.Helper()
	f, model, planner := prefixPlanner(t, selection)
	result, err := planner.Tick(f.ctx, f.missionID)
	if err != nil || result.Planned != 0 || len(result.Failures) != 1 || model.selCalls != 1 || model.calls != 0 {
		t.Fatalf("selection %+v accepted: result=%+v sel=%d plan=%d err=%v", selection, result, model.selCalls, model.calls, err)
	}
	c := f.casesByRev[fixtureRevision]
	_, raw, found, err := f.evidenceStore.FindBySubject(f.ctx, ghtriage.KindPlanFailure, string(c.CurrentWorkID)+"#1")
	if err != nil || !found || !strings.Contains(string(raw), `"class":"validation"`) {
		t.Fatalf("failure record %s found=%v err=%v", raw, found, err)
	}
}

func TestFileSelectionRejectsPathOutsidePrefix(t *testing.T) {
	research := []string{"internal/purpose/research.go"}
	for _, selection := range []ghtriage.FileSelection{
		{ContextFiles: research, AllowedPaths: []string{"code.go"}},
		{ContextFiles: []string{"code.go"}, AllowedPaths: research},
		{ContextFiles: []string{"internal/purpose/missing.go"}, AllowedPaths: research},
		{ContextFiles: research, AllowedPaths: []string{"internal/purpose"}},
		{ContextFiles: research, AllowedPaths: []string{"internal/purpose/research.go/x.go"}},
		{ContextFiles: research, AllowedPaths: nil},
	} {
		rejectedSelection(t, selection)
	}
}

func TestFileSelectionCapsCounts(t *testing.T) {
	many := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("internal/purpose/new%02d.go", i)
		}
		return out
	}
	research := []string{"internal/purpose/research.go"}
	rejectedSelection(t, ghtriage.FileSelection{ContextFiles: append(many(32), research...), AllowedPaths: research})
	rejectedSelection(t, ghtriage.FileSelection{ContextFiles: research, AllowedPaths: many(65)})
	raw, _ := json.Marshal(map[string][]string{"context_files": research, "allowed_paths": many(65)})
	if _, err := ghtriage.ParseFileSelection(raw); err == nil {
		t.Fatal("parser accepted 65 allowed paths")
	}
	if _, err := ghtriage.ParseFileSelection([]byte(`{"allowed_paths":["a.go"]}`)); err == nil {
		t.Fatal("parser accepted a selection without context_files")
	}
}

func TestPrefixModeRecordsSelectedPathsInPlanSource(t *testing.T) {
	research := []string{"internal/purpose/research.go"}
	f, model, planner := prefixPlanner(t, ghtriage.FileSelection{ContextFiles: research, AllowedPaths: research})
	result, err := planner.Tick(f.ctx, f.missionID)
	if err != nil || result.Planned != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	if in := model.selInput; in.Schema != ghtriage.FileSelectionSchema || in.Snapshot.Title != "Crash on save" ||
		strings.Join(in.TrackedFiles, ",") != "internal/purpose/research.go" {
		t.Fatalf("selection input %+v", in)
	}
	p, _ := storedPlan(t, f)
	if p.Source == nil || strings.Join(p.Source.AllowedPaths, ",") != "internal/purpose/research.go" {
		t.Fatalf("plan source %+v", p.Source)
	}
	files := model.input.Grounding.Context.Files
	if len(files) != 1 || files[0].Path != "internal/purpose/research.go" {
		t.Fatalf("context files %+v", files)
	}
}
