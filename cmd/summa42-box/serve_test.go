//go:build unix

package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// blockingLoop counts its starts and runs until the context ends.
func blockingLoop(name string, starts *atomic.Int32, args *[]string) serveLoop {
	return serveLoop{name: name, args: []string{"--flag-of-" + name}, run: func(ctx context.Context, got []string) error {
		if args != nil {
			*args = got
		}
		starts.Add(1)
		<-ctx.Done()
		return nil
	}}
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestServeRunsAllLoopsOnce(t *testing.T) {
	var intake, pipeline, worker atomic.Int32
	var intakeArgs []string
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		supervise(ctx, []serveLoop{
			blockingLoop("intake", &intake, &intakeArgs), blockingLoop("pipeline", &pipeline, nil), blockingLoop("worker", &worker, nil),
		}, time.Millisecond, 10*time.Millisecond, &lockedBuffer{})
		close(done)
	}()
	waitFor(t, "all three loops to start", func() bool { return intake.Load() == 1 && pipeline.Load() == 1 && worker.Load() == 1 })
	time.Sleep(30 * time.Millisecond) // several restart delays: a healthy loop is not started twice
	cancel()
	<-done
	if intake.Load() != 1 || pipeline.Load() != 1 || worker.Load() != 1 {
		t.Fatalf("starts = %d %d %d, want each loop started once", intake.Load(), pipeline.Load(), worker.Load())
	}
	if len(intakeArgs) != 1 || intakeArgs[0] != "--flag-of-intake" {
		t.Fatalf("intake got args %v, want its own section's flags", intakeArgs)
	}
}

func TestServeRestartsFailedLoop(t *testing.T) {
	var healthy, failing, panicking atomic.Int32
	log := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		supervise(ctx, []serveLoop{
			blockingLoop("healthy", &healthy, nil),
			{name: "failing", run: func(context.Context, []string) error {
				failing.Add(1)
				return errors.New("database is locked")
			}},
			{name: "panicking", run: func(context.Context, []string) error {
				panicking.Add(1)
				panic("nil map")
			}},
		}, time.Millisecond, 4*time.Millisecond, log)
		close(done)
	}()
	waitFor(t, "the failing and the panicking loop to be restarted", func() bool { return failing.Load() >= 3 && panicking.Load() >= 3 })
	cancel()
	<-done
	if healthy.Load() != 1 {
		t.Fatalf("the healthy loop was started %d times: a failing loop must not disturb it", healthy.Load())
	}
	output := log.String()
	for _, want := range []string{"failing stopped: database is locked; restarting in", "panicking stopped: panic: nil map; restarting in"} {
		if !strings.Contains(output, want) {
			t.Errorf("log does not contain %q:\n%s", want, output)
		}
	}
}

func TestServeRestartDelayGrowsToTheMaximum(t *testing.T) {
	log := &lockedBuffer{}
	var starts atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		supervise(ctx, []serveLoop{{name: "failing", run: func(context.Context, []string) error {
			starts.Add(1)
			return errors.New("boom")
		}}}, time.Millisecond, 4*time.Millisecond, log)
		close(done)
	}()
	waitFor(t, "four restarts", func() bool { return starts.Load() >= 5 })
	cancel()
	<-done
	output := log.String()
	first, second, third := strings.Index(output, "restarting in 1ms"), strings.Index(output, "restarting in 2ms"), strings.Index(output, "restarting in 4ms")
	if first < 0 || second < first || third < second || strings.Contains(output, "restarting in 8ms") {
		t.Fatalf("delays did not double from 1ms and stop at 4ms:\n%s", output)
	}
}

func TestServeStopsOnContextCancel(t *testing.T) {
	var starts atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		supervise(ctx, []serveLoop{
			blockingLoop("intake", &starts, nil),
			// A loop waiting for its restart must stop too, not start again.
			{name: "failing", run: func(context.Context, []string) error { return errors.New("boom") }},
		}, time.Hour, time.Hour, &lockedBuffer{})
		close(done)
	}()
	waitFor(t, "the loop to start", func() bool { return starts.Load() == 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop after its context was cancelled")
	}
}

func TestSecondServeRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.lock")
	release, err := lockServe(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockServe(path); !errors.Is(err, errServeRunning) {
		t.Fatalf("second lock: %v, want errServeRunning", err)
	}
	release()
	again, err := lockServe(path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	again()
}

func TestServeSplitsSectionFlags(t *testing.T) {
	own, sections, err := splitServeArgs([]string{
		"--restart-max", "30s",
		"--pipeline", "--mission", "m1", "--interval", "1m",
		"--intake", "--repository", "o/r",
		"--worker", "--workspace-root", "/w",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(own, " ") != "--restart-max 30s" ||
		strings.Join(sections["--pipeline"], " ") != "--mission m1 --interval 1m" ||
		strings.Join(sections["--intake"], " ") != "--repository o/r" ||
		strings.Join(sections["--worker"], " ") != "--workspace-root /w" {
		t.Fatalf("own = %v, sections = %v", own, sections)
	}
	loops := serveLoops(sections)
	if len(loops) != 3 || loops[0].name != "intake" || loops[1].name != "pipeline" || loops[2].name != "worker" {
		t.Fatalf("loops = %+v", loops)
	}
	if _, _, err := splitServeArgs([]string{"--restart-max", "30s"}); err == nil {
		t.Error("serve without a section was accepted")
	}
	if _, _, err := splitServeArgs([]string{"--worker", "--worker"}); err == nil {
		t.Error("a section given twice was accepted")
	}
}

func TestServeRefusesBadFlagsBeforeStartingAnything(t *testing.T) {
	// The real flag parsers of the three commands decide; nothing is started
	// and no Collective is opened, so these fail on the flags alone.
	cases := map[string][]string{
		"intake without its required flags": {"--intake"},
		"worker without a workspace":        {"--worker"},
		"worker run once":                   {"--worker", "--workspace-root", t.TempDir(), "--once"},
		"pipeline without an interval":      {"--pipeline", "--once"},
		"unknown worker flag":               {"--worker", "--workspace-root", t.TempDir(), "--no-such-flag"},
		"bad restart delays":                {"--restart-min", "1m", "--restart-max", "1s", "--worker", "--workspace-root", t.TempDir()},
		"stray argument":                    {"stray", "--worker", "--workspace-root", t.TempDir()},
		"control with a flag":               {"--control", "--port", "1"},
	}
	for name, args := range cases {
		t.Setenv("SUMMA42_HOME", filepath.Join(t.TempDir(), "no-collective-here"))
		err := runServe(context.Background(), args)
		if err == nil {
			t.Errorf("%s: serve started", name)
			continue
		}
		if strings.Contains(err.Error(), "load initialized Collective") {
			t.Errorf("%s: serve got as far as opening the Collective: %v", name, err)
		}
	}
}
