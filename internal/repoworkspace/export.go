package repoworkspace

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Identity struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}
type ExportFile struct {
	Path    string `json:"path"`
	Mode    string `json:"mode"`
	Content string `json:"content"`
	Delete  bool   `json:"delete"`
}
type Export struct {
	Candidate   Candidate    `json:"candidate"`
	BaseTreeSHA string       `json:"base_tree_sha"`
	RawCommit   string       `json:"raw_commit"`
	Author      Identity     `json:"author"`
	Committer   Identity     `json:"committer"`
	Message     string       `json:"message"`
	Files       []ExportFile `json:"files"`
}

var gitIdentity = regexp.MustCompile(`^([^<>\r\n]+) <([^<>\r\n]+)> ([0-9]+) ([+-][0-9]{4})$`)

func parseIdentity(raw string) (Identity, error) {
	m := gitIdentity.FindStringSubmatch(raw)
	if m == nil {
		return Identity{}, errors.New("unsupported commit identity")
	}
	sec, err := strconv.ParseInt(m[3], 10, 64)
	if err != nil {
		return Identity{}, err
	}
	hour, _ := strconv.Atoi(m[4][1:3])
	minute, _ := strconv.Atoi(m[4][3:5])
	if hour > 23 || minute > 59 {
		return Identity{}, errors.New("invalid commit timezone")
	}
	offset := (hour*60 + minute) * 60
	if m[4][0] == '-' {
		offset = -offset
	}
	return Identity{Name: m[1], Email: m[2], Date: time.Unix(sec, 0).In(time.FixedZone("", offset)).Format(time.RFC3339)}, nil
}
func identityHeader(i Identity) (string, error) {
	date, err := time.Parse(time.RFC3339, i.Date)
	if err != nil {
		return "", err
	}
	if i.Name == "" || i.Email == "" || strings.ContainsAny(i.Name+i.Email, "<>\r\n\x00") {
		return "", errors.New("invalid identity")
	}
	return fmt.Sprintf("%s <%s> %d %s", i.Name, i.Email, date.Unix(), date.Format("-0700")), nil
}
func (e Export) Validate() error {
	c := e.Candidate
	if !FullCommit(c.BaseSHA) || !FullCommit(c.CandidateSHA) || !FullCommit(c.TreeSHA) || !FullCommit(e.BaseTreeSHA) || len(e.Files) < 1 || len(e.Files) > 64 || len(e.Files) != len(c.ChangedPaths) || len(e.Message) > 4096 || !utf8.ValidString(e.Message) || strings.ContainsRune(e.Message, 0) {
		return errors.New("invalid publication export identity/limits")
	}
	author, err := identityHeader(e.Author)
	if err != nil {
		return err
	}
	committer, err := identityHeader(e.Committer)
	if err != nil {
		return err
	}
	raw := fmt.Sprintf("tree %s\nparent %s\nauthor %s\ncommitter %s\n\n%s", c.TreeSHA, c.BaseSHA, author, committer, e.Message)
	digest := sha1.Sum([]byte(fmt.Sprintf("commit %d\x00", len(raw)) + raw))
	if raw != e.RawCommit || hex.EncodeToString(digest[:]) != c.CandidateSHA {
		return errors.New("export cannot reproduce exact candidate commit")
	}
	total := 0
	previous := ""
	for n, f := range e.Files {
		if !candidatePath(f.Path) || f.Path <= previous || f.Path != c.ChangedPaths[n] || (f.Mode != "100644" && f.Mode != "100755") || !utf8.ValidString(f.Content) || strings.ContainsRune(f.Content, 0) || (f.Delete && f.Content != "") {
			return errors.New("invalid export file")
		}
		total += len(f.Content)
		previous = f.Path
	}
	if total > MaxContextBytes {
		return errors.New("export exceeds 256 KiB")
	}
	return nil
}
func ExportCandidate(ctx context.Context, dir string, c Candidate) (Export, error) {
	if err := VerifyCandidate(ctx, dir, c); err != nil {
		return Export{}, err
	}
	raw, err := git(ctx, dir, "cat-file", "commit", c.CandidateSHA)
	if err != nil {
		return Export{}, err
	}
	header, message, ok := strings.Cut(string(raw), "\n\n")
	lines := strings.Split(header, "\n")
	if !ok || len(lines) != 4 || lines[0] != "tree "+c.TreeSHA || lines[1] != "parent "+c.BaseSHA || !strings.HasPrefix(lines[2], "author ") || !strings.HasPrefix(lines[3], "committer ") {
		return Export{}, errors.New("publication supports unsigned ordinary single-parent commits only")
	}
	author, err := parseIdentity(strings.TrimPrefix(lines[2], "author "))
	if err != nil {
		return Export{}, err
	}
	committer, err := parseIdentity(strings.TrimPrefix(lines[3], "committer "))
	if err != nil {
		return Export{}, err
	}
	tree, err := git(ctx, dir, "rev-parse", c.BaseSHA+"^{tree}")
	if err != nil {
		return Export{}, err
	}
	out := Export{Candidate: c, BaseTreeSHA: strings.TrimSpace(string(tree)), RawCommit: string(raw), Author: author, Committer: committer, Message: message}
	total := 0
	for _, p := range c.ChangedPaths {
		file, err := reviewBlob(ctx, dir, c.CandidateSHA, p, &total)
		if err != nil {
			return Export{}, err
		}
		entry := ExportFile{Path: p}
		if file == nil {
			before, err := reviewBlob(ctx, dir, c.BaseSHA, p, new(int))
			if err != nil || before == nil {
				return Export{}, errors.New("cannot export deleted source blob")
			}
			entry.Mode = before.Mode
			entry.Delete = true
		} else {
			entry.Mode = file.Mode
			entry.Content = file.Content
		}
		out.Files = append(out.Files, entry)
	}
	if err := out.Validate(); err != nil {
		return Export{}, err
	}
	if err := VerifyCandidate(ctx, dir, c); err != nil {
		return Export{}, err
	}
	return out, nil
}
