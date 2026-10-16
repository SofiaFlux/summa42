package repoworkspace

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

const MaxReviewBytes = 512 * 1024

type ReviewFile struct {
	Mode    string `json:"mode"`
	Content string `json:"content"`
}
type FileChange struct {
	Path   string      `json:"path"`
	Before *ReviewFile `json:"before"`
	After  *ReviewFile `json:"after"`
}

// ReviewCandidate projects complete immutable blobs, never a truncated diff.
func ReviewCandidate(ctx context.Context, dir string, c Candidate) ([]FileChange, error) {
	if err := VerifyCandidate(ctx, dir, c); err != nil {
		return nil, err
	}
	if len(c.ChangedPaths) < 1 || len(c.ChangedPaths) > 64 {
		return nil, errors.New("review path limit exceeded")
	}
	total := 0
	changes := make([]FileChange, 0, len(c.ChangedPaths))
	for _, p := range c.ChangedPaths {
		before, err := reviewBlob(ctx, dir, c.BaseSHA, p, &total)
		if err != nil {
			return nil, err
		}
		after, err := reviewBlob(ctx, dir, c.CandidateSHA, p, &total)
		if err != nil {
			return nil, err
		}
		if before == nil && after == nil {
			return nil, errors.New("changed path has no source or candidate blob")
		}
		changes = append(changes, FileChange{Path: p, Before: before, After: after})
	}
	if err := VerifyCandidate(ctx, dir, c); err != nil {
		return nil, err
	}
	return changes, nil
}
func reviewBlob(ctx context.Context, dir, commit, p string, total *int) (*ReviewFile, error) {
	if !candidatePath(p) || !FullCommit(commit) {
		return nil, errors.New("invalid review blob identity")
	}
	raw, err := git(ctx, dir, "ls-tree", "-z", commit, "--", p)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	header, name, ok := strings.Cut(strings.TrimSuffix(string(raw), "\x00"), "\t")
	fields := strings.Fields(header)
	if !ok || name != p || len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
		return nil, errors.New("review supports regular file blobs only")
	}
	out := &boundedBuffer{limit: MaxReviewBytes - *total}
	if err := gitStream(ctx, dir, out, "cat-file", "blob", fields[2]); err != nil {
		return nil, err
	}
	if out.overflow {
		return nil, errors.New("complete review projection exceeds 512 KiB; narrow the work")
	}
	if !utf8.Valid(out.Bytes()) || strings.ContainsRune(out.String(), 0) {
		return nil, errors.New("review requires complete UTF-8 text")
	}
	*total += out.Len()
	return &ReviewFile{Mode: fields[0], Content: out.String()}, nil
}
