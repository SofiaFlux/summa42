package fieldfeedback

import (
	"context"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
)

func TestCandidateCorrelationUsesOnlyControlledSafeShape(t *testing.T) {
	observer, taskID, attemptID, _, _ := newObserverHarness(t)
	ctx := context.Background()
	observation, err := observer.Record(ctx, ObservationInput{
		TaskID: taskID, AttemptID: attemptID, Category: "RECOVERY_FRICTION",
		BasisClass: "TEST", SourceKind: "TEST", SummaryLocal: "client secret one",
		Enforcement: domain.EnforcementEnforced,
	})
	if err != nil {
		t.Fatal(err)
	}
	feedback := NewFeedback(observer.store, observer.clock)
	first, err := feedback.CreateCandidate(ctx, CandidateInput{
		ObservationIDs:    []domain.ID{observation.ID},
		GenericTaskClass:  domain.GenericTaskDebugging,
		Category:          "RECOVERY_FRICTION",
		ExpectedBehavior:  "recover without operator",
		ObservedBehavior:  "operator intervened for client A",
		StateTransitions:  []string{"EXECUTING", "FAILED", "ELIGIBLE", "EXECUTING"},
		Metrics:           NormalizedMetrics{RetryCount: int64ptr(2)},
		HumanIntervention: true, RecoveryResult: "RECOVERED",
		RuntimeVersion: "v1", ExecutorKind: "codex", ExecutorVersion: "x",
		Enforcement: domain.EnforcementEnforced,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := feedback.CreateCandidate(ctx, CandidateInput{
		ObservationIDs:    []domain.ID{observation.ID},
		GenericTaskClass:  domain.GenericTaskDebugging,
		Category:          "RECOVERY_FRICTION",
		ExpectedBehavior:  "different free text",
		ObservedBehavior:  "different customer-specific text",
		StateTransitions:  []string{"EXECUTING", "FAILED", "ELIGIBLE", "EXECUTING"},
		Metrics:           NormalizedMetrics{RetryCount: int64ptr(999)},
		HumanIntervention: false, RecoveryResult: "FAILED",
		RuntimeVersion: "v999", ExecutorKind: "codex", ExecutorVersion: "other",
		Enforcement: domain.EnforcementEnforced,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.CorrelationKey != second.CorrelationKey {
		t.Fatalf("safe-equivalent candidates got different correlation keys: %s != %s", first.CorrelationKey, second.CorrelationKey)
	}
	if first.ID == second.ID {
		t.Fatal("candidate creation unexpectedly deduplicated distinct local records")
	}
}

func TestCandidateRejectsUnsafeClassificationAndTransitionTokens(t *testing.T) {
	observer, taskID, _, _, _ := newObserverHarness(t)
	ctx := context.Background()
	observation, err := observer.Record(ctx, ObservationInput{
		TaskID: taskID, Category: "TEST", BasisClass: "TEST", SourceKind: "TEST",
		SummaryLocal: "local", Enforcement: domain.EnforcementEnforced,
	})
	if err != nil {
		t.Fatal(err)
	}
	feedback := NewFeedback(observer.store, observer.clock)

	if _, err := feedback.CreateCandidate(ctx, CandidateInput{
		ObservationIDs:   []domain.ID{observation.ID},
		GenericTaskClass: domain.GenericTaskClass("client-x/payment-migration"),
		Category:         "TEST", ExpectedBehavior: "x", ObservedBehavior: "y",
		StateTransitions: []string{"EXECUTING"}, Enforcement: domain.EnforcementEnforced,
	}); err == nil {
		t.Fatal("organization-specific GenericTaskClass was accepted")
	}
	if _, err := feedback.CreateCandidate(ctx, CandidateInput{
		ObservationIDs:   []domain.ID{observation.ID},
		GenericTaskClass: domain.GenericTaskOther,
		Category:         "TEST", ExpectedBehavior: "x", ObservedBehavior: "y",
		StateTransitions: []string{"client/internal/path"}, Enforcement: domain.EnforcementEnforced,
	}); err == nil {
		t.Fatal("arbitrary transition token was accepted")
	}
}

func TestCandidateLifecycleRejectsIllegalTerminalTransitions(t *testing.T) {
	observer, taskID, _, _, _ := newObserverHarness(t)
	ctx := context.Background()
	observation, err := observer.Record(ctx, ObservationInput{
		TaskID: taskID, Category: "TEST", BasisClass: "TEST", SourceKind: "TEST",
		SummaryLocal: "local", Enforcement: domain.EnforcementEnforced,
	})
	if err != nil {
		t.Fatal(err)
	}
	feedback := NewFeedback(observer.store, observer.clock)
	candidate, err := feedback.CreateCandidate(ctx, CandidateInput{
		ObservationIDs:   []domain.ID{observation.ID},
		GenericTaskClass: domain.GenericTaskTesting,
		Category:         "TEST", ExpectedBehavior: "x", ObservedBehavior: "y",
		StateTransitions: []string{"EXECUTING", "AWAITING_VERIFICATION"},
		Enforcement:      domain.EnforcementEnforced,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := feedback.transitionCandidate(ctx, candidate.ID, domain.FeedbackStateSanitizing); err != nil {
		t.Fatal(err)
	}
	if err := feedback.transitionCandidate(ctx, candidate.ID, domain.FeedbackStateSanitized); err != nil {
		t.Fatal(err)
	}
	if err := feedback.transitionCandidate(ctx, candidate.ID, domain.FeedbackStateExportReady); err != nil {
		t.Fatal(err)
	}
	if err := feedback.transitionCandidate(ctx, candidate.ID, domain.FeedbackStateReported); err != nil {
		t.Fatal(err)
	}
	if err := feedback.transitionCandidate(ctx, candidate.ID, domain.FeedbackStateLocalOnly); err == nil {
		t.Fatal("REPORTED candidate transitioned again")
	}

	second, err := feedback.CreateCandidate(ctx, CandidateInput{
		ObservationIDs:   []domain.ID{observation.ID},
		GenericTaskClass: domain.GenericTaskTesting,
		Category:         "TEST", ExpectedBehavior: "x", ObservedBehavior: "z",
		StateTransitions: []string{"EXECUTING"}, Enforcement: domain.EnforcementEnforced,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := feedback.transitionCandidate(ctx, second.ID, domain.FeedbackStateLocalOnly); err != nil {
		t.Fatal(err)
	}
	if err := feedback.transitionCandidate(ctx, second.ID, domain.FeedbackStateExportReady); err == nil {
		t.Fatal("LOCAL_ONLY transitioned to EXPORT_READY")
	}
}

func int64ptr(v int64) *int64 { return &v }
