package ghpublish

import (
	"net/http"
	"strings"
	"testing"
)

func TestObserveCI(t *testing.T) {
	headSHA := strings.Repeat("a", 40)
	tests := []struct {
		name           string
		checkRuns      []map[string]any
		requiredChecks []string
		expectedState  CIState
	}{
		{
			name: "all completed success",
			checkRuns: []map[string]any{
				{"name": "build", "status": "completed", "conclusion": "success", "head_sha": headSHA},
				{"name": "test", "status": "completed", "conclusion": "success", "head_sha": headSHA},
			},
			requiredChecks: []string{"build", "test"},
			expectedState:  CIStateSuccess,
		},
		{
			name: "one check in progress",
			checkRuns: []map[string]any{
				{"name": "build", "status": "completed", "conclusion": "success", "head_sha": headSHA},
				{"name": "test", "status": "in_progress", "conclusion": nil, "head_sha": headSHA},
			},
			requiredChecks: []string{"build", "test"},
			expectedState:  CIStatePending,
		},
		{
			name: "one check failed",
			checkRuns: []map[string]any{
				{"name": "build", "status": "completed", "conclusion": "success", "head_sha": headSHA},
				{"name": "test", "status": "completed", "conclusion": "failure", "head_sha": headSHA},
			},
			requiredChecks: []string{"build", "test"},
			expectedState:  CIStateFailed,
		},
		{
			name: "missing required check",
			checkRuns: []map[string]any{
				{"name": "build", "status": "completed", "conclusion": "success", "head_sha": headSHA},
			},
			requiredChecks: []string{"build", "test"},
			expectedState:  CIStatePending,
		},
		{
			name: "skipped check counts as non-failing",
			checkRuns: []map[string]any{
				{"name": "build", "status": "completed", "conclusion": "success", "head_sha": headSHA},
				{"name": "optional-lint", "status": "completed", "conclusion": "skipped", "head_sha": headSHA},
			},
			requiredChecks: []string{"build"},
			expectedState:  CIStateSuccess,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			httpClient := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
				if r.Method == "GET" && strings.Contains(r.URL.Path, "/commits/"+headSHA+"/check-runs") {
					return response(200, map[string]any{
						"total_count": len(tc.checkRuns),
						"check_runs":  tc.checkRuns,
					}), nil
				}
				return response(404, nil), nil
			})}
			observer, err := NewObserver(Config{
				Repository:       "owner/repo",
				CredentialSource: tokenSource{},
				HTTPClient:       httpClient,
			})
			if err != nil {
				t.Fatalf("NewObserver: %v", err)
			}
			obs, err := observer.ObserveCI(t.Context(), headSHA, tc.requiredChecks)
			if err != nil {
				t.Fatalf("ObserveCI error: %v", err)
			}
			if obs.State != tc.expectedState {
				t.Fatalf("expected state %s, got %s", tc.expectedState, obs.State)
			}
			if obs.HeadSHA != headSHA {
				t.Fatalf("expected head SHA %s, got %s", headSHA, obs.HeadSHA)
			}
		})
	}
}

func TestObserveReviews(t *testing.T) {
	headSHA := strings.Repeat("c", 40)
	oldSHA := strings.Repeat("b", 40)

	tests := []struct {
		name                 string
		reviews              []map[string]any
		expectedApproved     bool
		expectedChangesReq   bool
	}{
		{
			name: "approved on head SHA",
			reviews: []map[string]any{
				{"user": map[string]string{"login": "reviewer1"}, "state": "APPROVED", "commit_id": headSHA},
			},
			expectedApproved:   true,
			expectedChangesReq: false,
		},
		{
			name: "approved on old commit only",
			reviews: []map[string]any{
				{"user": map[string]string{"login": "reviewer1"}, "state": "APPROVED", "commit_id": oldSHA},
			},
			expectedApproved:   false,
			expectedChangesReq: false,
		},
		{
			name: "changes requested overrides approval",
			reviews: []map[string]any{
				{"user": map[string]string{"login": "reviewer1"}, "state": "APPROVED", "commit_id": headSHA},
				{"user": map[string]string{"login": "reviewer2"}, "state": "CHANGES_REQUESTED", "commit_id": headSHA},
			},
			expectedApproved:   true,
			expectedChangesReq: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			httpClient := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
				if r.Method == "GET" && strings.Contains(r.URL.Path, "/pulls/42/reviews") {
					return response(200, tc.reviews), nil
				}
				return response(404, nil), nil
			})}
			observer, err := NewObserver(Config{
				Repository:       "owner/repo",
				CredentialSource: tokenSource{},
				HTTPClient:       httpClient,
			})
			if err != nil {
				t.Fatalf("NewObserver: %v", err)
			}
			obs, err := observer.ObserveReviews(t.Context(), 42, headSHA)
			if err != nil {
				t.Fatalf("ObserveReviews error: %v", err)
			}
			if obs.Approved != tc.expectedApproved {
				t.Fatalf("expected approved=%v, got %v", tc.expectedApproved, obs.Approved)
			}
			if obs.ChangesRequested != tc.expectedChangesReq {
				t.Fatalf("expected changesRequested=%v, got %v", tc.expectedChangesReq, obs.ChangesRequested)
			}
		})
	}
}
