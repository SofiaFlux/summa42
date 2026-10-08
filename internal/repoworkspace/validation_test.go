package repoworkspace

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"
	"time"
	"unicode/utf8"
)

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
	got, err := ValidateCandidate(context.Background(), dir, c, [][]string{{"printf", "%s", "literal $(echo bad)"}, {"sh", "-c", "exit 7"}})
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
	got, err := ValidateCandidate(context.Background(), dir, c, [][]string{{"sh", "-c", "printf %s ${SUMMA42_TEST_SECRET-unset}"}})
	if err != nil || got[0].Output != "unset" {
		t.Fatalf("%+v %v", got, err)
	}
}
func TestValidationRejectsCandidateMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix fixture")
	}
	dir, c := validationFixture(t)
	if _, err := ValidateCandidate(context.Background(), dir, c, [][]string{{"sh", "-c", "printf altered > code.go"}}); err == nil {
		t.Fatal("modified candidate certified")
	}
}
func TestValidationCancelsProcessTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix fixture")
	}
	dir, c := validationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	got, err := ValidateCandidate(ctx, dir, c, [][]string{{"sh", "-c", "sleep 30 & wait"}})
	if err == nil && (len(got) != 1 || !got[0].TimedOut || got[0].Passed) {
		t.Fatalf("%+v %v", got, err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("child process survived cancellation")
	}
}

func TestValidationPersistsBoundedUTF8Output(t *testing.T) {
	dir, c := validationFixture(t)
	got, err := ValidateCandidate(context.Background(), dir, c, [][]string{{"python3", "-c", "import sys;sys.stdout.buffer.write(b'x'*16383+b'\\xe2\\x82\\xac'+b'\\xff'*100)"}})
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
