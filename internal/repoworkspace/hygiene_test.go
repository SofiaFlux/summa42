package repoworkspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hygieneFixture commits files on top of the source fixture and checks the
// result out, so edits to them have a base version that holds hidden characters.
func hygieneFixture(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	cfg := sourceFixture(t)
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(cfg.LocalPath, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, cfg.LocalPath, "add", name)
	}
	gitTest(t, cfg.LocalPath, "commit", "-qm", "hidden base")
	cfg.Commit = gitTest(t, cfg.LocalPath, "rev-parse", "HEAD")
	dir := filepath.Join(t.TempDir(), "checkout")
	if _, err := Checkout(context.Background(), cfg, dir); err != nil {
		t.Fatal(err)
	}
	return dir, cfg.Commit
}

func TestEditKeepsHiddenCharactersTheBaseAlreadyHolds(t *testing.T) {
	const bom, zwnj = "\ufeff", "\u200c"
	dir, base := hygieneFixture(t, map[string]string{
		"bom.go": bom + "package p\n",
		"zw.go":  "package p // a" + zwnj + "b\n",
	})
	for path, content := range map[string]string{
		"bom.go": bom + "package p\n\nfunc f() {}\n",
		"zw.go":  "package p // a" + zwnj + "b\n\nfunc f() {}\n",
	} {
		if _, err := PrepareCandidate(context.Background(), dir, base, []string{path}, []Edit{{Path: path, Content: content}}); err != nil {
			t.Errorf("%s: edit that keeps the base character rejected: %v", path, err)
		}
		if err := ResetCheckout(context.Background(), dir, base); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEditRejectsHiddenCharactersBeyondTheBase(t *testing.T) {
	const bom, zwnj, zwsp = "\ufeff", "\u200c", "\u200b"
	dir, base := hygieneFixture(t, map[string]string{
		"bom.go": bom + "package p\n",
		"zw.go":  "package p // a" + zwnj + "b\n",
	})
	for name, e := range map[string]Edit{
		"one more than the base":      {Path: "zw.go", Content: "package p // a" + zwnj + "b" + zwnj + "\n"},
		"a different code point":      {Path: "zw.go", Content: "package p // a" + zwnj + "b" + zwsp + "\n"},
		"replaced by another":         {Path: "bom.go", Content: zwsp + "package p\n"},
		"base character plus another": {Path: "bom.go", Content: bom + "package p" + bom + "\n"},
		"new file":                    {Path: "new.go", Content: "package p" + zwnj + "\n"},
	} {
		_, err := PrepareCandidate(context.Background(), dir, base, []string{e.Path}, []Edit{e})
		if err == nil || !strings.Contains(err.Error(), "hidden content") {
			t.Errorf("%s: PrepareCandidate = %v, want a hidden content error", name, err)
		}
	}
}
