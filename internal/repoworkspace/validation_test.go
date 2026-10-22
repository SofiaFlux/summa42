package repoworkspace

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/SofiaFlux/summa42/internal/modelrun/modelruntest"
)

// testValidator runs validation through the fake sudo of modelruntest: the same
// launcher as production, minus the privilege drop. It returns the fake model
// home and the sudo log.
func testValidator(t *testing.T, toolchain ...string) (v *Validator, home, sudoLog string) {
	t.Helper()
	home, sudoLog, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	v, err = NewValidator(modelruntest.User, toolchain)
	if err != nil {
		t.Fatal(err)
	}
	return v, home, sudoLog
}

func validationFixture(t *testing.T) (string, Candidate) {
	t.Helper()
	cfg, dir := candidateFixture(t)
	c, err := PrepareCandidate(context.Background(), dir, cfg.Commit, []string{"code.go"}, []Edit{{Path: "code.go", Content: "changed\n"}})
	if err != nil {
		t.Fatal(err)
	}
	return dir, c
}
func TestValidationRecordsRealFailureAndLiteralArgv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix fixture")
	}
	dir, c := validationFixture(t)
	v, _, _ := testValidator(t)
	got, err := v.ValidateCandidate(context.Background(), dir, c, [][]string{{"printf", "%s", "literal $(echo bad)"}, {"sh", "-c", "exit 7"}})
	if err != nil || len(got) != 2 || got[0].Output != "literal $(echo bad)" || !got[0].Passed || got[1].Passed || got[1].ExitCode != 7 {
		t.Fatalf("%+v %v", got, err)
	}
}
func TestValidationDoesNotInheritSecretEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix fixture")
	}
	t.Setenv("SUMMA42_TEST_SECRET", "secret")
	dir, c := validationFixture(t)
	v, _, _ := testValidator(t)
	got, err := v.ValidateCandidate(context.Background(), dir, c, [][]string{{"sh", "-c", "printf %s ${SUMMA42_TEST_SECRET-unset}"}})
	if err != nil || got[0].Output != "unset" {
		t.Fatalf("%+v %v", got, err)
	}
}

// The command runs in a copy on the model side: whatever model-written code
// does there cannot touch the Box's checkout.
func TestValidationRunsOnACopyAndLeavesTheCheckoutAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix fixture")
	}
	dir, c := validationFixture(t)
	v, home, sudoLog := testValidator(t)
	got, err := v.ValidateCandidate(context.Background(), dir, c, [][]string{
		{"sh", "-c", "printf altered > code.go; printf '%s|%s|' \"$PWD\" \"$(cat code.go)\"; test ! -e .git && echo nogit"}})
	if err != nil || len(got) != 1 || !got[0].Passed {
		t.Fatalf("%+v %v", got, err)
	}
	if !strings.HasPrefix(got[0].Output, filepath.Join(home, "tmp", "run.")) || !strings.HasSuffix(got[0].Output, "|altered|nogit\n") || strings.Contains(got[0].Output, dir) {
		t.Fatalf("command did not run in a model-side copy: %q", got[0].Output)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "code.go")); string(raw) != "changed\n" {
		t.Fatalf("model-side command altered the Box checkout: %q", raw)
	}
	logged, _ := os.ReadFile(sudoLog)
	if !strings.Contains(string(logged), "-u "+modelruntest.User+" -- ") {
		t.Fatalf("validation did not go through the model launcher: %q", logged)
	}
	left, _ := filepath.Glob(filepath.Join(home, "tmp", "run.*"))
	if len(left) != 0 {
		t.Fatalf("temp tree left behind: %v", left)
	}
}

func TestValidationFailsClosedWithoutAModelUser(t *testing.T) {
	dir, c := validationFixture(t)
	if _, err := (*Validator)(nil).ValidateCandidate(context.Background(), dir, c, [][]string{{"true"}}); err == nil {
		t.Fatal("nil validator ran the commands")
	}
	for _, name := range []string{"", " ", "root", "no-such-user-xyz"} {
		if _, err := NewValidator(name, nil); err == nil {
			t.Fatalf("validator built for user %q", name)
		}
	}
	testValidator(t)
	if _, err := NewValidator(modelruntest.User, []string{"relative/bin"}); err == nil {
		t.Fatal("relative toolchain dir accepted")
	}
	if _, err := NewValidator(modelruntest.User, []string{"/opt/a:/opt/b"}); err == nil {
		t.Fatal("toolchain dir with a path separator accepted")
	}
}

func TestValidationPathIsTheFixedModelPathPlusConfiguredToolchain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix fixture")
	}
	t.Setenv("PATH", "/home/box/bin:"+os.Getenv("PATH"))
	dir, c := validationFixture(t)
	v, _, _ := testValidator(t, "/opt/go/bin")
	got, err := v.ValidateCandidate(context.Background(), dir, c, [][]string{{"sh", "-c", "printf %s \"$PATH\""}})
	if err != nil || got[0].Output != "/opt/go/bin:/usr/local/bin:/usr/bin:/bin" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestValidationTimeoutIsPerCommandAndRecorded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix fixture")
	}
	old := commandTimeout
	commandTimeout = 1 * time.Second
	defer func() { commandTimeout = old }()
	dir, c := validationFixture(t)
	v, _, _ := testValidator(t)
	start := time.Now()
	got, err := v.ValidateCandidate(context.Background(), dir, c, [][]string{{"sh", "-c", "sleep 30"}, {"sh", "-c", "echo after"}})
	if err != nil || len(got) != 2 || !got[0].TimedOut || got[0].Passed || !got[1].Passed || got[1].Output != "after\n" {
		t.Fatalf("%+v %v", got, err)
	}
	if time.Since(start) > 15*time.Second {
		t.Fatal("timeout not enforced")
	}
}
func TestValidationCancelsProcessTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix fixture")
	}
	dir, c := validationFixture(t)
	v, _, _ := testValidator(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	got, err := v.ValidateCandidate(ctx, dir, c, [][]string{{"sh", "-c", "sleep 30 & wait"}})
	if err == nil && (len(got) != 1 || !got[0].TimedOut || got[0].Passed) {
		t.Fatalf("%+v %v", got, err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("child process survived cancellation")
	}
}

func TestValidationPersistsBoundedUTF8Output(t *testing.T) {
	dir, c := validationFixture(t)
	v, _, _ := testValidator(t)
	got, err := v.ValidateCandidate(context.Background(), dir, c, [][]string{{"python3", "-c", "import sys;sys.stdout.buffer.write(b'x'*16383+b'\\xe2\\x82\\xac'+b'\\xff'*100)"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var saved []CommandResult
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved[0].Output) > MaxValidationOutput || !utf8.ValidString(got[0].Output) || !saved[0].Truncated {
		t.Fatalf("unreplayable output len=%d", len(saved[0].Output))
	}
}

func TestValidationDoesNotInheritHostGoCaches(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix fixture")
	}
	t.Setenv("GOMODCACHE", "/host/mod")
	t.Setenv("GOPATH", "/host/path")
	t.Setenv("GOCACHE", "/host/x")
	dir, c := validationFixture(t)
	v, _, _ := testValidator(t)
	got, err := v.ValidateCandidate(context.Background(), dir, c, [][]string{{"sh", "-c", "printf '%s|%s|%s|%s' \"$GOCACHE\" \"$GOMODCACHE\" \"${GOPATH-unset}\" \"$TMPDIR\""}})
	if err != nil || len(got) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	parts := strings.Split(got[0].Output, "|")
	if len(parts) != 4 || parts[2] != "unset" || parts[3] == "" || !strings.HasPrefix(parts[0], parts[3]+"/") || !strings.HasPrefix(parts[1], parts[3]+"/") || parts[0] == parts[1] {
		t.Fatalf("host caches inherited or caches not private: %q", got[0].Output)
	}
}
func TestValidationTimeoutNotOverridable(t *testing.T) {
	t.Setenv("SUMMA42_VALIDATION_TIMEOUT", "1ms")
	dir, c := validationFixture(t)
	v, _, _ := testValidator(t)
	got, err := v.ValidateCandidate(context.Background(), dir, c, [][]string{{"sh", "-c", "sleep 0.3; echo ok"}})
	if err != nil || len(got) != 1 || got[0].TimedOut || !got[0].Passed {
		t.Fatalf("timeout override honored: %+v %v", got, err)
	}
}

func TestValidationReplyMustBeExactlyOneDocument(t *testing.T) {
	commands := [][]string{{"true"}}
	one := `{"results":[{"exit":0,"timed_out":false,"ms":1,"truncated":false,"output_b64":""}]}`
	for name, reply := range map[string]string{
		"trailing document": one + "\n" + one,
		"forged prefix":     `{"results":[{"exit":0,"timed_out":false,"ms":1,"truncated":false,"output_b64":""}]}{"x":1}`,
		"wrong count":       `{"results":[]}`,
		"unknown field":     `{"results":[{"exit":0,"timed_out":false,"ms":1,"truncated":false,"output_b64":"","extra":1}]}`,
		"bad base64":        `{"results":[{"exit":0,"timed_out":false,"ms":1,"truncated":false,"output_b64":"!!"}]}`,
		"not json":          `oops`,
	} {
		buf := &boundedBuffer{limit: 1 << 20}
		buf.Write([]byte(reply))
		if _, err := parseValidationReply(buf, commands, time.Second); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	buf := &boundedBuffer{limit: 1 << 20}
	buf.Write([]byte(one + "\n"))
	if got, err := parseValidationReply(buf, commands, time.Second); err != nil || len(got) != 1 || !got[0].Passed {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestValidationQuotesArgvLiterally(t *testing.T) {
	dir, c := validationFixture(t)
	v, _, _ := testValidator(t)
	got, err := v.ValidateCandidate(context.Background(), dir, c, [][]string{{"printf", "%s", "it's \"a\" $HOME `x`\nline"}})
	if err != nil || got[0].Output != "it's \"a\" $HOME `x`\nline" {
		t.Fatalf("%+v %v", got, err)
	}
}
