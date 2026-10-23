package workflowcase

import (
	"testing"
)

func TestBlockHoldsAnActiveCaseThroughAssess(t *testing.T) {
	svc, ctx, c := assessmentFixture(t)
	got, err := svc.Block(ctx, c.ID, "incident-2026-10-21", "blocked-evidence")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != Blocked || got.CurrentWorkID != "" || got.NextWork.Kind != "" || got.RemainingBudget != c.RemainingBudget {
		t.Fatalf("unexpected blocked case: %+v", got)
	}
	if n := assessmentCount(t, svc, ctx, c.ID); n != 1 {
		t.Fatalf("block left %d assessments, want 1 (it must go through Assess)", n)
	}
	if _, err := svc.Block(ctx, c.ID, "again", "blocked-evidence"); err == nil {
		t.Fatal("blocking a non-active case succeeded")
	}
	if _, err := svc.Block(ctx, c.ID, " ", "blocked-evidence"); err == nil {
		t.Fatal("blank reason accepted")
	}
}

func TestBlockRefusesUnknownEvidence(t *testing.T) {
	svc, ctx, c := assessmentFixture(t)
	if _, err := svc.Block(ctx, c.ID, "incident", "no-such-evidence"); err == nil {
		t.Fatal("block cited unknown evidence")
	}
	if got, _ := svc.Get(ctx, c.ID); got.State != Active {
		t.Fatalf("failed block changed the case: %s", got.State)
	}
}
