package ghtriage

import "encoding/json"

// ReviewerIdentity says which model gave a verdict. A replacement reviewer
// names the pool it stands in for, so a fallback verdict is never mistaken for
// the primary reviewer's. Records written before this field existed have none.
type ReviewerIdentity struct {
	Harness    string `json:"harness"`
	Model      string `json:"model"`
	FallbackOf string `json:"fallback_of,omitempty"`
}

// recordedReviewer is the identity a record carries. The primary reviewer
// records none, so its records stay byte-identical to those written before
// reviewers were recorded, and tasks and verdicts already in flight still bind.
func recordedReviewer(id *ReviewerIdentity) *ReviewerIdentity {
	if id == nil || id.FallbackOf == "" {
		return nil
	}
	return id
}

// planReviewPayload is the task payload. It is keyed by work, so it must not
// change with the reviewer: a replacement then reuses the work's task.
func planReviewPayload(expected PlanReviewRecord) ([]byte, error) {
	expected.Reviewer = nil
	return json.Marshal(expected)
}

// codeReviewPayload is the task payload, kept reviewer-free for the same reason.
func codeReviewPayload(expected CodeReviewRecord) ([]byte, error) {
	expected.Reviewer = nil
	return json.Marshal(expected)
}

// storedReviewer is the reviewer a stored verdict names. A completed review is
// bound to its own reviewer, not to whichever reviewer the stage routes to now,
// so an existing verdict is reused instead of being refused for its reviewer.
func storedReviewer(raw []byte) *ReviewerIdentity {
	var stored struct {
		Reviewer *ReviewerIdentity `json:"reviewer"`
	}
	if json.Unmarshal(raw, &stored) != nil {
		return nil
	}
	return stored.Reviewer
}

// isFallbackReview reports whether a replacement reviewer gave the verdict.
func isFallbackReview(id *ReviewerIdentity) bool {
	return id != nil && id.FallbackOf != ""
}

// sameReviewer compares the identities a record claims with the expected ones.
func sameReviewer(a, b *ReviewerIdentity) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
