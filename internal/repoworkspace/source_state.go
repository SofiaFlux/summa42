package repoworkspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// SourceState observes HEAD, index/worktree changes and bounded non-ignored untracked data.
// It detects source mutations; it is not a host filesystem or Git-metadata sandbox.
func SourceState(ctx context.Context, dir string) (string, error) {
	hash := sha256.New()
	for _, argv := range [][]string{{"rev-parse", "HEAD"}, {"status", "--porcelain", "-z", "--untracked-files=all"}, {"diff", "--no-ext-diff", "--no-textconv", "--binary", "--cached", "HEAD", "--"}, {"diff", "--no-ext-diff", "--no-textconv", "--binary", "--"}} {
		fmt.Fprintf(hash, "%q\x00", argv)
		if err := gitStream(ctx, dir, hash, argv...); err != nil {
			return "", err
		}
	}
	names, err := git(ctx, dir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	paths := strings.Split(strings.TrimSuffix(string(names), "\x00"), "\x00")
	if len(names) == 0 {
		paths = nil
	}
	if len(paths) > 64 {
		return "", errors.New("source has too many untracked files to observe")
	}
	remaining := int64(MaxContextBytes)
	for _, p := range paths {
		if !SafePath(p) {
			return "", errors.New("unsafe source untracked path")
		}
		target := filepath.Join(dir, filepath.FromSlash(p))
		info, err := os.Lstat(target)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(hash, "%q:%d\x00", p, info.Mode())
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(target)
			if err != nil {
				return "", err
			}
			remaining -= int64(len(link))
			io.WriteString(hash, link)
		} else if info.Mode().IsRegular() {
			f, err := os.Open(target)
			if err != nil {
				return "", err
			}
			n, readErr := io.Copy(hash, io.LimitReader(f, remaining+1))
			f.Close()
			if readErr != nil {
				return "", readErr
			}
			remaining -= n
		} else {
			return "", errors.New("source untracked file is not regular or a symlink")
		}
		if remaining < 0 {
			return "", errors.New("source untracked data exceeds observation limit")
		}
		io.WriteString(hash, "\x00")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
