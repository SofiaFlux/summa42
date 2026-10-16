package ghpublish

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/operations"
)

func TestMergeProvider(t *testing.T) {
	headSHA := strings.Repeat("e", 40)
	baseSHA := strings.Repeat("b", 40)
	mergeCommitSHA := strings.Repeat("f", 40)

	intent := MergeIntent{
		Repository:  "owner/repo",
		PullNumber:  42,
		HeadSHA:     headSHA,
		BaseBranch:  "main",
		MergeMethod: "merge",
	}

	t.Run("successful merge dispatch", func(t *testing.T) {
		putCalled := false
		httpClient := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
			path := r.URL.Path
			if r.Method == "GET" && strings.HasSuffix(path, "/pulls/42") {
				if putCalled {
					return response(200, map[string]any{
						"number":           42,
						"merged":           true,
						"merge_commit_sha": mergeCommitSHA,
						"head":             map[string]any{"sha": headSHA, "ref": "summa42/issue-1", "repo": map[string]string{"full_name": "owner/repo"}},
						"base":             map[string]any{"sha": baseSHA, "ref": "main", "repo": map[string]string{"full_name": "owner/repo"}},
					}), nil
				}
				return response(200, map[string]any{
					"number":           42,
					"merged":           false,
					"merge_commit_sha": nil,
					"head":             map[string]any{"sha": headSHA, "ref": "summa42/issue-1", "repo": map[string]string{"full_name": "owner/repo"}},
					"base":             map[string]any{"sha": baseSHA, "ref": "main", "repo": map[string]string{"full_name": "owner/repo"}},
				}), nil
			}
			if r.Method == "PUT" && strings.HasSuffix(path, "/pulls/42/merge") {
				putCalled = true
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["sha"] != headSHA {
					t.Fatalf("expected sha %s, got %v", headSHA, body["sha"])
				}
				return response(200, map[string]any{
					"sha":     mergeCommitSHA,
					"merged":  true,
					"message": "Pull Request successfully merged",
				}), nil
			}
			return response(404, nil), nil
		})}

		provider, err := NewMergeProvider(Config{
			Repository:       "owner/repo",
			CredentialSource: tokenSource{},
			HTTPClient:       httpClient,
		})
		if err != nil {
			t.Fatal(err)
		}

		rawIntent, err := provider.CanonicalIntent(intent)
		if err != nil {
			t.Fatal(err)
		}

		outcome, err := provider.Dispatch(t.Context(), operations.ProviderDispatchRequest{
			CanonicalIntent: rawIntent,
		})
		if err != nil {
			t.Fatal(err)
		}
		if outcome.State != domain.OperationConfirmedEffect {
			t.Fatalf("expected confirmed effect, got %s", outcome.State)
		}
		if outcome.ProviderReference != mergeCommitSHA {
			t.Fatalf("expected reference %s, got %s", mergeCommitSHA, outcome.ProviderReference)
		}
		if !putCalled {
			t.Fatal("PUT /merge was never called")
		}
	})

	t.Run("reconciles already merged PR without duplicate call", func(t *testing.T) {
		putCalled := false
		httpClient := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
			if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls/42") {
				return response(200, map[string]any{
					"number":           42,
					"merged":           true,
					"merge_commit_sha": mergeCommitSHA,
					"head":             map[string]any{"sha": headSHA, "ref": "summa42/issue-1", "repo": map[string]string{"full_name": "owner/repo"}},
					"base":             map[string]any{"sha": baseSHA, "ref": "main", "repo": map[string]string{"full_name": "owner/repo"}},
				}), nil
			}
			if r.Method == "PUT" {
				putCalled = true
				return response(405, map[string]string{"message": "Pull Request is not mergeable"}), nil
			}
			return response(404, nil), nil
		})}

		provider, err := NewMergeProvider(Config{
			Repository:       "owner/repo",
			CredentialSource: tokenSource{},
			HTTPClient:       httpClient,
		})
		if err != nil {
			t.Fatal(err)
		}

		rawIntent, _ := provider.CanonicalIntent(intent)
		outcome, err := provider.Dispatch(t.Context(), operations.ProviderDispatchRequest{
			CanonicalIntent: rawIntent,
		})
		if err != nil {
			t.Fatal(err)
		}
		if outcome.State != domain.OperationConfirmedEffect || outcome.ProviderReference != mergeCommitSHA {
			t.Fatalf("expected confirmed effect with sha %s, got %+v", mergeCommitSHA, outcome)
		}
		if putCalled {
			t.Fatal("PUT /merge should not have been called for already merged PR")
		}
	})

	t.Run("head moved fails closed", func(t *testing.T) {
		otherHead := strings.Repeat("9", 40)
		httpClient := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
			if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls/42") {
				return response(200, map[string]any{
					"number":           42,
					"merged":           false,
					"merge_commit_sha": nil,
					"head":             map[string]any{"sha": otherHead, "ref": "summa42/issue-1", "repo": map[string]string{"full_name": "owner/repo"}},
					"base":             map[string]any{"sha": baseSHA, "ref": "main", "repo": map[string]string{"full_name": "owner/repo"}},
				}), nil
			}
			return response(404, nil), nil
		})}

		provider, err := NewMergeProvider(Config{
			Repository:       "owner/repo",
			CredentialSource: tokenSource{},
			HTTPClient:       httpClient,
		})
		if err != nil {
			t.Fatal(err)
		}

		rawIntent, _ := provider.CanonicalIntent(intent)
		outcome, err := provider.Dispatch(t.Context(), operations.ProviderDispatchRequest{
			CanonicalIntent: rawIntent,
		})
		if err == nil {
			t.Fatal("expected error when remote head has moved")
		}
		if outcome.State == domain.OperationConfirmedEffect {
			t.Fatal("outcome must not be confirmed when head moved")
		}
	})
}
