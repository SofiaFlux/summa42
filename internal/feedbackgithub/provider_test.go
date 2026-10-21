package feedbackgithub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/fieldfeedback"
	"github.com/SofiaFlux/summa42/internal/operations"
)

type memoryFeedback struct{ artifact domain.SanitizedFeedback }

func (m memoryFeedback) SanitizedFeedback(context.Context, domain.ID) (domain.SanitizedFeedback, error) {
	return m.artifact, nil
}

func TestProviderCreateUsesAuthOnlyOnGitHubRequestAndNeverLeaksToken(t *testing.T) {
	token := "ghp_super_secret_token_value"
	tokenFile := writeTokenFile(t, token, 0o600)
	var gotAuth, gotBody, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":42,"html_url":"https://github.com/owner/repo/issues/42"}`))
	}))
	defer server.Close()

	artifact := domain.SanitizedFeedback{ID: "feedback-1", ContentJSON: `{"category":"TEST"}`, Fingerprint: "fp-1"}
	provider, err := New(Config{
		APIBaseURL: server.URL, Repository: "owner/repo",
		CredentialSource: FileCredentialSource{Path: tokenFile}, HTTPClient: server.Client(),
	}, memoryFeedback{artifact: artifact})
	if err != nil {
		t.Fatal(err)
	}

	canonical, err := provider.CanonicalIntent(fieldfeedback.EmitIntent{SanitizedFeedbackID: artifact.ID, Destination: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canonical), token) {
		t.Fatal("token leaked into canonical intent")
	}

	outcome, err := provider.Dispatch(context.Background(), operations.ProviderDispatchRequest{CanonicalIntent: canonical})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer "+token {
		t.Fatalf("Authorization=%q", gotAuth)
	}
	if gotPath != "/repos/owner/repo/issues" {
		t.Fatalf("path=%q", gotPath)
	}
	if strings.Contains(gotBody, token) {
		t.Fatal("token leaked into request body")
	}
	if strings.Contains(outcome.ProviderReference, token) {
		t.Fatal("token leaked into provider reference")
	}
}

func TestProviderReconcileFindsExactMarkerWithoutPost(t *testing.T) {
	tokenFile := writeTokenFile(t, "token-value", 0o600)
	var postCount, getCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			getCount++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"number":7,"html_url":"https://github.com/owner/repo/issues/7","body":"safe\n<!-- summa42-feedback:fp-7 -->"}]`))
		case http.MethodPost:
			postCount++
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer server.Close()
	artifact := domain.SanitizedFeedback{ID: "feedback-7", ContentJSON: `{"category":"TEST"}`, Fingerprint: "fp-7"}
	provider, err := New(Config{APIBaseURL: server.URL, Repository: "owner/repo", CredentialSource: FileCredentialSource{Path: tokenFile}, HTTPClient: server.Client()}, memoryFeedback{artifact})
	if err != nil {
		t.Fatal(err)
	}
	canonical, _ := provider.CanonicalIntent(fieldfeedback.EmitIntent{SanitizedFeedbackID: artifact.ID, Destination: "owner/repo"})
	outcome, err := provider.LookupOutcome(context.Background(), operations.ProviderDispatchRequest{CanonicalIntent: canonical})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.State != domain.OperationConfirmedEffect || !strings.HasSuffix(outcome.ProviderReference, "/issues/7") {
		t.Fatalf("outcome=%+v", outcome)
	}
	if postCount != 0 || getCount != 1 {
		t.Fatalf("POST=%d GET=%d", postCount, getCount)
	}
}

func TestProviderErrorsSanitizeBodiesAndAmbiguous5xxReturnsError(t *testing.T) {
	token := "secret-token"
	tokenFile := writeTokenFile(t, token, 0o600)
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte("sensitive response " + token))
			}))
			defer server.Close()
			artifact := domain.SanitizedFeedback{ID: "feedback-1", ContentJSON: `{"category":"TEST"}`, Fingerprint: "fp"}
			provider, err := New(Config{APIBaseURL: server.URL, Repository: "owner/repo", CredentialSource: FileCredentialSource{Path: tokenFile}, HTTPClient: server.Client()}, memoryFeedback{artifact})
			if err != nil {
				t.Fatal(err)
			}
			canonical, _ := provider.CanonicalIntent(fieldfeedback.EmitIntent{SanitizedFeedbackID: artifact.ID, Destination: "owner/repo"})
			_, err = provider.Dispatch(context.Background(), operations.ProviderDispatchRequest{CanonicalIntent: canonical})
			if err == nil {
				t.Fatal("expected error")
			}
			if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "sensitive response") {
				t.Fatalf("sensitive API body leaked in error: %v", err)
			}
		})
	}
}

func TestFileCredentialSourceRequiresPrivateRegularFile(t *testing.T) {
	ctx := context.Background()
	private := writeTokenFile(t, "token", 0o600)
	token, err := (FileCredentialSource{Path: private}).Token(ctx)
	if err != nil || token != "token" {
		t.Fatalf("private token=%q err=%v", token, err)
	}

	public := writeTokenFile(t, "token", 0o644)
	if _, err := (FileCredentialSource{Path: public}).Token(ctx); err == nil {
		t.Fatal("group/world-readable token file accepted")
	}
	dir := t.TempDir()
	if _, err := (FileCredentialSource{Path: dir}).Token(ctx); err == nil {
		t.Fatal("directory accepted as credential file")
	}
}

func TestConfigRejectsInvalidRepository(t *testing.T) {
	for _, repo := range []string{"owner", "owner/repo/extra", "../repo", "owner/../repo", "owner repo/x"} {
		if _, err := New(Config{APIBaseURL: "https://api.github.com", Repository: repo, CredentialSource: staticCredential("x")}, memoryFeedback{}); err == nil {
			t.Fatalf("repository %q accepted", repo)
		}
	}
}

type staticCredential string

func (s staticCredential) Token(context.Context) (string, error) { return string(s), nil }

func writeTokenFile(t *testing.T, token string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "github.token")
	if err := os.WriteFile(path, []byte(token+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIssueBodyContainsStableMarker(t *testing.T) {
	payload := fieldfeedback.IssuePayload{Title: "x", Body: `{"x":1}`, Marker: "summa42-feedback:abc"}
	req := issueCreateRequest(payload)
	var body map[string]any
	if err := json.Unmarshal(req, &body); err != nil {
		t.Fatal(err)
	}
	text, _ := body["body"].(string)
	if !strings.Contains(text, "<!-- summa42-feedback:abc -->") {
		t.Fatalf("body=%q", text)
	}
}

func TestProviderReconcilePaginatesBeforeConfirmingNoEffect(t *testing.T) {
	tokenFile := writeTokenFile(t, "token-value", 0o600)
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method %s", r.Method)
		}
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		w.Header().Set("Content-Type", "application/json")
		switch page {
		case "1":
			issues := make([]issueResponse, 100)
			for i := range issues {
				issues[i] = issueResponse{Number: int64(i + 1), Body: "no marker"}
			}
			if err := json.NewEncoder(w).Encode(issues); err != nil {
				t.Fatal(err)
			}
		case "2":
			if err := json.NewEncoder(w).Encode([]issueResponse{{
				Number:  101,
				HTMLURL: "https://github.com/owner/repo/issues/101",
				Body:    "safe\n<!-- summa42-feedback:fp-old -->",
			}}); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unexpected page %q", page)
		}
	}))
	defer server.Close()

	artifact := domain.SanitizedFeedback{ID: "feedback-old", ContentJSON: `{"category":"TEST"}`, Fingerprint: "fp-old"}
	provider, err := New(Config{
		APIBaseURL: server.URL, Repository: "owner/repo",
		CredentialSource: FileCredentialSource{Path: tokenFile}, HTTPClient: server.Client(),
	}, memoryFeedback{artifact: artifact})
	if err != nil {
		t.Fatal(err)
	}
	canonical, _ := provider.CanonicalIntent(fieldfeedback.EmitIntent{SanitizedFeedbackID: artifact.ID, Destination: "owner/repo"})
	outcome, err := provider.LookupOutcome(context.Background(), operations.ProviderDispatchRequest{CanonicalIntent: canonical})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.State != domain.OperationConfirmedEffect {
		t.Fatalf("outcome=%s, want CONFIRMED_EFFECT", outcome.State)
	}
	if len(pages) != 2 || pages[0] != "1" || pages[1] != "2" {
		t.Fatalf("pages=%v, want [1 2]", pages)
	}
}

func TestConfigRejectsPlaintextNonLoopbackAPI(t *testing.T) {
	_, err := New(Config{
		APIBaseURL:       "http://github-proxy.example.test",
		Repository:       "owner/repo",
		CredentialSource: staticCredential("token"),
	}, memoryFeedback{})
	if err == nil {
		t.Fatal("plaintext non-loopback GitHub API accepted")
	}
}
