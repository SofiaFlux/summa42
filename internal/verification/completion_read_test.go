package verification

import (
	"github.com/SofiaFlux/summa42/internal/domain"
	"testing"
)

func TestCompletionEvidenceIsBoundToExactTaskAndAttempt(t *testing.T) {
	f := newFixture(t)
	ev := f.put(t, "result")
	if _, err := f.verify.CompleteAttempt(f.ctx, f.attempt.ID, CompletionManifest{EvidenceIDs: []domain.ID{ev.ID}}); err != nil {
		t.Fatal(err)
	}
	ids, err := f.verify.CompletionEvidence(f.ctx, f.task.ID, f.attempt.ID)
	if err != nil || len(ids) != 1 || ids[0] != ev.ID {
		t.Fatalf("%v %v", ids, err)
	}
	if _, err := f.verify.CompletionEvidence(f.ctx, "other-task", f.attempt.ID); err == nil {
		t.Fatal("cross-task completion accepted")
	}
}
