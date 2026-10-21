package main

import (
	"context"
	"encoding/json"
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/ghtriage"
)

func TestRunGHPlanPrintsEmptyResultForMissionWithoutCandidates(t *testing.T) {
	config := initializedCollective(t)
	f := newTriageDriverFixture(t, config)
	f.Close()
	readStdout := captureStdout(t)
	err := runGHPlan(context.Background(), []string{
		"--mission", string(f.mission), "--model-binary", usableModelBinary(t), "--model-timeout", "5s",
	})
	if err != nil {
		t.Fatal(err)
	}
	var result ghtriage.PlannerResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(readStdout())), &result); err != nil {
		t.Fatal(err)
	}
	if result.Planned != 0 || len(result.Failures) != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestRunGHPlanRejectsMissingMissionAndContext(t *testing.T) {
	if err := runGHPlan(context.Background(), nil); err == nil {
		t.Fatal("accepted no mission")
	}
	if err := runGHPlan(nil, []string{"--mission", "mission-1"}); err == nil {
		t.Fatal("accepted nil context")
	}
}

func TestPlannerConfigUsesModelTimeoutFlag(t *testing.T) {
	flags := flag.NewFlagSet("run-gh-plan", flag.ContinueOnError)
	registerTriageModelFlags(flags, "model")
	if err := flags.Parse([]string{"--model-timeout", "150s", "--model-binary", usableModelBinary(t)}); err != nil {
		t.Fatal(err)
	}
	modelConfig, err := triageModelConfig(flags)
	if err != nil {
		t.Fatal(err)
	}
	if got := plannerConfig(modelConfig).ModelTimeout; got != 150*time.Second {
		t.Fatalf("planner ModelTimeout = %v, want 150s", got)
	}
}
