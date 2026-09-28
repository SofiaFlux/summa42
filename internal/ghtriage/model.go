package ghtriage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const ReviewerQuestion = "Does the recorded classification follow from what this issue " +
	"actually describes? Answer with JSON containing only a single boolean field named plausible."

// ClassifierModel turns a prepared, bounded context into signals. It never
// returns a disposition: stage 3 owns that.
type ClassifierModel interface {
	Classify(ctx context.Context, input Stage2Input) (Stage2Output, error)
}

// ReviewerModel answers one plausibility question with one boolean.
type ReviewerModel interface {
	Review(ctx context.Context, input ReviewInput) (bool, error)
}

type ReviewInput struct {
	Schema   string   `json:"schema"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Question string   `json:"question"`
	Decision Decision `json:"decision"`
}

type reviewResponse struct {
	Plausible *bool `json:"plausible"`
}

// stage2Response mirrors Stage2Output with pointer booleans, so an absent
// field stays distinguishable from an explicit false. Stage2Output cannot
// carry that distinction, and the difference is disposition-bearing: a
// truncated response carrying no is_actionable would otherwise read as false
// and be disposed of as not-actionable, where the same document with
// is_actionable true is ready-to-plan. The model proposes, deterministic code
// disposes — so an incomplete proposal is rejected, not completed.
type stage2Response struct {
	IsActionable  *bool         `json:"is_actionable"`
	NeedsRepro    *bool         `json:"needs_repro"`
	Scope         Scope         `json:"scope"`
	SuggestedType SuggestedType `json:"suggested_type"`
	Rationale     string        `json:"rationale"`
}

// ParseStage2Output accepts exactly one conforming object. Unknown fields are
// rejected rather than ignored, so a model that invents a field is treated as
// non-conforming rather than partially understood. Both booleans must be
// present: an absent one is not read as false.
func ParseStage2Output(raw []byte) (Stage2Output, error) {
	var response stage2Response
	if err := decodeExactlyOne(raw, &response); err != nil {
		return Stage2Output{}, err
	}
	if response.IsActionable == nil {
		return Stage2Output{}, errors.New("stage 2 response carries no is_actionable field")
	}
	if response.NeedsRepro == nil {
		return Stage2Output{}, errors.New("stage 2 response carries no needs_repro field")
	}
	out := Stage2Output{
		IsActionable:  *response.IsActionable,
		NeedsRepro:    *response.NeedsRepro,
		Scope:         response.Scope,
		SuggestedType: response.SuggestedType,
		Rationale:     response.Rationale,
	}
	if strings.TrimSpace(out.Rationale) == "" {
		return Stage2Output{}, errors.New("stage 2 rationale is required")
	}
	switch out.Scope {
	case ScopeSmall, ScopeMedium, ScopeLarge, ScopeUnknown:
	default:
		return Stage2Output{}, fmt.Errorf("stage 2 scope %q is outside the vocabulary", out.Scope)
	}
	switch out.SuggestedType {
	case SuggestedTypeBug, SuggestedTypeFeature, SuggestedTypeQuestion, SuggestedTypeOther:
	default:
		return Stage2Output{}, fmt.Errorf("stage 2 suggested_type %q is outside the vocabulary", out.SuggestedType)
	}
	return out, nil
}

// ParseReviewVerdict accepts a response whose only content is plausible.
func ParseReviewVerdict(raw []byte) (bool, error) {
	var response reviewResponse
	if err := decodeExactlyOne(raw, &response); err != nil {
		return false, err
	}
	if response.Plausible == nil {
		return false, errors.New("review response carries no plausible field")
	}
	return *response.Plausible, nil
}

func decodeExactlyOne(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode model response: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("model response must contain exactly one JSON object and nothing else")
	}
	return nil
}
