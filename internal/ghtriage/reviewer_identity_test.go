package ghtriage

import (
	"encoding/json"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
)

func TestFallbackVerdictsAreMarkedAsFallbacks(t *testing.T) {
	primary := &ReviewerIdentity{Harness: "commandcode", Model: "deepseek/deepseek-v4-pro"}
	fallback := &ReviewerIdentity{Harness: "commandcode", Model: "reflection/beam:free", FallbackOf: "commandcode:plan"}
	if isFallbackReview(nil) || isFallbackReview(primary) {
		t.Fatal("a primary or unrecorded reviewer was marked as a fallback")
	}
	if !isFallbackReview(fallback) {
		t.Fatal("a replacement reviewer was not marked as a fallback")
	}
}

func TestBindingRequiresTheExpectedReviewer(t *testing.T) {
	primary := &ReviewerIdentity{Harness: "commandcode", Model: "deepseek/deepseek-v4-pro"}
	other := &ReviewerIdentity{Harness: "commandcode", Model: "reflection/beam:free", FallbackOf: "commandcode:plan"}
	if !sameReviewer(nil, nil) || !sameReviewer(primary, &ReviewerIdentity{Harness: "commandcode", Model: "deepseek/deepseek-v4-pro"}) {
		t.Fatal("equal reviewers were not the same")
	}
	if sameReviewer(primary, nil) || sameReviewer(nil, primary) || sameReviewer(primary, other) {
		t.Fatal("different reviewers were the same")
	}
}

// A record written before reviewers were recorded has no reviewer field; it
// must still bind to an expectation that has none.
func TestLegacyPlanReviewRecordStillBinds(t *testing.T) {
	task := domain.Task{ID: "task-1", CurrentAttemptID: "attempt-1"}
	expected := PlanReviewRecord{Schema: PlanReviewVersion, CaseID: "case-1", WorkID: "work-1", PlanEvidenceID: "plan-1", ContextEvidenceID: "ctx-1", BaseSHA: "base"}
	record := expected
	record.TaskID, record.AttemptID = task.ID, task.CurrentAttemptID
	record.PlanReviewOutput = PlanReviewOutput{Verdict: "ACCEPT", Reason: "ok"}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	object := evidence.EvidenceObject{Kind: KindPlanReview}
	if _, err := validateReviewRecord(object, raw, expected, task); err != nil {
		t.Fatalf("legacy record refused: %v", err)
	}
}

// A verdict from a replacement reviewer does not bind to the primary's expectation.
func TestPlanReviewRecordFromAnotherReviewerIsRefused(t *testing.T) {
	task := domain.Task{ID: "task-1", CurrentAttemptID: "attempt-1"}
	expected := PlanReviewRecord{Schema: PlanReviewVersion, CaseID: "case-1", WorkID: "work-1", PlanEvidenceID: "plan-1", ContextEvidenceID: "ctx-1", BaseSHA: "base",
		Reviewer: &ReviewerIdentity{Harness: "commandcode", Model: "deepseek/deepseek-v4-pro"}}
	record := expected
	record.Reviewer = &ReviewerIdentity{Harness: "commandcode", Model: "reflection/beam:free", FallbackOf: "commandcode:plan"}
	record.TaskID, record.AttemptID = task.ID, task.CurrentAttemptID
	record.PlanReviewOutput = PlanReviewOutput{Verdict: "ACCEPT", Reason: "ok"}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	object := evidence.EvidenceObject{Kind: KindPlanReview}
	if _, err := validateReviewRecord(object, raw, expected, task); err == nil {
		t.Fatal("a fallback verdict bound to the primary reviewer's expectation")
	}
}

// The task payload must not depend on the reviewer: the task is keyed by work,
// so a replacement reviewer has to reuse the work's task, not conflict with it.
func TestTaskPayloadIgnoresTheReviewer(t *testing.T) {
	plain := PlanReviewRecord{Schema: PlanReviewVersion, CaseID: "case-1", WorkID: "work-1", BaseSHA: "base"}
	fallback := plain
	fallback.Reviewer = &ReviewerIdentity{Harness: "opencode", Model: "opencode/big-pickle", FallbackOf: "commandcode:plan"}
	a, err := planReviewPayload(plain)
	if err != nil {
		t.Fatal(err)
	}
	b, err := planReviewPayload(fallback)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("plan payload changes with the reviewer:\n%s\n%s", a, b)
	}

	code := CodeReviewRecord{Schema: CodeReviewVersion, CaseID: "case-1", WorkID: "work-1"}
	codeFallback := code
	codeFallback.Reviewer = fallback.Reviewer
	c, err := codeReviewPayload(code)
	if err != nil {
		t.Fatal(err)
	}
	d, err := codeReviewPayload(codeFallback)
	if err != nil {
		t.Fatal(err)
	}
	if string(c) != string(d) {
		t.Fatalf("code payload changes with the reviewer:\n%s\n%s", c, d)
	}
}

func TestPrimaryReviewerRecordsNoIdentity(t *testing.T) {
	if recordedReviewer(&ReviewerIdentity{Harness: "commandcode", Model: "deepseek/deepseek-v4-pro"}) != nil {
		t.Fatal("the primary reviewer recorded an identity; in-flight records would stop binding")
	}
	if recordedReviewer(&ReviewerIdentity{Harness: "opencode", FallbackOf: "x"}) == nil {
		t.Fatal("a replacement reviewer recorded no identity")
	}
}
