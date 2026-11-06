package repoworkspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}
func sourceFixture(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	gitTest(t, dir, "init", "-q")
	gitTest(t, dir, "config", "user.name", "Test")
	gitTest(t, dir, "config", "user.email", "test@example.invalid")
	os.WriteFile(filepath.Join(dir, "code.go"), []byte("old code\n"), 0600)
	gitTest(t, dir, "add", "code.go")
	gitTest(t, dir, "commit", "-qm", "base")
	return Config{LocalPath: dir, Repository: "o/r", Commit: gitTest(t, dir, "rev-parse", "HEAD"), Paths: []string{"code.go"}}
}

func TestCapturePinsCommitDespiteHeadAndDirtyWorktree(t *testing.T) {
	cfg := sourceFixture(t)
	os.WriteFile(filepath.Join(cfg.LocalPath, "code.go"), []byte("new code\n"), 0600)
	gitTest(t, cfg.LocalPath, "commit", "-qam", "new")
	os.WriteFile(filepath.Join(cfg.LocalPath, "code.go"), []byte("dirty\n"), 0600)
	t.Setenv("GIT_DIR", t.TempDir())
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "alias.show")
	t.Setenv("GIT_CONFIG_VALUE_0", "!exit 99")
	got, err := Capture(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Commit != cfg.Commit || len(got.Files) != 1 || got.Files[0].Content != "old code\n" {
		t.Fatalf("unpinned: %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
}
func TestCaptureRejectsUnsafeAndUnboundedContext(t *testing.T) {
	for _, p := range []string{"../code.go", "/code.go", ".git/config", "a/../code.go", "code.go\\x", "missing"} {
		t.Run(p, func(t *testing.T) {
			cfg := sourceFixture(t)
			cfg.Paths = []string{p}
			if _, err := Capture(t.Context(), cfg); err == nil {
				t.Fatal("unsafe context accepted")
			}
		})
	}
	cfg := sourceFixture(t)
	cfg.Commit = "HEAD"
	if _, err := Capture(t.Context(), cfg); err == nil {
		t.Fatal("mutable ref accepted")
	}
	cfg = sourceFixture(t)
	os.WriteFile(filepath.Join(cfg.LocalPath, "large"), []byte(strings.Repeat("x", MaxContextBytes+1)), 0600)
	gitTest(t, cfg.LocalPath, "add", "large")
	gitTest(t, cfg.LocalPath, "commit", "-qm", "large")
	cfg.Commit = gitTest(t, cfg.LocalPath, "rev-parse", "HEAD")
	cfg.Paths = []string{"large"}
	if _, err := Capture(t.Context(), cfg); err == nil {
		t.Fatal("oversized context accepted")
	}
}
func TestCaptureRejectsSymlink(t *testing.T) {
	cfg := sourceFixture(t)
	if err := os.Symlink("code.go", filepath.Join(cfg.LocalPath, "link")); err != nil {
		t.Skip(err)
	}
	gitTest(t, cfg.LocalPath, "add", "link")
	gitTest(t, cfg.LocalPath, "commit", "-qm", "link")
	cfg.Commit = gitTest(t, cfg.LocalPath, "rev-parse", "HEAD")
	cfg.Paths = []string{"link"}
	if _, err := Capture(t.Context(), cfg); err == nil {
		t.Fatal("link accepted")
	}
}

func TestCaptureRequiresCommitObject(t *testing.T) {
	cfg := sourceFixture(t)
	cfg.Commit = gitTest(t, cfg.LocalPath, "rev-parse", "HEAD^{tree}")
	if _, err := Capture(t.Context(), cfg); err == nil {
		t.Fatal("tree object accepted as base commit")
	}
}
func TestCheckoutIsFreshDetachedAndDoesNotRunSourceHooks(t *testing.T) {
	cfg := sourceFixture(t)
	marker := filepath.Join(t.TempDir(), "hook-fired")
	os.WriteFile(filepath.Join(cfg.LocalPath, ".git", "hooks", "post-checkout"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700)
	dest := filepath.Join(t.TempDir(), "checkout")
	got, err := Checkout(t.Context(), cfg, dest)
	if err != nil {
		t.Fatal(err)
	}
	if got.Commit != cfg.Commit || gitTest(t, dest, "rev-parse", "HEAD") != cfg.Commit {
		t.Fatal("wrong checkout")
	}
	if out := gitTest(t, dest, "branch", "--show-current"); out != "" {
		t.Fatal("checkout is not detached")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("source hook ran")
	}
	os.WriteFile(filepath.Join(dest, "code.go"), []byte("changed"), 0600)
	original, _ := os.ReadFile(filepath.Join(cfg.LocalPath, "code.go"))
	if string(original) != "old code\n" {
		t.Fatal("source mutated")
	}
	if _, err := Checkout(t.Context(), cfg, dest); err == nil {
		t.Fatal("existing workspace overwritten")
	}
}

func TestCheckoutDoesNotBorrowSourceAlternates(t *testing.T) {
	cfg := sourceFixture(t)
	borrower := filepath.Join(t.TempDir(), "borrower")
	gitTest(t, cfg.LocalPath, "clone", "--shared", cfg.LocalPath, borrower)
	cfg.LocalPath = borrower
	dest := filepath.Join(t.TempDir(), "checkout")
	if _, err := Checkout(t.Context(), cfg, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Fatal("checkout still borrows writable source objects")
	}
}

func TestCaptureDoesNotFetchMissingPartialCloneObjects(t *testing.T) {
	cfg := sourceFixture(t)
	gitTest(t, cfg.LocalPath, "config", "uploadpack.allowFilter", "true")
	partial := filepath.Join(t.TempDir(), "partial")
	gitTest(t, cfg.LocalPath, "clone", "--filter=blob:none", "--no-checkout", "file://"+cfg.LocalPath, partial)
	blob := gitTest(t, partial, "rev-parse", cfg.Commit+":code.go")
	probe := exec.Command("git", "-C", partial, "cat-file", "-e", blob)
	probe.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1")
	if err := probe.Run(); err == nil {
		t.Fatal("fixture unexpectedly has the promised blob")
	}
	cfg.LocalPath = partial
	if _, err := Capture(t.Context(), cfg); err == nil {
		t.Fatal("source capture fetched missing objects instead of failing offline")
	}
}

func TestGCRemovesTerminalAttemptDirs(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"live", "dead"} {
		if err := os.MkdirAll(filepath.Join(root, name, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "stray-file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := GC(root, func(dir string) bool { return filepath.Base(dir) == "live" }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "dead")); !os.IsNotExist(err) {
		t.Fatalf("terminal dir kept: %v", err)
	}
	for _, kept := range []string{"live", "stray-file"} {
		if _, err := os.Stat(filepath.Join(root, kept)); err != nil {
			t.Fatalf("%s removed: %v", kept, err)
		}
	}
	if err := GC("relative", func(string) bool { return false }); err == nil {
		t.Fatal("relative root accepted")
	}
}

func TestFitContextKeepsPinnedAndDropsWhatOverflows(t *testing.T) {
	cfg := sourceFixture(t)
	third := MaxContextBytes / 3
	for name, size := range map[string]int{"a": third, "b": third, "big": MaxContextBytes, "c": third} {
		os.WriteFile(filepath.Join(cfg.LocalPath, name), []byte(strings.Repeat("x", size)), 0600)
	}
	gitTest(t, cfg.LocalPath, "add", ".")
	gitTest(t, cfg.LocalPath, "commit", "-qm", "files")
	cfg.Commit = gitTest(t, cfg.LocalPath, "rev-parse", "HEAD")
	got, err := FitContext(t.Context(), cfg.LocalPath, cfg.Commit, []string{"code.go", "a", "big", "b", "c"}, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "code.go,a,b" {
		t.Fatalf("fit = %v, want code.go,a,b (big and then c overflow)", got)
	}
	cfg.Paths = got
	if _, err := Capture(t.Context(), cfg); err != nil {
		t.Fatalf("fitted context does not capture: %v", err)
	}
	if got, err := FitContext(t.Context(), cfg.LocalPath, cfg.Commit, []string{"big", "a"}, 1, 0); err != nil || strings.Join(got, ",") != "big" {
		t.Fatalf("pinned file must be kept even when it overflows: %v %v", got, err)
	}
}

// A smaller budget drops what a harness that truncates long input could not see.
func TestFitContextHonoursASmallerBudget(t *testing.T) {
	cfg := sourceFixture(t)
	for name, size := range map[string]int{"a": 30000, "b": 30000, "c": 30000} {
		os.WriteFile(filepath.Join(cfg.LocalPath, name), []byte(strings.Repeat("x", size)), 0600)
	}
	gitTest(t, cfg.LocalPath, "add", ".")
	gitTest(t, cfg.LocalPath, "commit", "-qm", "files")
	cfg.Commit = gitTest(t, cfg.LocalPath, "rev-parse", "HEAD")
	got, err := FitContext(t.Context(), cfg.LocalPath, cfg.Commit, []string{"a", "b", "c"}, 0, 70000)
	if err != nil || strings.Join(got, ",") != "a,b" {
		t.Fatalf("fit = %v %v, want a,b under a 70000 byte budget", got, err)
	}
	if got, _ := FitContext(t.Context(), cfg.LocalPath, cfg.Commit, []string{"a", "b", "c"}, 0, 0); strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("budget 0 must keep the repository maximum: %v", got)
	}
}

func TestReadFileTableRejections(t *testing.T) {
	cfg := sourceFixture(t)
	dir := cfg.LocalPath

	// 1. Commit a directory
	os.MkdirAll(filepath.Join(dir, "somedir"), 0755)
	os.WriteFile(filepath.Join(dir, "somedir", "sub.go"), []byte("package somedir\n"), 0600)

	// 2. Commit a symlink
	_ = os.Symlink("code.go", filepath.Join(dir, "link_inside"))
	_ = os.Symlink("/etc/passwd", filepath.Join(dir, "link_outside"))

	// 3. Commit a binary file with NUL byte
	os.WriteFile(filepath.Join(dir, "binary.bin"), []byte("hello\x00world"), 0600)

	// 4. Commit a large file (2000 bytes)
	os.WriteFile(filepath.Join(dir, "oversized.txt"), []byte(strings.Repeat("a", 2000)), 0600)

	gitTest(t, dir, "add", "-A")
	gitTest(t, dir, "commit", "-qm", "more files")
	commit := gitTest(t, dir, "rev-parse", "HEAD")

	rejections := []struct {
		name     string
		path     string
		maxBytes int
	}{
		{"parent path", "../x", 10000},
		{"absolute path", "/etc/passwd", 10000},
		{"clean parent traversal", "a/../../x", 10000},
		{"git config", ".git/config", 10000},
		{"case insensitive git path", "sub/.GIT/HEAD", 10000},
		{"sensitive env", ".env", 10000},
		{"sensitive env suffix", ".env.local", 10000},
		{"sensitive id_rsa", "deploy/id_rsa", 10000},
		{"sensitive pem", "certs/server.pem", 10000},
		{"sensitive key", "keys/private.key", 10000},
		{"sensitive token", "auth/api_token_prod", 10000},
		{"sensitive credentials", "config/credentials.json", 10000},
		{"sensitive netrc", ".netrc", 10000},
		{"symlink inside", "link_inside", 10000},
		{"symlink outside", "link_outside", 10000},
		{"directory", "somedir", 10000},
		{"non existent", "missing.go", 10000},
		{"binary with NUL", "binary.bin", 10000},
		{"file over maxBytes", "oversized.txt", 1000},
	}

	for _, tc := range rejections {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadFile(t.Context(), dir, commit, tc.path, tc.maxBytes)
			if err == nil {
				t.Fatalf("expected ReadFile to reject %q, but it succeeded", tc.path)
			}
		})
	}
}

func TestReadFileReadsCommitContentNotWorkingTree(t *testing.T) {
	cfg := sourceFixture(t)
	dir := cfg.LocalPath
	commit := cfg.Commit // points to "old code\n"

	// Dirty the working tree
	os.WriteFile(filepath.Join(dir, "code.go"), []byte("modified dirty code\n"), 0600)

	file, err := ReadFile(t.Context(), dir, commit, "code.go", 10000)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if file.Content != "old code\n" {
		t.Fatalf("expected commit content 'old code\\n', got %q", file.Content)
	}
}

func TestReadFileSHA256MatchesCapture(t *testing.T) {
	cfg := sourceFixture(t)
	dir := cfg.LocalPath
	commit := cfg.Commit

	file, err := ReadFile(t.Context(), dir, commit, "code.go", 10000)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	snap, err := Capture(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Capture failed: %v", err)
	}
	if len(snap.Files) != 1 {
		t.Fatalf("expected 1 file from capture, got %d", len(snap.Files))
	}

	if file.SHA256 != snap.Files[0].SHA256 {
		t.Fatalf("SHA256 mismatch: ReadFile=%q Capture=%q", file.SHA256, snap.Files[0].SHA256)
	}
}
