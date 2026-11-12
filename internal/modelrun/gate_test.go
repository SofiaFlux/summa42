package modelrun_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/modelrun"
	"github.com/SofiaFlux/summa42/internal/modelrun/modelruntest"
)

// gateCommand installs the fake launcher and returns a command whose model
// writes its pid and then runs until it is killed.
func gateCommand(t *testing.T) (*modelrun.Cmd, string) {
	t.Helper()
	home, _, cleanup, err := modelruntest.InstallScript(escapingSudo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	// Whatever a test sets, the next test starts with a gate that allows.
	t.Cleanup(func() { modelrun.SetGate(func(context.Context) error { return nil }) })
	pidFile := filepath.Join(t.TempDir(), "pid")
	bin := script(t, "echo $$ > "+pidFile+"\nwhile :; do sleep 1; done\n")
	cmd, err := modelrun.Command(context.Background(), modelrun.Spec{RunAs: fakeUser(home), Binary: bin, Timeout: time.Hour, WaitDelay: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return cmd, pidFile
}

func TestLauncherWithoutGateRefused(t *testing.T) {
	cmd, pidFile := gateCommand(t)
	modelrun.SetGate(nil)
	if err := cmd.Start(); !errors.Is(err, modelrun.ErrRefused) {
		t.Fatalf("Start without a gate = %v, want ErrRefused", err)
	}
	if fileExists(pidFile) {
		t.Fatal("the model ran although no gate was registered")
	}
}

func TestLauncherRefusesWhenFrozen(t *testing.T) {
	cmd, pidFile := gateCommand(t)
	modelrun.SetGate(func(context.Context) error { return fmt.Errorf("%w: system is frozen", modelrun.ErrRefused) })
	err := cmd.Run()
	if !errors.Is(err, modelrun.ErrRefused) {
		t.Fatalf("Run while frozen = %v, want ErrRefused", err)
	}
	time.Sleep(100 * time.Millisecond)
	if fileExists(pidFile) {
		t.Fatal("the model ran while the gate refused")
	}
}

func TestLauncherRefusesWhenGateFails(t *testing.T) {
	cmd, pidFile := gateCommand(t)
	modelrun.SetGate(func(context.Context) error { return errors.New("database is unreadable") })
	if err := cmd.Start(); !errors.Is(err, modelrun.ErrRefused) {
		t.Fatalf("Start with a gate that cannot answer = %v, want ErrRefused (no answer means no)", err)
	}
	if fileExists(pidFile) {
		t.Fatal("the model ran although the gate could not answer")
	}
}

func TestFreezeKillsRunningModelWithinBound(t *testing.T) {
	defer modelrun.SetGateTimingForTest(50 * time.Millisecond)()
	defer modelrun.SetKillGraceForTest(300 * time.Millisecond)()
	cmd, pidFile := gateCommand(t)
	var frozen atomic.Bool
	modelrun.SetGate(func(context.Context) error {
		if frozen.Load() {
			return fmt.Errorf("%w: system is frozen", modelrun.ErrRefused)
		}
		return nil
	})
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, pidFile)
	time.Sleep(200 * time.Millisecond) // several checks pass while not frozen
	if !pidFileAlive2(pidFile) {
		t.Fatal("the model was stopped although the gate allowed it")
	}
	frozenAt := time.Now()
	frozen.Store(true)
	err := cmd.Wait()
	elapsed := time.Since(frozenAt)
	if !errors.Is(err, modelrun.ErrRefused) {
		t.Fatalf("Wait after a freeze = %v, want ErrRefused", err)
	}
	// One check interval (50 ms) plus the kill; far below WaitDelay (20 s).
	if elapsed > 5*time.Second {
		t.Fatalf("from freeze to the end of the model took %s", elapsed)
	}
	if pidFileAlive(t, pidFile) {
		t.Fatal("the model survived the freeze")
	}
}

func TestGateThatCannotAnswerStopsARunningModelAfterThreeChecks(t *testing.T) {
	defer modelrun.SetGateTimingForTest(50 * time.Millisecond)()
	defer modelrun.SetKillGraceForTest(300 * time.Millisecond)()
	cmd, pidFile := gateCommand(t)
	var broken atomic.Bool
	var failures atomic.Int32
	modelrun.SetGate(func(context.Context) error {
		if !broken.Load() {
			return nil
		}
		// Two failures, one answer, then failures only: the count restarts.
		if n := failures.Add(1); n == 3 {
			return nil
		}
		return errors.New("database is locked")
	})
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, pidFile)
	broken.Store(true)
	err := cmd.Wait()
	if !errors.Is(err, modelrun.ErrRefused) {
		t.Fatalf("Wait = %v, want ErrRefused", err)
	}
	if got := failures.Load(); got < 6 {
		t.Fatalf("stopped after %d checks; two failures and an answer must not stop it, three failures in a row must", got)
	}
	if pidFileAlive(t, pidFile) {
		t.Fatal("the model survived a gate that stopped answering")
	}
}

// A fake sudo like slowEscapingSudo, but the marker comes a full second late:
// longer than a cancel waits for it in these tests.
const lateMarkerSudo = "while [ \"$1\" != -- ]; do shift; done\nshift\n" +
	"case \"$1\" in */kill|kill) exec \"$@\";; esac\n" +
	"exec setsid -w sh -c 'sleep 1; exec \"$@\"' sh \"$@\"\n"

// waitBounded waits for the call, and on timeout kills the model's process
// group itself (the fake sudo keeps the test's uid) so the test cannot hang.
func waitBounded(t *testing.T, cmd *modelrun.Cmd, pidFile string, bound time.Duration) (time.Duration, error) {
	t.Helper()
	started := time.Now()
	result := make(chan error, 1)
	go func() { result <- cmd.Wait() }()
	select {
	case err := <-result:
		return time.Since(started), err
	case <-time.After(bound):
	}
	if raw, err := os.ReadFile(pidFile); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
			if pgid, err := syscall.Getpgid(pid); err == nil {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		}
	}
	<-result
	t.Fatalf("the model was still running %s after the gate refused", bound)
	return 0, nil
}

// A refusal whose first kill cannot reach the model side (its process-group
// marker is not known yet, and local signals do not cross the uid boundary)
// must not be the only attempt: the launcher keeps trying until the model is
// gone. Before the fix the watcher sent one cancel and returned, and the model
// ran on to its own timeout.
func TestFreezeBeforeTheMarkerStillKillsTheModel(t *testing.T) {
	defer modelrun.SetGateTimingForTest(50 * time.Millisecond)()
	defer modelrun.SetMarkerWaitForTest(100 * time.Millisecond)()
	defer modelrun.SetKillGraceForTest(300 * time.Millisecond)()
	home, sudoLog, cleanup, err := modelruntest.InstallScript(lateMarkerSudo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Cleanup(func() { modelrun.SetGate(func(context.Context) error { return nil }) })
	pidFile := filepath.Join(t.TempDir(), "pid")
	bin := script(t, "echo $$ > "+pidFile+"\nwhile :; do sleep 1; done\n")
	cmd, err := modelrun.Command(context.Background(), modelrun.Spec{RunAs: fakeUser(home), Binary: bin, Timeout: time.Hour, WaitDelay: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var frozen atomic.Bool
	modelrun.SetGate(func(context.Context) error {
		if frozen.Load() {
			return fmt.Errorf("%w: system is frozen", modelrun.ErrRefused)
		}
		return nil
	})
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	frozen.Store(true) // the first refusal comes before the marker
	// Far below WaitDelay (20 s): only the model-side kill can end it in time.
	elapsed, err := waitBounded(t, cmd, pidFile, 8*time.Second)
	if !errors.Is(err, modelrun.ErrRefused) {
		t.Fatalf("Wait after a freeze = %v, want ErrRefused", err)
	}
	logged, _ := os.ReadFile(sudoLog)
	if !strings.Contains(string(logged), "-u "+modelruntest.User+" -- /bin/kill -TERM -- -") {
		t.Fatalf("no model-side kill through sudo after %s: %q", elapsed, logged)
	}
	if fileExists(pidFile) && pidFileAlive(t, pidFile) {
		t.Fatal("the model survived a freeze that came before the marker")
	}
}

// A refusal arms WaitDelay as a context cancel does: when nothing ends the
// launcher (no marker ever comes and it ignores TERM), the Box abandons it
// after WaitDelay rather than at the call's own timeout.
func TestFreezeAppliesWaitDelay(t *testing.T) {
	defer modelrun.SetGateTimingForTest(50 * time.Millisecond)()
	defer modelrun.SetMarkerWaitForTest(50 * time.Millisecond)()
	// This sudo never runs the prologue, ignores TERM, and its kill does nothing.
	home, _, cleanup, err := modelruntest.InstallScript("while [ \"$1\" != -- ]; do shift; done\nshift\n" +
		"case \"$1\" in */kill|kill) exit 0;; esac\ntrap '' TERM\nwhile :; do sleep 1; done\n")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Cleanup(func() { modelrun.SetGate(func(context.Context) error { return nil }) })
	cmd, err := modelrun.Command(context.Background(), modelrun.Spec{RunAs: fakeUser(home), Binary: script(t, "exit 0\n"), Timeout: time.Hour, WaitDelay: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	var frozen atomic.Bool
	modelrun.SetGate(func(context.Context) error {
		if frozen.Load() {
			return fmt.Errorf("%w: system is frozen", modelrun.ErrRefused)
		}
		return nil
	})
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	frozen.Store(true)
	started := time.Now()
	result := make(chan error, 1)
	go func() { result <- cmd.Wait() }()
	select {
	case err := <-result:
		if !errors.Is(err, modelrun.ErrRefused) {
			t.Fatalf("Wait after a freeze = %v, want ErrRefused", err)
		}
	case <-time.After(8 * time.Second):
		_ = cmd.Process.Kill()
		<-result
		t.Fatalf("Wait did not return within %s of a freeze although WaitDelay is 500 ms", time.Since(started))
	}
}
