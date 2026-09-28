package fakemodel

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/SofiaFlux/summa42/internal/ghtriage"
)

func scriptedOutput(rationale string) ghtriage.Stage2Output {
	return ghtriage.Stage2Output{
		IsActionable:  true,
		NeedsRepro:    false,
		Scope:         ghtriage.ScopeSmall,
		SuggestedType: ghtriage.SuggestedTypeBug,
		Rationale:     rationale,
	}
}

// ClassifyCallCount reads as a call count, and the two agree only until a call
// fails or the script runs out. Tasks 4 and 8 will read it as "how many times
// did stage 2 run", so both divergence points are pinned here: a call that
// returns Err records its input without consuming a response, and a call that
// runs out of script records its input without either. The count is the number
// of scripted responses handed out; len(ClassifyInputs) is the number of calls.
func TestClassifyCallCountCountsConsumedScriptedResponses(t *testing.T) {
	failing := New()
	failing.Err = errors.New("model provider unavailable")
	_, err := failing.Classify(context.Background(), ghtriage.Stage2Input{Schema: ghtriage.Stage2InputSchema})
	if !errors.Is(err, failing.Err) {
		t.Fatalf("Classify() error = %v, want the scripted %v", err, failing.Err)
	}
	if got := failing.ClassifyCallCount(); got != 0 {
		t.Fatalf("ClassifyCallCount() = %d after a call that returned Err, want 0: it counts consumed responses, not calls", got)
	}
	if got := len(failing.ClassifyInputs); got != 1 {
		t.Fatalf("len(ClassifyInputs) = %d, want 1: the input is recorded even when the call fails", got)
	}

	one := New()
	one.Scripted = []ghtriage.Stage2Output{scriptedOutput("a crash on the next call")}
	first, err := one.Classify(context.Background(), ghtriage.Stage2Input{Schema: ghtriage.Stage2InputSchema})
	if err != nil {
		t.Fatal(err)
	}
	if first.Rationale != "a crash on the next call" {
		t.Fatalf("the first call got %q, want the only scripted response", first.Rationale)
	}
	if _, err := one.Classify(context.Background(), ghtriage.Stage2Input{Schema: ghtriage.Stage2InputSchema}); err == nil {
		t.Fatal("a call past the end of the script was treated as success")
	}
	if got := one.ClassifyCallCount(); got != 1 {
		t.Fatalf("ClassifyCallCount() = %d after two calls and one scripted response, want 1", got)
	}
	if got := len(one.ClassifyInputs); got != 2 {
		t.Fatalf("len(ClassifyInputs) = %d after two calls, want 2: this is the true call count", got)
	}
}

// The recording fields are appended under the mutex, so the whole fake is safe
// to share across the stage-2 and stage-3 goroutines a pipeline runs them in.
// That is a claim about a mutex, so the test is one the race detector can fail:
// without the lock these appends race, and with it the inputs are all
// recorded. The slices are read only after Wait, which is the only point at
// which reading them is not itself the race.
func TestConcurrentClassifyAndReviewRecordEveryInput(t *testing.T) {
	const (
		classifyCalls = 8
		reviewCalls   = 8
	)
	fake := New()
	fake.Scripted = make([]ghtriage.Stage2Output, classifyCalls)
	fake.ScriptedReview = make([]bool, reviewCalls)

	var wg sync.WaitGroup
	for i := 0; i < classifyCalls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := fake.Classify(context.Background(), ghtriage.Stage2Input{Schema: ghtriage.Stage2InputSchema}); err != nil {
				t.Error(err)
			}
		}()
	}
	for i := 0; i < reviewCalls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := fake.Review(context.Background(), ghtriage.ReviewInput{Schema: ghtriage.ReviewSchema}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if got := len(fake.ClassifyInputs); got != classifyCalls {
		t.Fatalf("len(ClassifyInputs) = %d after %d concurrent calls, want %d", got, classifyCalls, classifyCalls)
	}
	if got := len(fake.ReviewInputs); got != reviewCalls {
		t.Fatalf("len(ReviewInputs) = %d after %d concurrent calls, want %d", got, reviewCalls, reviewCalls)
	}
	if got := fake.ClassifyCallCount(); got != classifyCalls {
		t.Fatalf("ClassifyCallCount() = %d, want %d", got, classifyCalls)
	}
	if got := fake.ReviewCallCount(); got != reviewCalls {
		t.Fatalf("ReviewCallCount() = %d, want %d", got, reviewCalls)
	}
}
