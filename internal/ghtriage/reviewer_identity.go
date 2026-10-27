package ghtriage

// ReviewerIdentity says which model gave a verdict. A replacement reviewer
// names the pool it stands in for, so a fallback verdict is never mistaken for
// the primary reviewer's. Records written before this field existed have none.
type ReviewerIdentity struct {
	Harness    string `json:"harness"`
	Model      string `json:"model"`
	FallbackOf string `json:"fallback_of,omitempty"`
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
