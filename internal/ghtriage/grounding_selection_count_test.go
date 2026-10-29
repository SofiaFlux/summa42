package ghtriage

import (
	"strings"
	"testing"
)

// A context-count failure names the count, so a log line says whether the model
// chose nothing or chose too much.
func TestSelectionErrorsNameTheCount(t *testing.T) {
	cfg := &GroundingConfig{AllowedPrefixes: []string{"internal/"}}
	err := checkSelection(FileSelection{AllowedPaths: []string{"internal/x.go"}}, cfg, map[string]bool{})
	if err == nil || !strings.Contains(err.Error(), "got 0 (pinned 0, selected 0)") {
		t.Fatalf("empty selection error = %v, want the count", err)
	}
}

// With no pinned files and no context chosen, the files the change will touch are
// read anyway, so they serve as context. New files are not context: there is
// nothing to read yet.
func TestEmptyContextFallsBackToExistingAllowedPaths(t *testing.T) {
	tracked := map[string]bool{"internal/a.go": true}
	got := contextFallback(FileSelection{AllowedPaths: []string{"internal/a.go", "internal/new.go"}}, nil, tracked)
	if len(got.ContextFiles) != 1 || got.ContextFiles[0] != "internal/a.go" {
		t.Fatalf("fallback context = %v, want [internal/a.go]", got.ContextFiles)
	}
	kept := contextFallback(FileSelection{AllowedPaths: []string{"internal/a.go"}}, []string{"docs/pin.md"}, tracked)
	if len(kept.ContextFiles) != 0 {
		t.Fatalf("pinned context present, fallback must not add: %v", kept.ContextFiles)
	}
}
