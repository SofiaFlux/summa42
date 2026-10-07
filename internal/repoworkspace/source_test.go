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
	probe := exec.Command("git", "--no-lazy-fetch", "-C", partial, "cat-file", "-e", blob)
	if err := probe.Run(); err == nil {
		t.Fatal("fixture unexpectedly has the promised blob")
	}
	cfg.LocalPath = partial
	if _, err := Capture(t.Context(), cfg); err == nil {
		t.Fatal("source capture fetched missing objects instead of failing offline")
	}
}
