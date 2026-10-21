package main

import (
	"strings"
	"testing"
)

func TestExperienceCLIWiresProposalOutcomeAndEvaluation(t *testing.T) {
	api := &fakeControlAPI{}
	out := executeCommand(t, newRootCommandWithClient(api), "experience", "propose",
		"--grant", "grant-1", "--executor", "codex", "--evidence-observation", "obs-1")
	if api.experienceProposalCalls != 1 || !strings.Contains(out, "canonical scope repo.review") {
		t.Fatalf("proposal calls=%d output=%q", api.experienceProposalCalls, out)
	}
	out = executeCommand(t, newRootCommandWithClient(api), "experience", "observe-outcome", "task-1", "--accepted")
	if api.experienceOutcomeCalls != 1 || !strings.Contains(out, "derived from canonical state") {
		t.Fatalf("outcome calls=%d output=%q", api.experienceOutcomeCalls, out)
	}
	out = executeCommand(t, newRootCommandWithClient(api), "experience", "evaluate", "proposal-1")
	if api.experienceEvaluateCalls != 1 || !strings.Contains(out, "verified samples=3") {
		t.Fatalf("evaluate calls=%d output=%q", api.experienceEvaluateCalls, out)
	}
}
