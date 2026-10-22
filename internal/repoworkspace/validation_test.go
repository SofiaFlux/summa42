package repoworkspace

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/SofiaFlux/summa42/internal/modelrun/modelruntest"
)

// testValidator runs validation through the fake sudo of modelruntest: the same
// launcher as production, minus the privilege drop. It returns the fake model
// home and the sudo log.
//
// The returned home is the validation user's (where scratch directories
// appear). The model home is made unreadable, as the isolation probe requires
// of the validation user.
func testValidator(t *testing.T, toolchain ...string) (v *Validator, home, sudoLog string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	_, sudoLog, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	if err := modelruntest.LockModelHome(); err != nil {
		t.Fatal(err)
	}
	v, err = NewValidator(modelruntest.ValidationUser, modelruntest.User, toolchain, 0, filepath.Join(t.TempDir(), "validation.lock"))
	if err != nil {
		t.Fatal(err)
	}
	return v, modelruntest.ValidationHome(), sudoLog
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
	if !strings.Contains(string(logged), "-u "+modelruntest.ValidationUser+" -- ") {
		t.Fatalf("validation did not go through the launcher as the validation user: %q", logged)
	}
	left, _ := filepath.Glob(filepath.Join(home, "tmp", "run.*"))
	if len(left) != 0 {
		t.Fatalf("temp tree left behind: %v", left)
	}
}

func TestValidationFailsClosedWithoutADistinctValidationUser(t *testing.T) {
	dir, c := validationFixture(t)
	if _, err := (*Validator)(nil).ValidateCandidate(context.Background(), dir, c, [][]string{{"true"}}); err == nil {
		t.Fatal("nil validator ran the commands")
	}
	testValidator(t) // installs the fake users
	self, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", " ", "root", "no-such-user-xyz", self.Username, modelruntest.User} {
		if _, err := NewValidator(name, modelruntest.User, nil, 0, filepath.Join(t.TempDir(), "validation.lock")); err == nil {
			t.Fatalf("validator built for validation user %q", name)
		}
	}
	for _, model := range []string{"", "root", "no-such-user-xyz", self.Username} {
		if _, err := NewValidator(modelruntest.ValidationUser, model, nil, 0, filepath.Join(t.TempDir(), "validation.lock")); err == nil {
			t.Fatalf("validator built for model user %q", model)
		}
	}
	if _, err := NewValidator(modelruntest.ValidationUser, modelruntest.User, []string{"relative/bin"}, 0, filepath.Join(t.TempDir(), "validation.lock")); err == nil {
		t.Fatal("relative toolchain dir accepted")
	}
	if _, err := NewValidator(modelruntest.ValidationUser, modelruntest.User, []string{"/opt/a:/opt/b"}, 0, filepath.Join(t.TempDir(), "validation.lock")); err == nil {
		t.Fatal("toolchain dir with a path separator accepted")
	}
	for _, d := range []time.Duration{-time.Second, time.Millisecond, 25 * time.Hour} {
		if _, err := NewValidator(modelruntest.ValidationUser, modelruntest.User, nil, d, filepath.Join(t.TempDir(), "validation.lock")); err == nil {
			t.Fatalf("command timeout %s accepted", d)
		}
	}
}

func TestValidationTimeoutDefaultsToTenMinutesAndIsConfigurable(t *testing.T) {
	v, _, _ := testValidator(t)
	if v.timeout != 10*time.Minute || DefaultCommandTimeout != 10*time.Minute {
		t.Fatalf("default per-command timeout is %s", v.timeout)
	}
	custom, err := NewValidator(modelruntest.ValidationUser, modelruntest.User, nil, 25*time.Minute, filepath.Join(t.TempDir(), "validation.lock"))
	if err != nil || custom.timeout != 25*time.Minute {
		t.Fatalf("%v %v", custom, err)
	}
}

// Candidate code must not run where the model's logins live: the validation
// user must not be able to read the model user's home.
func TestValidationRefusesWhenTheValidationUserCanReadTheModelHome(t *testing.T) {
	dir, c := validationFixture(t)
	v, _, _ := testValidator(t)
	modelHome := v.modelHome
	if err := os.Chmod(modelHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := v.ValidateCandidate(context.Background(), dir, c, [][]string{{"true"}}); err == nil || !strings.Contains(err.Error(), "can read") {
		t.Fatalf("validation ran with a readable model home: %v", err)
	}
}

// Whatever the outcome, every process of the validation user is killed and
// its scratch removed afterwards, through sudo as that user.
func TestValidationSweepsTheValidationUserAfterEveryRun(t *testing.T) {
	dir, c := validationFixture(t)
	v, home, sudoLog := testValidator(t)
	stale := filepath.Join(home, "tmp", "run.stale")
	if err := os.MkdirAll(filepath.Join(stale, "ro"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(stale, "ro"), 0o500)
	for name, commands := range map[string][][]string{"pass": {{"true"}}, "fail": {{"sh", "-c", "exit 3"}}} {
		before, _ := os.ReadFile(sudoLog)
		if _, err := v.ValidateCandidate(context.Background(), dir, c, commands); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		after, _ := os.ReadFile(sudoLog)
		added := string(after[len(before):])
		if !strings.Contains(added, "-u "+modelruntest.ValidationUser+" -- ") || !strings.Contains(added, "-KILL -1") {
			t.Fatalf("%s: no kill-all sweep as the validation user: %q", name, added)
		}
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Fatalf("%s: scratch dirs not removed (%v)", name, err)
		}
	}
	// A cancelled run is swept too.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before, _ := os.ReadFile(sudoLog)
	if _, err := v.ValidateCandidate(ctx, dir, c, [][]string{{"true"}}); err == nil {
		t.Fatal("cancelled context ran")
	}
	after, _ := os.ReadFile(sudoLog)
	if !strings.Contains(string(after[len(before):]), "-KILL -1") {
		t.Fatal("a cancelled run was not swept")
	}
}

func TestValidationUsesPerRunGoCacheUnderScratchDir(t *testing.T) {
	dir, c := validationFixture(t)
	v, home, _ := testValidator(t)
	got, err := v.ValidateCandidate(context.Background(), dir, c, [][]string{{"sh", "-c", "printf %s \"$GOCACHE\""}})
	if err != nil || len(got) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	if !strings.HasPrefix(got[0].Output, filepath.Join(home, "tmp", "run.")) || !strings.HasSuffix(got[0].Output, filepath.Join("go-cache")) {
		t.Fatalf("GOCACHE is not under the run's scratch dir: %q", got[0].Output)
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
	dir, c := validationFixture(t)
	v, _, _ := testValidator(t)
	v.timeout = 1 * time.Second
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
	if len(parts) != 4 || parts[2] != "unset" || parts[3] == "" || parts[0] == "/host/x" || !strings.HasPrefix(parts[1], parts[3]+"/") || parts[0] == parts[1] {
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
		"trailing closers":  one + "\n}}]]",
	} {
		buf := &boundedBuffer{limit: 1 << 20}
		buf.Write([]byte(reply))
		if _, err := parseValidationReply(buf, commands, time.Second); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	empty := &boundedBuffer{limit: 1 << 20}
	empty.Write([]byte(`{"results":[]}}}]]`))
	if _, err := parseValidationReply(empty, nil, time.Second); err == nil {
		t.Fatal("closing brackets after the document accepted")
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

// candidateWithFiles commits the given files as a candidate on top of the
// fixture's base commit.
func candidateWithFiles(t *testing.T, files map[string]string, deletes ...string) (string, Candidate) {
	t.Helper()
	cfg, dir := candidateFixture(t)
	var allowed []string
	var edits []Edit
	for name, content := range files {
		allowed = append(allowed, name)
		edits = append(edits, Edit{Path: name, Content: content})
	}
	for _, name := range deletes {
		allowed = append(allowed, name)
		edits = append(edits, Edit{Path: name, Delete: true})
	}
	c, err := PrepareCandidate(context.Background(), dir, cfg.Commit, allowed, edits)
	if err != nil {
		t.Fatal(err)
	}
	return dir, c
}

// A real compiler through the real script: the file-size limit must leave room
// for the compiler's package archives (the runtime alone is several MiB).
func TestValidationRunsARealGoTestAndVet(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("needs go on PATH and a Unix shell")
	}
	dir, c := candidateWithFiles(t, map[string]string{
		"go.mod":       "module example.invalid/tiny\n\ngo 1.20\n",
		"tiny.go":      "package tiny\n\nfunc Add(a, b int) int { return a + b }\n",
		"tiny_test.go": "package tiny\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
	}, "code.go")
	v, _, _ := testValidator(t, filepath.Dir(goBin))
	got, err := v.ValidateCandidate(context.Background(), dir, c, [][]string{{"go", "test", "-p", "1", "./...", "-count=1"}, {"go", "vet", "-p", "1", "./..."}})
	if err != nil || len(got) != 2 || !got[0].Passed || !got[1].Passed {
		t.Fatalf("a real go test/vet did not pass: %+v %v", got, err)
	}
}

func processAlive(pid string) bool {
	raw, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return false
	}
	// Field 3 is the state; a zombie is dead for our purposes.
	fields := strings.Fields(string(raw[strings.LastIndexByte(string(raw), ')')+1:]))
	return len(fields) > 0 && fields[0] != "Z"
}

// Cancelling a validation must stop the running command (which sits in its own
// process group because coreutils timeout makes one) and remove the scratch.
func TestCancelledValidationLeavesNoProcessAndNoScratch(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	dir, c := validationFixture(t)
	v, home, _ := testValidator(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := v.ValidateCandidate(ctx, dir, c, [][]string{{"sh", "-c", "echo $$ > " + pidFile + "; exec sleep 300"}})
		done <- err
	}()
	var pid string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if raw, _ := os.ReadFile(pidFile); strings.HasSuffix(string(raw), "\n") {
			pid = strings.TrimSpace(string(raw))
			break
		}
	}
	if pid == "" {
		t.Fatal("command never started")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled validation reported success")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("cancelled validation did not return")
	}
	for deadline := time.Now().Add(5 * time.Second); processAlive(pid) && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
	}
	if processAlive(pid) {
		syscall.Kill(mustAtoi(t, pid), syscall.SIGKILL)
		t.Fatal("the validation command survived the cancel")
	}
	if left, _ := filepath.Glob(filepath.Join(home, "tmp", "run.*")); len(left) != 0 {
		t.Fatalf("scratch left behind: %v", left)
	}
}

func mustAtoi(t *testing.T, s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// An in-tree .gitattributes must not hide files from, or rewrite files for,
// the validation copy.
func TestValidationArchiveIgnoresInTreeGitAttributes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix fixture")
	}
	dir, c := candidateWithFiles(t, map[string]string{
		".gitattributes": "hidden/** export-ignore\nsecret_test.go export-ignore\nstamp.txt export-subst\n",
		"hidden/h.go":    "package hidden\n",
		"secret_test.go": "package x\n",
		"stamp.txt":      "$Format:%H$\n",
	})
	v, _, _ := testValidator(t)
	got, err := v.ValidateCandidate(context.Background(), dir, c, [][]string{{"sh", "-c", "test -f hidden/h.go && test -f secret_test.go && cat stamp.txt"}})
	if err != nil || len(got) != 1 || !got[0].Passed || got[0].Output != "$Format:%H$\n" {
		t.Fatalf("export attributes changed the validated tree: %+v %v", got, err)
	}
	// The identity of the candidate is untouched by the override.
	if err := VerifyCandidate(context.Background(), dir, c); err != nil {
		t.Fatal(err)
	}
}

func TestValidationLockSerializesValidatorsAcrossProcesses(t *testing.T) {
	dir, c := validationFixture(t)
	first, _, _ := testValidator(t)
	lock := first.lockPath
	second, err := NewValidator(modelruntest.ValidationUser, modelruntest.User, nil, 0, lock)
	if err != nil {
		t.Fatal(err)
	}
	// Two Validators stand in for two Box processes: they share only the lock
	// file. Overlapping runs would collide on the busy marker (or one run's
	// sweep would kill the other's command).
	busy := filepath.Join(t.TempDir(), "busy")
	cmd := [][]string{{"sh", "-c", "mkdir " + busy + " || exit 9; sleep 1; rmdir " + busy}}
	errs := make(chan error, 2)
	for _, v := range []*Validator{first, second} {
		go func(v *Validator) {
			got, err := v.ValidateCandidate(context.Background(), dir, c, cmd)
			if err == nil && (len(got) != 1 || got[0].ExitCode != 0) {
				err = fmt.Errorf("overlapping validation: %+v", got)
			}
			errs <- err
		}(v)
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestValidatorRequiresAnAbsoluteLockPath(t *testing.T) {
	testValidator(t)
	for _, lock := range []string{"", "relative/validation.lock"} {
		if _, err := NewValidator(modelruntest.ValidationUser, modelruntest.User, nil, 0, lock); err == nil {
			t.Fatalf("lock path %q accepted", lock)
		}
	}
}
