package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const draftedProfile = `{"name":"newcli","binaries":["newcli"],"args":["--print","--plan"],"model_required":false,"read_only":["--plan"],"no_tools":false,"input":{"format":"stdin"},"output":{"format":"text"}}`

func adoptFixture(t *testing.T) (home, pending string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("SUMMA42_HOME", home)
	pending = filepath.Join(home, "model-profiles", "pending")
	if err := os.MkdirAll(pending, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pending, "newcli.json"), []byte(draftedProfile), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, pending
}

func TestAdoptNeedsAProbeOfTheExactBytes(t *testing.T) {
	home, pending := adoptFixture(t)
	adopt := func() error {
		return runModelProfileAdopt(context.Background(), []string{"--name=newcli", "--yes"})
	}
	if err := adopt(); err == nil || !strings.Contains(err.Error(), "model-probe") {
		t.Fatalf("an unprobed profile was adopted: %v", err)
	}
	if err := stampProbe(pending, "newcli", "m"); err != nil {
		t.Fatal(err)
	}
	// The profile is edited after its probe: the stamp no longer matches.
	edited := strings.Replace(draftedProfile, `"--print"`, `"--quiet"`, 1)
	if err := os.WriteFile(filepath.Join(pending, "newcli.json"), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := adopt(); err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("a profile edited after its probe was adopted: %v", err)
	}
	if err := stampProbe(pending, "newcli", "m"); err != nil {
		t.Fatal(err)
	}
	if err := adopt(); err != nil {
		t.Fatalf("a probed profile was refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "model-profiles", "newcli.json")); err != nil {
		t.Fatalf("adopted profile is not active: %v", err)
	}
	if _, err := os.Stat(stampPath(pending, "newcli")); err == nil {
		t.Error("the stamp outlived the adoption")
	}
}

func TestBuiltinProfilesNeedNoStamp(t *testing.T) {
	if err := stampProbe(t.TempDir(), "claude", "m"); err != nil {
		t.Fatalf("a built-in profile has no file to stamp: %v", err)
	}
}
