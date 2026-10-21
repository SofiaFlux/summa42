package main

import (
	"strings"
	"testing"
)

func TestFeedbackHumanOutputSeparatesLocalAndExportSurfaces(t *testing.T) {
	api := &fakeControlAPI{}
	out := executeCommand(t, newRootCommandWithClient(api), "feedback", "inspect", "feedback-1")
	if !strings.Contains(out, "LOCAL — DO NOT EXPORT") || !strings.Contains(out, "SANITIZED EXPORT ARTIFACT") {
		t.Fatalf("inspect output=%q", out)
	}
	out = executeCommand(t, newRootCommandWithClient(api), "feedback", "emit", "feedback-1")
	if !strings.Contains(out, "Scheduled governed work") || !strings.Contains(out, "No issue was sent directly") {
		t.Fatalf("emit output=%q", out)
	}
	out = executeCommand(t, newRootCommandWithClient(api), "feedback", "observe", "--category", "TEST", "--summary", "local sensitive")
	if !strings.Contains(out, "LOCAL — DO NOT EXPORT") || strings.Contains(out, "local sensitive") {
		t.Fatalf("observe output leaks or lacks label: %q", out)
	}
	out = executeCommand(t, newRootCommandWithClient(api), "feedback", "scan")
	if strings.Contains(out, "local-only") || !strings.Contains(out, "RECOVERY_FRICTION") {
		t.Fatalf("scan output=%q", out)
	}
	out = executeCommand(t, newRootCommandWithClient(api), "feedback", "candidate",
		"--observation", "obs-1", "--generic-task-class", "DEBUGGING", "--category", "TEST",
		"--expected", "local expected", "--observed", "local observed")
	if api.feedbackCandidateCalls != 1 || !strings.Contains(out, "LOCAL — DO NOT EXPORT") {
		t.Fatalf("candidate calls=%d output=%q", api.feedbackCandidateCalls, out)
	}
	out = executeCommand(t, newRootCommandWithClient(api), "feedback", "sanitize", "feedback-created")
	if api.feedbackSanitizeCalls != 1 || !strings.Contains(out, "Immutable artifact sanitized-created") {
		t.Fatalf("sanitize calls=%d output=%q", api.feedbackSanitizeCalls, out)
	}
}
