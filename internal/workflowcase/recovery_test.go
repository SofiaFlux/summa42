package workflowcase

import (
	"context"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/purpose"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/testutil"
	"github.com/SofiaFlux/summa42/internal/workflow"
)

func TestCaseSurvivesStoreReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := state.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.DB().Close() })
	clk := testutil.NewClock(time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC))
	purposes := purpose.New(store, clk)
	missionID, err := purposes.CreateMission(ctx, "Review incoming work")
	if err != nil {
		t.Fatal(err)
	}
	seedFixtureEvidence(t, ctx, store, clk)
	svc := New(store, clk, purposes)
	observation := sampleObservation(missionID)
	observation.Grant = workflow.Grant{Capabilities: []string{"read", "write"}, Actions: []string{"comment"}}
	initial, err := svc.Ensure(ctx, observation)
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Assess(ctx, continueRequest(initial))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := state.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.DB().Close() })
	recovered, err := New(reopened, clk, purpose.New(reopened, clk)).Get(ctx, initial.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != Active || recovered.CurrentWorkID != result.Case.CurrentWorkID ||
		recovered.CurrentWorkID == initial.CurrentWorkID || recovered.CompletedSteps != 1 ||
		recovered.RemainingBudget != 4 || recovered.NextWork.Kind != "publish" {
		t.Fatalf("reopened case lost transition: %+v", recovered)
	}
	if !reflect.DeepEqual(recovered, result.Case) {
		t.Fatalf("reopened case differs from persisted assessment: recovered=%+v assessed=%+v", recovered, result.Case)
	}
}

func TestConcurrentEnsureCreatesOneCase(t *testing.T) {
	svc, _, missionID, ctx := setupEnsure(t)
	request := sampleObservation(missionID)
	var cases [2]Case
	var errs [2]error
	var wg sync.WaitGroup
	for i := range cases {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cases[i], errs[i] = svc.Ensure(ctx, request)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if cases[0].ID == "" || cases[0].ID != cases[1].ID || cases[0].CurrentWorkID != cases[1].CurrentWorkID {
		t.Fatalf("concurrent replay returned different cases: %+v %+v", cases[0], cases[1])
	}
	var count int
	if err := svc.store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM workflow_cases WHERE mission_id=? AND source=? AND object_id=? AND revision_id=?", missionID, request.Source, request.ObjectID, request.RevisionID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("concurrent replay created %d cases, want 1", count)
	}
}

func TestFailedAssessmentDoesNotMutateCase(t *testing.T) {
	svc, ctx, initial := assessmentFixture(t)
	request := continueRequest(initial)
	request.Assessment.Next.ProposedActions = []string{"delete"}
	if _, err := svc.Assess(ctx, request); err == nil {
		t.Fatal("unauthorized action was accepted")
	}
	stored, err := svc.Get(ctx, initial.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored, initial) {
		t.Fatalf("rejected assessment changed case: before=%+v after=%+v", initial, stored)
	}
	if count := assessmentCount(t, svc, ctx, initial.ID); count != 0 {
		t.Fatalf("rejected assessment inserted %d rows", count)
	}
}
