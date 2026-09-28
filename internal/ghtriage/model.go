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

// ParseStage2Output accepts exactly one conforming object. Unknown fields are
// rejected rather than ignored, so a model that invents a field is treated as
// non-conforming rather than partially understood.
func ParseStage2Output(raw []byte) (Stage2Output, error) {
	var out Stage2Output
	if err := decodeExactlyOne(raw, &out); err != nil {
		return Stage2Output{}, err
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
