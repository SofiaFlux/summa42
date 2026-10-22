package modelrun_test

import (
	"bytes"
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/modelrun"
	"github.com/SofiaFlux/summa42/internal/modelrun/modelruntest"
)

func fakeUser(home string) *user.User {
	return &user.User{Uid: "4242", Username: modelruntest.User, HomeDir: home}
}

func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bin.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSecretsTravelInEnvironmentNotArgv(t *testing.T) {
	home, sudoLog, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	t.Setenv("OPENAI_API_KEY", "sk-very-secret")
	t.Setenv("GITHUB_TOKEN", "ghp_boxtoken")
	bin := script(t, `printf '%s|%s|%s|%s' "$OPENAI_API_KEY" "$GITHUB_TOKEN" "$HOME" "${SUDO_USER-unset}"`)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd, err := modelrun.Command(ctx, modelrun.Spec{RunAs: fakeUser(home), Binary: bin, Allowlist: []string{"OPENAI_API_KEY", "MISSING"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range cmd.Args {
		if strings.Contains(a, "sk-very-secret") || strings.Contains(a, "ghp_") {
			t.Fatalf("secret on argv: %q", a)
		}
	}
	out, err := runOutput(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), "sk-very-secret||"+home+"|unset"; got != want {
		t.Fatalf("model saw %q, want %q", got, want)
	}
	logged, _ := os.ReadFile(sudoLog)
	if strings.Contains(string(logged), "sk-very-secret") || !strings.Contains(string(logged), "--preserve-env=OPENAI_API_KEY ") {
		t.Fatalf("sudo saw %q", logged)
	}
}

func TestDeadlineBecomesTimeoutOnTheModelSide(t *testing.T) {
	home, sudoLog, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd, err := modelrun.Command(ctx, modelrun.Spec{RunAs: fakeUser(home), Binary: script(t, "exit 0")})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	logged, _ := os.ReadFile(sudoLog)
	if !strings.Contains(string(logged), "/usr/bin/timeout --kill-after=15s 90s /bin/sh -c") {
		t.Fatalf("launch %q has no timeout wrapper derived from the context deadline", logged)
	}
	if cmd.WaitDelay <= 0 {
		t.Fatal("WaitDelay unset")
	}
}

// The real sudo cannot be signalled reliably across the uid boundary. A launcher
// that ignores SIGTERM and leaves a child holding the pipes must still be
// abandoned by WaitDelay rather than blocking the Box until the model exits.
func TestLauncherThatIgnoresSignalsIsBoundedByWaitDelay(t *testing.T) {
	home, _, cleanup, err := modelruntest.InstallScript("trap '' TERM INT HUP\nsleep 30 &\nwait\n")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	cmd, err := modelrun.Command(ctx, modelrun.Spec{RunAs: fakeUser(home), Binary: script(t, "exit 0"), WaitDelay: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	runErr := cmd.Run()
	if runErr == nil {
		t.Fatal("expected an error")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("Run blocked %s on a launcher that ignores signals", elapsed)
	}
}

func TestCommandRefusesBadInput(t *testing.T) {
	home, _, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ok := context.Background()
	if _, err := modelrun.Command(ok, modelrun.Spec{RunAs: fakeUser(home), Binary: "relative/bin", Timeout: time.Second}); err == nil {
		t.Fatal("relative binary accepted")
	}
	if _, err := modelrun.Command(ok, modelrun.Spec{RunAs: fakeUser(home), Binary: "/bin/true"}); err == nil {
		t.Fatal("call without a deadline or timeout accepted")
	}
	if _, err := modelrun.Command(ok, modelrun.Spec{RunAs: fakeUser(home), Binary: "/bin/true", Timeout: time.Second, Allowlist: []string{"A B"}}); err == nil {
		t.Fatal("bad env name accepted")
	}
	if _, err := modelrun.Command(ok, modelrun.Spec{Binary: "/bin/true", Timeout: time.Second}); err == nil {
		t.Fatal("unresolved user accepted")
	}
}

func TestResolveRunAsUserFailsClosed(t *testing.T) {
	_, _, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	self, _ := user.Current()
	for _, name := range []string{"", " ", "root", self.Username, "no-such-user-xyz"} {
		if _, err := modelrun.ResolveRunAsUser(name); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	if _, err := modelrun.ResolveRunAsUser(modelruntest.User); err != nil {
		t.Fatal(err)
	}
}

func pidFileAlive(t *testing.T, path string) (alive bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	// A killed process is a zombie until its parent reaps it; give that a moment.
	for i := 0; i < 100; i++ {
		if syscall.Kill(pid, 0) != nil {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if raw, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(raw)) != "" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

// A fake sudo that escapes the Box's process group, like the real setuid sudo
// whose signals the Box uid cannot deliver. Only the model-side kill can stop it.
const escapingSudo = "while [ \"$1\" != -- ]; do shift; done\nshift\nexec setsid -w \"$@\"\n"

func TestEarlyCancelKillsTheModelSideGroupThroughSudo(t *testing.T) {
	home, sudoLog, cleanup, err := modelruntest.InstallScript(escapingSudo)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	pidFile := filepath.Join(t.TempDir(), "pid")
	bin := script(t, "echo $$ > "+pidFile+"\nwhile :; do sleep 1; done\n")
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	cmd, err := modelrun.Command(ctx, modelrun.Spec{RunAs: fakeUser(home), Binary: bin, WaitDelay: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, pidFile)
	started := time.Now()
	cmd.Cancel()
	_ = cmd.Wait()
	if elapsed := time.Since(started); elapsed > 8*time.Second {
		t.Fatalf("early cancel took %s", elapsed)
	}
	if pidFileAlive(t, pidFile) {
		t.Fatal("model process survived an early cancel")
	}
	if strings.Contains(stderr.String(), "__S42_PGID") {
		t.Fatalf("side-channel marker leaked to the caller: %q", stderr.String())
	}
	logged, _ := os.ReadFile(sudoLog)
	if !strings.Contains(string(logged), "-u "+modelruntest.User+" -- /bin/kill -TERM -- -") {
		t.Fatalf("no model-side kill through sudo: %q", logged)
	}
}

func TestEarlyCancelEscalatesToKillAfterTheGrace(t *testing.T) {
	home, sudoLog, cleanup, err := modelruntest.InstallScript(escapingSudo)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	defer modelrun.SetKillGraceForTest(300 * time.Millisecond)()
	pidFile := filepath.Join(t.TempDir(), "pid")
	bin := script(t, "trap '' TERM\necho $$ > "+pidFile+"\nwhile :; do sleep 1; done\n")
	cmd, err := modelrun.Command(context.Background(), modelrun.Spec{RunAs: fakeUser(home), Binary: bin, Timeout: time.Hour, WaitDelay: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, pidFile)
	started := time.Now()
	cmd.Cancel()
	_ = cmd.Wait()
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("escalation took %s", elapsed)
	}
	if pidFileAlive(t, pidFile) {
		t.Fatal("a model that ignores TERM survived the KILL escalation")
	}
	logged, _ := os.ReadFile(sudoLog)
	if !strings.Contains(string(logged), "/bin/kill -KILL -- -") {
		t.Fatalf("no KILL escalation: %q", logged)
	}
}

func TestScratchGivesAPrivatePerRunDirectoryThatIsRemoved(t *testing.T) {
	home, _, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	bin := script(t, "touch inside\nprintf '%s|%s' \"$PWD\" \"$TMPDIR\"")
	run := func() string {
		cmd, err := modelrun.Command(context.Background(), modelrun.Spec{RunAs: fakeUser(home), Binary: bin, Timeout: 20 * time.Second, Scratch: true})
		if err != nil {
			t.Fatal(err)
		}
		out, err := runOutput(cmd)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	first, second := run(), run()
	for _, got := range []string{first, second} {
		parts := strings.Split(got, "|")
		if len(parts) != 2 || parts[0] != parts[1] || !strings.HasPrefix(parts[0], filepath.Join(home, "tmp", "run.")) {
			t.Fatalf("scratch dir is not under the model tmp: %q", got)
		}
		if _, err := os.Stat(parts[0]); !os.IsNotExist(err) {
			t.Fatalf("scratch dir %s was not removed (%v)", parts[0], err)
		}
	}
	if first == second {
		t.Fatal("two runs shared a scratch dir")
	}
	if _, err := modelrun.Command(context.Background(), modelrun.Spec{RunAs: fakeUser(home), Binary: bin, Timeout: time.Second, Scratch: true, Dir: "/tmp"}); err == nil {
		t.Fatal("Scratch together with Dir accepted")
	}
}

func TestScratchFailsClosedWhenTheModelTmpIsMissing(t *testing.T) {
	home, _, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	os.RemoveAll(filepath.Join(home, "tmp"))
	cmd, err := modelrun.Command(context.Background(), modelrun.Spec{RunAs: fakeUser(home), Binary: "/bin/true", Timeout: 20 * time.Second, Scratch: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Run(); err == nil {
		t.Fatal("ran without a scratch directory")
	}
}

// The wrapper's own shell variables must not shadow forwarded names, and a name
// in the reserved namespace is refused.
func TestLowercaseAllowlistNamesKeepTheBoxValue(t *testing.T) {
	home, _, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	t.Setenv("dir", "box-dir")
	t.Setenv("home", "box-home")
	t.Setenv("path", "box-path")
	t.Setenv("lang", "box-lang")
	bin := script(t, `printf '%s|%s|%s|%s' "$dir" "$home" "$path" "$lang"`)
	cmd, err := modelrun.Command(context.Background(), modelrun.Spec{RunAs: fakeUser(home), Binary: bin, Timeout: 20 * time.Second, Allowlist: []string{"dir", "home", "path", "lang"}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := runOutput(cmd)
	if err != nil || string(out) != "box-dir|box-home|box-path|box-lang" {
		t.Fatalf("%q %v", out, err)
	}
	for _, name := range []string{"__s42_dir", "__s42_anything"} {
		if _, err := modelrun.Command(context.Background(), modelrun.Spec{RunAs: fakeUser(home), Binary: "/bin/true", Timeout: time.Second, Allowlist: []string{name}}); err == nil {
			t.Fatalf("reserved name %q accepted in Allowlist", name)
		}
		if _, err := modelrun.Command(context.Background(), modelrun.Spec{RunAs: fakeUser(home), Binary: "/bin/true", Timeout: time.Second, Env: map[string]string{name: "x"}}); err == nil {
			t.Fatalf("reserved name %q accepted in Env", name)
		}
	}
}

func TestSpecTimeoutIsTheMinimumOfTheTwoLimits(t *testing.T) {
	home, sudoLog, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	cmd, err := modelrun.Command(ctx, modelrun.Spec{RunAs: fakeUser(home), Binary: script(t, "exit 0"), Timeout: 90 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	logged, _ := os.ReadFile(sudoLog)
	if !strings.Contains(string(logged), "--kill-after=15s 90s ") {
		t.Fatalf("Spec.Timeout ignored under a later deadline: %q", logged)
	}
}

func TestModelGetsTheFixedPathNotTheBoxPath(t *testing.T) {
	home, _, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	t.Setenv("PATH", "/home/box/bin:relative:"+os.Getenv("PATH"))
	t.Setenv("LANG", "xx_XX.box")
	bin := script(t, `printf '%s|%s' "$PATH" "$LANG"`)
	run := func(env map[string]string) string {
		cmd, err := modelrun.Command(context.Background(), modelrun.Spec{RunAs: fakeUser(home), Binary: bin, Timeout: 20 * time.Second, Env: env})
		if err != nil {
			t.Fatal(err)
		}
		out, err := runOutput(cmd)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	if got := run(nil); got != "/usr/local/bin:/usr/bin:/bin|C.UTF-8" {
		t.Fatalf("model inherited the Box PATH or LANG: %q", got)
	}
	if got := run(map[string]string{"PATH": "/opt/go/bin:/usr/bin"}); got != "/opt/go/bin:/usr/bin|C.UTF-8" {
		t.Fatalf("explicit PATH ignored: %q", got)
	}
}

func resetProbe(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { modelrun.RequireUnreadable() })
}

func TestProbeRefusesWhenTheModelUserCanReadASecret(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	home, _, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	resetProbe(t)
	secrets := t.TempDir()
	db := filepath.Join(secrets, "state.db")
	keys := filepath.Join(secrets, "keys")
	os.WriteFile(db, []byte("x"), 0o600)
	os.Mkdir(keys, 0o700)
	spec := modelrun.Spec{RunAs: fakeUser(home), Binary: "/bin/true", Timeout: 20 * time.Second}

	modelrun.RequireUnreadable(db, keys) // the fake sudo keeps the test's uid, so these are readable
	if _, err := modelrun.Command(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "can read") {
		t.Fatalf("readable secret accepted: %v", err)
	}
	os.Chmod(db, 0)
	os.Chmod(keys, 0)
	defer os.Chmod(keys, 0o700)
	modelrun.RequireUnreadable(db, keys)
	if _, err := modelrun.Command(context.Background(), spec); err != nil {
		t.Fatalf("unreadable secrets refused: %v", err)
	}
	os.Chmod(db, 0o600) // readable again: the verdict is cached per user, one probe only
	if _, err := modelrun.Command(context.Background(), spec); err != nil {
		t.Fatalf("probe ran twice: %v", err)
	}
	modelrun.RequireUnreadable(filepath.Join(secrets, "missing"))
	if _, err := modelrun.Command(context.Background(), spec); err == nil {
		t.Fatal("a path that cannot be checked counted as unreadable")
	}
}

func TestProbeNeedsAWorkingLauncher(t *testing.T) {
	home, _, cleanup, err := modelruntest.InstallScript("exit 1\n")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	resetProbe(t)
	modelrun.RequireUnreadable("/etc/shadow")
	if _, err := modelrun.Command(context.Background(), modelrun.Spec{RunAs: fakeUser(home), Binary: "/bin/true", Timeout: 20 * time.Second}); err == nil {
		t.Fatal("a launcher that fails every command passed the probe")
	}
}

// runOutput is Output for a modelrun.Cmd, which refuses the promoted one.
func runOutput(cmd *modelrun.Cmd) ([]byte, error) {
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return out.Bytes(), err
}
