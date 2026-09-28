package fakemodel

import (
	"context"
	"errors"
	"sync"

	"github.com/SofiaFlux/summa42/internal/ghtriage"
)

// Fake is the deterministic test double for both model interfaces. It records
// every input it received, so a test can assert on the exact prepared document
// and not only on the resulting disposition.
//
// Classify and Review are safe for concurrent use. The recording fields are
// appended under that lock, so a test that calls the fake from several
// goroutines can read ClassifyInputs and ReviewInputs once they have joined;
// reading them from a goroutine still running is a race in the test.
type Fake struct {
	mu sync.Mutex

	Scripted       []ghtriage.Stage2Output
	ScriptedReview []bool
	ClassifyInputs []ghtriage.Stage2Input
	ReviewInputs   []ghtriage.ReviewInput
	Err            error

	classifyCalls int
	reviewCalls   int
}

func New() *Fake { return &Fake{} }

func (f *Fake) Classify(ctx context.Context, input ghtriage.Stage2Input) (ghtriage.Stage2Output, error) {
	if err := ctx.Err(); err != nil {
		return ghtriage.Stage2Output{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ClassifyInputs = append(f.ClassifyInputs, input)
	if f.Err != nil {
		return ghtriage.Stage2Output{}, f.Err
	}
	if f.classifyCalls >= len(f.Scripted) {
		return ghtriage.Stage2Output{}, errors.New("fakemodel: no scripted classifier response left")
	}
	out := f.Scripted[f.classifyCalls]
	f.classifyCalls++
	return out, nil
}

func (f *Fake) Review(ctx context.Context, input ghtriage.ReviewInput) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ReviewInputs = append(f.ReviewInputs, input)
	if f.Err != nil {
		return false, f.Err
	}
	if f.reviewCalls >= len(f.ScriptedReview) {
		return false, errors.New("fakemodel: no scripted reviewer response left")
	}
	plausible := f.ScriptedReview[f.reviewCalls]
	f.reviewCalls++
	return plausible, nil
}

// ClassifyCallCount returns the number of scripted classifier responses
// consumed, not the number of Classify calls made. It stays 0 when Err is set,
// even though the call happened and its input was recorded, and it reads 1
// rather than 2 once a single scripted response is exhausted. For a true call
// count use len(ClassifyInputs), which grows on every call that got past the
// context check.
func (f *Fake) ClassifyCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.classifyCalls
}

// ReviewCallCount returns the number of scripted reviewer responses consumed,
// not the number of Review calls made; it is the reviewer-side counterpart of
// ClassifyCallCount. For a true call count use len(ReviewInputs).
func (f *Fake) ReviewCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reviewCalls
}
