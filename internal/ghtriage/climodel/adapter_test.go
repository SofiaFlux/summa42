package climodel

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/ghtriage"
)

// writeScript creates an executable that consumes the prompt on stdin and
// prints body on stdout, so the adapter can be driven without a real model.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "model.sh")
	script := "#!/bin/sh\ncat > /dev/null\ncat <<'JSON'\n" + body + "\nJSON\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNewRejectsAnUnusableConfiguration(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("an empty binary was accepted")
	}
	if _, err := New(Config{Binary: filepath.Join(t.TempDir(), "absent"), Timeout: time.Second}); err == nil {
		t.Fatal("a missing binary was accepted")
	}
	if _, err := New(Config{Binary: writeScript(t, "{}")}); err == nil {
		t.Fatal("a zero timeout was accepted")
	}
}

func TestClassifyRequiresExactlyOneJSONObject(t *testing.T) {
	valid := `{"is_actionable":true,"needs_repro":false,"scope":"small","suggested_type":"bug","rationale":"enough detail"}`
	cases := []struct {
		name    string
		stdout  string
		wantErr bool
	}{
		{"one object", valid, false},
		{"two objects", valid + "\n" + valid, true},
		{"trailing prose", valid + "\nsome explanation", true},
		{"not json", "I cannot answer that", true},
		{"wrong field type", `{"is_actionable":"yes"}`, true},
		{"unknown field", `{"is_actionable":true,"needs_repro":false,"scope":"small","suggested_type":"bug","rationale":"x","severity":"high"}`, true},
		{"missing rationale", `{"is_actionable":true,"needs_repro":false,"scope":"small","suggested_type":"bug"}`, true},
		{"scope outside the vocabulary", `{"is_actionable":true,"needs_repro":false,"scope":"enormous","suggested_type":"bug","rationale":"x"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter, err := New(Config{Binary: writeScript(t, tc.stdout), Timeout: 10 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			_, err = adapter.Classify(context.Background(), ghtriage.Stage2Input{Schema: ghtriage.Stage2InputSchema})
			if tc.wantErr && err == nil {
				t.Fatal("a non-conforming response was accepted")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("a conforming response was rejected: %v", err)
			}
		})
	}
}

func TestReviewReturnsOnlyABoolean(t *testing.T) {
	adapter, err := New(Config{Binary: writeScript(t, `{"plausible":true}`), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	plausible, err := adapter.Review(context.Background(), ghtriage.ReviewInput{Schema: ghtriage.ReviewSchema})
	if err != nil {
		t.Fatal(err)
	}
	if !plausible {
		t.Fatal("plausible = false, want true")
	}
	verbose, err := New(Config{Binary: writeScript(t, `{"plausible":true,"severity":"high"}`), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verbose.Review(context.Background(), ghtriage.ReviewInput{Schema: ghtriage.ReviewSchema}); err == nil {
		t.Fatal("a verdict carrying a severity was accepted")
	}
	missing, err := New(Config{Binary: writeScript(t, `{}`), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missing.Review(context.Background(), ghtriage.ReviewInput{Schema: ghtriage.ReviewSchema}); err == nil {
		t.Fatal("a verdict without plausible was accepted")
	}
}

// The model prints a fully conforming response and *then* exits non-zero, so
// the exit status is the only thing that can produce an error. A script that
// failed silently instead would not pin this: an empty response is rejected by
// the parser on its own account, so the test would still pass against an
// adapter that ignored the exit status entirely.
func TestClassifyFailsWhenTheProcessFails(t *testing.T) {
	failing := filepath.Join(t.TempDir(), "failing.sh")
	script := "#!/bin/sh\ncat > /dev/null\ncat <<'JSON'\n" +
		`{"is_actionable":true,"needs_repro":false,"scope":"small","suggested_type":"bug","rationale":"x"}` +
		"\nJSON\nexit 3\n"
	if err := os.WriteFile(failing, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	adapter, err := New(Config{Binary: failing, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Classify(context.Background(), ghtriage.Stage2Input{Schema: ghtriage.Stage2InputSchema}); err == nil {
		t.Fatal("a failing model process was treated as success")
	}
}
