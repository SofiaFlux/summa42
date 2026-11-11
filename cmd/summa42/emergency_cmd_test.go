package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

func emergencyCollective(t *testing.T) (string, localconfig.Config) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "collective")
	t.Setenv("SUMMA42_HOME", home)
	runCLI(t, "init", "--home", home)
	cfg, err := localconfig.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	return home, cfg
}

func runCLIOutput(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCommand()
	cmd.SetArgs(args)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	err := cmd.ExecuteContext(t.Context())
	return output.String(), err
}

func emergencyRow(t *testing.T, path, query string) (string, int) {
	t.Helper()
	store, err := state.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	var text string
	var number int
	if err := store.DB().QueryRow(query).Scan(&text, &number); err != nil {
		t.Fatal(err)
	}
	return text, number
}

func TestEmergencyStatusShowsArmedNominalAndSevenDayHorizon(t *testing.T) {
	home, _ := emergencyCollective(t)
	output, err := runCLIOutput(t, "emergency", "status", "--home", home)
	if err != nil {
		t.Fatalf("emergency status: %v\n%s", err, output)
	}
	for _, want := range []string{"Break glass: ARMED", "Dead man switch: NOMINAL", "Owner horizon: 168h0m0s"} {
		if !strings.Contains(output, want) {
			t.Fatalf("status output lacks %q:\n%s", want, output)
		}
	}
}

func TestEmergencyFreezeAndResumeAreOwnerSigned(t *testing.T) {
	home, cfg := emergencyCollective(t)
	if output, err := runCLIOutput(t, "emergency", "freeze", "--home", home); err == nil {
		t.Fatalf("freeze without --reason succeeded:\n%s", output)
	}
	if output, err := runCLIOutput(t, "emergency", "freeze", "--home", home, "--reason", "stop everything"); err != nil {
		t.Fatalf("emergency freeze: %v\n%s", err, output)
	}
	state, signed := emergencyRow(t, cfg.DatabasePath, `
		SELECT c.break_glass_state, (SELECT count(*) FROM emergency_audit_events WHERE event_type = 'BREAK_GLASS_TRIGGERED' AND length(signature) = 64)
		FROM emergency_controls c WHERE c.singleton = 1`)
	if state != "FROZEN" || signed != 1 {
		t.Fatalf("break glass = %s with %d signed audit events, want FROZEN and 1", state, signed)
	}
	output, err := runCLIOutput(t, "emergency", "status", "--home", home)
	if err != nil || !strings.Contains(output, "Break glass: FROZEN") || !strings.Contains(output, "stop everything") {
		t.Fatalf("status while frozen (err %v):\n%s", err, output)
	}

	// A key that is not the Owner's cannot lift the freeze.
	strangerKey := filepath.Join(t.TempDir(), "stranger.key")
	if _, err := identity.CreateLocalEd25519(strangerKey, "owner"); err != nil {
		t.Fatal(err)
	}
	if output, err := runCLIOutput(t, "emergency", "resume", "--home", home, "--owner-key", strangerKey, "--reason", "let me in"); err == nil {
		t.Fatalf("resume with a non-Owner key succeeded:\n%s", output)
	}
	if state, _ := emergencyRow(t, cfg.DatabasePath, `SELECT break_glass_state, 0 FROM emergency_controls WHERE singleton = 1`); state != "FROZEN" {
		t.Fatalf("break glass = %s after a refused resume, want FROZEN", state)
	}

	if output, err := runCLIOutput(t, "emergency", "resume", "--home", home, "--reason", "verified"); err != nil {
		t.Fatalf("emergency resume: %v\n%s", err, output)
	}
	if state, _ := emergencyRow(t, cfg.DatabasePath, `SELECT break_glass_state, 0 FROM emergency_controls WHERE singleton = 1`); state != "RESTORED" {
		t.Fatalf("break glass = %s after resume, want RESTORED", state)
	}
	output, err = runCLIOutput(t, "--json", "emergency", "status", "--home", home)
	if err != nil || !strings.Contains(output, `"break_glass":"RESTORED"`) || strings.Contains(output, "freeze_reason") {
		t.Fatalf("JSON status after resume must not repeat the old freeze reason (err %v):\n%s", err, output)
	}
}

func TestEmergencyHeartbeatSetsTheOwnerHorizon(t *testing.T) {
	home, cfg := emergencyCollective(t)
	if output, err := runCLIOutput(t, "emergency", "heartbeat", "--home", home); err != nil {
		t.Fatalf("emergency heartbeat: %v\n%s", err, output)
	}
	if _, timeout := emergencyRow(t, cfg.DatabasePath, `SELECT state, heartbeat_timeout_seconds FROM dead_man_switch WHERE singleton = 1`); timeout != 604800 {
		t.Fatalf("horizon after a plain heartbeat = %ds, want it unchanged at 604800", timeout)
	}
	if output, err := runCLIOutput(t, "emergency", "heartbeat", "--home", home, "--timeout", "720h"); err != nil {
		t.Fatalf("emergency heartbeat --timeout: %v\n%s", err, output)
	}
	state, timeout := emergencyRow(t, cfg.DatabasePath, `SELECT state, heartbeat_timeout_seconds FROM dead_man_switch WHERE singleton = 1`)
	if state != "NOMINAL" || timeout != 2592000 {
		t.Fatalf("dead man = %s with horizon %ds, want NOMINAL and 2592000", state, timeout)
	}
	if output, err := runCLIOutput(t, "emergency", "heartbeat", "--home", home, "--timeout", "10s"); err == nil {
		t.Fatalf("a ten-second horizon was accepted:\n%s", output)
	}
}
