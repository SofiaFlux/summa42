package repoworkspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExportCandidatePreservesExactCommitAndChanges(t *testing.T) {
	cfg, dir := candidateFixture(t)
	if err := os.Chmod(filepath.Join(dir, "code.go"), 0755); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "add", "code.go")
	gitTest(t, dir, "-c", "user.name=test", "-c", "user.email=test@invalid", "commit", "-qm", "mode")
	base := gitTest(t, dir, "rev-parse", "HEAD")
	c, err := PrepareCandidate(t.Context(), dir, base, []string{"code.go", "new.go"}, []Edit{{Path: "code.go", Delete: true}, {Path: "new.go", Content: "package new\n"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ExportCandidate(t.Context(), dir, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
	if got.Candidate.CandidateSHA != c.CandidateSHA || len(got.Files) != 2 || !got.Files[0].Delete || got.Files[0].Mode != "100755" || got.Files[1].Content != "package new\n" || got.Author.Name != "Summa42" || got.BaseTreeSHA == "" {
		t.Fatalf("%+v", got)
	}
	_ = cfg
	got.Message = "changed"
	if err := got.Validate(); err == nil {
		t.Fatal("rewritten identity accepted")
	}
	os.WriteFile(filepath.Join(dir, "new.go"), []byte("dirty"), 0644)
	if _, err := ExportCandidate(t.Context(), dir, c); err == nil {
		t.Fatal("dirty export")
	}
}
