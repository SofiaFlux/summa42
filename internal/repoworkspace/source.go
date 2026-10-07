// Package repoworkspace prepares pinned source and supervised local candidates.
package repoworkspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const Schema = "repository.context.v1"
const MaxContextBytes = 256 * 1024
const MaxFiles = 32

type Config struct {
	LocalPath, Repository, Commit string
	Paths                         []string
}
type File struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Content string `json:"content"`
}
type Snapshot struct {
	Schema     string `json:"schema"`
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
	Files      []File `json:"files"`
}

func SafePath(p string) bool {
	return p != "" && len(p) <= 240 && utf8.ValidString(p) && !strings.ContainsAny(p, "\\\x00\r\n:") && !path.IsAbs(p) && path.Clean(p) == p && p != "." && p != ".." && !strings.HasPrefix(p, "../") && p != ".git" && !strings.HasPrefix(p, ".git/")
}
func FullCommit(s string) bool {
	if len(s) != 40 || s != strings.ToLower(s) {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func (s Snapshot) Validate() error {
	if s.Schema != Schema || strings.Count(s.Repository, "/") != 1 || strings.TrimSpace(s.Repository) != s.Repository || strings.HasPrefix(s.Repository, "/") || strings.HasSuffix(s.Repository, "/") || !FullCommit(s.Commit) || len(s.Files) < 1 || len(s.Files) > MaxFiles {
		return errors.New("invalid repository context identity or size")
	}
	total := 0
	previous := ""
	for _, f := range s.Files {
		hash := sha256.Sum256([]byte(f.Content))
		if !SafePath(f.Path) || f.Path <= previous || !utf8.ValidString(f.Content) || strings.ContainsRune(f.Content, 0) || f.SHA256 != hex.EncodeToString(hash[:]) {
			return errors.New("invalid context file, order or hash")
		}
		total += len(f.Content)
		previous = f.Path
	}
	if total > MaxContextBytes {
		return errors.New("repository context exceeds byte limit")
	}
	return nil
}
func (s Snapshot) Canonical() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(s)
}

// boundedBuffer consumes the entire pipe but retains only the bounded prefix.
type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Len() int       { return b.buffer.Len() }
func (b *boundedBuffer) Bytes() []byte  { return b.buffer.Bytes() }
func (b *boundedBuffer) String() string { return b.buffer.String() }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if n > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	_, err := b.buffer.Write(p)
	return n, err
}
func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	out := &boundedBuffer{limit: MaxContextBytes + 1}
	if err := gitStream(ctx, dir, out, args...); err != nil {
		return nil, err
	}
	if out.overflow {
		return nil, errors.New("Git output exceeds context limit")
	}
	return out.Bytes(), nil
}
func gitStream(ctx context.Context, dir string, out io.Writer, args ...string) error {
	binary, err := exec.LookPath("git")
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	fixed := []string{"--no-lazy-fetch", "--no-replace-objects", "--literal-pathspecs", "-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "core.autocrlf=false", "-c", "protocol.allow=never", "-c", "protocol.file.allow=always"}
	cmd := exec.CommandContext(runCtx, binary, append(fixed, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_SYSTEM=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1"}
	allowedProtocols := ""
	if len(args) > 0 && args[0] == "clone" {
		allowedProtocols = "file"
	}
	cmd.Env = append(cmd.Env, "GIT_ALLOW_PROTOCOL="+allowedProtocols)
	stderr := &boundedBuffer{limit: 4096}
	cmd.Stdout = out
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if runCtx.Err() != nil {
			return runCtx.Err()
		}
		return fmt.Errorf("git failed: %w: %s", err, stderr.String())
	}
	return nil
}

func Capture(ctx context.Context, cfg Config) (Snapshot, error) {
	snapshot := Snapshot{Schema: Schema, Repository: cfg.Repository, Commit: cfg.Commit}
	if !FullCommit(cfg.Commit) || len(cfg.Paths) < 1 || len(cfg.Paths) > MaxFiles || !filepath.IsAbs(cfg.LocalPath) {
		return Snapshot{}, errors.New("explicit absolute local repository, full commit and bounded file selection required")
	}
	paths := append([]string(nil), cfg.Paths...)
	objectType, err := git(ctx, cfg.LocalPath, "cat-file", "-t", cfg.Commit)
	if err != nil {
		return Snapshot{}, err
	}
	if strings.TrimSpace(string(objectType)) != "commit" {
		return Snapshot{}, errors.New("source SHA must identify a commit")
	}
	sort.Strings(paths)
	total := 0
	for i, p := range paths {
		if !SafePath(p) || (i > 0 && paths[i-1] == p) {
			return Snapshot{}, errors.New("unsafe or duplicate context path")
		}
		entry, err := git(ctx, cfg.LocalPath, "ls-tree", "-z", cfg.Commit, "--", p)
		if err != nil {
			return Snapshot{}, err
		}
		parts := bytes.Split(entry, []byte{'\t'})
		if len(parts) != 2 || string(parts[1]) != p+"\x00" {
			return Snapshot{}, fmt.Errorf("context file %q is not a regular tracked file", p)
		}
		fields := strings.Fields(string(parts[0]))
		if len(fields) != 3 || (fields[0] != "100644" && fields[0] != "100755") || fields[1] != "blob" {
			return Snapshot{}, fmt.Errorf("context file %q must not be a symlink or submodule", p)
		}
		raw, err := git(ctx, cfg.LocalPath, "cat-file", "blob", fields[2])
		if err != nil {
			return Snapshot{}, err
		}
		total += len(raw)
		if total > MaxContextBytes {
			return Snapshot{}, errors.New("repository context exceeds byte limit")
		}
		hash := sha256.Sum256(raw)
		snapshot.Files = append(snapshot.Files, File{Path: p, SHA256: hex.EncodeToString(hash[:]), Content: string(raw)})
	}
	if err := snapshot.Validate(); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func Checkout(ctx context.Context, cfg Config, destination string) (Snapshot, error) {
	snapshot, err := Capture(ctx, cfg)
	if err != nil {
		return Snapshot{}, err
	}
	if !filepath.IsAbs(destination) {
		return Snapshot{}, errors.New("absolute fresh checkout destination required")
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		return Snapshot{}, errors.New("checkout destination already exists or cannot be inspected")
	}
	if _, err := git(ctx, "", "clone", "--no-hardlinks", "--dissociate", "--no-checkout", "--template=", "--", cfg.LocalPath, destination); err != nil {
		return Snapshot{}, err
	}
	// Keep the isolated clone for diagnosis on failure; never overwrite an existing workspace.
	if _, err := git(ctx, destination, "checkout", "--detach", cfg.Commit, "--"); err != nil {
		return Snapshot{}, err
	}
	head, err := git(ctx, destination, "rev-parse", "HEAD")
	if err != nil {
		return Snapshot{}, err
	}
	if strings.TrimSpace(string(head)) != cfg.Commit {
		return Snapshot{}, errors.New("checkout commit differs from pinned source")
	}
	return snapshot, nil
}
