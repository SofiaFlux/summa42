//go:build realhost

// Real-host isolation checks (README "Verify isolation on a real host"). They
// need the users from scripts/setup-model-user.sh and real sudo:
//
//	S42_TOOLCHAIN=/usr/local/go1.27/bin go test -tags realhost -run RealHost -v ./internal/repoworkspace
package repoworkspace

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/modelrun"
)

const (
	realModelUser      = "summa42-model"
	realValidationUser = "summa42-validate"
)

// processesOf lists the processes of user u (empty when there are none).
func processesOf(u string) string {
	out, _ := exec.Command("ps", "-o", "pid=,args=", "-u", u).Output()
	return strings.TrimSpace(string(out))
}

// noProcessesWithin fails unless u has no processes within d.
func noProcessesWithin(t *testing.T, u string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		left := processesOf(u)
		if left == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still has processes after %s:\n%s", u, d, left)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func scratchDirsOf(t *testing.T, u string) string {
	t.Helper()
	out, err := exec.Command("sudo", "-n", "-u", u, "--", "/bin/sh", "-c", `ls -d "$HOME"/tmp/run.* 2>/dev/null || true`).Output()
	if err != nil {
		t.Fatalf("list scratch dirs of %s: %v", u, err)
	}
	return strings.TrimSpace(string(out))
}

func TestRealHostModelCallStripsTheMarker(t *testing.T) {
	runAs, err := modelrun.ResolveRunAsUser(realModelUser)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	cmd, err := modelrun.Command(context.Background(), modelrun.Spec{
		RunAs: runAs, Binary: "/bin/sh", Args: []string{"-c", "echo out; echo err >&2; id -un; pwd"},
		Scratch: true, Timeout: time.Minute,
		Unreadable: []string{filepath.Join(home, ".summa42", "keys", "owner.key"), filepath.Join(home, ".config", "gh", "hosts.yml")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 3 || lines[0] != "out" || lines[1] != realModelUser || !strings.Contains(lines[2], "/tmp/run.") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if strings.Contains(stderr.String(), "__S42_PGID") || strings.TrimSpace(stderr.String()) != "err" {
		t.Fatalf("stderr = %q (marker must be stripped, no sudo warnings)", stderr.String())
	}
	if left := scratchDirsOf(t, realModelUser); strings.Contains(left, lines[2]) {
		t.Fatalf("scratch dir %s left behind", lines[2])
	}
}

func TestRealHostCancelKillsTheModelProcesses(t *testing.T) {
	runAs, err := modelrun.ResolveRunAsUser(realModelUser)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	// A child that ignores TERM and a grandchild in the same group.
	cmd, err := modelrun.Command(ctx, modelrun.Spec{
		RunAs: runAs, Binary: "/bin/sh", Args: []string{"-c", "trap '' TERM; sleep 600 & sleep 600"},
		Scratch: true, Timeout: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	if processesOf(realModelUser) == "" {
		t.Fatal("model call is not running as the model user")
	}
	cancel()
	_ = cmd.Wait()
	noProcessesWithin(t, realModelUser, 20*time.Second)
	// ponytail: a SIGKILLed call keeps its scratch until the day-old sweep.
	t.Logf("scratch after cancel: %q", scratchDirsOf(t, realModelUser))
}

func TestRealHostProbeRefusesAReadableSecret(t *testing.T) {
	runAs, err := modelrun.ResolveRunAsUser(realModelUser)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "s42-probe-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = modelrun.Command(context.Background(), modelrun.Spec{
		RunAs: runAs, Binary: "/bin/true", Scratch: true, Timeout: time.Minute, Unreadable: []string{secret},
	})
	if err == nil || !strings.Contains(err.Error(), "can read") {
		t.Fatalf("readable secret not refused: %v", err)
	}
}

// candidateRepo is a committed one-package Go module with a passing test.
func candidateRepo(t *testing.T) (string, Candidate) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/smoke\n\ngo 1.27\n")
	git("add", ".")
	git("commit", "-q", "-m", "base")
	base := git("rev-parse", "HEAD")
	write("smoke.go", "package smoke\n\nfunc Two() int { return 2 }\n")
	write("smoke_test.go", "package smoke\n\nimport \"testing\"\n\nfunc TestTwo(t *testing.T) {\n\tif Two() != 2 {\n\t\tt.Fatal(Two())\n\t}\n}\n")
	git("add", ".")
	git("commit", "-q", "-m", "candidate")
	c, err := inspectCandidate(context.Background(), dir, base, git("rev-parse", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	return dir, c
}

func realValidator(t *testing.T, toolchain []string) *Validator {
	t.Helper()
	v, err := NewValidator(realValidationUser, realModelUser, toolchain, 10*time.Minute, filepath.Join(t.TempDir(), "validation.lock"))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestRealHostValidationRunsGoTest(t *testing.T) {
	toolchain := os.Getenv("S42_TOOLCHAIN")
	if toolchain == "" {
		t.Skip("S42_TOOLCHAIN (a Go 1.27 bin dir summa42-validate can execute) not set")
	}
	dir, c := candidateRepo(t)
	results, err := realValidator(t, []string{toolchain}).ValidateCandidate(context.Background(), dir, c, [][]string{{"go", "test", "./..."}, {"/bin/sh", "-c", "id -un"}})
	if err != nil {
		t.Fatal(err)
	}
	if !results[0].Passed {
		t.Fatalf("go test failed: %+v", results[0])
	}
	if strings.TrimSpace(results[1].Output) != realValidationUser {
		t.Fatalf("validation ran as %q", results[1].Output)
	}
	if left := processesOf(realValidationUser); left != "" {
		t.Fatalf("validation left processes:\n%s", left)
	}
	if left := scratchDirsOf(t, realValidationUser); left != "" {
		t.Fatalf("validation left scratch dirs: %s", left)
	}
}

func TestRealHostCancelledValidationLeavesNothing(t *testing.T) {
	dir, c := candidateRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Second)
		cancel()
	}()
	_, err := realValidator(t, nil).ValidateCandidate(ctx, dir, c, [][]string{{"/bin/sh", "-c", "trap '' TERM; sleep 600 & sleep 600"}})
	if err == nil {
		t.Fatal("cancelled validation reported success")
	}
	noProcessesWithin(t, realValidationUser, 20*time.Second)
	if left := scratchDirsOf(t, realValidationUser); left != "" {
		t.Fatalf("cancelled validation left scratch dirs: %s", left)
	}
}

func TestRealHostValidationCannotReadTheModelHome(t *testing.T) {
	dir, c := candidateRepo(t)
	results, err := realValidator(t, nil).ValidateCandidate(context.Background(), dir, c,
		[][]string{{"/bin/sh", "-c", "ls /var/lib/summa42-model; cat /var/lib/summa42-model/.claude/.credentials.json"}})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Passed {
		t.Fatalf("validation user read the model home: %s", results[0].Output)
	}
}

func TestRealHostSweepOfAnIdleUserIsClean(t *testing.T) {
	runAs, err := modelrun.ResolveRunAsUser(realValidationUser)
	if err != nil {
		t.Fatal(err)
	}
	if err := modelrun.Sweep(context.Background(), runAs); err != nil {
		t.Fatalf("sweep of an idle user reported: %v", err)
	}
}
