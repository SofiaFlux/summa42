package verification

import (
	"context"
	"database/sql"
	"errors"
	"github.com/SofiaFlux/summa42/internal/domain"
	"testing"
)

func TestCompletionGuardRollsBackCompletion(t *testing.T) {
	f := newFixture(t)
	ev := f.put(t, "result")
	denied := errors.New("superseded work")
	_, err := f.verify.CompleteAttemptWithGuard(f.ctx, f.attempt.ID, CompletionManifest{EvidenceIDs: []domain.ID{ev.ID}}, func(context.Context, *sql.Tx) error { return denied })
	if !errors.Is(err, denied) {
		t.Fatal(err)
	}
	task, _ := f.execution.Task(f.ctx, f.task.ID)
	if task.State != domain.TaskExecuting {
		t.Fatal("guard failed after completion")
	}
	if _, err := f.verify.CompletionEvidence(f.ctx, f.task.ID, f.attempt.ID); err == nil {
		t.Fatal("manifest escaped rejected transaction")
	}
}

func TestGuardedAcceptanceReplayRevalidatesAndMatchesRequest(t *testing.T) {
	f := newFixture(t)
	ev := f.put(t, "result")
	if _, err := f.verify.CompleteAttempt(f.ctx, f.attempt.ID, CompletionManifest{EvidenceIDs: []domain.ID{ev.ID}}); err != nil {
		t.Fatal(err)
	}
	req := AcceptanceRequest{VerifierID: "independent", VerifierType: "review", CriteriaMet: true, EvidenceIDs: []domain.ID{ev.ID}}
	guard := func(context.Context, *sql.Tx) error { return nil }
	first, err := f.verify.AcceptTaskWithGuard(f.ctx, f.task.ID, req, guard)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.verify.AcceptTaskWithGuard(f.ctx, f.task.ID, req, guard)
	if err != nil || second.ID != first.ID {
		t.Fatalf("guarded replay %+v %v", second, err)
	}
	denied := errors.New("grant revoked")
	if _, err := f.verify.AcceptTaskWithGuard(f.ctx, f.task.ID, req, func(context.Context, *sql.Tx) error { return denied }); !errors.Is(err, denied) {
		t.Fatal("revocation bypassed", err)
	}
	req.VerifierType = "different"
	if _, err := f.verify.AcceptTaskWithGuard(f.ctx, f.task.ID, req, guard); err == nil {
		t.Fatal("different acceptance replayed")
	}
}

func TestAcceptanceGuardRollsBackAcceptance(t *testing.T) {
	f := newFixture(t)
	ev := f.put(t, "result")
	if _, err := f.verify.CompleteAttempt(f.ctx, f.attempt.ID, CompletionManifest{EvidenceIDs: []domain.ID{ev.ID}}); err != nil {
		t.Fatal(err)
	}
	denied := errors.New("revision changed")
	_, err := f.verify.AcceptTaskWithGuard(f.ctx, f.task.ID, AcceptanceRequest{VerifierID: "reviewer", VerifierType: "test", CriteriaMet: true, EvidenceIDs: []domain.ID{ev.ID}}, func(context.Context, *sql.Tx) error { return denied })
	if !errors.Is(err, denied) {
		t.Fatal(err)
	}
	task, _ := f.execution.Task(f.ctx, f.task.ID)
	if task.State != domain.TaskAwaitingVerification {
		t.Fatal("accepted despite guard")
	}
	if _, found, err := f.verify.FindAcceptance(f.ctx, f.task.ID); err != nil || found {
		t.Fatalf("acceptance leaked %v %v", found, err)
	}
}
