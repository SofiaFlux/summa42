package fakemodel

import (
	"context"
	"errors"

	"github.com/SofiaFlux/summa42/internal/ghtriage"
)

// Fake is the deterministic test double for both model interfaces. It records
// every input it received, so a test can assert on the exact prepared document
// and not only on the resulting disposition.
type Fake struct {
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

func (f *Fake) ClassifyCallCount() int { return f.classifyCalls }
func (f *Fake) ReviewCallCount() int   { return f.reviewCalls }
