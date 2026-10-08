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
	"github.com/SofiaFlux/summa42/internal/verification"
)

type codeReviewModel struct {
	calls            atomic.Int64
	verdict          string
	onCall           func(ghtriage.CodeReviewInput)
	entered, release chan struct{}
}

type reviewHookClock struct {
	now  time.Time
	hook func()
}

func (c *reviewHookClock) Now() time.Time {
	if c.hook != nil {
		h := c.hook
		c.hook = nil
		h()
	}
	return c.now
}
func TestCodeReviewReplayCannotSwallowRevokedGrant(t *testing.T) {
	f, cfg, id, _ := implementedFixture(t)
	m := &codeReviewModel{verdict: "ACCEPT"}
	r := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, m)
	first, err := r.Review(f.ctx, id, cfg.Source.LocalPath)
	if err != nil || !first.Accepted {
		t.Fatalf("%+v %v", first, err)
	}
	clk := &reviewHookClock{now: f.clock.Now(), hook: func() {
		if _, err := f.store.DB().ExecContext(f.ctx, `UPDATE workflow_cases SET grant_json='{"Capabilities":[],"Actions":[]}' WHERE case_id=?`, id); err != nil {
			t.Fatal(err)
		}
	}}
	r = ghtriage.NewCodeReviewer(f.cases, f.execSvc, verification.New(f.store, clk, f.execSvc), f.evidenceStore, m)
	got, err := r.Review(f.ctx, id, cfg.Source.LocalPath)
	if err == nil || got.Accepted {
		t.Fatalf("revoked success %+v %v", got, err)
	}
	if m.calls.Load() != 1 {
		t.Fatal("review repeated")
	}
}

func (m *codeReviewModel) ReviewCode(ctx context.Context, in ghtriage.CodeReviewInput) (ghtriage.PlanReviewOutput, error) {
	m.calls.Add(1)
	if m.onCall != nil {
		m.onCall(in)
	}
	if m.entered != nil {
		close(m.entered)
		select {
		case <-m.release:
		case <-ctx.Done():
			return ghtriage.PlanReviewOutput{}, ctx.Err()
		}
	}
	return ghtriage.PlanReviewOutput{Verdict: m.verdict, Reason: "independent assessment"}, nil
}
func implementedFixture(t *testing.T, commands ...[]string) (*driverFixture, ghtriage.GroundingConfig, domain.ID, ghtriage.ImplementationResult) {
	t.Helper()
	f, cfg, id := readyImplementation(t, commands...)
	got, err := ghtriage.NewImplementer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, &implementationModel{}).Prepare(f.ctx, id, cfg.Source.LocalPath, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return f, cfg, id, got
}
func TestCodeReviewAcceptsOnlyPassingReviewedImplementation(t *testing.T) {
	for _, verdict := range []string{"ACCEPT", "REVISE", "BLOCK"} {
		t.Run(verdict, func(t *testing.T) {
			f, cfg, id, impl := implementedFixture(t)
			m := &codeReviewModel{verdict: verdict}
			m.onCall = func(in ghtriage.CodeReviewInput) {
				if in.Implementation.CandidateSHA != impl.Record.CandidateSHA || len(in.Changes) != 1 || in.Changes[0].Before.Content != "package test\n" || in.Changes[0].After.Content != "package test\n// implemented\n" {
					t.Fatalf("bad pinned input %+v", in)
				}
			}
			r := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, m)
			got, err := r.Review(f.ctx, id, cfg.Source.LocalPath)
			if err != nil {
				t.Fatal(err)
			}
			if got.Accepted != (verdict == "ACCEPT") || got.Record == nil || got.EvidenceID == "" {
				t.Fatalf("%+v", got)
			}
			task, _ := f.execSvc.Task(f.ctx, impl.Record.TaskID)
			want := domain.TaskAwaitingVerification
			if verdict == "ACCEPT" {
				want = domain.TaskSucceeded
			}
			if task.State != want {
				t.Fatalf("state=%s want=%s", task.State, want)
			}
			replay, err := r.Review(f.ctx, id, cfg.Source.LocalPath)
			if err != nil || !replay.Reused || replay.EvidenceID != got.EvidenceID || m.calls.Load() != 1 {
				t.Fatalf("replay=%+v %v calls=%d", replay, err, m.calls.Load())
			}
		})
	}
}
func TestCodeReviewFailedTestsCannotBeAccepted(t *testing.T) {
	f, cfg, id, impl := implementedFixture(t, []string{"git", "diff", "--invalid-flag"})
	m := &codeReviewModel{verdict: "ACCEPT"}
	got, err := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, m).Review(f.ctx, id, cfg.Source.LocalPath)
	if err != nil || got.Accepted {
		t.Fatalf("%+v %v", got, err)
	}
	task, _ := f.execSvc.Task(f.ctx, impl.Record.TaskID)
	if task.State != domain.TaskAwaitingVerification {
		t.Fatal(task.State)
	}
}
func TestCodeReviewRejectsDriftAndRevocation(t *testing.T) {
	for _, mode := range []string{"candidate", "source", "revision", "grant", "expired", "model-mutation"} {
		t.Run(mode, func(t *testing.T) {
			f, cfg, id, impl := implementedFixture(t)
			m := &codeReviewModel{verdict: "ACCEPT", onCall: func(in ghtriage.CodeReviewInput) {
				switch mode {
				case "candidate":
					os.WriteFile(filepath.Join(impl.Record.Workspace, "code.go"), []byte("drift"), 0644)
				case "source":
					os.WriteFile(filepath.Join(cfg.Source.LocalPath, "code.go"), []byte("drift"), 0644)
				case "revision":
					f.registerRevision(t, "2026-09-28T10:00:01Z")
				case "grant":
					f.store.DB().ExecContext(f.ctx, `UPDATE workflow_cases SET grant_json='{"Capabilities":[],"Actions":[]}' WHERE case_id=?`, id)
				case "expired":
					f.clock.Advance(ghtriage.CodeReviewLeaseDuration + time.Second)
				case "model-mutation":
					in.Implementation.Commands[0].Passed = false
					in.Plan.Source.ValidationCommands[0][0] = "malicious"
					in.Implementation.ChangedPaths[0] = "escape"
				}
			}}
			got, err := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, m).Review(f.ctx, id, cfg.Source.LocalPath)
			if mode == "model-mutation" {
				if err != nil || !got.Accepted {
					t.Fatalf("provider mutation changed contract %+v %v", got, err)
				}
				return
			}
			if err == nil || got.Accepted {
				t.Fatalf("mode=%s %+v %v", mode, got, err)
			}
			task, _ := f.execSvc.Task(f.ctx, impl.Record.TaskID)
			if task.State != domain.TaskAwaitingVerification {
				t.Fatal("stale acceptance")
			}
		})
	}
}
func TestCodeReviewResumesAfterPersistenceFailure(t *testing.T) {
	for _, table := range []string{"attempt_completion_records", "acceptance_records"} {
		t.Run(table, func(t *testing.T) {
			f, cfg, id, _ := implementedFixture(t)
			m := &codeReviewModel{verdict: "ACCEPT"}
			r := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, m)
			_, err := f.store.DB().ExecContext(f.ctx, `CREATE TRIGGER interrupt_review BEFORE INSERT ON `+table+` BEGIN SELECT RAISE(ABORT,'interrupted'); END`)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Review(f.ctx, id, cfg.Source.LocalPath); err == nil {
				t.Fatal("injected failure missed")
			}
			if _, err := f.store.DB().ExecContext(f.ctx, `DROP TRIGGER interrupt_review`); err != nil {
				t.Fatal(err)
			}
			got, err := r.Review(f.ctx, id, cfg.Source.LocalPath)
			if err != nil || !got.Accepted || m.calls.Load() != 1 {
				t.Fatalf("%+v %v calls=%d", got, err, m.calls.Load())
			}
		})
	}
}
func TestCodeReviewClaimsBeforeConcurrentModelCall(t *testing.T) {
	f, cfg, id, _ := implementedFixture(t)
	m := &codeReviewModel{verdict: "ACCEPT", entered: make(chan struct{}), release: make(chan struct{})}
	r := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, m)
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	first := make(chan error, 1)
	go func() { _, err := r.Review(ctx, id, cfg.Source.LocalPath); first <- err }()
	select {
	case <-m.entered:
	case <-ctx.Done():
		t.Fatal("not entered")
	}
	got, err := r.Review(ctx, id, cfg.Source.LocalPath)
	close(m.release)
	if err != nil || !got.Busy {
		t.Fatalf("%+v %v", got, err)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

func TestCodeReviewExpiredStagedVerdictNeedsOperatorRecovery(t *testing.T) {
	f, cfg, id, impl := implementedFixture(t)
	m := &codeReviewModel{verdict: "ACCEPT"}
	r := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, m)
	_, err := f.store.DB().ExecContext(f.ctx, `CREATE TRIGGER interrupt_review BEFORE INSERT ON attempt_completion_records BEGIN SELECT RAISE(ABORT,'interrupted'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Review(f.ctx, id, cfg.Source.LocalPath); err == nil {
		t.Fatal("interruption missed")
	}
	f.store.DB().ExecContext(f.ctx, `DROP TRIGGER interrupt_review`)
	f.clock.Advance(ghtriage.CodeReviewLeaseDuration + time.Second)
	if _, err := r.Review(f.ctx, id, cfg.Source.LocalPath); err == nil {
		t.Fatal("expired review completed")
	}
	if m.calls.Load() != 1 {
		t.Fatal("paid review repeated")
	}
	task, _ := f.execSvc.Task(f.ctx, impl.Record.TaskID)
	if task.State != domain.TaskAwaitingVerification {
		t.Fatal("expired acceptance")
	}
}
func TestCodeReviewRejectsMissingImplementationBindingBeforeModel(t *testing.T) {
	f, cfg, id, impl := implementedFixture(t)
	m := &codeReviewModel{verdict: "ACCEPT"}
	f.store.DB().ExecContext(f.ctx, `UPDATE attempt_completion_records SET manifest_json='{"version":1,"evidence_ids":[]}' WHERE task_id=?`, impl.Record.TaskID)
	if _, err := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, m).Review(f.ctx, id, cfg.Source.LocalPath); err == nil {
		t.Fatal("missing completion accepted")
	}
	if m.calls.Load() != 0 {
		t.Fatal("model called before canonical validation")
	}
}

func TestCodeReviewCannotAcceptChallengedPlanReview(t *testing.T) {
	f, cfg, id, impl := implementedFixture(t)
	_, raw, err := f.evidenceStore.Get(f.ctx, impl.Record.ReviewEvidenceID)
	if err != nil {
		t.Fatal(err)
	}
	var review ghtriage.PlanReviewRecord
	if err := json.Unmarshal(raw, &review); err != nil {
		t.Fatal(err)
	}
	m := &codeReviewModel{verdict: "ACCEPT", onCall: func(ghtriage.CodeReviewInput) {
		if err := f.execSvc.ChallengeTask(f.ctx, review.TaskID, domain.ChallengeTask, "plan unsafe", nil); err != nil {
			t.Fatal(err)
		}
	}}
	got, err := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, m).Review(f.ctx, id, cfg.Source.LocalPath)
	task, _ := f.execSvc.Task(f.ctx, impl.Record.TaskID)
	if err == nil || got.Accepted || task.State == domain.TaskSucceeded {
		t.Fatalf("accepted challenged chain %+v %v %s", got, err, task.State)
	}
}

func TestCodeReviewCannotAcceptChallengedTriage(t *testing.T) {
	f, cfg, id, impl := implementedFixture(t)
	var triageID domain.ID
	if err := f.store.DB().QueryRowContext(f.ctx, `SELECT task_id FROM tasks WHERE task_class=? AND state=?`, ghtriage.TaskClass, domain.TaskSucceeded).Scan(&triageID); err != nil {
		t.Fatal(err)
	}
	m := &codeReviewModel{verdict: "ACCEPT", onCall: func(ghtriage.CodeReviewInput) {
		if err := f.execSvc.ChallengeTask(f.ctx, triageID, domain.ChallengeTask, "triage unsafe", nil); err != nil {
			t.Fatal(err)
		}
	}}
	got, err := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, m).Review(f.ctx, id, cfg.Source.LocalPath)
	task, _ := f.execSvc.Task(f.ctx, impl.Record.TaskID)
	if err == nil || got.Accepted || task.State == domain.TaskSucceeded {
		t.Fatalf("accepted challenged triage %+v %v %s", got, err, task.State)
	}
}
