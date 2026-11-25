package modelrun_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/modelrun"
	"github.com/SofiaFlux/summa42/internal/modelrun/modelruntest"
)

func reviewerUser(home string) *user.User {
	return &user.User{Uid: "4444", Username: "summa42-review-test", HomeDir: home}
}

func TestSameUserForBothRolesRefused(t *testing.T) {
	dir := t.TempDir()
	prodHome := filepath.Join(dir, "prod")
	revHome := filepath.Join(dir, "rev")
	valHome := filepath.Join(dir, "val")
	for _, p := range []string{prodHome, revHome, valHome} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	prod := &user.User{Uid: "4242", Username: "summa42-model-test", HomeDir: prodHome}
	val := &user.User{Uid: "4343", Username: "summa42-validate-test", HomeDir: valHome}
	self, _ := user.Current()

	modelrun.SetSeamsForTest("/bin/true", func(name string) (*user.User, error) {
		switch name {
		case "summa42-review-test":
			return reviewerUser(revHome), nil
		case "same-uid":
			return &user.User{Uid: prod.Uid, Username: "same-uid", HomeDir: filepath.Join(dir, "other")}, nil
		case "same-home":
			return &user.User{Uid: "4545", Username: "same-home", HomeDir: prodHome}, nil
		case "val-same-home":
			return &user.User{Uid: "4646", Username: "val-same-home", HomeDir: prodHome}, nil
		case prod.Username:
			return prod, nil
		case val.Username:
			return val, nil
		case "root":
			return &user.User{Uid: "0", Username: "root", HomeDir: "/root"}, nil
		}
		return user.Lookup(name)
	})

	u, err := modelrun.ResolveReviewerUser("summa42-review-test", prod, val)
	if err != nil || u.Username != "summa42-review-test" {
		t.Fatalf("valid reviewer rejected: %v %v", u, err)
	}
	for _, bad := range []string{"", "  ", "root", self.Username, "no-such-user-xyz", prod.Username, val.Username, "same-uid", "same-home"} {
		if _, err := modelrun.ResolveReviewerUser(bad, prod, val); err == nil {
			t.Fatalf("ResolveReviewerUser(%q) accepted despite conflict/invalidity", bad)
		}
	}
	if _, err := modelrun.ResolveValidationUser("val-same-home", prod); err == nil {
		t.Fatal("ResolveValidationUser accepted a user sharing producer's HomeDir")
	}
	if _, err := modelrun.ResolveValidationUser(val.Username, prod, reviewerUser(revHome)); err != nil {
		t.Fatalf("distinct validation user rejected: %v", err)
	}
	if _, err := modelrun.ResolveValidationUser("summa42-review-test", prod, reviewerUser(revHome)); err == nil {
		t.Fatal("ResolveValidationUser accepted reviewer user")
	}
}

func TestReviewerCannotReadProducerHome(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	home, _, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	resetProbe(t)
	t.Cleanup(func() { modelrun.RegisterRoleHomes("", "") })

	revHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(revHome, "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}

	modelrun.RegisterRoleHomes(home, revHome)

	revSpec := modelrun.Spec{RunAs: reviewerUser(revHome), Binary: "/bin/true", Timeout: 20 * time.Second}
	// Producer home is readable (0755/0700 to test uid): reviewer must refuse.
	if _, err := modelrun.Command(context.Background(), revSpec); err == nil {
		t.Fatal("reviewer launched while producer home was readable")
	}
	// Mode 0100 (--x------) fails `test -r` but allows traversal (`test -x`): must still refuse.
	if err := os.Chmod(home, 0o100); err != nil {
		t.Fatal(err)
	}
	modelrun.RegisterRoleHomes(home, revHome)
	if _, err := modelrun.Command(context.Background(), revSpec); err == nil {
		t.Fatal("reviewer launched while producer home was traversable (mode 0100)")
	}
	// Mode 0000: unreadable and non-traversable -> passes.
	if err := os.Chmod(home, 0); err != nil {
		t.Fatal(err)
	}
	modelrun.RegisterRoleHomes(home, revHome)
	if _, err := modelrun.Command(context.Background(), revSpec); err != nil {
		t.Fatalf("reviewer refused when producer home was mode 0: %v", err)
	}
}

func TestProducerCannotReadReviewerHome(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	home, _, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	resetProbe(t)
	t.Cleanup(func() { modelrun.RegisterRoleHomes("", "") })

	revHome := t.TempDir()
	modelrun.RegisterRoleHomes(home, revHome)

	prodSpec := modelrun.Spec{RunAs: fakeUser(home), Binary: "/bin/true", Timeout: 20 * time.Second}
	if _, err := modelrun.Command(context.Background(), prodSpec); err == nil {
		t.Fatal("producer launched while reviewer home was readable")
	}
	if err := os.Chmod(revHome, 0o100); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(revHome, 0o700)
	modelrun.RegisterRoleHomes(home, revHome)
	if _, err := modelrun.Command(context.Background(), prodSpec); err == nil {
		t.Fatal("producer launched while reviewer home was traversable (mode 0100)")
	}
	if err := os.Chmod(revHome, 0); err != nil {
		t.Fatal(err)
	}
	modelrun.RegisterRoleHomes(home, revHome)
	if _, err := modelrun.Command(context.Background(), prodSpec); err != nil {
		t.Fatalf("producer refused when reviewer home was mode 0: %v", err)
	}
}

func TestNothingSurvivesAModelCall(t *testing.T) {
	t.Run("pgid background child killed and scratch removed on normal exit", func(t *testing.T) {
		home, _, cleanup, err := modelruntest.Install()
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()

		childPIDFile := filepath.Join(t.TempDir(), "child.pid")
		bin := script(t, "sleep 600 </dev/null >/dev/null 2>&1 &\n"+
			"echo $! > "+childPIDFile+".tmp && mv "+childPIDFile+".tmp "+childPIDFile+"\n"+
			"printf '%s' \"$PWD\"\n")
		cmd, err := modelrun.Command(context.Background(), modelrun.Spec{
			RunAs: fakeUser(home), Binary: bin, Timeout: 20 * time.Second, Scratch: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		out, err := runOutput(cmd)
		if err != nil {
			t.Fatalf("run failed: %v", err)
		}
		scratchDir := strings.TrimSpace(string(out))
		if !strings.HasPrefix(scratchDir, filepath.Join(home, "tmp", "run.s42.")) {
			t.Fatalf("unexpected scratch dir %q", scratchDir)
		}
		if _, err := os.Stat(scratchDir); !os.IsNotExist(err) {
			t.Fatalf("scratch dir %s survived exit 0 (%v)", scratchDir, err)
		}
		waitForFile(t, childPIDFile)
		if nonZombiePIDFileAlive(t, childPIDFile) {
			t.Fatal("background child survived normal model call exit")
		}
	})

	t.Run("pgid scratch removed on cancel", func(t *testing.T) {
		home, _, cleanup, err := modelruntest.InstallScript(escapingSudo)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		defer modelrun.SetKillGraceForTest(200 * time.Millisecond)()

		scratchLog := filepath.Join(t.TempDir(), "scratch")
		pidFile := filepath.Join(t.TempDir(), "pid")
		bin := script(t, "trap '' TERM\nprintf '%s' \"$PWD\" > "+scratchLog+"\n"+atomicPIDWrite(pidFile)+"while :; do sleep 1; done\n")
		cmd, err := modelrun.Command(context.Background(), modelrun.Spec{
			RunAs: fakeUser(home), Binary: bin, Timeout: time.Hour, WaitDelay: 20 * time.Second, Scratch: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		waitForFile(t, pidFile)
		waitForFile(t, scratchLog)
		scratchDir := strings.TrimSpace(mustReadFile(t, scratchLog))
		cmd.Cancel()
		_ = cmd.Wait()

		if _, err := os.Stat(scratchDir); !os.IsNotExist(err) {
			t.Fatalf("scratch dir %s survived cancel (%v)", scratchDir, err)
		}
	})

	t.Run("cgroup cleanup and survivor detection via seam", func(t *testing.T) {
		home, sudoLog, cleanup, err := modelruntest.Install()
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()

		cgRoot := t.TempDir()
		boxCg := filepath.Join(cgRoot, "box.slice")
		if err := os.MkdirAll(boxCg, 0o755); err != nil {
			t.Fatal(err)
		}
		// Ensure cleanup of any leaked sleeper if test fails.
		var bgPID int
		t.Cleanup(func() {
			if bgPID > 0 {
				_ = syscall.Kill(bgPID, syscall.SIGKILL)
			}
		})

		// 1. Clean cgroup.kill path: cgroup dir created, cgroup.kill written "1", dir removed.
		var seenKillPath string
		defer modelrun.SetCgroupForTest(modelrun.CgroupTestConfig{
			Root:        cgRoot,
			SelfRelPath: "/box.slice",
			ValidMagic:  true,
			UseKernelFD: false, // ext4 tempdir cannot accept clone3(CLONE_INTO_CGROUP)
			OnStart: func(callDir string) {
				seenKillPath = filepath.Join(callDir, "cgroup.kill")
				_ = os.WriteFile(seenKillPath, []byte(""), 0o600)
			},
		})()

		var seenCallDir string
		cmd, err := modelrun.Command(context.Background(), modelrun.Spec{
			RunAs: fakeUser(home), Binary: "/bin/true", Timeout: 20 * time.Second, Scratch: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Run(); err != nil {
			t.Fatalf("cgroup clean run failed: %v", err)
		}
		entries, _ := os.ReadDir(boxCg)
		if len(entries) != 0 {
			t.Fatalf("cgroup dir left behind after clean run: %+v", entries)
		}
		if seenKillPath == "" {
			t.Fatal("cgroup OnStart was not called")
		}

		// 2. Fallback when cgroup.kill is absent + live survivor in cgroup.procs -> ErrSurvivorDetected.
		defer modelrun.SetCgroupForTest(modelrun.CgroupTestConfig{
			Root:        cgRoot,
			SelfRelPath: "/box.slice",
			ValidMagic:  true,
			UseKernelFD: false,
			OnStart: func(callDir string) {
				seenCallDir = callDir
				// Omit cgroup.kill so sudo kill path runs, and seed a live PID (our own) in cgroup.procs.
				_ = os.WriteFile(filepath.Join(callDir, "cgroup.procs"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
			},
		})()
		// Make fake sudo refuse/no-op on /bin/kill so our own PID isn't signalled!
		home2, sudoLog2, cleanup2, err := modelruntest.InstallScript(
			"while [ \"$1\" != -- ]; do shift; done\nshift\ncase \"$1\" in */kill|kill) exit 0;; esac\nexec \"$@\"\n",
		)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup2()
		_ = sudoLog
		cmd2, err := modelrun.Command(context.Background(), modelrun.Spec{
			RunAs: fakeUser(home2), Binary: "/bin/true", Timeout: 20 * time.Second, Scratch: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		err = cmd2.Run()
		if !errors.Is(err, modelrun.ErrSurvivorDetected) {
			t.Fatalf("expected ErrSurvivorDetected, got %v", err)
		}
		if _, statErr := os.Stat(seenCallDir); statErr != nil {
			t.Fatalf("cgroup dir %s should stay for inspection on survivor: %v", seenCallDir, statErr)
		}
		_ = os.RemoveAll(seenCallDir)
		logged, _ := os.ReadFile(sudoLog2)
		if !strings.Contains(string(logged), "/bin/kill -KILL") {
			t.Fatalf("expected sudo /bin/kill fallback when cgroup.kill missing: %q", logged)
		}

		// 3. EBUSY on cgroup mkdir falls back cleanly to pgid mode without failing the call.
		defer modelrun.SetCgroupForTest(modelrun.CgroupTestConfig{
			Root:        cgRoot,
			SelfRelPath: "/box.slice",
			ValidMagic:  true,
			MkdirErr:    syscall.EBUSY,
		})()
		cmd3, err := modelrun.Command(context.Background(), modelrun.Spec{
			RunAs: fakeUser(home2), Binary: "/bin/true", Timeout: 20 * time.Second, Scratch: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd3.Run(); err != nil {
			t.Fatalf("EBUSY cgroup fallback failed: %v", err)
		}

		// 4. Start() failure with UseCgroupFD=true on a non-cgroupfs dir rebuilds a fresh exec.Cmd and retries cleanly.
		defer modelrun.SetCgroupForTest(modelrun.CgroupTestConfig{
			Root:        cgRoot,
			SelfRelPath: "/box.slice",
			ValidMagic:  true,
			UseKernelFD: true,
		})()
		echoScript := script(t, "cat\n")
		cmd4, err := modelrun.Command(context.Background(), modelrun.Spec{
			RunAs: fakeUser(home2), Binary: echoScript, Timeout: 20 * time.Second, Scratch: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		var out4, err4 bytes.Buffer
		cmd4.Stdin = strings.NewReader("retry-ok")
		cmd4.Stdout = &out4
		cmd4.Stderr = &err4
		if err := cmd4.Run(); err != nil {
			t.Fatalf("Start() cgroup retry failed: %v (stderr=%q)", err, err4.String())
		}
		if out4.String() != "retry-ok" {
			t.Fatalf("Start() retry stdout = %q, want retry-ok", out4.String())
		}
		if entries, _ := os.ReadDir(boxCg); len(entries) != 0 {
			t.Fatalf("cgroup dir left behind after Start() retry: %+v", entries)
		}
	})

	t.Run("pgid survivor detected when child escapes pgid keeping scratch cwd", func(t *testing.T) {
		home, _, cleanup, err := modelruntest.Install()
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()

		escapedPIDFile := filepath.Join(t.TempDir(), "escaped.pid")
		// setsid detaches from the call's pgid while keeping cwd inside scratchDir.
		bin := script(t, "setsid sh -c 'echo $$ > "+escapedPIDFile+".tmp && mv "+escapedPIDFile+".tmp "+escapedPIDFile+"; sleep 600' </dev/null >/dev/null 2>&1 &\n"+
			"i=0; while [ ! -s "+escapedPIDFile+" ] && [ $i -lt 50 ]; do sleep 0.02; i=$((i+1)); done\n")
		cmd, err := modelrun.Command(context.Background(), modelrun.Spec{
			RunAs: fakeUser(home), Binary: bin, Timeout: 20 * time.Second, Scratch: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		err = cmd.Run()
		if !errors.Is(err, modelrun.ErrSurvivorDetected) {
			t.Fatalf("expected ErrSurvivorDetected for setsid escapee, got %v", err)
		}
		if nonZombiePIDFileAlive(t, escapedPIDFile) {
			t.Fatal("escaped setsid survivor stayed alive after ErrSurvivorDetected cleanup")
		}
	})
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func nonZombiePIDFileAlive(t *testing.T, path string) bool {
	t.Helper()
	pid, err := strconv.Atoi(strings.TrimSpace(mustReadFile(t, path)))
	if err != nil || pid <= 0 {
		t.Fatalf("bad pid file %s: %v", path, err)
	}
	for i := 0; i < 100; i++ {
		if syscall.Kill(pid, 0) != nil {
			return false
		}
		if runtime.GOOS == "linux" {
			if raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
				if idx := bytes.LastIndexByte(raw, ')'); idx >= 0 && len(raw) > idx+2 {
					if state := raw[idx+2]; state == 'Z' || state == 'X' {
						return false
					}
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}
