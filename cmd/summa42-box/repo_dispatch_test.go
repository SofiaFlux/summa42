package main

import (
	"context"
	"strings"
	"testing"
)

func TestRepoDispatchAliasesAccepted(t *testing.T) {
	ctx := context.Background()

	// Testing run-repo-triage-driver with empty args yields standard missing --mission error
	err := runGHTriageDriver(ctx, []string{})
	if err == nil || !strings.Contains(err.Error(), "run-gh-triage-driver requires --mission") {
		t.Fatalf("unexpected error for runGHTriageDriver: %v", err)
	}

	// Testing run-repo-intake with empty args yields standard required flags error
	_, _, err = parseGHIntakeFlags([]string{})
	if err == nil || !strings.Contains(err.Error(), "run-gh-intake requires") {
		t.Fatalf("unexpected error for parseGHIntakeFlags: %v", err)
	}

	// Testing run-repo-plan with empty args yields standard required flags error
	err = runGHPlan(ctx, []string{})
	if err == nil || !strings.Contains(err.Error(), "run-gh-plan requires --mission") {
		t.Fatalf("unexpected error for runGHPlan: %v", err)
	}

	// Testing run-repo-plan-review with empty args yields standard required flags error
	err = runGHPlanReview(ctx, []string{})
	if err == nil || !strings.Contains(err.Error(), "run-gh-plan-review requires --mission") {
		t.Fatalf("unexpected error for runGHPlanReview: %v", err)
	}
}
