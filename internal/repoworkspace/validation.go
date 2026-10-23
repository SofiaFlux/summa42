package repoworkspace

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/SofiaFlux/summa42/internal/modelrun"
)

// DefaultCommandTimeout is the per-command validation limit. It is generous
// on purpose: a cold build of a large module can take minutes.
const DefaultCommandTimeout = 10 * time.Minute

// maxCommandTimeout bounds a configured per-command limit.
const maxCommandTimeout = 24 * time.Hour

// archiveTimeout bounds streaming the candidate archive; its pace is set by
// the model side's tar and the machine, so it is far above the 30 s of the
// other git calls.
const archiveTimeout = 5 * time.Minute

// fileSizeBlocks is the validation file-size limit in 512-byte blocks (dash's
// unit; bash counts 1024-byte blocks, which only doubles it): 2 GiB. It exists
// to stop a runaway single file, not to bound disk use; the compiler's package
// archives alone exceed a few MiB. Disk use is bounded by the scratch tree
// being removed after every run and by the per-command time limit.
const fileSizeBlocks = 4194304
const MaxValidationOutput = 16 * 1024

type CommandResult struct {
	Argv      []string `json:"argv"`
	ExitCode  int      `json:"exit_code"`
	Passed    bool     `json:"passed"`
	TimedOut  bool     `json:"timed_out"`
	Output    string   `json:"output"`
	Truncated bool     `json:"truncated"`
}

// Validator runs the operator's validation commands against a candidate as a
// dedicated validation user. The commands execute model-written code (a test
// the model added runs in `go test`), so they must never run with the Box's
// uid (it can read the Box keys, database and tokens) nor with the model
// user's (its home holds the CLI logins and configuration the independent code
// review relies on). The validation user has no credentials at all.
type Validator struct {
	runAs     *user.User
	modelHome string
	toolchain []string
	timeout   time.Duration
	logf      func(format string, args ...any)
	lockPath  string
	// One validation at a time: the sweep after a run kills every process of
	// the validation user and removes its scratch directories.
	mu sync.Mutex
}

// NewValidator fails closed: the validation user must be named and usable
// (modelrun.ResolveValidationUser: not root, not the Box user, not the model
// user) and so must the model user, whose home the validation user must not be
// able to read. toolchainDirs are absolute directories put in front of the
// validation user's fixed PATH (/usr/local/bin:/usr/bin:/bin) so the Go
// toolchain is found there; they must be readable and executable by the
// validation user. The Box's own PATH is never forwarded. commandTimeout is
// the per-command limit; zero means DefaultCommandTimeout.
func NewValidator(validationUser, modelUser string, toolchainDirs []string, commandTimeout time.Duration, lockPath string) (*Validator, error) {
	model, err := modelrun.ResolveRunAsUser(modelUser)
	if err != nil {
		return nil, fmt.Errorf("validation: %w", err)
	}
	runAs, err := modelrun.ResolveValidationUser(validationUser, model)
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
	if commandTimeout == 0 {
		commandTimeout = DefaultCommandTimeout
	}
	if commandTimeout < time.Second || commandTimeout > maxCommandTimeout {
		return nil, fmt.Errorf("validation command timeout %s must be between 1s and %s", commandTimeout, maxCommandTimeout)
	}
	lockPath = strings.TrimSpace(lockPath)
	if !filepath.IsAbs(lockPath) {
		return nil, errors.New("validation lock path must be absolute: the lock serializes validation across Box processes")
	}
	return &Validator{runAs: runAs, modelHome: model.HomeDir, toolchain: dirs, timeout: commandTimeout, logf: log.Printf, lockPath: lockPath}, nil
}

// Timeout returns the per-command validation timeout.
func (v *Validator) Timeout() time.Duration {
	return v.timeout
}

// ValidateCandidate streams the candidate commit (git archive) to the
// validation user, runs each command in a private scratch copy under that
// user's ~/tmp with a private GOMODCACHE and a per-run GOCACHE, and returns
// the recorded results. The Box keeps the evidence; it only sees bounded output, exit codes
// and timings, as JSON from the script. Whatever the outcome or cancel, every
// process of the validation user is killed and its scratch removed afterwards.
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
	v.mu.Lock()
	defer v.mu.Unlock()

	// Cross-process serialization: an exclusive OS file lock around the whole
	// validation (including the sweep), so two Box processes validating at
	// once cannot kill each other's commands via the validation user sweep.
	releaseLock, err := lockFile(ctx, v.lockPath)
	if err != nil {
		return nil, err
	}
	defer releaseLock()
	defer v.sweep(ctx)
	if err := sameCandidate(ctx, dir, c); err != nil {
		return nil, err
	}
	if err := ignoreExportAttributes(ctx, dir); err != nil {
		return nil, err
	}
	path := strings.Join(append(append([]string(nil), v.toolchain...), modelrun.DefaultPATH), ":")
	limit := v.timeout
	cmd, err := modelrun.Command(ctx, modelrun.Spec{
		RunAs: v.runAs, Binary: "/bin/sh", Args: []string{"-c", validationScript(commands, limit)},
		Scratch: true, Env: map[string]string{"PATH": path}, Unreadable: []string{v.modelHome},
		// Archive streaming, every command's limit plus its kill grace.
		Timeout: archiveTimeout + time.Duration(len(commands))*(limit+validationKillAfter) + time.Minute,
	})
	if err != nil {
		return nil, err
	}
	archive, feed := io.Pipe()
	archiveDone := make(chan error, 1)
	go func() {
		err := gitStreamTimeout(ctx, dir, feed, archiveTimeout, "archive", "--format=tar", c.CandidateSHA)
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
	b.WriteString(`w=$PWD; src=$w/src; gc=$w/go-cache
mkdir "$src" "$w/home" "$w/t" "$gc" || exit 125
tar -xf - -C "$src" || exit 125
j=; p=
# The command runs in its own process group (coreutils timeout makes one), so a
# TERM to this script's group would miss it: kill that group here, before the
# scratch is removed. The sweep after the run is the backstop.
cancel() {
  trap '' TERM HUP INT
  if [ -n "$p" ]; then kill -s KILL -- "-$p" 2>/dev/null; kill -s KILL "$p" 2>/dev/null; fi
  exit 143
}
trap cancel TERM HUP INT
run() {
  i=$1; shift
  s=$(date +%s%N)
  ( cd "$src" && ulimit -f ` + strconv.Itoa(fileSizeBlocks) + ` && exec /usr/bin/timeout --kill-after=` + strconv.FormatInt(int64(validationKillAfter/time.Second), 10) + `s ` + strconv.FormatInt(seconds, 10) + `s /usr/bin/env -i       PATH="$PATH" HOME="$w/home" TMPDIR="$w/t" GOCACHE="$gc" GOMODCACHE="$w/home/go-mod-cache"       GOFLAGS=-modcacherw GOPROXY=off GOTOOLCHAIN=local GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null       GIT_TERMINAL_PROMPT=0 GIT_NO_LAZY_FETCH=1 "$@" ) >"$w/out.$i" 2>&1 </dev/null &
  p=$!
  wait "$p"
  rc=$?
  p=
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

// sweep is the backstop after every run: kill whatever the validation user
// still has (escaped daemons included) and remove its scratch. Best effort,
// logged, and independent of the caller's cancellation.
func (v *Validator) sweep(ctx context.Context) {
	sweepCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := modelrun.Sweep(sweepCtx, v.runAs); err != nil {
		v.logf("validation: sweeping user %s after a run failed: %v", v.runAs.Username, err)
	}
}

// ignoreExportAttributes makes `git archive` ignore attributes from the
// archived tree: a candidate's .gitattributes could otherwise hide files
// (export-ignore) or rewrite them (export-subst) in the validated copy.
// $GIT_DIR/info/attributes has the highest precedence and, unlike in-tree
// files, is not part of the candidate. The checkout is a clone the Box
// created, so this file is the Box's to write; only these two attributes are
// set, so the diff and the candidate identity are unchanged.
func ignoreExportAttributes(ctx context.Context, dir string) error {
	out, err := git(ctx, dir, "rev-parse", "--git-path", "info/attributes")
	if err != nil {
		return err
	}
	target := strings.TrimSpace(string(out))
	if target == "" {
		return errors.New("cannot locate the checkout's info/attributes")
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, []byte("* -export-ignore -export-subst\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, target)
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
	// More() is false before a stray } or ], so check the bytes themselves.
	if len(bytes.TrimSpace(raw.Bytes()[decoder.InputOffset():])) != 0 {
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
