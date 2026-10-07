package repoworkspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func candidateFixture(t *testing.T) (Config, string) {
	t.Helper()
	cfg := sourceFixture(t)
	dir := filepath.Join(t.TempDir(), "checkout")
	if _, err := Checkout(context.Background(), cfg, dir); err != nil {
		t.Fatal(err)
	}
	return cfg, dir
}
func TestCandidatePinsParentAndExplicitChanges(t *testing.T) {
	cfg, dir := candidateFixture(t)
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0600)
	// A dirty checkout must never be silently incorporated into a candidate.
	if _, err := PrepareCandidate(context.Background(), dir, cfg.Commit, []string{"code.go"}, []Edit{{Path: "code.go", Content: "changed\n"}}); err == nil {
		t.Fatal("dirty checkout accepted")
	}
	os.Remove(filepath.Join(dir, ".gitignore"))
	got, err := PrepareCandidate(context.Background(), dir, cfg.Commit, []string{"code.go", "new.txt"}, []Edit{{Path: "code.go", Delete: true}, {Path: "new.txt", Content: "hello\n"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseSHA != cfg.Commit || !FullCommit(got.CandidateSHA) || !FullCommit(got.TreeSHA) || len(got.DiffHash) != 64 || strings.Join(got.ChangedPaths, ",") != "code.go,new.txt" {
		t.Fatalf("candidate %+v", got)
	}
	if gitTest(t, dir, "rev-parse", "HEAD^") != cfg.Commit {
		t.Fatal("wrong parent")
	}
	if gitTest(t, cfg.LocalPath, "rev-parse", "HEAD") != cfg.Commit {
		t.Fatal("source modified")
	}
	if data, _ := os.ReadFile(filepath.Join(cfg.LocalPath, "code.go")); string(data) != "old code\n" {
		t.Fatal("source worktree modified")
	}
}
func TestCandidateRejectsInvalidEditsBeforeWriting(t *testing.T) {
	for _, edits := range [][]Edit{
		nil, {{Path: "../outside", Content: "x"}}, {{Path: "code.go", Content: "ok"}, {Path: "other", Content: "bad"}},
		{{Path: "code.go", Content: "x"}, {Path: "code.go", Content: "y"}}, {{Path: "code.go", Content: "\x00"}},
		{{Path: "code.go", Delete: true, Content: "x"}}, {{Path: "code.go", Content: strings.Repeat("x", MaxContextBytes+1)}},
	} {
		cfg, dir := candidateFixture(t)
		if _, err := PrepareCandidate(context.Background(), dir, cfg.Commit, []string{"code.go"}, edits); err == nil {
			t.Fatalf("accepted %+v", edits)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "code.go"))
		if string(data) != "old code\n" {
			t.Fatal("partial application")
		}
	}
}
func TestCandidateRejectsSymlinkParent(t *testing.T) {
	cfg, dir := candidateFixture(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "link")); err != nil {
		t.Skip(err)
	}
	gitTest(t, dir, "add", "link")
	gitTest(t, dir, "-c", "user.name=Test", "-c", "user.email=t@example.invalid", "commit", "-qm", "link")
	base := gitTest(t, dir, "rev-parse", "HEAD")
	if _, err := PrepareCandidate(context.Background(), dir, base, []string{"link/file"}, []Edit{{Path: "link/file", Content: "x"}}); err == nil {
		t.Fatal("symlink parent accepted")
	}
	_ = cfg
}
func TestCandidateRejectsNoop(t *testing.T) {
	cfg, dir := candidateFixture(t)
	if _, err := PrepareCandidate(context.Background(), dir, cfg.Commit, []string{"code.go"}, []Edit{{Path: "code.go", Content: "old code\n"}}); err == nil {
		t.Fatal("empty diff accepted")
	}
}
func TestCandidateIncludesExplicitIgnoredFile(t *testing.T) {
	_, dir := candidateFixture(t)
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0600)
	gitTest(t, dir, "add", ".gitignore")
	gitTest(t, dir, "-c", "user.name=Test", "-c", "user.email=t@example.invalid", "commit", "-qm", "ignore")
	base := gitTest(t, dir, "rev-parse", "HEAD")
	c, err := PrepareCandidate(context.Background(), dir, base, []string{"ignored.txt"}, []Edit{{Path: "ignored.txt", Content: "allowed"}})
	if err != nil || len(c.ChangedPaths) != 1 || c.ChangedPaths[0] != "ignored.txt" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestCandidateHashesMaximumSizedNewFile(t *testing.T) {
	cfg, dir := candidateFixture(t)
	got, err := PrepareCandidate(context.Background(), dir, cfg.Commit, []string{"new.txt"}, []Edit{{Path: "new.txt", Content: strings.Repeat("xxxxxxx\n", MaxContextBytes/8)}})
	if err != nil || len(got.DiffHash) != 64 {
		t.Fatalf("legal edit rejected: %+v %v", got, err)
	}
}

func TestCandidateHashesLargeReplacementDiff(t *testing.T) {
	_, dir := candidateFixture(t)
	os.WriteFile(filepath.Join(dir, "code.go"), []byte(strings.Repeat("old line\n", 17000)), 0600)
	gitTest(t, dir, "add", "code.go")
	gitTest(t, dir, "-c", "user.name=Test", "-c", "user.email=t@example.invalid", "commit", "-qm", "large base")
	base := gitTest(t, dir, "rev-parse", "HEAD")
	if _, err := PrepareCandidate(context.Background(), dir, base, []string{"code.go"}, []Edit{{Path: "code.go", Content: strings.Repeat("new line\n", 17000)}}); err != nil {
		t.Fatal(err)
	}
}
