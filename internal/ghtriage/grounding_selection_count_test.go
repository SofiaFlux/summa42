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
