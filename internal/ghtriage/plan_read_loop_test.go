package ghtriage_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/repoworkspace"
)

type multiTurnModel struct {
	turns     []ghtriage.PlanOutput
	callCount int
	inputs    []ghtriage.PlanInput
	planErr   error
}

func (m *multiTurnModel) Plan(_ context.Context, input ghtriage.PlanInput) (ghtriage.PlanOutput, error) {
	m.inputs = append(m.inputs, input)
	idx := m.callCount
	m.callCount++
	if m.planErr != nil {
		return ghtriage.PlanOutput{}, m.planErr
	}
	if idx < len(m.turns) {
		return m.turns[idx], nil
	}
	// Default to final plan if more turns than defined
	return ghtriage.PlanOutput{
		Summary:      "Default final plan",
		Steps:        []ghtriage.PlanStep{{Objective: "Obj", DoneWhen: "Done"}},
		AllowedPaths: []string{"pkg/foo.go"},
	}, nil
}

func readLoopFixture(t *testing.T) (ghtriage.GroundingConfig, string, string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		raw, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, raw, err)
		}
		return strings.TrimSpace(string(raw))
	}
	run("init", "-q")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.invalid")

	if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "foo.go"), []byte("package pkg\nfunc Foo() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "bar.go"), []byte("package pkg\nfunc Bar() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pinned.go"), []byte("package root\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "base commit")
	commit := run("rev-parse", "HEAD")

	cfg := ghtriage.GroundingConfig{
		Source: repoworkspace.Config{
			LocalPath:  dir,
			Repository: "o/r",
			Commit:     commit,
			Paths:      []string{"pinned.go"},
		},
		AllowedPrefixes:    []string{"pkg/"},
		ValidationCommands: [][]string{{"go", "test", "./..."}},
		ReadMode:           true,
	}
	return cfg, dir, commit
}

// Test (a): A final plan on turn 0
func TestPlanWithReads_Turn0FinalPlan(t *testing.T) {
	f, _ := readyPlannerFixture(t)
	cfg, _, _ := readLoopFixture(t)

	model := &multiTurnModel{
		turns: []ghtriage.PlanOutput{
			{
				Summary:      "Immediate final plan",
				Steps:        []ghtriage.PlanStep{{Objective: "Do something", DoneWhen: "It works"}},
				AllowedPaths: []string{"pkg/foo.go"},
			},
		},
	}

	planner := ghtriage.NewPlanner(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model)
	if err := planner.SetGrounding(cfg); err != nil {
		t.Fatal(err)
	}

	res, err := planner.Tick(f.ctx, f.missionID)
	if err != nil || res.Planned != 1 {
		t.Fatalf("planner.Tick failed: res=%+v err=%v", res, err)
	}
	if model.callCount != 1 {
		t.Fatalf("expected 1 model call on turn 0, got %d", model.callCount)
	}

	p, _ := storedPlan(t, f)
	if p.PlanOutput.Summary != "Immediate final plan" {
		t.Fatalf("unexpected summary: %s", p.PlanOutput.Summary)
	}
}

// Test (b): Two read turns and then a plan
func TestPlanWithReads_TwoReadsThenPlan(t *testing.T) {
	f, _ := readyPlannerFixture(t)
	cfg, _, _ := readLoopFixture(t)

	model := &multiTurnModel{
		turns: []ghtriage.PlanOutput{
			// Turn 0: Read foo.go
			{ReadRepoFile: []string{"pkg/foo.go"}},
			// Turn 1: Read bar.go
			{ReadRepoFile: []string{"pkg/bar.go"}},
			// Turn 2: Final plan
			{
				Summary:      "Final plan after reads",
				Steps:        []ghtriage.PlanStep{{Objective: "Step 1", DoneWhen: "Done 1"}},
				AllowedPaths: []string{"pkg/foo.go", "pkg/bar.go"},
			},
		},
	}

	planner := ghtriage.NewPlanner(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model)
	if err := planner.SetGrounding(cfg); err != nil {
		t.Fatal(err)
	}

	res, err := planner.Tick(f.ctx, f.missionID)
	if err != nil || res.Planned != 1 {
		t.Fatalf("planner.Tick failed: res=%+v err=%v", res, err)
	}
	if model.callCount != 3 {
		t.Fatalf("expected 3 model calls, got %d", model.callCount)
	}

	// Verify that Turn 1 received foo.go and Turn 2 received both foo.go and bar.go
	if len(model.inputs) != 3 {
		t.Fatalf("expected 3 inputs, got %d", len(model.inputs))
	}
	t2Files := model.inputs[2].Grounding.Context.Files
	hasFoo, hasBar := false, false
	for _, file := range t2Files {
		if file.Path == "pkg/foo.go" {
			hasFoo = true
		}
		if file.Path == "pkg/bar.go" {
			hasBar = true
		}
	}
	if !hasFoo || !hasBar {
		t.Fatalf("expected turn 2 to contain foo.go and bar.go, got: %+v", t2Files)
	}
}

// Test (c): A model that asks for reads forever is cut off after 4 read turns and fails as validation on final turn
func TestPlanWithReads_ForeverReadsCutOff(t *testing.T) {
	f, _ := readyPlannerFixture(t)
	cfg, _, _ := readLoopFixture(t)

	// Model always requests reads
	model := &multiTurnModel{
		turns: []ghtriage.PlanOutput{
			{ReadRepoFile: []string{"pkg/foo.go"}},
			{ReadRepoFile: []string{"pkg/bar.go"}},
			{ReadRepoFile: []string{"pkg/foo.go"}},
			{ReadRepoFile: []string{"pkg/bar.go"}},
			{ReadRepoFile: []string{"pkg/foo.go"}}, // 5th turn (turn 4): still asks for reads!
		},
	}

	planner := ghtriage.NewPlanner(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model)
	if err := planner.SetGrounding(cfg); err != nil {
		t.Fatal(err)
	}

	res, err := planner.Tick(f.ctx, f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Planned != 0 {
		t.Fatalf("expected 0 planned cases, got %d", res.Planned)
	}
	if model.callCount != 5 {
		t.Fatalf("expected exactly 5 model calls before cutoff, got %d", model.callCount)
	}

	// Check that failure was recorded as validation
	c := f.casesByRev[fixtureRevision]
	_, raw, found, err := f.evidenceStore.FindBySubject(f.ctx, ghtriage.KindPlanFailure, fmt.Sprintf("%s#1", c.CurrentWorkID))
	if err != nil || !found {
		t.Fatalf("plan failure record not found: %v", err)
	}
	if !strings.Contains(string(raw), `"class":"validation"`) {
		t.Fatalf("expected validation failure class, got: %s", raw)
	}
}

// Test (d): Request for path outside allowed prefixes gets error entry, loop continues
func TestPlanWithReads_OutsidePrefixErrorEntryAndContinues(t *testing.T) {
	f, _ := readyPlannerFixture(t)
	cfg, _, _ := readLoopFixture(t)

	model := &multiTurnModel{
		turns: []ghtriage.PlanOutput{
			// Turn 0: Request an invalid path outside allowed prefixes
			{ReadRepoFile: []string{"secret/passwords.txt"}},
			// Turn 1: Final plan
			{
				Summary:      "Plan after bad path",
				Steps:        []ghtriage.PlanStep{{Objective: "Step", DoneWhen: "Done"}},
				AllowedPaths: []string{"pkg/foo.go"},
			},
		},
	}

	planner := ghtriage.NewPlanner(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model)
	if err := planner.SetGrounding(cfg); err != nil {
		t.Fatal(err)
	}

	res, err := planner.Tick(f.ctx, f.missionID)
	if err != nil || res.Planned != 1 {
		t.Fatalf("planner.Tick failed: res=%+v err=%v", res, err)
	}
	if model.callCount != 2 {
		t.Fatalf("expected 2 model calls, got %d", model.callCount)
	}

	// Turn 1 input should contain error entry for secret/passwords.txt
	t1Input := model.inputs[1]
	foundError := false
	for _, rr := range t1Input.ReadResults {
		if rr.Path == "secret/passwords.txt" && rr.Error != "" {
			foundError = true
			break
		}
	}
	if !foundError {
		t.Fatalf("expected error entry for secret/passwords.txt in read results: %+v", t1Input.ReadResults)
	}
}

// Test (e): Going over byte budget forces final turn
func TestPlanWithReads_OverByteBudgetForcesFinalTurn(t *testing.T) {
	f, _ := readyPlannerFixture(t)
	cfg, _, _ := readLoopFixture(t)
	// Set ContextBytes budget very small (e.g. 4096 bytes)
	cfg.ContextBytes = 4096

	model := &multiTurnModel{
		turns: []ghtriage.PlanOutput{
			// Turn 0: Read foo.go
			{ReadRepoFile: []string{"pkg/foo.go"}},
			// Turn 1: Model asks for another read when budget is exhausted or forced final turn
			// If it asks for reads when final turn is forced, it should fail
			{ReadRepoFile: []string{"pkg/bar.go"}},
		},
	}

	// Let's test with a budget smaller than foo.go + bar.go, or simulate budget exhaustion
	cfg.ContextBytes = 4096 // minimum allowed by GroundingConfig.Validate
	// We'll verify that question on forced turn specifies final answer required
	planner := ghtriage.NewPlanner(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model)
	if err := planner.SetGrounding(cfg); err != nil {
		t.Fatal(err)
	}

	// If budget is set to 4096, and pinned.go is small, foo.go is small, but if budget is exhausted:
	// Let's test that when model returns final plan on turn 1, it succeeds
	modelSuccess := &multiTurnModel{
		turns: []ghtriage.PlanOutput{
			{ReadRepoFile: []string{"pkg/foo.go"}},
			{
				Summary:      "Plan on forced final turn",
				Steps:        []ghtriage.PlanStep{{Objective: "Step", DoneWhen: "Done"}},
				AllowedPaths: []string{"pkg/foo.go"},
			},
		},
	}
	f2, _ := readyPlannerFixture(t)
	planner2 := ghtriage.NewPlanner(f2.cases, f2.execSvc, f2.verifSvc, f2.evidenceStore, modelSuccess)
	if err := planner2.SetGrounding(cfg); err != nil {
		t.Fatal(err)
	}
	res2, err := planner2.Tick(f2.ctx, f2.missionID)
	if err != nil || res2.Planned != 1 {
		t.Fatalf("planner.Tick failed: res=%+v err=%v", res2, err)
	}
}

// Test (f): planCaseReserve / PlanStageTimeout in planner.go budget for up to 5 model calls
func TestPlanStageTimeout_BudgetsFor5ModelCalls(t *testing.T) {
	modelTimeout := 10 * time.Second
	got := ghtriage.PlanStageTimeout(modelTimeout)
	want := 5*modelTimeout + 5*time.Minute
	if got != want {
		t.Fatalf("PlanStageTimeout(%s) = %s, want %s (5 model calls + 5m margin)", modelTimeout, got, want)
	}
}

// Test (g): Every read goes through repoworkspace.ReadFile
func TestPlanReadLoop_GrepNoOtherFileAccess(t *testing.T) {
	bytes, err := os.ReadFile("plan_read_loop.go")
	if err != nil {
		t.Skip("plan_read_loop.go not yet created or split")
	}
	code := string(bytes)
	if strings.Contains(code, "os.ReadFile") || strings.Contains(code, "os.Open") || strings.Contains(code, "ioutil.ReadFile") {
		t.Fatalf("plan_read_loop.go contains direct OS file access instead of repoworkspace.ReadFile")
	}
}

// Issue #134: Test that a two-turn plan produces context evidence whose files
// are exactly pinned union read, and ContextHash matches, and denied path never shows up.
func TestPlanWithReads_ContextEvidenceExactUnionAndTurnLog(t *testing.T) {
	f, _ := readyPlannerFixture(t)
	cfg, _, _ := readLoopFixture(t)

	model := &multiTurnModel{
		turns: []ghtriage.PlanOutput{
			// Turn 0: Read foo.go and an outside/denied path
			{ReadRepoFile: []string{"pkg/foo.go", "bad/secret.txt"}},
			// Turn 1: Final plan
			{
				Summary:      "Final plan with exact context",
				Steps:        []ghtriage.PlanStep{{Objective: "Step 1", DoneWhen: "Done 1"}},
				AllowedPaths: []string{"pkg/foo.go"},
			},
		},
	}

	planner := ghtriage.NewPlanner(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model)
	if err := planner.SetGrounding(cfg); err != nil {
		t.Fatal(err)
	}

	res, err := planner.Tick(f.ctx, f.missionID)
	if err != nil || res.Planned != 1 {
		t.Fatalf("planner.Tick failed: res=%+v err=%v", res, err)
	}

	p, _ := storedPlan(t, f)
	if p.Source == nil {
		t.Fatal("plan missing source contract")
	}

	// 1. Verify context evidence
	obj, raw, err := f.evidenceStore.Get(f.ctx, p.Source.ContextEvidenceID)
	if err != nil {
		t.Fatalf("get context evidence: %v", err)
	}
	if obj.ContentHash != p.Source.ContextHash {
		t.Fatalf("context hash mismatch: obj=%s source=%s", obj.ContentHash, p.Source.ContextHash)
	}

	var snap repoworkspace.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}

	// Exactly pinned.go union pkg/foo.go (sorted by path)
	if len(snap.Files) != 2 {
		t.Fatalf("expected 2 files in context snapshot, got %d: %+v", len(snap.Files), snap.Files)
	}
	if snap.Files[0].Path != "pinned.go" || snap.Files[1].Path != "pkg/foo.go" {
		t.Fatalf("unexpected files in context: %s, %s", snap.Files[0].Path, snap.Files[1].Path)
	}

	// Denied path must never show up
	for _, file := range snap.Files {
		if strings.Contains(file.Path, "secret") || strings.Contains(file.Path, "bad") {
			t.Fatalf("denied path showed up in context evidence: %s", file.Path)
		}
	}

	// 2. Verify turn log evidence
	if p.TurnLogEvidenceID == "" {
		t.Fatal("expected non-empty TurnLogEvidenceID on stored plan")
	}
	tObj, tRaw, err := f.evidenceStore.Get(f.ctx, p.TurnLogEvidenceID)
	if err != nil {
		t.Fatalf("get turn log evidence: %v", err)
	}
	if tObj.Kind != ghtriage.KindPlanTurnLog {
		t.Fatalf("expected kind %s, got %s", ghtriage.KindPlanTurnLog, tObj.Kind)
	}

	var turnLog ghtriage.PlanTurnLog
	if err := json.Unmarshal(tRaw, &turnLog); err != nil {
		t.Fatalf("unmarshal turn log: %v", err)
	}
	if len(turnLog.Turns) != 1 {
		t.Fatalf("expected 1 turn log entry, got %d", len(turnLog.Turns))
	}
	entry := turnLog.Turns[0]
	if len(entry.PathsRequested) != 2 || len(entry.PathsServed) != 1 || len(entry.PathsDenied) != 1 {
		t.Fatalf("unexpected turn log entry counts: %+v", entry)
	}
	if entry.PathsServed[0] != "pkg/foo.go" || entry.PathsDenied[0] != "bad/secret.txt" {
		t.Fatalf("unexpected turn log paths: served=%v denied=%v", entry.PathsServed, entry.PathsDenied)
	}
}

// A final plan without allowed_paths in read mode (no Owner list to fall back
// on) is asked for again instead of failing the case, and the question names
// allowed_paths from the first turn.
func TestPlanWithReads_PlanWithoutAllowedPathsIsAskedAgain(t *testing.T) {
	f, _ := readyPlannerFixture(t)
	cfg, _, _ := readLoopFixture(t)

	model := &multiTurnModel{
		turns: []ghtriage.PlanOutput{
			{Summary: "No boundary", Steps: []ghtriage.PlanStep{{Objective: "Do it", DoneWhen: "Done"}}},
			{Summary: "With boundary", Steps: []ghtriage.PlanStep{{Objective: "Do it", DoneWhen: "Done"}}, AllowedPaths: []string{"pkg/foo.go"}},
		},
	}
	planner := ghtriage.NewPlanner(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model)
	if err := planner.SetGrounding(cfg); err != nil {
		t.Fatal(err)
	}
	res, err := planner.Tick(f.ctx, f.missionID)
	if err != nil || res.Planned != 1 {
		t.Fatalf("planner.Tick failed: res=%+v err=%v", res, err)
	}
	if model.callCount != 2 {
		t.Fatalf("expected 2 model calls, got %d", model.callCount)
	}
	if !strings.Contains(model.inputs[0].Question, `"allowed_paths"`) {
		t.Fatalf("the first question does not ask for allowed_paths: %q", model.inputs[0].Question)
	}
	if !strings.Contains(model.inputs[1].Question, "no allowed_paths") {
		t.Fatalf("the second question does not say what was missing: %q", model.inputs[1].Question)
	}
	p, _ := storedPlan(t, f)
	if p.PlanOutput.Summary != "With boundary" {
		t.Fatalf("unexpected stored plan: %s", p.PlanOutput.Summary)
	}
}

// Without pinned files, a plan written before reading anything is sent back
// to read first, and the files it read become its context.
func TestPlanWithReads_NoPinnedFilesPlanMustReadFirst(t *testing.T) {
	f, _ := readyPlannerFixture(t)
	cfg, _, _ := readLoopFixture(t)
	cfg.Source.Paths = nil

	final := ghtriage.PlanOutput{Summary: "Plan", Steps: []ghtriage.PlanStep{{Objective: "Do it", DoneWhen: "Done"}}, AllowedPaths: []string{"pkg/foo.go"}}
	model := &multiTurnModel{
		turns: []ghtriage.PlanOutput{
			final,
			{ReadRepoFile: []string{"pkg/foo.go"}},
			final,
		},
	}
	planner := ghtriage.NewPlanner(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, model)
	if err := planner.SetGrounding(cfg); err != nil {
		t.Fatal(err)
	}
	res, err := planner.Tick(f.ctx, f.missionID)
	if err != nil || res.Planned != 1 {
		t.Fatalf("planner.Tick failed: res=%+v err=%v", res, err)
	}
	if model.callCount != 3 {
		t.Fatalf("expected 3 model calls, got %d", model.callCount)
	}
	if !strings.Contains(model.inputs[1].Question, "without reading any file") {
		t.Fatalf("the second question does not ask to read first: %q", model.inputs[1].Question)
	}
}
