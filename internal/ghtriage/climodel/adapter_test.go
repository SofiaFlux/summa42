package climodel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	// A positive timeout is set on every empty-binary case: Config{} also has a
	// zero timeout, so leaving it out would let this pass on the timeout guard
	// with the missing-binary check deleted.
	if _, err := New(Config{Binary: "", Timeout: time.Second}); err == nil {
		t.Fatal("an empty binary was accepted")
	}
	if _, err := New(Config{Binary: filepath.Join(t.TempDir(), "absent"), Timeout: time.Second}); err == nil {
		t.Fatal("a missing binary was accepted")
	}
	if _, err := New(Config{Binary: writeScript(t, "{}")}); err == nil {
		t.Fatal("a zero timeout was accepted")
	}
}

func TestReviewPlanUsesSeparateStrictInvocation(t *testing.T) {
	adapter, err := New(Config{Binary: writeScript(t, `{"verdict":"ACCEPT","reason":"scope fits"}`), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	out, err := adapter.ReviewPlan(t.Context(), ghtriage.PlanReviewInput{Schema: ghtriage.PlanReviewVersion})
	if err != nil || out.Verdict != "ACCEPT" {
		t.Fatalf("%+v %v", out, err)
	}
	adapter, err = New(Config{Binary: writeScript(t, `{"verdict":"ACCEPT","reason":"ok","grant":"write"}`), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.ReviewPlan(t.Context(), ghtriage.PlanReviewInput{}); err == nil {
		t.Fatal("extra authority field accepted")
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
		{"model supplied a disposition", `{"is_actionable":true,"needs_repro":false,"scope":"small","suggested_type":"bug","rationale":"x","disposition":"ready-to-plan"}`, true},
		{"missing rationale", `{"is_actionable":true,"needs_repro":false,"scope":"small","suggested_type":"bug"}`, true},
		{"blank rationale", `{"is_actionable":true,"needs_repro":false,"scope":"small","suggested_type":"bug","rationale":"   "}`, true},
		{"scope outside the vocabulary", `{"is_actionable":true,"needs_repro":false,"scope":"enormous","suggested_type":"bug","rationale":"x"}`, true},
		// Both booleans must be present: an absent one reads as false and is
		// disposed of as not-actionable, where the same document with
		// is_actionable true is ready-to-plan. And suggested_type is checked
		// nowhere else — Decision.Validate never inspects it — so an unchecked
		// value lands verbatim in the canonical decision record.
		{"missing is_actionable", `{"needs_repro":false,"scope":"small","suggested_type":"bug","rationale":"x"}`, true},
		{"missing needs_repro", `{"is_actionable":true,"scope":"small","suggested_type":"bug","rationale":"x"}`, true},
		// What is rejected is an absent field, not false. A model that answers
		// false has answered, and a not-actionable disposition is one stage 3
		// makes from this very field; a guard written the other way round turns
		// every honest negative into a parse error and reports a model outage
		// for correct output. Nothing else in the table carries a false, so
		// without this row the guard can be inverted and the suite stays green.
		{"explicit false is_actionable", `{"is_actionable":false,"needs_repro":false,"scope":"small","suggested_type":"bug","rationale":"an ask, not a defect"}`, false},
		{"suggested_type outside the vocabulary", `{"is_actionable":true,"needs_repro":false,"scope":"small","suggested_type":"epic","rationale":"x"}`, true},
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

func TestPlanUsesStrictPlanOutput(t *testing.T) {
	adapter, err := New(Config{Binary: writeScript(t, `{"summary":"Fix the crash","steps":[{"objective":"Add a regression test","done_when":"It fails before the fix"}]}`), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := adapter.Plan(context.Background(), ghtriage.PlanInput{Schema: ghtriage.PlanSchema, Question: ghtriage.PlanQuestion})
	if err != nil || len(plan.Steps) != 1 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	bad, err := New(Config{Binary: writeScript(t, `{"summary":"Fix the crash","steps":[],"extra":true}`), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Plan(context.Background(), ghtriage.PlanInput{}); err == nil {
		t.Fatal("accepted malformed plan output")
	}
}

// The model prints a fully conforming response, writes to stderr and *then*
// exits non-zero, so the exit status is the only thing that can produce an
// error. A script that failed silently instead would not pin this: an empty
// response is rejected by the parser on its own account, so the test would
// still pass against an adapter that ignored the exit status entirely. The
// stderr assertion is what pins the capture, which is otherwise unpinned: drop
// the ": %s" and the suite stays green.
// Config.Args is what makes a provider CLI that needs a subcommand usable at
// all: the prompt is on stdin and nothing else, so without arguments the binary
// is exec'd with nothing and reads no prompt. The field was reachable only from
// a hand-built Config, so a configuration that dropped it on the way in - a flag
// set that never read it, say - would leave the default model exec'd with zero
// arguments and every attempt failing after the full timeout, with no test
// anywhere that had ever run a binary with an argument.
func TestClassifyPassesTheConfiguredArgumentsToTheBinary(t *testing.T) {
	const marker = "the-binary-ran-with-its-arguments"
	binary := filepath.Join(t.TempDir(), "model.sh")
	script := "#!/bin/sh\ncat > /dev/null\n" +
		"for arg in \"$@\"; do echo " + marker + " \"$arg\" >&2; done\n" +
		`cat <<'JSON'` + "\n" +
		`{"is_actionable":true,"needs_repro":false,"scope":"small","suggested_type":"bug","rationale":"x"}` +
		"\nJSON\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	adapter, err := New(Config{Binary: binary, Args: []string{"exec", "--model", "gpt-5"}, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := adapter.Classify(context.Background(), ghtriage.Stage2Input{Schema: ghtriage.Stage2InputSchema}); err != nil {
		t.Fatalf("a conforming response from a binary taking arguments was rejected: %v", err)
	}
	// The adapter captures stderr and returns it only on a failure, so the
	// arguments a binary was called with are visible on the failing path, which
	// is the one an operator reads anyway.
	failing := failingScript(t, marker)
	adapter, err = New(Config{Binary: failing, Args: []string{"exec", "--model", "gpt-5"}, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Classify(context.Background(), ghtriage.Stage2Input{Schema: ghtriage.Stage2InputSchema})
	if err == nil {
		t.Fatal("a failing model process was treated as success")
	}
	// Both the presence of the arguments and their order, which is the whole
	// reason the flag is repeatable rather than a single string: a subcommand and
	// its own options have to reach the binary in the order they were written.
	for _, want := range []string{
		marker + " exec", marker + " --model", marker + " gpt-5",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not carry %q, so the arguments did not reach the binary in order", err, want)
		}
	}
}

// failingScript writes a binary that reports the arguments it was given on
// stderr and then fails, which is the only way to see them: a successful run
// returns stdout and keeps stderr.
func failingScript(t *testing.T, marker string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "failing.sh")
	script := "#!/bin/sh\ncat > /dev/null\n" +
		"for arg in \"$@\"; do echo " + marker + " \"$arg\" >&2; done\nexit 3\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClassifyFailsWhenTheProcessFails(t *testing.T) {
	const stderrText = "model provider unavailable"
	failing := filepath.Join(t.TempDir(), "failing.sh")
	script := "#!/bin/sh\ncat > /dev/null\ncat <<'JSON'\n" +
		`{"is_actionable":true,"needs_repro":false,"scope":"small","suggested_type":"bug","rationale":"x"}` +
		"\nJSON\necho '" + stderrText + "' >&2\nexit 3\n"
	if err := os.WriteFile(failing, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	adapter, err := New(Config{Binary: failing, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Classify(context.Background(), ghtriage.Stage2Input{Schema: ghtriage.Stage2InputSchema})
	if err == nil {
		t.Fatal("a failing model process was treated as success")
	}
	if !strings.Contains(err.Error(), stderrText) {
		t.Fatalf("error %q does not carry the model's stderr %q", err, stderrText)
	}
}

// The model is normally a wrapper script or a CLI around a provider SDK, so the
// work runs in a grandchild that inherits the stdout pipe. Killing only the
// direct child leaves that grandchild holding the pipe, so Run blocks on the
// copy goroutine until the grandchild exits by itself: a 300ms timeout against
// a 3s child takes 3s. Bounding the whole subtree is what makes the configured
// timeout mean anything, and the deadline error is what makes a hung model
// distinguishable from one that crashed.
//
// The script also writes to stderr before it hangs, which is the other half of
// the report. A provider that says why it is about to stall and then stalls is
// the case worth diagnosing, and the stderr capture is otherwise unpinned on
// this path: return the bare runCtx.Err() instead and the errors.Is assertion
// still passes while the diagnostic is gone.
func TestTimeoutBoundsTheWholeSubtree(t *testing.T) {
	const (
		timeout    = 300 * time.Millisecond
		childLives = 3 * time.Second
		allowed    = 2 * time.Second
		stderrText = "model provider unavailable"
	)
	slow := filepath.Join(t.TempDir(), "slow.sh")
	script := "#!/bin/sh\ncat > /dev/null\necho '" + stderrText + "' >&2\nsleep 3 &\nwait\n"
	if err := os.WriteFile(slow, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	adapter, err := New(Config{Binary: slow, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = adapter.Classify(context.Background(), ghtriage.Stage2Input{Schema: ghtriage.Stage2InputSchema})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("a model that overran its timeout was treated as success")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v is not distinguishable from a crashed model", err)
	}
	if !strings.Contains(err.Error(), stderrText) {
		t.Fatalf("error %q does not carry the model's stderr %q", err, stderrText)
	}
	if elapsed > allowed {
		t.Fatalf("the timeout did not bound the subtree: %s elapsed, want roughly %s and not the child's %s",
			elapsed, timeout, childLives)
	}
}

func TestImplementationUsesStrictStructuredEdits(t *testing.T) {
	adapter, err := New(Config{Binary: writeScript(t, `{"summary":"change","edits":[{"path":"code.go","content":"package test\n","delete":false}]}`), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	out, err := adapter.Implement(t.Context(), ghtriage.ImplementationInput{Schema: ghtriage.ImplementationVersion})
	if err != nil || len(out.Edits) != 1 || out.Edits[0].Path != "code.go" {
		t.Fatalf("%+v %v", out, err)
	}
	adapter, err = New(Config{Binary: writeScript(t, `{"summary":"change","edits":[{"path":"code.go","content":"x","delete":false}],"run":"model-shell"}`), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Implement(t.Context(), ghtriage.ImplementationInput{}); err == nil {
		t.Fatal("model command accepted")
	}
}
