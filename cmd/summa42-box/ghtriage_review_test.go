package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/ghtriage"
)

// A case whose review could not be performed has to reach the operator as a
// non-zero exit. The reviewer records nothing for it and changes nothing, so a
// command that exited 0 over one would report a healthy tick for ever and the
// only trace would be a line on stderr inside a loop nobody reads. Skipped is
// not a failure: a case with no decision to review is a case with nothing wrong
// with it.
func TestGHTriageReviewTickFailureFailsTheCommandOnAPerCaseFailure(t *testing.T) {
	if err := ghtriageReviewTickFailure(ghtriage.ReviewResult{Reviewed: 2, Skipped: 1}); err != nil {
		t.Fatalf("a tick that reviewed and skipped returned %v, want nil", err)
	}
	err := ghtriageReviewTickFailure(ghtriage.ReviewResult{Reviewed: 1, Failed: 2})
	if err == nil {
		t.Fatal("a tick with per-case failures returned nil")
	}
	for _, want := range []string{"2", "could not be reviewed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

// The reviewer is the one triage component that calls the model, so it takes the
// model flags the worker takes and the mission the driver takes. The two model
// flags are not optional: triageModelConfig reads them off the flag set, and a
// reviewer with no model would fail every case it looked at.
func TestParseGHTriageReviewFlagsTakesAMissionAndTheModelFlags(t *testing.T) {
	mission, config, err := parseGHTriageReviewFlags([]string{
		"--mission", "  mission-1  ", "--model-binary", "/usr/bin/codex", "--model-timeout", "5s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if mission != "mission-1" {
		t.Fatalf("mission = %q, want the trimmed id", mission)
	}
	if config.Binary != "/usr/bin/codex" || config.Timeout != 5*time.Second {
		t.Fatalf("model config = %+v, want the binary and timeout the flags carry", config)
	}
	for name, args := range map[string][]string{
		"no mission":          {"--model-binary", "/usr/bin/codex"},
		"blank":               {"--mission", "   "},
		"blank model":         {"--mission", "mission-1", "--model-binary", "   "},
		"unparseable timeout": {"--mission", "mission-1", "--model-timeout", "soon"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseGHTriageReviewFlags(args); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
	// The model flags have defaults, as the worker's do, so omitting them is not
	// an error: what has to be honoured is the value, and triageModelConfig is
	// what refuses a bad one.
	if _, config, err := parseGHTriageReviewFlags([]string{"--mission", "mission-1"}); err != nil {
		t.Fatalf("defaulted model flags: %v", err)
	} else if config.Binary != "codex" || config.Timeout != 60*time.Second {
		t.Fatalf("defaulted model config = %+v, want the worker's defaults", config)
	}
}

// The whole chain over a real Collective: the box opens, the index and the
// reviewer are built from it, the tick runs and the result comes back on stdout
// as JSON. A mission with no case to review is the cheapest way to prove every
// one of those happened, and it is the state a mission is in before intake runs.
func TestRunGHTriageReviewPrintsAnEmptyResultForAMissionWithNothingToReview(t *testing.T) {
	ctx := context.Background()
	config := initializedCollective(t)
	f := newTriageDriverFixture(t, config)
	f.Close()

	readStdout := captureStdout(t)
	err := runGHTriageReview(ctx, []string{
		"--mission", string(f.mission), "--model-binary", usableModelBinary(t), "--model-timeout", "5s",
	})
	if err != nil {
		t.Fatalf("run-gh-triage-review over a mission with nothing to review: %v", err)
	}
	var result ghtriage.ReviewResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(readStdout())), &result); err != nil {
		t.Fatalf("stdout is not a review result: %v", err)
	}
	if result.Reviewed != 0 || result.Skipped != 0 || result.Failed != 0 {
		t.Fatalf("result = %+v, want every counter zero", result)
	}
}

// A reviewer that cannot ask its one question has nothing to do, and saying so
// is the difference between an operator fixing a path and an operator reading a
// mission of zero verdicts. The box's own notice explains the executor it did
// not register; this is the one that stops the command.
func TestRunGHTriageReviewFailsTheCommandWithoutAUsableModel(t *testing.T) {
	ctx := context.Background()
	config := initializedCollective(t)
	f := newTriageDriverFixture(t, config)
	f.Close()

	readStdout := captureStdout(t)
	readStderr := captureStderr(t)
	err := runGHTriageReview(ctx, []string{
		"--mission", string(f.mission), "--model-binary", filepath.Join(t.TempDir(), "absent"),
	})
	if err == nil {
		t.Fatal("run-gh-triage-review ran with no model binary")
	}
	if !strings.Contains(err.Error(), "--model-binary") {
		t.Fatalf("error %q does not name the flag that is wrong", err)
	}
	if out := readStdout(); strings.TrimSpace(out) != "" {
		t.Fatalf("stdout = %q, want nothing: the command did not run a tick", out)
	}
	if stderr := readStderr(); !strings.Contains(stderr, "not registered") {
		t.Fatalf("stderr = %q, want the box's own degraded-model notice", stderr)
	}
}

// The identity is a mission ID, and a mistyped one is not a command error: the
// reviewer finds no cases and reports the empty tick it actually ran. The same
// is true of the context, which no subcommand may run without.
func TestRunGHTriageReviewRefusesToRunWithoutAMissionOrContext(t *testing.T) {
	initializedCollective(t)
	if err := runGHTriageReview(context.Background(), nil); err == nil {
		t.Fatal("run-gh-triage-review ran without --mission")
	}
	if err := runGHTriageReview(nil, []string{"--mission", "mission-1"}); err == nil {
		t.Fatal("run-gh-triage-review ran with no context")
	}
}
