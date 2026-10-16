package repoworkspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

type Edit struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Delete  bool   `json:"delete"`
}
type Candidate struct {
	BaseSHA      string   `json:"base_sha"`
	CandidateSHA string   `json:"candidate_sha"`
	TreeSHA      string   `json:"tree_sha"`
	DiffHash     string   `json:"diff_hash"`
	ChangedPaths []string `json:"changed_paths"`
}

func ValidateEdits(allowed []string, edits []Edit) error {
	if len(allowed) < 1 || len(allowed) > 64 || len(edits) < 1 || len(edits) > 64 {
		return errors.New("bounded allowed paths and edits required")
	}
	permitted := map[string]bool{}
	for _, p := range allowed {
		if !candidatePath(p) || permitted[p] {
			return errors.New("invalid allowed path")
		}
		permitted[p] = true
	}
	seen := map[string]bool{}
	total := 0
	for _, e := range edits {
		if !candidatePath(e.Path) || !permitted[e.Path] || seen[e.Path] || !utf8.ValidString(e.Content) || strings.ContainsRune(e.Content, 0) || (e.Delete && e.Content != "") {
			return errors.New("edit violates exact owner scope or content contract")
		}
		seen[e.Path] = true
		total += len(e.Content)
	}
	if total > MaxContextBytes {
		return errors.New("edits exceed byte limit")
	}
	return nil
}
func candidatePath(p string) bool {
	if !SafePath(p) {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}
func cleanCheckout(ctx context.Context, dir, sha string) error {
	if !filepath.IsAbs(dir) || !FullCommit(sha) {
		return errors.New("absolute checkout and full commit required")
	}
	head, err := git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(head)) != sha {
		return errors.New("checkout HEAD differs from pinned commit")
	}
	status, err := git(ctx, dir, "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return err
	}
	if len(status) != 0 {
		return errors.New("checkout is not clean")
	}
	return nil
}
func safeEditTarget(dir, base, p string, ctx context.Context) error {
	parts := strings.Split(p, "/")
	for i := range parts {
		rel := strings.Join(parts[:i+1], "/")
		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(rel)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if i < len(parts)-1 {
			if !info.IsDir() {
				return errors.New("edit parent is not a regular directory")
			}
		} else if !info.Mode().IsRegular() {
			return errors.New("edit target is not a regular file")
		}
		entry, err := git(ctx, dir, "ls-tree", base, "--", rel)
		if err != nil {
			return err
		}
		if bytes.HasPrefix(entry, []byte("160000 ")) {
			return errors.New("submodule edits are not supported")
		}
	}
	return nil
}
func PrepareCandidate(ctx context.Context, dir, base string, allowed []string, edits []Edit) (Candidate, error) {
	if err := ValidateEdits(allowed, edits); err != nil {
		return Candidate{}, err
	}
	if err := cleanCheckout(ctx, dir, base); err != nil {
		return Candidate{}, err
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return Candidate{}, errors.New("regular checkout directory required")
	}
	for _, e := range edits {
		if err := safeEditTarget(dir, base, e.Path, ctx); err != nil {
			return Candidate{}, err
		}
		if e.Delete {
			if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(e.Path))); err != nil {
				return Candidate{}, err
			}
		}
	}
	paths := make([]string, 0, len(edits))
	for _, e := range edits {
		target := filepath.Join(dir, filepath.FromSlash(e.Path))
		paths = append(paths, e.Path)
		if e.Delete {
			if err := os.Remove(target); err != nil {
				return Candidate{}, err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return Candidate{}, err
		}
		if err := os.WriteFile(target, []byte(e.Content), 0644); err != nil {
			return Candidate{}, err
		}
	}
	sort.Strings(paths)
	if _, err := git(ctx, dir, append([]string{"add", "-f", "--"}, paths...)...); err != nil {
		return Candidate{}, err
	}
	delta, err := git(ctx, dir, "diff", "--cached", "--name-only", "-z", "--no-renames")
	if err != nil {
		return Candidate{}, err
	}
	if len(delta) == 0 {
		return Candidate{}, errors.New("implementation produced no change")
	}
	if _, err := git(ctx, dir, "-c", "user.name=Summa42", "-c", "user.email=summa42@localhost.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "Prepare supervised implementation candidate"); err != nil {
		return Candidate{}, err
	}
	head, err := git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return Candidate{}, err
	}
	result, err := inspectCandidate(ctx, dir, base, strings.TrimSpace(string(head)))
	if err != nil {
		return Candidate{}, err
	}
	for _, p := range result.ChangedPaths {
		if !containsPath(allowed, p) {
			return Candidate{}, fmt.Errorf("candidate exceeds scope: %s", p)
		}
	}
	return result, nil
}
func containsPath(paths []string, p string) bool {
	for _, v := range paths {
		if v == p {
			return true
		}
	}
	return false
}
func inspectCandidate(ctx context.Context, dir, base, head string) (Candidate, error) {
	if err := cleanCheckout(ctx, dir, head); err != nil {
		return Candidate{}, err
	}
	parent, err := git(ctx, dir, "rev-parse", "HEAD^")
	if err != nil {
		return Candidate{}, err
	}
	if strings.TrimSpace(string(parent)) != base {
		return Candidate{}, errors.New("candidate parent differs from base")
	}
	tree, err := git(ctx, dir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return Candidate{}, err
	}
	names, err := git(ctx, dir, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--name-only", "-z", base, head, "--")
	if err != nil {
		return Candidate{}, err
	}
	paths := strings.Split(strings.TrimSuffix(string(names), "\x00"), "\x00")
	sort.Strings(paths)
	if len(names) == 0 {
		return Candidate{}, errors.New("candidate has no changed paths")
	}
	for _, p := range paths {
		if !candidatePath(p) {
			return Candidate{}, errors.New("candidate contains unsafe path")
		}
	}
	hash := sha256.New()
	if err := gitStream(ctx, dir, hash, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--binary", "--full-index", base, head, "--"); err != nil {
		return Candidate{}, err
	}

	return Candidate{BaseSHA: base, CandidateSHA: head, TreeSHA: strings.TrimSpace(string(tree)), DiffHash: hex.EncodeToString(hash.Sum(nil)), ChangedPaths: paths}, nil
}

// VerifyCandidate rechecks identity without rerunning validation commands.
func VerifyCandidate(ctx context.Context, dir string, c Candidate) error {
	return sameCandidate(ctx, dir, c)
}
