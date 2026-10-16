package repoworkspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewCandidateContainsCompletePinnedChanges(t *testing.T) {
	cfg, repo := candidateFixture(t)
	base := cfg.Commit
	c, err := PrepareCandidate(t.Context(), repo, base, []string{"code.go", "new.go"}, []Edit{{Path: "code.go", Delete: true}, {Path: "new.go", Content: "package new\n"}})
	if err != nil {
		t.Fatal(err)
	}
	changes, err := ReviewCandidate(t.Context(), repo, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || changes[0].Path != "code.go" || changes[0].Before == nil || changes[0].Before.Content != "old code\n" || changes[0].After != nil || changes[1].Before != nil || changes[1].After.Content != "package new\n" {
		t.Fatalf("%+v", changes)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.go"), []byte("drift"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReviewCandidate(t.Context(), repo, c); err == nil {
		t.Fatal("dirty candidate reviewed")
	}
}
func TestReviewCandidateRejectsOversizedAndInvalidSource(t *testing.T) {
	for _, content := range []string{strings.Repeat("a", MaxReviewBytes+1), "bad\xff"} {
		t.Run("rejected", func(t *testing.T) {
			cfg, repo := candidateFixture(t)
			base := cfg.Commit
			if err := os.WriteFile(filepath.Join(repo, "code.go"), []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := git(t.Context(), repo, "add", "code.go"); err != nil {
				t.Fatal(err)
			}
			if _, err := git(t.Context(), repo, "-c", "user.name=test", "-c", "user.email=test@invalid", "commit", "-qm", "large base"); err != nil {
				t.Fatal(err)
			}
			raw, err := git(t.Context(), repo, "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			base = strings.TrimSpace(string(raw))
			c, err := PrepareCandidate(t.Context(), repo, base, []string{"code.go"}, []Edit{{Path: "code.go", Content: "package fixed\n"}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ReviewCandidate(t.Context(), repo, c); err == nil {
				t.Fatal("unreviewable source accepted")
			}
		})
	}
}
