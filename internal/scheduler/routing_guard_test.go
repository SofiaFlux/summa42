package scheduler_test

import (
	"context"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/scheduler"
)

func TestMissingExecutorBlocksMappedClass(t *testing.T) {
	routing := scheduler.TaskClassRouting{"github.issue.triage": "github-issue-triage"}
	_, _, err := routing.PreferredExecutor(context.Background(), domain.Task{TaskClass: "github.issue.triage"}, []string{"copilot"})
	if err == nil {
		t.Fatal("missing required executor silently permits the alphabetical fallback")
	}
}

func TestBlankExecutorBlocksMappedClass(t *testing.T) {
	routing := scheduler.TaskClassRouting{"github.issue.triage": " "}
	_, _, err := routing.PreferredExecutor(context.Background(), domain.Task{TaskClass: "github.issue.triage"}, []string{"copilot"})
	if err == nil {
		t.Fatal("invalid required routing silently permits fallback")
	}
}
