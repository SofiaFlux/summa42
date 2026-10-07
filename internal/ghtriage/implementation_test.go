package ghtriage_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/repoworkspace"
)

type implementationModel struct {
	calls            atomic.Int64
	onCall           func(ghtriage.ImplementationInput)
	entered, release chan struct{}
}

func (m *implementationModel) Implement(ctx context.Context, in ghtriage.ImplementationInput) (ghtriage.ImplementationOutput, error) {
	m.calls.Add(1)
	if m.onCall != nil {
		m.onCall(in)
	}
	if m.entered != nil {
		close(m.entered)
		select {
		case <-m.release:
		case <-ctx.Done():
			return ghtriage.ImplementationOutput{}, ctx.Err()
		}
	}
	return ghtriage.ImplementationOutput{Summary: "Apply bounded change", Edits: []repoworkspace.Edit{{Path: "code.go", Content: "package test\n// implemented\n"}}}, nil
}
func readyImplementation(t *testing.T, commands ...[]string) (*driverFixture, ghtriage.GroundingConfig, domain.ID) {
	t.Helper()
	f, plannerModel := readyPlannerFixture(t)
	cfg := groundingFixture(t)
	cfg.ValidationCommands = [][]string{{"git", "diff", "--exit-code"}}
	if len(commands) > 0 {
		cfg.ValidationCommands = commands
	}
	c := f.casesByRev[fixtureRevision]
	_, err := f.store.DB().ExecContext(f.ctx, `UPDATE workflow_cases SET grant_json=? WHERE case_id=?`, `{"Capabilities":["github.issue.read","workspace.repo.write","workspace.test"],"Actions":["github.issue.read","workspace.repo.write","workspace.test"]}`, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	planner := ghtriage.NewPlanner(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, plannerModel)
	if err := planner.SetGrounding(cfg); err != nil {
		t.Fatal(err)
	}
	out, err := planner.Tick(f.ctx, f.missionID)
	if err != nil || out.Planned != 1 {
		t.Fatalf("plan %+v %v", out, err)
	}
	review := &planReviewModel{output: ghtriage.PlanReviewOutput{Verdict: "ACCEPT", Reason: "scope fits"}}
	reviewed, err := ghtriage.NewPlanReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, review).Tick(f.ctx, f.missionID)
	if err != nil || reviewed.Reviewed != 1 || len(reviewed.Failures) != 0 {
		t.Fatalf("review %+v %v", reviewed, err)
	}
	return f, cfg, c.ID
}
func TestImplementationProducesBoundCandidateWithoutAcceptingTask(t *testing.T) {
	f, cfg, id := readyImplementation(t)
	m := &implementationModel{}
	impl := ghtriage.NewImplementer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, m)
	got, err := impl.Prepare(f.ctx, id, cfg.Source.LocalPath, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got.Record == nil || !got.Record.AllPassed || got.Record.BaseSHA != cfg.Source.Commit || got.Record.PlanEvidenceID == "" || got.Record.ReviewEvidenceID == "" || got.EvidenceID == "" {
		t.Fatalf("%+v", got)
	}
	task, err := f.execSvc.Task(f.ctx, got.Record.TaskID)
	if err != nil || task.State != domain.TaskAwaitingVerification {
		t.Fatalf("task %+v %v", task, err)
	}
	c, _ := f.cases.Get(f.ctx, id)
	if c.NextWork.Kind != ghtriage.ImplementationWorkKind {
		t.Fatal("advanced without code review")
	}
	replay, err := impl.Prepare(f.ctx, id, cfg.Source.LocalPath, t.TempDir())
	if err != nil || !replay.Reused || replay.EvidenceID != got.EvidenceID || m.calls.Load() != 1 {
		t.Fatalf("replay %+v %v calls=%d", replay, err, m.calls.Load())
	}
	data, _ := os.ReadFile(filepath.Join(cfg.Source.LocalPath, "code.go"))
	if string(data) != "package test\n" {
		t.Fatal("source changed")
	}
}
func TestImplementationChecksGrantAndSourceBeforeModel(t *testing.T) {
	for _, mode := range []string{"grant", "source", "review-binding"} {
		t.Run(mode, func(t *testing.T) {
			f, cfg, id := readyImplementation(t)
			m := &implementationModel{}
			source := cfg.Source.LocalPath
			switch mode {
			case "grant":
				f.store.DB().ExecContext(f.ctx, `UPDATE workflow_cases SET grant_json='{"Capabilities":["github.issue.read"],"Actions":["github.issue.read"]}' WHERE case_id=?`, id)
			case "source":
				source = t.TempDir()
			case "review-binding":
				f.store.DB().ExecContext(f.ctx, `UPDATE acceptance_records SET verifier_type='wrong' WHERE verifier_type='github.issue.plan.review.binding.v1'`)
			}
			_, err := ghtriage.NewImplementer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, m).Prepare(f.ctx, id, source, t.TempDir())
			if err == nil || m.calls.Load() != 0 {
				t.Fatalf("mode=%s err=%v calls=%d", mode, err, m.calls.Load())
			}
		})
	}
}
func TestImplementationDoesNotLetModelMutateOwnerContract(t *testing.T) {
	f, cfg, id := readyImplementation(t)
	m := &implementationModel{onCall: func(in ghtriage.ImplementationInput) {
		in.Plan.Source.AllowedPaths[0] = "escape"
		in.Plan.Source.ValidationCommands[0][0] = "missing-model-command"
	}}
	got, err := ghtriage.NewImplementer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, m).Prepare(f.ctx, id, cfg.Source.LocalPath, t.TempDir())
	if err != nil || !got.Record.AllPassed || got.Record.Commands[0].Argv[0] != "git" {
		t.Fatalf("%+v %v", got, err)
	}
}
func TestImplementationClaimsBeforeConcurrentPaidCall(t *testing.T) {
	f, cfg, id := readyImplementation(t)
	m := &implementationModel{entered: make(chan struct{}), release: make(chan struct{})}
	impl := ghtriage.NewImplementer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, m)
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	first := make(chan error, 1)
	go func() { _, err := impl.Prepare(ctx, id, cfg.Source.LocalPath, t.TempDir()); first <- err }()
	select {
	case <-m.entered:
	case <-ctx.Done():
		t.Fatal("model not entered")
	}
	second, err := impl.Prepare(ctx, id, cfg.Source.LocalPath, t.TempDir())
	close(m.release)
	if err != nil || !second.Busy || m.calls.Load() != 1 {
		t.Fatalf("%+v %v calls=%d", second, err, m.calls.Load())
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}
func TestImplementationExpiredLeaseCannotComplete(t *testing.T) {
	f, cfg, id := readyImplementation(t)
	m := &implementationModel{onCall: func(ghtriage.ImplementationInput) {
		f.clock.Advance(ghtriage.ImplementationLeaseDuration + time.Second)
	}}
	got, err := ghtriage.NewImplementer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, m).Prepare(f.ctx, id, cfg.Source.LocalPath, t.TempDir())
	if err == nil || got.Record != nil {
		t.Fatalf("expired result %+v %v", got, err)
	}
}
func TestImplementationOutputStrictAndBounded(t *testing.T) {
	for _, raw := range []string{`{"summary":"x","edits":[],"command":"extra"}`, `{"summary":"x","edits":[]}`, `{"summary":"x","edits":[{"path":"code.go","content":"x","delete":false}]} {}`} {
		if _, err := ghtriage.ParseImplementationOutput([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	good := ghtriage.ImplementationOutput{Summary: "change", Edits: []repoworkspace.Edit{{Path: "code.go", Content: "x"}}}
	raw, _ := json.Marshal(good)
	if _, err := ghtriage.ParseImplementationOutput(raw); err != nil {
		t.Fatal(err)
	}
}

func TestImplementationNewRevisionDuringModelCannotComplete(t *testing.T) {
	f, cfg, id := readyImplementation(t)
	m := &implementationModel{onCall: func(ghtriage.ImplementationInput) { f.registerRevision(t, "2026-09-28T10:00:01Z") }}
	got, err := ghtriage.NewImplementer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, m).Prepare(f.ctx, id, cfg.Source.LocalPath, t.TempDir())
	if err == nil || got.Record != nil {
		t.Fatalf("stale result %+v %v", got, err)
	}
}

func TestImplementationResumesStagedCompletionWithoutPaidReplay(t *testing.T) {
	script := filepath.Join(t.TempDir(), "validate.sh")
	counter := filepath.Join(t.TempDir(), "counter")
	f, cfg, id := readyImplementation(t, []string{script})
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf run >> '"+counter+"'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	m := &implementationModel{}
	_, err := f.store.DB().ExecContext(f.ctx, `CREATE TRIGGER interrupt_implementation_completion BEFORE INSERT ON attempt_completion_records BEGIN SELECT RAISE(ABORT,'injected completion interruption'); END`)
	if err != nil {
		t.Fatal(err)
	}
	impl := ghtriage.NewImplementer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, m)
	if _, err := impl.Prepare(f.ctx, id, cfg.Source.LocalPath, t.TempDir()); err == nil {
		t.Fatal("interruption not observed")
	}
	if _, err := f.store.DB().ExecContext(f.ctx, `DROP TRIGGER interrupt_implementation_completion`); err != nil {
		t.Fatal(err)
	}
	got, err := impl.Prepare(f.ctx, id, cfg.Source.LocalPath, t.TempDir())
	if err != nil || !got.Reused || m.calls.Load() != 1 {
		t.Fatalf("completion replay %+v %v calls=%d", got, err, m.calls.Load())
	}
	data, err := os.ReadFile(counter)
	if err != nil || string(data) != "run" {
		t.Fatalf("validation replayed: %q %v", data, err)
	}
}

func TestImplementationDoesNotRerunExpiredStagedWork(t *testing.T) {
	f, cfg, id := readyImplementation(t)
	m := &implementationModel{}
	f.store.DB().ExecContext(f.ctx, `CREATE TRIGGER interrupt_implementation_completion BEFORE INSERT ON attempt_completion_records BEGIN SELECT RAISE(ABORT,'injected completion interruption'); END`)
	impl := ghtriage.NewImplementer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, m)
	_, _ = impl.Prepare(f.ctx, id, cfg.Source.LocalPath, t.TempDir())
	f.store.DB().ExecContext(f.ctx, `DROP TRIGGER interrupt_implementation_completion`)
	f.clock.Advance(ghtriage.ImplementationLeaseDuration + time.Second)
	_, err := impl.Prepare(f.ctx, id, cfg.Source.LocalPath, t.TempDir())
	if err == nil || m.calls.Load() != 1 {
		t.Fatalf("expired staged run repeated: %v calls=%d", err, m.calls.Load())
	}
}
func TestImplementationValidationCannotModifySource(t *testing.T) {
	script := filepath.Join(t.TempDir(), "validation.sh")
	f, cfg, id := readyImplementation(t, []string{script})
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf changed > '"+filepath.Join(cfg.Source.LocalPath, "code.go")+"'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	got, err := ghtriage.NewImplementer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, &implementationModel{}).Prepare(f.ctx, id, cfg.Source.LocalPath, t.TempDir())
	if err == nil || got.Record != nil {
		t.Fatalf("source mutation certified: %+v %v", got, err)
	}
}
