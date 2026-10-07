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
