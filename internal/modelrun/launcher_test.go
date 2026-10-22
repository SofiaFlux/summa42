package modelrun_test

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/modelrun"
	"github.com/SofiaFlux/summa42/internal/modelrun/modelruntest"
)

// sudo can print warnings (for example an unresolvable host name, common on
// WSL) before the launcher's prologue runs. They must not hide the marker.
func TestMarkerIsFoundAfterLeadingSudoLines(t *testing.T) {
	body := "echo 'sudo: unable to resolve host box: Name or service not known' >&2\n" +
		"while [ \"$1\" != -- ]; do shift; done\nshift\nexec setsid -w \"$@\"\n"
	home, sudoLog, cleanup, err := modelruntest.InstallScript(body)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)

	pidFile := filepath.Join(t.TempDir(), "pid")
	bin := script(t, "echo $$ > "+pidFile+"\necho model-says-hi >&2\nwhile :; do sleep 1; done\n")
	cmd, err := modelrun.Command(context.Background(), modelrun.Spec{RunAs: fakeUser(home), Binary: bin, Timeout: time.Hour, WaitDelay: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, pidFile)
	time.Sleep(200 * time.Millisecond)
	cmd.Cancel()
	_ = cmd.Wait()
	if pidFileAlive(t, pidFile) {
		t.Fatal("the pgid marker behind a sudo warning was missed: the model survived the cancel")
	}
	got := stderr.String()
	if strings.Contains(got, "__S42_PGID") {
		t.Fatalf("marker leaked: %q", got)
	}
	if !strings.Contains(got, "sudo: unable to resolve host box") || !strings.Contains(got, "model-says-hi") {
		t.Fatalf("sudo's line or the model's output was dropped: %q", got)
	}
	if strings.Contains(logged.String(), "no process-group marker") {
		t.Fatalf("warned although the marker arrived: %q", logged.String())
	}
	if data, _ := os.ReadFile(sudoLog); !strings.Contains(string(data), "/bin/kill -TERM -- -") {
		t.Fatalf("no model-side kill: %q", data)
	}
}

func TestMissingMarkerLogsAWarning(t *testing.T) {
	// A launcher that never runs the prologue.
	home, _, cleanup, err := modelruntest.InstallScript("exec /bin/true\n")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	cmd, err := modelrun.Command(context.Background(), modelrun.Spec{RunAs: fakeUser(home), Binary: "/bin/true", Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged.String(), "no process-group marker") {
		t.Fatalf("no warning when the marker never arrived: %q", logged.String())
	}
}

func TestOutputAndCombinedOutputAreRefused(t *testing.T) {
	home, _, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	cmd, err := modelrun.Command(context.Background(), modelrun.Spec{RunAs: fakeUser(home), Binary: "/bin/true", Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.Output(); err == nil || out != nil {
		t.Fatalf("Output ran: %q %v", out, err)
	}
	if out, err := cmd.CombinedOutput(); err == nil || out != nil {
		t.Fatalf("CombinedOutput ran: %q %v", out, err)
	}
	if cmd.Process != nil {
		t.Fatal("a refused Output still started the process")
	}
}
