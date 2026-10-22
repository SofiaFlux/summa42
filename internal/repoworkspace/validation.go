package repoworkspace

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SofiaFlux/summa42/internal/modelrun"
)

const ValidationTimeout = 60 * time.Second
const MaxValidationOutput = 16 * 1024

type CommandResult struct {
	Argv      []string `json:"argv"`
	ExitCode  int      `json:"exit_code"`
	Passed    bool     `json:"passed"`
	TimedOut  bool     `json:"timed_out"`
	Output    string   `json:"output"`
	Truncated bool     `json:"truncated"`
}

// Validator runs the operator's validation commands against a candidate as the
// dedicated model user. The commands execute model-written code (a test the
// model added runs in `go test`), so they must never run with the Box's uid:
// that uid can read the Box keys, database and tokens.
type Validator struct {
	runAs     *user.User
	toolchain []string
}

// NewValidator fails closed: the model user must be named and usable
// (modelrun.ResolveRunAsUser). toolchainDirs are absolute directories put in
// front of the model user's fixed PATH (/usr/local/bin:/usr/bin:/bin) so the
// Go toolchain is found there; they must be readable and executable by the
// model user. The Box's own PATH is never forwarded.
func NewValidator(runAsUser string, toolchainDirs []string) (*Validator, error) {
	runAs, err := modelrun.ResolveRunAsUser(runAsUser)
	if err != nil {
		return nil, fmt.Errorf("validation: %w", err)
	}
	dirs := make([]string, 0, len(toolchainDirs))
	for _, dir := range toolchainDirs {
		if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || strings.ContainsAny(dir, ":\x00\n") {
			return nil, fmt.Errorf("validation toolchain dir %q must be a clean absolute path without ':'", dir)
		}
		dirs = append(dirs, dir)
	}
	return &Validator{runAs: runAs, toolchain: dirs}, nil
}

// commandTimeout is the per-command limit (a variable only for tests).
var commandTimeout = ValidationTimeout

// ValidateCandidate streams the candidate commit (git archive) to the model
// user, runs each command in a private scratch copy under the model user's
// ~/tmp with private GOCACHE and GOMODCACHE, and returns the recorded results.
// The Box keeps the evidence; it only sees bounded output, exit codes and
// timings, as JSON from the model-side script.
func (v *Validator) ValidateCandidate(ctx context.Context, dir string, c Candidate, commands [][]string) ([]CommandResult, error) {
	if v == nil || v.runAs == nil {
		return nil, errors.New("validation requires a dedicated model user: none is configured")
	}
	if len(commands) < 1 || len(commands) > 8 {
		return nil, errors.New("1–8 validation commands required")
	}
	for _, argv := range commands {
		if len(argv) < 1 || len(argv) > 32 || strings.TrimSpace(argv[0]) == "" {
			return nil, errors.New("bounded command argv required")
		}
		for _, arg := range argv {
			if strings.ContainsRune(arg, 0) {
				return nil, errors.New("NUL in command argv")
			}
		}
	}
	if err := sameCandidate(ctx, dir, c); err != nil {
		return nil, err
	}
	path := strings.Join(append(append([]string(nil), v.toolchain...), modelrun.DefaultPATH), ":")
	limit := commandTimeout
	cmd, err := modelrun.Command(ctx, modelrun.Spec{
		RunAs: v.runAs, Binary: "/bin/sh", Args: []string{"-c", validationScript(commands, limit)},
		Scratch: true, Env: map[string]string{"PATH": path},
		// Archive extraction, every command's limit plus its kill grace.
		Timeout: time.Duration(len(commands))*(limit+validationKillAfter) + time.Minute,
	})
	if err != nil {
		return nil, err
	}
	archive, feed := io.Pipe()
	archiveDone := make(chan error, 1)
	go func() {
		err := gitStream(ctx, dir, feed, "archive", "--format=tar", c.CandidateSHA)
		feed.CloseWithError(err)
		archiveDone <- err
	}()
	stdout := &boundedBuffer{limit: maxValidationReply}
	stderr := &boundedBuffer{limit: 4096}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = archive, stdout, stderr
	runErr := cmd.Run()
	archive.Close()
	archiveErr := <-archiveDone
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if runErr != nil {
		return nil, fmt.Errorf("validation on the model side failed: %w: %s", runErr, strings.TrimSpace(stderr.String()))
	}
	if archiveErr != nil {
		return nil, fmt.Errorf("streaming the candidate to the model side: %w", archiveErr)
	}
	results, err := parseValidationReply(stdout, commands, limit)
	if err != nil {
		return nil, err
	}
	if err := sameCandidate(ctx, dir, c); err != nil {
		return results, err
	}
	return results, nil
}

const (
	validationKillAfter = 5 * time.Second
	maxValidationReply  = 1 << 20
)

// validationScript is the model-side program. It runs as the model user inside
// the scratch directory modelrun created (its cwd, removed afterwards): it
// unpacks the tar on stdin, runs each command with a clean environment,
// private caches and a hard time and file-size limit, and prints one JSON
// document with a base64 body per command.
func validationScript(commands [][]string, limit time.Duration) string {
	var b strings.Builder
	seconds := int64((limit + time.Second - 1) / time.Second)
	b.WriteString(`w=$PWD; src=$w/src
mkdir "$src" "$w/home" || exit 125
tar -xf - -C "$src" || exit 125
j=
run() {
  i=$1; shift
  s=$(date +%s%N)
  ( cd "$src" && ulimit -f 4096 && exec /usr/bin/timeout --kill-after=` + strconv.FormatInt(int64(validationKillAfter/time.Second), 10) + `s ` + strconv.FormatInt(seconds, 10) + `s /usr/bin/env -i       PATH="$PATH" HOME="$w/home" TMPDIR="$w/home" GOCACHE="$w/home/go-cache" GOMODCACHE="$w/home/go-mod-cache"       GOFLAGS=-modcacherw GOPROXY=off GOTOOLCHAIN=local GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null       GIT_TERMINAL_PROMPT=0 GIT_NO_LAZY_FETCH=1 "$@" ) >"$w/out.$i" 2>&1 </dev/null
  rc=$?
  e=$(date +%s%N)
  ms=$(( (e - s) / 1000000 ))
  size=$(wc -c <"$w/out.$i")
  body=$(head -c ` + strconv.Itoa(MaxValidationOutput) + ` "$w/out.$i" | base64 -w0)
  trunc=false; [ "$size" -gt ` + strconv.Itoa(MaxValidationOutput) + ` ] && trunc=true
  to=false
  if [ "$rc" -eq 124 ] || { [ "$rc" -eq 137 ] && [ "$ms" -ge ` + strconv.FormatInt(seconds*1000, 10) + ` ]; }; then to=true; fi
  j="$j${j:+,}{\"exit\":$rc,\"timed_out\":$to,\"ms\":$ms,\"truncated\":$trunc,\"output_b64\":\"$body\"}"
}
`)
	for i, argv := range commands {
		b.WriteString("run " + strconv.Itoa(i))
		for _, arg := range argv {
			b.WriteString(" '" + strings.ReplaceAll(arg, "'", `'\''`) + "'")
		}
		b.WriteString("\n")
	}
	b.WriteString("printf '{\"results\":[%s]}\\n' \"$j\"\n")
	return b.String()
}

type validationReply struct {
	Results []struct {
		Exit      int    `json:"exit"`
		TimedOut  bool   `json:"timed_out"`
		MS        int64  `json:"ms"`
		Truncated bool   `json:"truncated"`
		OutputB64 string `json:"output_b64"`
	} `json:"results"`
}

// parseValidationReply accepts exactly one well-formed JSON document with one
// result per command; anything else (including trailing data, which is how
// model code writing to the script's stdout would show up) is an error.
func parseValidationReply(raw *boundedBuffer, commands [][]string, limit time.Duration) ([]CommandResult, error) {
	if raw.overflow {
		return nil, errors.New("validation reply exceeds its size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw.Bytes()))
	decoder.DisallowUnknownFields()
	var reply validationReply
	if err := decoder.Decode(&reply); err != nil {
		return nil, fmt.Errorf("validation reply is not valid JSON: %w", err)
	}
	if decoder.More() {
		return nil, errors.New("validation reply has trailing data")
	}
	if len(reply.Results) != len(commands) {
		return nil, fmt.Errorf("validation reply has %d results for %d commands", len(reply.Results), len(commands))
	}
	results := make([]CommandResult, 0, len(commands))
	for i, r := range reply.Results {
		body, err := base64.StdEncoding.DecodeString(r.OutputB64)
		if err != nil || len(body) > MaxValidationOutput {
			return nil, fmt.Errorf("validation reply output %d is malformed", i)
		}
		text := strings.ToValidUTF8(string(body), "\uFFFD")
		truncated := r.Truncated || len(text) > MaxValidationOutput
		if len(text) > MaxValidationOutput {
			text = text[:MaxValidationOutput]
			for !utf8.ValidString(text) {
				text = text[:len(text)-1]
			}
		}
		timedOut := r.TimedOut
		result := CommandResult{
			Argv: append([]string(nil), commands[i]...), ExitCode: r.Exit, TimedOut: timedOut,
			Passed: r.Exit == 0 && !timedOut, Output: text, Truncated: truncated,
		}
		results = append(results, result)
	}
	return results, nil
}
func sameCandidate(ctx context.Context, dir string, c Candidate) error {
	actual, err := inspectCandidate(ctx, dir, c.BaseSHA, c.CandidateSHA)
	if err != nil {
		return err
	}
	left, _ := json.Marshal(c)
	right, _ := json.Marshal(actual)
	if string(left) != string(right) {
		return errors.New("candidate identity changed during validation")
	}
	return nil
}
