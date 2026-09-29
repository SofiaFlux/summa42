package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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
