package scheduler_test

import (
	"context"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/scheduler"
)

func TestTaskClassRoutingSelectsTheMappedExecutor(t *testing.T) {
	routing := scheduler.TaskClassRouting{"github.issue.triage": "github-issue-triage"}

	got, found, err := routing.PreferredExecutor(
		context.Background(), domain.Task{TaskClass: "github.issue.triage"},
		[]string{"copilot", "github-issue-triage"})
	if err != nil {
		t.Fatal(err)
	}
	if !found || got != "github-issue-triage" {
		t.Fatalf("routing returned (%q, %v), want (github-issue-triage, true)", got, found)
	}
}

func TestTaskClassRoutingDeclinesAnUnmappedClass(t *testing.T) {
	routing := scheduler.TaskClassRouting{"github.issue.triage": "github-issue-triage"}

	got, found, err := routing.PreferredExecutor(
		context.Background(), domain.Task{TaskClass: "shell"}, []string{"copilot"})
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatalf("an unmapped task class was routed to %q", got)
	}
}

func TestTaskClassRoutingRejectsWhenTheKindIsNotEligible(t *testing.T) {
	routing := scheduler.TaskClassRouting{"github.issue.triage": "github-issue-triage"}

	got, found, err := routing.PreferredExecutor(
		context.Background(), domain.Task{TaskClass: "github.issue.triage"}, []string{"copilot"})
	if err == nil {
		t.Fatal("missing mapped executor must block fallback")
	}
	if found {
		t.Fatalf("an ineligible executor kind was selected: %q", got)
	}
}

func TestTaskClassRoutingRejectsAnEmptyMapping(t *testing.T) {
	routing := scheduler.TaskClassRouting{"github.issue.triage": "  "}

	found, err := foundFor(routing)
	if err == nil {
		t.Fatal("empty mapped executor must block fallback")
	}
	if found {
		t.Fatal("a blank executor kind was selected")
	}
}

func foundFor(routing scheduler.TaskClassRouting) (bool, error) {
	_, found, err := routing.PreferredExecutor(
		context.Background(), domain.Task{TaskClass: "github.issue.triage"}, []string{"  "})
	return found, err
}
