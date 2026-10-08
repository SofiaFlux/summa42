package ghpublish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/SofiaFlux/summa42/internal/repoworkspace"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type tokenSource struct{}

func (tokenSource) Token(context.Context) (string, error) { return "private-test-token", nil }

type transport func(*http.Request) (*http.Response, error)

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) { return t(r) }
func response(status int, v any) *http.Response {
	raw, _ := json.Marshal(v)
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}
}
func exportFixture(t *testing.T) repoworkspace.Export {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		c := exec.Command("git", args...)
		c.Dir = dir
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	os.WriteFile(filepath.Join(dir, "code.go"), []byte("package old\n"), 0644)
	git("add", "code.go")
	git("-c", "user.name=fixture", "-c", "user.email=fixture@invalid", "commit", "-qm", "base")
	base := git("rev-parse", "HEAD")
	c, err := repoworkspace.PrepareCandidate(t.Context(), dir, base, []string{"code.go"}, []repoworkspace.Edit{{Path: "code.go", Content: "package new\n"}})
	if err != nil {
		t.Fatal(err)
	}
	e, err := repoworkspace.ExportCandidate(t.Context(), dir, c)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func branchIntent(t *testing.T) BranchIntent {
	return BranchIntent{Repository: "o/r", BaseBranch: "main", HeadBranch: "summa42/issue-42-0123456789abcdef", Export: exportFixture(t)}
}
func TestBranchProviderPreservesCandidateAndNeverOverwritesRef(t *testing.T) {
	for _, mode := range []string{"success", "tree-mismatch", "commit-mismatch", "existing-conflict"} {
		t.Run(mode, func(t *testing.T) {
			intent := branchIntent(t)
			writes := 0
			ref := ""
			if mode == "existing-conflict" {
				ref = strings.Repeat("a", 40)
			}
			client := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") != "Bearer private-test-token" {
					t.Fatal("credential missing")
				}
				path := r.URL.Path
				if r.Method == "GET" && strings.HasSuffix(path, "/git/ref/heads/main") {
					return response(200, map[string]any{"object": map[string]string{"sha": intent.Export.Candidate.BaseSHA}}), nil
				}
				if r.Method == "GET" && strings.Contains(path, "/git/ref/heads/summa42/") {
					if ref == "" {
						return response(404, nil), nil
					}
					return response(200, map[string]any{"object": map[string]string{"sha": ref}}), nil
				}
				if r.Method == "GET" && strings.Contains(path, "/git/commits/") {
					return response(200, map[string]any{"sha": intent.Export.Candidate.BaseSHA, "tree": map[string]string{"sha": intent.Export.BaseTreeSHA}}), nil
				}
				if r.Method == "POST" {
					writes++
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					switch {
					case strings.HasSuffix(path, "/git/trees"):
						sha := intent.Export.Candidate.TreeSHA
						if mode == "tree-mismatch" {
							sha = strings.Repeat("b", 40)
						}
						return response(201, map[string]string{"sha": sha}), nil
					case strings.HasSuffix(path, "/git/commits"):
						if body["message"] != intent.Export.Message {
							t.Fatal("message changed")
						}
						sha := intent.Export.Candidate.CandidateSHA
						if mode == "commit-mismatch" {
							sha = strings.Repeat("b", 40)
						}
						return response(201, map[string]string{"sha": sha}), nil
					case strings.HasSuffix(path, "/git/refs"):
						ref = body["sha"].(string)
						return response(201, map[string]any{"object": map[string]string{"sha": ref}}), nil
					}
				}
				t.Fatalf("unexpected %s %s", r.Method, path)
				return nil, nil
			})}
			p, err := NewBranchProvider(Config{Repository: "o/r", CredentialSource: tokenSource{}, HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := p.CanonicalIntent(intent)
			if err != nil {
				t.Fatal(err)
			}
			out, err := p.Dispatch(t.Context(), operations.ProviderDispatchRequest{CanonicalIntent: raw})
			if mode == "success" {
				if err != nil || out.State != domain.OperationConfirmedEffect || ref != intent.Export.Candidate.CandidateSHA || writes != 3 {
					t.Fatalf("%+v %v writes=%d", out, err, writes)
				}
				before := writes
				out, err = p.LookupOutcome(t.Context(), operations.ProviderDispatchRequest{CanonicalIntent: raw})
				if err != nil || out.State != domain.OperationConfirmedEffect || writes != before {
					t.Fatal("lookup wrote or lost effect")
				}
			} else {
				if err == nil || ref == intent.Export.Candidate.CandidateSHA {
					t.Fatal("mismatched candidate referenced")
				}
				if mode == "existing-conflict" && writes != 0 {
					t.Fatal("existing ref overwritten")
				}
			}
		})
	}
}
func TestPRProviderDraftReadbackAndUncertainRecovery(t *testing.T) {
	intent := PRIntent{Repository: "o/r", BaseBranch: "main", HeadBranch: "summa42/issue-42-0123456789abcdef", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Title: "Issue 42", Body: "record\n<!-- summa42:publication:" + strings.Repeat("c", 64) + " -->", Marker: "summa42:publication:" + strings.Repeat("c", 64)}
	writes := 0
	created := false
	pr := map[string]any{"number": 7, "html_url": "https://github.com/o/r/pull/7", "title": intent.Title, "body": intent.Body, "draft": true, "head": map[string]any{"ref": intent.HeadBranch, "sha": intent.HeadSHA, "repo": map[string]string{"full_name": "o/r"}}, "base": map[string]any{"ref": intent.BaseBranch, "sha": intent.BaseSHA, "repo": map[string]string{"full_name": "o/r"}}}
	client := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" && strings.Contains(r.URL.Path, "/git/ref/heads/") {
			sha := intent.BaseSHA
			if strings.Contains(r.URL.Path, "/summa42/") {
				sha = intent.HeadSHA
			}
			return response(200, map[string]any{"object": map[string]string{"sha": sha}}), nil
		}
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls") {
			if r.URL.Query().Get("page") == "1" {
				return response(200, make([]map[string]any, 100)), nil
			}
			if created {
				return response(200, []any{pr}), nil
			}
			return response(200, []any{}), nil
		}
		if r.Method == "POST" {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["draft"] != true || body["head"] != intent.HeadBranch {
				t.Fatal("non-draft/wrong head")
			}
			writes++
			created = true
			return nil, errors.New("lost acknowledgement")
		}
		t.Fatalf("unexpected request %s", r.URL)
		return nil, nil
	})}
	p, err := NewPRProvider(Config{Repository: "o/r", CredentialSource: tokenSource{}, HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := p.CanonicalIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Dispatch(t.Context(), operations.ProviderDispatchRequest{CanonicalIntent: raw}); err == nil {
		t.Fatal("missing ack treated certain")
	}
	got, err := p.LookupOutcome(t.Context(), operations.ProviderDispatchRequest{CanonicalIntent: raw})
	if err != nil || got.State != domain.OperationConfirmedEffect || writes != 1 {
		t.Fatalf("%+v %v writes=%d", got, err, writes)
	}
	created = false
	got, err = p.LookupOutcome(t.Context(), operations.ProviderDispatchRequest{CanonicalIntent: raw})
	if err != nil || got.State != domain.OperationOutcomeUnknown || writes != 1 {
		t.Fatal("missing effect proved absent or rewrote")
	}
}
func TestProvidersRejectUnsafeConfigurationAndDescriptor(t *testing.T) {
	for _, base := range []string{"http://example.com", "https://user:pass@github.com", "https://api.github.com/?token=x"} {
		if _, err := NewPRProvider(Config{APIBaseURL: base, Repository: "o/r", CredentialSource: tokenSource{}}); err == nil {
			t.Fatal("unsafe endpoint", base)
		}
	}
	p, err := NewBranchProvider(Config{Repository: "o/r", CredentialSource: tokenSource{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.CanonicalIntent(PRIntent{}); err == nil {
		t.Fatal("wrong typed intent")
	}
}

func TestClientBoundsAndSanitizesTransport(t *testing.T) {
	for _, mode := range []string{"redirect", "large", "secret-error"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			h := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
				calls++
				switch mode {
				case "redirect":
					res := response(302, nil)
					res.Header.Set("Location", "https://other.invalid/steal")
					return res, nil
				case "large":
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", (1<<20)+1)))}, nil
				default:
					return nil, errors.New("private-test-token")
				}
			})}
			c, err := newClient(Config{Repository: "o/r", CredentialSource: tokenSource{}, HTTPClient: h})
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.request(t.Context(), "GET", "/pulls", nil, new(any))
			if err == nil || strings.Contains(err.Error(), "private-test-token") || calls != 1 {
				t.Fatalf("unsafe request: %v calls=%d", err, calls)
			}
		})
	}
}

func TestPRProviderRejectsNullLookupBeforeAnyWrite(t *testing.T) {
	for _, later := range []bool{false, true} {
		t.Run(fmt.Sprint(later), func(t *testing.T) {
			v := PRIntent{Repository: "o/r", BaseBranch: "main", HeadBranch: "summa42/issue-42-0123456789abcdef", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Title: "Issue 42", Marker: "summa42:publication:" + strings.Repeat("c", 64)}
			v.Body = "<!-- " + v.Marker + " -->"
			writes := 0
			h := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
				if r.Method == "POST" {
					writes++
					return response(201, nil), nil
				}
				if strings.Contains(r.URL.Path, "/git/ref/heads/") {
					sha := v.BaseSHA
					if strings.Contains(r.URL.Path, "/summa42/") {
						sha = v.HeadSHA
					}
					return response(200, map[string]any{"object": map[string]string{"sha": sha}}), nil
				}
				if later && r.URL.Query().Get("page") == "1" {
					page := make([]pull, 100)
					page[0] = pull{Number: 7, Title: v.Title, Body: v.Body, Draft: true}
					page[0].Head.Ref = v.HeadBranch
					page[0].Head.SHA = v.HeadSHA
					page[0].Head.Repo.FullName = v.Repository
					page[0].Base.Ref = v.BaseBranch
					page[0].Base.SHA = v.BaseSHA
					page[0].Base.Repo.FullName = v.Repository
					return response(200, page), nil
				}
				return response(200, nil), nil
			})}
			p, err := NewPRProvider(Config{Repository: "o/r", CredentialSource: tokenSource{}, HTTPClient: h})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := p.CanonicalIntent(v)
			if err != nil {
				t.Fatal(err)
			}
			out, err := p.Dispatch(t.Context(), operations.ProviderDispatchRequest{CanonicalIntent: raw})
			if err == nil || writes != 0 || out.State != domain.OperationOutcomeUnknown {
				t.Fatalf("inconclusive lookup wrote: %+v %v writes=%d", out, err, writes)
			}
		})
	}
}
