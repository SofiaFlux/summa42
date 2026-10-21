package domain

import "testing"

func TestOperationStateTerminality(t *testing.T) {
	if !OperationConfirmedEffect.Terminal() {
		t.Fatal("confirmed effect must be terminal")
	}
	if OperationPrepared.Terminal() {
		t.Fatal("prepared is not terminal")
	}
}

func TestAddendumTaskStatesAreDefined(t *testing.T) {
	if TaskChallenged != "CHALLENGED" || TaskExpired != "EXPIRED" {
		t.Fatalf("unexpected addendum states: %q %q", TaskChallenged, TaskExpired)
	}
}

func TestFieldFeedbackStatesAreDefined(t *testing.T) {
	if FeedbackStateCandidate != "CANDIDATE" || FeedbackStateReported != "REPORTED" || FeedbackStateRejectedUnsafe != "REJECTED_UNSAFE" {
		t.Fatalf("unexpected feedback states: %q %q %q", FeedbackStateCandidate, FeedbackStateReported, FeedbackStateRejectedUnsafe)
	}
	if SanitizationPass != "PASS" || SanitizationUncertain != "UNCERTAIN" {
		t.Fatalf("unexpected sanitization states: %q %q", SanitizationPass, SanitizationUncertain)
	}
	if ApprovalPending != "PENDING" || ApprovalConsumed != "CONSUMED" {
		t.Fatalf("unexpected approval states: %q %q", ApprovalPending, ApprovalConsumed)
	}
	if ExperienceShadow != "SHADOW" || ExperienceRolledBack != "ROLLED_BACK" {
		t.Fatalf("unexpected experience states: %q %q", ExperienceShadow, ExperienceRolledBack)
	}
}
