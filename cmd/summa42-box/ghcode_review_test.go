package main

import (
	"context"
	"testing"
)

func TestCodeReviewCLIRejectsMissingOrRelativeSource(t *testing.T) {
	if err := runGHCodeReview(nil, nil); err == nil {
		t.Fatal("nil context accepted")
	}
	for _, args := range [][]string{nil, {"--case", "c"}, {"--case", "c", "--source-repo", "relative"}, {"--case", "c", "--source-repo", "/tmp", "extra"}, {"--case", "c", "--source-repo", "/tmp", "--model-binary", "/missing"}} {
		if err := runGHCodeReview(context.Background(), args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
