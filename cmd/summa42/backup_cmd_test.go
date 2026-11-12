package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/backup"
	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/emergency"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/purpose"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

// backedUpCollective is an initialized Collective with a Mission and three
// pieces of evidence, a backup of it, and the Owner's public key as hex.
func backedUpCollective(t *testing.T) (home string, cfg localconfig.Config, directory, publicKey string, evidenceIDs []string) {
	t.Helper()
	home, cfg = emergencyCollective(t)
	ctx := context.Background()
	store, err := state.Open(ctx, cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := purpose.New(store, clock.System{}).CreateMission(ctx, "Maintain this repository safely"); err != nil {
		t.Fatal(err)
	}
	evidenceStore, err := evidence.New(store, cfg.EvidencePath, clock.System{})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{`{"n":1}`, `{"n":2}`, `{"n":3}`} {
		object, err := evidenceStore.Put(ctx, bytes.NewReader([]byte(text)), evidence.Metadata{MediaType: "application/json", Kind: "TEST"})
		if err != nil {
			t.Fatal(err)
		}
		evidenceIDs = append(evidenceIDs, string(object.ID))
	}
	store.DB().Close()
	owner, err := identity.NewLocalEd25519(filepath.Join(home, "keys", "owner.key"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	publicKey = hex.EncodeToString(owner.PublicKey())
	directory = filepath.Join(t.TempDir(), "backup")
	output, err := runCLIOutput(t, "backup", "create", directory, "--home", home)
	if err != nil {
		t.Fatalf("backup create: %v\n%s", err, output)
	}
	if !strings.Contains(output, publicKey) {
		t.Fatalf("backup create does not print the Owner public key:\n%s", output)
	}
	return home, cfg, directory, publicKey, evidenceIDs
}

func backupFiles(t *testing.T, directory string) []string {
	t.Helper()
	var files []string
	if err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			relative, _ := filepath.Rel(directory, path)
			files = append(files, filepath.ToSlash(relative))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

func TestBackupCreateVerifyRestore(t *testing.T) {
	_, _, directory, publicKey, evidenceIDs := backedUpCollective(t)
	if output, err := runCLIOutput(t, "backup", "verify", directory, "--owner-public-key", publicKey); err != nil || !strings.Contains(output, "intact") {
		t.Fatalf("backup verify: %v\n%s", err, output)
	}
	restored := filepath.Join(t.TempDir(), "restored-home")
	output, err := runCLIOutput(t, "--json", "backup", "restore", directory, "--into", restored, "--owner-public-key", publicKey)
	if err != nil {
		t.Fatalf("backup restore: %v\n%s", err, output)
	}
	var result backup.RestoreResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, output)
	}

	// The restored state is the Collective: its Mission and its evidence are
	// readable through the real services, and it is frozen.
	ctx := context.Background()
	store, err := state.Open(ctx, result.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	var missions int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM missions`).Scan(&missions); err != nil || missions != 1 {
		t.Fatalf("restored missions: %d %v", missions, err)
	}
	evidenceStore, err := evidence.New(store, result.EvidencePath, clock.System{})
	if err != nil {
		t.Fatal(err)
	}
	for n, id := range evidenceIDs {
		_, raw, err := evidenceStore.Get(ctx, domain.ID(id))
		if err != nil || !strings.Contains(string(raw), `"n":`) {
			t.Fatalf("restored evidence %d: %q %v", n, raw, err)
		}
	}
	breakGlass, _, err := emergency.NewService(store, clock.System{}, "").GetStatus(ctx)
	if err != nil || breakGlass.State != emergency.BreakGlassFrozen || !strings.Contains(breakGlass.FreezeReason, "restored from the backup") {
		t.Fatalf("restored Collective is not frozen: %+v %v", breakGlass, err)
	}
	if err := emergency.NewService(store, clock.System{}, "").CheckExecutionAllowed(ctx, "external_operation"); err == nil {
		t.Fatal("the restored Collective allows an external operation")
	}
}

func TestBackupExcludesEveryCredentialFile(t *testing.T) {
	home, cfg := emergencyCollective(t)
	secrets := map[string]string{
		"github_token": "ghp_secret_token_value", "gh-app.pem": "-----BEGIN RSA PRIVATE KEY----- secret-pem",
		"gh-app.json": `{"app":"secret-app"}`, "discord_auth.json": `{"token":"secret-discord"}`,
		"discord_webhook": "https://discord.com/api/webhooks/1/secret-webhook", "openrouter_key": "sk-or-secret",
	}
	for name, content := range secrets {
		if err := os.WriteFile(filepath.Join(home, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A credential dropped next to the evidence must not travel either.
	if err := os.WriteFile(filepath.Join(cfg.EvidencePath, "stray_token"), []byte("stray-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "backup")
	if output, err := runCLIOutput(t, "backup", "create", directory, "--home", home); err != nil {
		t.Fatalf("backup create: %v\n%s", err, output)
	}
	ownerKey, err := os.ReadFile(filepath.Join(home, "keys", "owner.key"))
	if err != nil {
		t.Fatal(err)
	}
	forbidden := [][]byte{[]byte(cfg.ControlToken), bytes.TrimSpace(ownerKey), []byte("stray-secret")}
	for _, content := range secrets {
		forbidden = append(forbidden, []byte(content))
	}
	for _, file := range backupFiles(t, directory) {
		if file != backup.DatabaseName && file != backup.ManifestName && file != backup.SignatureName && !strings.HasPrefix(file, "evidence/blobs/sha256/") {
			t.Errorf("the backup contains %s", file)
		}
		raw, err := os.ReadFile(filepath.Join(directory, filepath.FromSlash(file)))
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range forbidden {
			if len(secret) > 0 && bytes.Contains(raw, secret) {
				t.Errorf("%s contains a secret (%q...)", file, secret[:min(8, len(secret))])
			}
		}
	}
}

func TestRestoreRefusesUnsignedManifest(t *testing.T) {
	_, _, directory, publicKey, _ := backedUpCollective(t)
	restore := func(key string) (string, error) {
		return runCLIOutput(t, "backup", "restore", directory, "--into", filepath.Join(t.TempDir(), "home"), "--owner-public-key", key)
	}
	stranger, err := identity.CreateLocalEd25519(filepath.Join(t.TempDir(), "stranger.key"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if output, err := restore(hex.EncodeToString(stranger.PublicKey())); err == nil {
		t.Fatalf("restored with another key:\n%s", output)
	}
	if output, err := restore("not-a-key"); err == nil {
		t.Fatalf("restored with a malformed key:\n%s", output)
	}

	manifestPath := filepath.Join(directory, backup.ManifestName)
	original, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, bytes.Replace(original, []byte(`"schema"`), []byte(`"schema" `), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := restore(publicKey); err == nil {
		t.Fatalf("restored from a changed manifest:\n%s", output)
	}
	if err := os.WriteFile(manifestPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(directory, backup.SignatureName)); err != nil {
		t.Fatal(err)
	}
	if output, err := restore(publicKey); err == nil {
		t.Fatalf("restored without a signature:\n%s", output)
	}
}

func TestVerifyDetectsCorruptEvidence(t *testing.T) {
	_, _, directory, publicKey, _ := backedUpCollective(t)
	var blob string
	for _, file := range backupFiles(t, directory) {
		if strings.HasPrefix(file, "evidence/") {
			blob = filepath.Join(directory, filepath.FromSlash(file))
		}
	}
	if blob == "" {
		t.Fatal("the backup holds no evidence blob")
	}
	verify := func() error {
		_, err := runCLIOutput(t, "backup", "verify", directory, "--owner-public-key", publicKey)
		return err
	}
	original, err := os.ReadFile(blob)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, append([]byte("x"), original[1:]...), 0o600); err != nil {
		t.Fatal(err)
	}
	if verify() == nil {
		t.Fatal("a changed evidence blob passed verification")
	}
	if err := os.Remove(blob); err != nil {
		t.Fatal(err)
	}
	if verify() == nil {
		t.Fatal("a missing evidence blob passed verification")
	}
	if err := os.WriteFile(blob, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verify(); err != nil {
		t.Fatalf("the repaired backup does not verify: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "extra"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if verify() == nil {
		t.Fatal("a file that is not in the manifest passed verification")
	}
}

func TestRestoreRefusesNonEmptyHome(t *testing.T) {
	home, _, directory, publicKey, _ := backedUpCollective(t)
	if output, err := runCLIOutput(t, "backup", "restore", directory, "--into", home, "--owner-public-key", publicKey); err == nil {
		t.Fatalf("restored over an existing home:\n%s", output)
	}
	if output, err := runCLIOutput(t, "backup", "create", directory, "--home", home); err == nil {
		t.Fatalf("a second backup was written into a directory that is not empty:\n%s", output)
	}
}

func TestBackupWhileBoxIsWriting(t *testing.T) {
	home, cfg := emergencyCollective(t)
	ctx, cancel := context.WithCancel(context.Background())
	store, err := state.Open(ctx, cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	evidenceStore, err := evidence.New(store, cfg.EvidencePath, clock.System{})
	if err != nil {
		t.Fatal(err)
	}
	// Another connection keeps adding evidence, as a running Box does.
	var writer sync.WaitGroup
	written := 0
	writer.Add(1)
	go func() {
		defer writer.Done()
		for n := 0; ctx.Err() == nil; n++ {
			if _, err := evidenceStore.Put(ctx, strings.NewReader(`{"written":`+time.Now().Format("150405.000000")+`}`), evidence.Metadata{MediaType: "application/json", Kind: "TEST"}); err == nil {
				written++
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)
	owner, err := identity.NewLocalEd25519(filepath.Join(home, "keys", "owner.key"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 3; round++ {
		directory := filepath.Join(t.TempDir(), "backup")
		if output, err := runCLIOutput(t, "backup", "create", directory, "--home", home); err != nil {
			t.Fatalf("backup while writing: %v\n%s", err, output)
		}
		if _, err := backup.Verify(context.Background(), directory, owner.PublicKey()); err != nil {
			t.Fatalf("a backup taken while the store was written does not verify: %v", err)
		}
	}
	cancel()
	writer.Wait()
	if written == 0 {
		t.Fatal("the writer wrote nothing; the test did not exercise a concurrent write")
	}
}
