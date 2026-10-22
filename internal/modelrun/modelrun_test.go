package modelrun_test

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strings"
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
	out, err := cmd.Output()
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
	if !strings.Contains(string(logged), "-- /usr/bin/timeout --kill-after=15s 90s /bin/sh -c") {
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
