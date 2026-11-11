package workflowcase

import (
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/testutil"
	"github.com/SofiaFlux/summa42/internal/workflow"
)

func TestListSummariesOrdersByUpdateAndCarriesTheLastReason(t *testing.T) {
	svc, _, missionID, ctx := setupEnsure(t)
	clk := svc.clock.(*testutil.Clock)
	ensure := func(object string) Case {
		observation := sampleObservation(missionID)
		observation.ObjectID = object
		c, err := svc.Ensure(ctx, observation)
		if err != nil {
			t.Fatal(err)
		}
		clk.Advance(time.Minute)
		return c
	}
	first, second, third := ensure("item-1"), ensure("item-2"), ensure("item-3")
	// Assessing the oldest case makes it the most recently updated one.
	if _, err := svc.Assess(ctx, AssessmentRequest{
		CaseID: first.ID, WorkID: first.CurrentWorkID,
		Assessment:      workflow.Assessment{Verdict: workflow.Unknown, Reason: "cannot tell", EvidenceIDs: []string{"evidence"}},
		RemainingBudget: first.RemainingBudget, ProgressSignature: "blocked",
	}); err != nil {
		t.Fatal(err)
	}

	all, err := svc.ListSummaries(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Case.ID != first.ID || all[1].Case.ID != third.ID || all[2].Case.ID != second.ID {
		t.Fatalf("order = %v %v %v, want the assessed case, then newest first", all[0].Case.ObjectID, all[1].Case.ObjectID, all[2].Case.ObjectID)
	}
	if all[0].Case.State != Blocked || all[0].LastAssessmentReason != "cannot tell" {
		t.Fatalf("assessed case summary: %+v", all[0])
	}
	if all[1].LastAssessmentReason != "" || all[1].Case.NextWork.Kind != "inspect" {
		t.Fatalf("unassessed case summary: %+v", all[1])
	}
	if !all[0].UpdatedAt.After(all[1].UpdatedAt) {
		t.Fatalf("updated_at not carried: %v, %v", all[0].UpdatedAt, all[1].UpdatedAt)
	}

	blocked, err := svc.ListSummaries(ctx, Blocked)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 1 || blocked[0].Case.ID != first.ID {
		t.Fatalf("blocked filter returned %d cases", len(blocked))
	}
	closed, err := svc.ListSummaries(ctx, Closed)
	if err != nil || len(closed) != 0 {
		t.Fatalf("closed filter: %d cases, %v", len(closed), err)
	}
}
