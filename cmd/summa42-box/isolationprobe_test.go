package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/SofiaFlux/summa42/internal/localconfig"
)

func TestIsolationProbeCoversEverySecretFileByFullPath(t *testing.T) {
	root := t.TempDir()
	summa := filepath.Join(root, ".summa42")
	keys := filepath.Join(summa, "keys")
	os.MkdirAll(keys, 0o700)
	for _, f := range []string{"owner.key", "signing.key", "other.pem"} {
		os.WriteFile(filepath.Join(keys, f), []byte("k"), 0o600)
	}
	cfg := localconfig.Config{DatabasePath: filepath.Join(summa, "state.db"), EvidencePath: filepath.Join(summa, "evidence")}
	boxHome := filepath.Join(root, "boxhome")

	required, optional := isolationProbePaths(summa, cfg, boxHome)

	for _, want := range []string{
		summa, keys, filepath.Join(keys, "owner.key"), filepath.Join(keys, "signing.key"), filepath.Join(keys, "other.pem"),
		cfg.DatabasePath, cfg.EvidencePath, filepath.Join(summa, "config.json"),
	} {
		if !slices.Contains(required, want) {
			t.Errorf("required paths lack %s: %v", want, required)
		}
	}
	for _, want := range []string{
		cfg.DatabasePath + "-wal", cfg.DatabasePath + "-shm",
		filepath.Join(summa, "github_token"), filepath.Join(summa, "gh-app.pem"), filepath.Join(summa, "discord_auth.json"),
		filepath.Join(boxHome, ".config", "gh", "hosts.yml"),
	} {
		if !slices.Contains(optional, want) {
			t.Errorf("optional paths lack %s: %v", want, optional)
		}
	}
}
