package modelrun_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
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
