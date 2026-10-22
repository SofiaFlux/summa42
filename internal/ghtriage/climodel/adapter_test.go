package climodel

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/modelrun/modelruntest"
)

const testUser = modelruntest.User

var testHome, fakeSudoLog string

// TestMain swaps the real sudo for a script that drops its options and runs the
// rest directly, and the user database for one fake model user, so the whole
// argv contract is exercised without privileges.
func TestMain(m *testing.M) {
	home, sudoLog, cleanup, err := modelruntest.Install()
	if err != nil {
		panic(err)
	}
	testHome, fakeSudoLog = home, sudoLog
	code := m.Run()
	cleanup()
	os.Exit(code)
}

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
	if _, err := New(Config{RunAsUser: testUser, Binary: "", Timeout: time.Second}); err == nil {
		t.Fatal("an empty binary was accepted")
	}
	if _, err := New(Config{RunAsUser: testUser, Binary: filepath.Join(t.TempDir(), "absent"), Timeout: time.Second}); err == nil {
		t.Fatal("a missing binary was accepted")
	}
	if _, err := New(Config{RunAsUser: testUser, Binary: writeScript(t, "{}")}); err == nil {
		t.Fatal("a zero timeout was accepted")
	}
}

func TestReviewPlanUsesSeparateStrictInvocation(t *testing.T) {
	adapter, err := New(Config{RunAsUser: testUser, Binary: writeScript(t, `{"verdict":"ACCEPT","reason":"scope fits"}`), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	out, err := adapter.ReviewPlan(t.Context(), ghtriage.PlanReviewInput{Schema: ghtriage.PlanReviewVersion})
	if err != nil || out.Verdict != "ACCEPT" {
		t.Fatalf("%+v %v", out, err)
	}
	adapter, err = New(Config{RunAsUser: testUser, Binary: writeScript(t, `{"verdict":"ACCEPT","reason":"ok","grant":"write"}`), Timeout: time.Second})
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
			adapter, err := New(Config{RunAsUser: testUser, Binary: writeScript(t, tc.stdout), Timeout: 10 * time.Second})
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
	adapter, err := New(Config{RunAsUser: testUser, Binary: writeScript(t, `{"plausible":true}`), Timeout: 10 * time.Second})
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
	verbose, err := New(Config{RunAsUser: testUser, Binary: writeScript(t, `{"plausible":true,"severity":"high"}`), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verbose.Review(context.Background(), ghtriage.ReviewInput{Schema: ghtriage.ReviewSchema}); err == nil {
		t.Fatal("a verdict carrying a severity was accepted")
	}
	missing, err := New(Config{RunAsUser: testUser, Binary: writeScript(t, `{}`), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missing.Review(context.Background(), ghtriage.ReviewInput{Schema: ghtriage.ReviewSchema}); err == nil {
		t.Fatal("a verdict without plausible was accepted")
	}
}

func TestPlanUsesStrictPlanOutput(t *testing.T) {
	adapter, err := New(Config{RunAsUser: testUser, Binary: writeScript(t, `{"summary":"Fix the crash","steps":[{"objective":"Add a regression test","done_when":"It fails before the fix"}]}`), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := adapter.Plan(context.Background(), ghtriage.PlanInput{Schema: ghtriage.PlanSchema, Question: ghtriage.PlanQuestion})
	if err != nil || len(plan.Steps) != 1 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	bad, err := New(Config{RunAsUser: testUser, Binary: writeScript(t, `{"summary":"Fix the crash","steps":[],"extra":true}`), Timeout: 10 * time.Second})
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
	adapter, err := New(Config{RunAsUser: testUser, Binary: binary, Args: []string{"exec", "--model", "gpt-5"}, Timeout: 10 * time.Second})
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
	adapter, err = New(Config{RunAsUser: testUser, Binary: failing, Args: []string{"exec", "--model", "gpt-5"}, Timeout: 10 * time.Second})
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
	adapter, err := New(Config{RunAsUser: testUser, Binary: failing, Timeout: 10 * time.Second})
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
	adapter, err := New(Config{RunAsUser: testUser, Binary: slow, Timeout: timeout})
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
	adapter, err := New(Config{RunAsUser: testUser, Binary: writeScript(t, `{"summary":"change","edits":[{"path":"code.go","content":"package test\n","delete":false}]}`), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	out, err := adapter.Implement(t.Context(), ghtriage.ImplementationInput{Schema: ghtriage.ImplementationVersion})
	if err != nil || len(out.Edits) != 1 || out.Edits[0].Path != "code.go" {
		t.Fatalf("%+v %v", out, err)
	}
	adapter, err = New(Config{RunAsUser: testUser, Binary: writeScript(t, `{"summary":"change","edits":[{"path":"code.go","content":"x","delete":false}],"run":"model-shell"}`), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Implement(t.Context(), ghtriage.ImplementationInput{}); err == nil {
		t.Fatal("model command accepted")
	}
}

func TestCodeReviewUsesStrictVerdict(t *testing.T) {
	for _, raw := range []string{`{"verdict":"ACCEPT","reason":"fits"}`, `{"verdict":"ACCEPT","reason":"fits","publish":true}`, `{"verdict":"YES","reason":"fits"}`} {
		a, err := New(Config{RunAsUser: testUser, Binary: writeScript(t, raw), Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		out, err := a.ReviewCode(t.Context(), ghtriage.CodeReviewInput{Schema: ghtriage.CodeReviewVersion})
		if raw == `{"verdict":"ACCEPT","reason":"fits"}` {
			if err != nil || out.Verdict != "ACCEPT" {
				t.Fatalf("%+v %v", out, err)
			}
		} else if err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func envScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "env.sh")
	script := "#!/bin/sh\ncat > /dev/null\nenv | sed 's/=.*//' | tr '\\n' ' ' | sed 's/^/{\"verdict\":\"ACCEPT\",\"reason\":\"/; s/ $/\"}/'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func childEnvNames(t *testing.T, config Config) string {
	t.Helper()
	config.RunAsUser, config.Binary, config.Timeout = testUser, envScript(t), 5*time.Second
	adapter, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	out, err := adapter.ReviewPlan(t.Context(), ghtriage.PlanReviewInput{})
	if err != nil {
		t.Fatal(err)
	}
	return " " + out.Reason
}

func TestAdapterChildEnvIsMinimal(t *testing.T) {
	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN", "SUMMA42_CONTROL_TOKEN", "FOO"} {
		t.Setenv(name, "secret")
	}
	names := childEnvNames(t, Config{})
	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN", "SUMMA42_CONTROL_TOKEN", "FOO"} {
		if strings.Contains(names, " "+name+" ") {
			t.Fatalf("child env leaked %s: %s", name, names)
		}
	}
	if !strings.Contains(names, " PATH ") {
		t.Fatalf("child env lacks PATH: %s", names)
	}
}

func TestAdapterChildEnvHonoursAllowlist(t *testing.T) {
	t.Setenv("FOO", "1")
	t.Setenv("BAR", "1")
	names := childEnvNames(t, Config{EnvAllowlist: []string{"FOO"}})
	if !strings.Contains(names, " FOO ") || strings.Contains(names, " BAR ") {
		t.Fatalf("child env = %s, want FOO and not BAR", names)
	}
}

func TestNewFailsClosedWithoutADedicatedUser(t *testing.T) {
	self, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	bin := writeScript(t, "{}")
	for name, runAs := range map[string]string{"unset": "", "blank": "  ", "root": "root", "self": self.Username, "unknown": "no-such-user-xyz"} {
		if _, err := New(Config{RunAsUser: runAs, Binary: bin, Timeout: time.Second}); err == nil {
			t.Fatalf("%s: New accepted run-as user %q", name, runAs)
		}
	}
	_, err = New(Config{Binary: bin, Timeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "SUMMA42_MODEL_USER") {
		t.Fatalf("unset user error = %v, want it to name SUMMA42_MODEL_USER", err)
	}
}

// The model is launched through sudo with env -i: its HOME is the model user's
// home, never the Box's, and no Box variable outside the allowlist reaches it.
func TestRunLaunchesAsTheModelUserWithACleanEnvironment(t *testing.T) {
	t.Setenv("HOME", "/home/box-owner")
	t.Setenv("GITHUB_TOKEN", "secret")
	t.Setenv("ALLOWED", "yes")
	script := filepath.Join(t.TempDir(), "probe.sh")
	body := "#!/bin/sh\ncat > /dev/null\nprintf '{\"verdict\":\"ACCEPT\",\"reason\":\"home=%s tmp=%s pwd=%s tok=%s allowed=%s\"}' \"$HOME\" \"$TMPDIR\" \"$(pwd)\" \"$GITHUB_TOKEN\" \"$ALLOWED\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	adapter, err := New(Config{RunAsUser: testUser, Binary: script, Timeout: 5 * time.Second, EnvAllowlist: []string{"ALLOWED", "HOME"}})
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(fakeSudoLog)
	out, err := adapter.ReviewPlan(t.Context(), ghtriage.PlanReviewInput{})
	if err != nil {
		t.Fatal(err)
	}
	want := "home=" + testHome + " tmp=" + testHome + "/tmp pwd=" + testHome + " tok= allowed=yes"
	if out.Reason != want {
		t.Fatalf("model saw %q, want %q", out.Reason, want)
	}
	logged, err := os.ReadFile(fakeSudoLog)
	if err != nil || !strings.HasPrefix(string(logged), "-n --preserve-env=ALLOWED -u "+testUser+" -- /usr/bin/timeout --kill-after=15s 5s /bin/sh -c") {
		t.Fatalf("launcher saw %q (%v), want sudo -n --preserve-env=ALLOWED -u %s -- timeout ...", logged, err, testUser)
	}
	if strings.Contains(string(logged), "/home/box-owner") || strings.Contains(string(logged), "secret") || strings.Contains(string(logged), "ALLOWED=yes") {
		t.Fatalf("Box state leaked into the launch command: %q", logged)
	}
}
