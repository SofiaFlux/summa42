package repoworkspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
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

func ValidateCandidate(ctx context.Context, dir string, c Candidate, commands [][]string) ([]CommandResult, error) {
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
	home, err := os.MkdirTemp("", "summa42-validation-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(home)
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + home, "GOCACHE=" + filepath.Join(home, "go-cache"), "GOPROXY=off", "GOTOOLCHAIN=local", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0"}
	var results []CommandResult
	for _, argv := range commands {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		commandCtx, cancel := context.WithTimeout(ctx, ValidationTimeout)
		cmd := exec.CommandContext(commandCtx, argv[0], argv[1:]...)
		cmd.Dir = dir
		cmd.Env = env
		isolateValidationProcess(cmd)
		output := &boundedBuffer{limit: MaxValidationOutput}
		cmd.Stdout = output
		cmd.Stderr = output
		runErr := cmd.Run()
		text := strings.ToValidUTF8(output.String(), "\uFFFD")
		truncated := output.overflow || len(text) > MaxValidationOutput
		if len(text) > MaxValidationOutput {
			text = text[:MaxValidationOutput]
			for !utf8.ValidString(text) {
				text = text[:len(text)-1]
			}
		}
		result := CommandResult{Argv: append([]string(nil), argv...), ExitCode: 0, Passed: runErr == nil, TimedOut: commandCtx.Err() != nil, Output: text, Truncated: truncated}
		if runErr != nil {
			result.ExitCode = -1
			var exit *exec.ExitError
			if errors.As(runErr, &exit) {
				result.ExitCode = exit.ExitCode()
			}
		}
		cancel()
		results = append(results, result)
		if err := sameCandidate(ctx, dir, c); err != nil {
			return results, err
		}
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
