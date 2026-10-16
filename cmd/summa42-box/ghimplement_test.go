package main

import (
	"context"
	"testing"
)

func TestImplementationCLIRejectsMissingOrRelativePaths(t *testing.T) {
	if err := runGHImplement(nil, nil); err == nil {
		t.Fatal("nil context accepted")
	}
	for _, args := range [][]string{nil, {"--case", "c"}, {"--case", "c", "--source-repo", "relative", "--workspace-root", "/tmp"}, {"--case", "c", "--source-repo", "/tmp", "--workspace-root", "relative"}, {"--case", "c", "--source-repo", "/tmp", "--workspace-root", "/tmp", "extra"}, {"--case", "c", "--source-repo", "/tmp", "--workspace-root", "/tmp", "--model-binary", "/does-not-exist"}} {
		if err := runGHImplement(context.Background(), args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
