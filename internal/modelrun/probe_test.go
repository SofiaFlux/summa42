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

func validationUser() *user.User {
	return &user.User{Uid: "4343", Username: modelruntest.ValidationUser, HomeDir: modelruntest.ValidationHome()}
}

func TestProbeSkipsMissingOptionalPathsButRefusesMissingRequiredOnes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	home, _, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	resetProbe(t)
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	os.WriteFile(locked, []byte("x"), 0o600)
	os.Chmod(locked, 0)
	spec := modelrun.Spec{RunAs: fakeUser(home), Binary: "/bin/true", Timeout: 20 * time.Second}

	modelrun.RegisterProbe([]string{locked}, []string{filepath.Join(dir, "github_token")})
	if _, err := modelrun.Command(context.Background(), spec); err != nil {
		t.Fatalf("a missing optional path refused: %v", err)
	}
	modelrun.RegisterProbe([]string{locked, filepath.Join(dir, "owner.key")}, nil)
	if _, err := modelrun.Command(context.Background(), spec); err == nil {
		t.Fatal("a missing required path passed")
	}
	// An optional path that exists is checked like any other.
	readable := filepath.Join(dir, "gh-app.pem")
	os.WriteFile(readable, []byte("x"), 0o600)
	modelrun.RegisterProbe([]string{locked}, []string{readable})
	if _, err := modelrun.Command(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "can read") {
		t.Fatalf("a readable optional secret passed: %v", err)
	}
}

func TestProbeCoversExtraPathsAndEveryUserSeparately(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	home, sudoLog, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	resetProbe(t)
	dir := t.TempDir()
	locked := filepath.Join(dir, "state.db")
	os.WriteFile(locked, []byte("x"), 0o600)
	os.Chmod(locked, 0)
	modelrun.RequireUnreadable(locked)
	for _, u := range []*user.User{fakeUser(home), validationUser()} {
		if _, err := modelrun.Command(context.Background(), modelrun.Spec{RunAs: u, Binary: "/bin/true", Timeout: 20 * time.Second}); err != nil {
			t.Fatal(err)
		}
	}
	logged, _ := os.ReadFile(sudoLog)
	for _, name := range []string{modelruntest.User, modelruntest.ValidationUser} {
		if !strings.Contains(string(logged), "-u "+name+" -- ") || strings.Count(string(logged), "-u "+name+" -- ") < 2 {
			t.Fatalf("user %s was not probed: %q", name, logged)
		}
	}

	// The validation user may not read the model home (Spec.Unreadable).
	readableHome := t.TempDir()
	spec := modelrun.Spec{RunAs: validationUser(), Binary: "/bin/true", Timeout: 20 * time.Second, Unreadable: []string{readableHome}}
	if _, err := modelrun.Command(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "can read") {
		t.Fatalf("a readable extra path passed: %v", err)
	}
	os.Chmod(readableHome, 0)
	defer os.Chmod(readableHome, 0o700)
	modelrun.RequireUnreadable(locked)
	if _, err := modelrun.Command(context.Background(), spec); err != nil {
		t.Fatalf("an unreadable extra path refused: %v", err)
	}
	spec.Unreadable = []string{filepath.Join(dir, "no-such-home")}
	if _, err := modelrun.Command(context.Background(), spec); err == nil {
		t.Fatal("a missing extra path passed")
	}
}

func TestResolveValidationUserMustDifferFromTheModelUser(t *testing.T) {
	home, _, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	model := fakeUser(home)
	if u, err := modelrun.ResolveValidationUser(modelruntest.ValidationUser, model); err != nil || u.Username != modelruntest.ValidationUser {
		t.Fatalf("%v %v", u, err)
	}
	self, _ := user.Current()
	for _, name := range []string{"", "  ", "root", "no-such-user-xyz", modelruntest.User, self.Username} {
		if _, err := modelrun.ResolveValidationUser(name, model); err == nil {
			t.Fatalf("validation user %q accepted", name)
		}
	}
	if _, err := modelrun.ResolveValidationUser(modelruntest.ValidationUser, nil); err == nil {
		t.Fatal("validation user accepted without a model user")
	}
}

func TestSweepKillsEverythingOfTheUserAndRemovesItsScratch(t *testing.T) {
	_, sudoLog, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	vu := validationUser()
	stale := filepath.Join(vu.HomeDir, "tmp", "run.abc", "ro")
	os.MkdirAll(stale, 0o755)
	os.WriteFile(filepath.Join(stale, "f"), []byte("x"), 0o600)
	os.Chmod(stale, 0o500)
	keep := filepath.Join(vu.HomeDir, "tmp", "other")
	os.MkdirAll(keep, 0o755)
	if err := modelrun.Sweep(context.Background(), vu); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(vu.HomeDir, "tmp", "run.abc")); !os.IsNotExist(err) {
		t.Fatalf("scratch survived: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("a non-scratch dir was removed: %v", err)
	}
	logged, _ := os.ReadFile(sudoLog)
	if !strings.Contains(string(logged), "-n -u "+modelruntest.ValidationUser+" -- ") || !strings.Contains(string(logged), "-KILL -1") {
		t.Fatalf("no kill-all through sudo as the validation user: %q", logged)
	}
	if err := modelrun.Sweep(context.Background(), nil); err == nil {
		t.Fatal("sweep without a user")
	}
}
