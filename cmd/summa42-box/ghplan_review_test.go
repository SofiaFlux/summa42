package main

import (
	"context"
	"testing"
)

func TestPlanReviewRequiresMissionAndConfiguredModel(t *testing.T) {
	if err := runGHPlanReview(nil, nil); err == nil {
		t.Fatal("nil context accepted")
	}
	if err := runGHPlanReview(context.Background(), nil); err == nil {
		t.Fatal("missing mission accepted")
	}
	if err := runGHPlanReview(context.Background(), []string{"--mission", "mission-1", "--model-binary", "/does-not-exist"}); err == nil {
		t.Fatal("missing model accepted")
	}
}
