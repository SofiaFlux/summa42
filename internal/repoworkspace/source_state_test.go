package repoworkspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSourceStateObservesIndexAndUntrackedContents(t *testing.T) {
	cfg := sourceFixture(t)
	ctx := context.Background()
	first, err := SourceState(ctx, cfg.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(cfg.LocalPath, "code.go"), []byte("dirty\n"), 0600)
	second, err := SourceState(ctx, cfg.LocalPath)
	if err != nil || second == first {
		t.Fatalf("worktree not observed: %v", err)
	}
	gitTest(t, cfg.LocalPath, "add", "code.go")
	third, err := SourceState(ctx, cfg.LocalPath)
	if err != nil || third == second {
		t.Fatalf("index not observed: %v", err)
	}
	os.WriteFile(filepath.Join(cfg.LocalPath, "new.txt"), []byte("one"), 0600)
	fourth, err := SourceState(ctx, cfg.LocalPath)
	if err != nil || fourth == third {
		t.Fatalf("untracked file not observed: %v", err)
	}
	os.WriteFile(filepath.Join(cfg.LocalPath, "new.txt"), []byte("two"), 0600)
	fifth, err := SourceState(ctx, cfg.LocalPath)
	if err != nil || fifth == fourth {
		t.Fatalf("untracked contents not observed: %v", err)
	}
	again, err := SourceState(ctx, cfg.LocalPath)
	if err != nil || again != fifth {
		t.Fatalf("dirty state unstable: %v", err)
	}
}
