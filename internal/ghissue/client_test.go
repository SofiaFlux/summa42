package ghissue

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func tokenFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const validIssueJSON = `{"number":7,"title":"t","body":"","state":"open","user":{"login":"maint"},"assignees":[],"labels":[],"updated_at":"2026-09-24T10:00:00Z","html_url":"https://github.com/o/r/issues/7"}`

// Both Link headers below follow the form GitHub documents for its own
// repository-issues pagination,
// <.../repositories/1300192/issues?per_page=…&page=…>; rel="…",
// whose target uses the numeric repository path and carries only the pagination
// parameters. githubPageOnlyLink is that documented form byte-for-byte in path
// and query. githubPerPageLink is derived from it rather than quoted: same
// documented form, but with per_page set to the page size this client pins
// (pageSize) instead of the one in GitHub's own example. Only the
// api.github.com origin is rewritten to the test server, and nothing else
// about either header is.
const (
	githubPageOnlyLink = `<https://api.github.com/repositories/1300192/issues?page=2>; rel="prev", ` +
		`<https://api.github.com/repositories/1300192/issues?page=4>; rel="next", ` +
		`<https://api.github.com/repositories/1300192/issues?page=515>; rel="last", ` +
		`<https://api.github.com/repositories/1300192/issues?page=1>; rel="first"`
	githubPerPageLink = `<https://api.github.com/repositories/1300192/issues?per_page=100&page=2>; rel="next", ` +
		`<https://api.github.com/repositories/1300192/issues?per_page=100&page=7715>; rel="last"`
)

func TestListIssuesRequestsOpenIssuesPageWithBearerToken(t *testing.T) {
	var seen *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		_, _ = w.Write([]byte("[" + validIssueJSON + "]"))
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Repository: "o/r", TokenFile: tokenFile(t, "sekrit\n")})
	if err != nil {
		t.Fatal(err)
	}
	items, bad, next, err := client.ListIssues(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || len(bad) != 0 || next != "" {
		t.Fatalf("items=%d bad=%d next=%q", len(items), len(bad), next)
	}
	if seen.Method != http.MethodGet {
		t.Fatalf("method = %s, want GET", seen.Method)
	}
	if seen.URL.Path != "/repos/o/r/issues" {
		t.Fatalf("path = %q", seen.URL.Path)
	}
	if seen.URL.Query().Get("state") != "open" || seen.URL.Query().Get("per_page") != "100" {
		t.Fatalf("query = %q", seen.URL.RawQuery)
	}
	if seen.Header.Get("Authorization") != "Bearer sekrit" {
		t.Fatalf("authorization = %q", seen.Header.Get("Authorization"))
	}
	if seen.Header.Get("Accept") != "application/vnd.github+json" {
		t.Fatalf("accept = %q", seen.Header.Get("Accept"))
	}
	if client.Name() != "o/r" {
		t.Fatalf("name = %q", client.Name())
	}
	var lister IssueLister = client
	if lister.Name() != "o/r" {
		t.Fatal("client does not satisfy IssueLister")
	}
}

// The two Link headers below are described where they are defined: both follow
// the form GitHub documents for its own repository-issues pagination, and only
// the api.github.com origin is rewritten to the test server, so path and query
// stay byte-for-byte as written there. Each header advances to the page its own
// rel="next" names.
func TestListIssuesFollowsGitHubDocumentedLinkHeaders(t *testing.T) {
	headers := map[string]struct {
		link     string
		wantPage string
	}{
		"page only":     {githubPageOnlyLink, "4"},
		"with per_page": {githubPerPageLink, "2"},
	}
	for name, header := range headers {
		t.Run(name, func(t *testing.T) {
			var paths []string
			var served int
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
				served++
				if served > 1 {
					_, _ = w.Write([]byte("[]"))
					return
				}
				w.Header().Set("Link", strings.ReplaceAll(header.link, "https://api.github.com", server.URL))
				_, _ = w.Write([]byte("[" + validIssueJSON + "]"))
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, Repository: "o/r", TokenFile: tokenFile(t, "sekrit")})
			if err != nil {
				t.Fatal(err)
			}
			_, _, next, err := client.ListIssues(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			if _, _, last, err := client.ListIssues(context.Background(), next); err != nil || last != "" {
				t.Fatalf("last=%q err=%v", last, err)
			}
			want := "/repos/o/r/issues?state=open&per_page=100&page=" + header.wantPage
			if len(paths) != 2 || paths[1] != want {
				t.Fatalf("requests = %v, want the follow-up request %q", paths, want)
			}
		})
	}
}

func TestListIssuesSplitsUnparseableItems(t *testing.T) {
	malformedLabels := `{"number":8,"title":"t","state":"open","user":{"login":"maint"},` +
		`"updated_at":"2026-09-24T10:00:00Z","html_url":"https://github.com/o/r/issues/8","labels":"bug"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[" + validIssueJSON + `,{"number":8},` + malformedLabels + `]`))
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Repository: "o/r", TokenFile: tokenFile(t, "sekrit")})
	if err != nil {
		t.Fatal(err)
	}
	items, bad, _, err := client.ListIssues(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || len(bad) != 2 {
		t.Fatalf("items=%d bad=%d", len(items), len(bad))
	}
	for _, entry := range bad {
		if entry.Raw == nil {
			t.Fatal("unparseable entry lost its raw item")
		}
	}
}

func TestListIssuesRejectsNonArrayBodies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("null"))
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Repository: "o/r", TokenFile: tokenFile(t, "sekrit")})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := client.ListIssues(context.Background(), ""); err == nil {
		t.Fatal("expected error for null body")
	}
}

func TestListIssuesRejectsRedirects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example/repos/o/r/issues", http.StatusFound)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Repository: "o/r", TokenFile: tokenFile(t, "sekrit")})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := client.ListIssues(context.Background(), ""); err == nil {
		t.Fatal("expected error for redirect response")
	}
}

func TestNewRejectsUnsafeConfiguration(t *testing.T) {
	token := tokenFile(t, "sekrit")
	cases := []Config{
		{BaseURL: "https://api.github.com", Repository: "o", TokenFile: token},
		{BaseURL: "https://api.github.com", Repository: "../x", TokenFile: token},
		{BaseURL: "https://api.github.com", Repository: "o/r/extra", TokenFile: token},
		{BaseURL: "https://user:pass@api.github.com", Repository: "o/r", TokenFile: token},
		{BaseURL: "http://api.github.com", Repository: "o/r", TokenFile: token},
		{BaseURL: "https://api.github.com", Repository: "o/r", TokenFile: " "},
		{BaseURL: "https://evil.example", Repository: "o/r", TokenFile: token},
		{BaseURL: "https://api.github.com:8443", Repository: "o/r", TokenFile: token},
		{BaseURL: "https://api.github.com/v3", Repository: "o/r", TokenFile: token},
		{BaseURL: "https://api.github.com?x=1", Repository: "o/r", TokenFile: token},
		{BaseURL: "https://api.github.com#frag", Repository: "o/r", TokenFile: token},
		{BaseURL: "https://ghe.example", Repository: "o/r", TokenFile: token},
	}
	for i, cfg := range cases {
		if _, err := New(cfg); err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}

// Trigger: a typo in SUMMA42_GITHUB_TOKEN_FILE used to pass New untouched, so
// the Box opened and created its evidence directory before the first tick died
// on a stat error. Every other credential-shaped mistake here is a startup
// error; this one must be too.
func TestNewRejectsUnusableTokenFiles(t *testing.T) {
	loose := filepath.Join(t.TempDir(), "loose")
	if err := os.WriteFile(loose, []byte("sekrit"), 0o644); err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{
		"empty":             tokenFile(t, "  \n"),
		"loose permissions": loose,
		"missing":           filepath.Join(t.TempDir(), "absent"),
		"directory":         t.TempDir(),
	}
	for name, path := range paths {
		t.Run(name, func(t *testing.T) {
			if _, err := New(Config{BaseURL: "https://api.github.com", Repository: "o/r", TokenFile: path}); err == nil {
				t.Fatal("expected a startup error")
			}
		})
	}
}

// The token contents must not be cached: a rotated file with loosened
// permissions has to be caught by the next request, not only at startup.
func TestListIssuesReReadsTokenFilePerRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[]"))
	}))
	defer server.Close()
	path := tokenFile(t, "sekrit")
	client, err := New(Config{BaseURL: server.URL, Repository: "o/r", TokenFile: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := client.ListIssues(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := client.ListIssues(context.Background(), ""); err == nil {
		t.Fatal("expected the per-request read to reject the rotated credential file")
	}
}

func TestListIssuesSanitizesHTTPFailures(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError, http.StatusBadGateway} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"token sekrit rejected"}`))
		}))
		client, err := New(Config{BaseURL: server.URL, Repository: "o/r", TokenFile: tokenFile(t, "sekrit")})
		if err != nil {
			t.Fatal(err)
		}
		_, _, _, err = client.ListIssues(context.Background(), "")
		server.Close()
		if err == nil {
			t.Fatalf("status %d: expected error", status)
		}
		if !strings.Contains(err.Error(), strconv.Itoa(status)) {
			t.Fatalf("status %d: err = %v, want status in message", status, err)
		}
		if strings.Contains(err.Error(), "sekrit") {
			t.Fatalf("status %d leaked token: %v", status, err)
		}
	}
}

func TestListIssuesEnforcesResponseCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`["` + strings.Repeat("p", 4096) + `"]`))
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Repository: "o/r", TokenFile: tokenFile(t, "sekrit"), MaxResponseBytes: 128})
	if err != nil {
		t.Fatal(err)
	}
	if client.maxResponseBytes != 128 {
		t.Fatalf("maxResponseBytes = %d, want 128", client.maxResponseBytes)
	}
	if _, _, _, err := client.ListIssues(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "128") {
		t.Fatalf("err = %v, want response cap error", err)
	}
}

func TestNewDefaultsResponseCapToSixteenMiB(t *testing.T) {
	body := []byte(`["` + strings.Repeat("p", 2<<20) + `"]`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Repository: "o/r", TokenFile: tokenFile(t, "sekrit")})
	if err != nil {
		t.Fatal(err)
	}
	if client.maxResponseBytes != 16<<20 {
		t.Fatalf("maxResponseBytes = %d, want %d", client.maxResponseBytes, 16<<20)
	}
	if _, _, _, err := client.ListIssues(context.Background(), ""); err != nil {
		t.Fatalf("2 MiB page rejected under the default cap: %v", err)
	}
	wide, err := New(Config{BaseURL: server.URL, Repository: "o/r", TokenFile: tokenFile(t, "sekrit"), MaxResponseBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if wide.maxResponseBytes != 4<<20 {
		t.Fatalf("maxResponseBytes = %d, want %d", wide.maxResponseBytes, 4<<20)
	}
	if _, _, _, err := wide.ListIssues(context.Background(), ""); err != nil {
		t.Fatalf("2 MiB page rejected under an explicit 4 MiB cap: %v", err)
	}
}

func TestListIssuesRejectsNextLinkWithUnexpectedQuery(t *testing.T) {
	cases := []struct {
		name  string
		query string
	}{
		{"closed state", "state=closed&per_page=100&page=2"},
		{"pinned state", "state=open&per_page=100&page=2"},
		{"missing page", "per_page=100"},
		{"page one", "per_page=100&page=1"},
		{"non numeric page", "per_page=100&page=next"},
		{"repeated page", "per_page=100&page=2&page=3"},
		{"extra parameter", "per_page=2&page=2&direction=desc"},
		{"no query", ""},
	}
	for _, test := range cases {
		var requests int
		var server *httptest.Server
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			w.Header().Set("Link", `<`+server.URL+`/repositories/1300192/issues?`+test.query+`>; rel="next"`)
			_, _ = w.Write([]byte("[]"))
		}))
		client, err := New(Config{BaseURL: server.URL, Repository: "o/r", TokenFile: tokenFile(t, "sekrit")})
		if err != nil {
			t.Fatal(err)
		}
		_, _, _, err = client.ListIssues(context.Background(), "")
		server.Close()
		if err == nil {
			t.Fatalf("%s: expected error", test.name)
		}
		if requests != 1 {
			t.Fatalf("%s: requests = %d, want 1", test.name, requests)
		}
	}
}

// The client rebuilds every follow-up request from its own pinned page size, so
// a next link whose per_page disagrees would make the page number mean a
// different window of issues: it is rejected rather than reinterpreted, while
// the absent and pinned forms stay valid.
func TestListIssuesRejectsNextLinkWithUnpinnedPerPage(t *testing.T) {
	cases := []struct {
		name    string
		perPage string
		wantErr bool
	}{
		{"absent", "", false},
		{"pinned", "per_page=100&", false},
		{"smaller window", "per_page=50&", true},
		{"larger window", "per_page=200&", true},
		{"zero", "per_page=0&", true},
		{"negative", "per_page=-1&", true},
		{"non numeric", "per_page=many&", true},
	}
	for _, test := range cases {
		var paths []string
		var server *httptest.Server
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
			if len(paths) == 1 {
				w.Header().Set("Link", `<`+server.URL+`/repositories/1300192/issues?`+test.perPage+`page=2>; rel="next"`)
			}
			_, _ = w.Write([]byte("[]"))
		}))
		client, err := New(Config{BaseURL: server.URL, Repository: "o/r", TokenFile: tokenFile(t, "sekrit")})
		if err != nil {
			t.Fatal(err)
		}
		_, _, next, err := client.ListIssues(context.Background(), "")
		if err == nil {
			var last string
			_, _, last, err = client.ListIssues(context.Background(), next)
			if err == nil && (next != "2" || last != "") {
				t.Fatalf("%s: next=%q last=%q, want the page 2 next and no further page", test.name, next, last)
			}
		}
		server.Close()
		if test.wantErr {
			if err == nil {
				t.Fatalf("%s: expected an error", test.name)
			}
			if len(paths) != 1 {
				t.Fatalf("%s: requests = %v, want the rejected link never followed", test.name, paths)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: err = %v", test.name, err)
		}
		want := "/repos/o/r/issues?state=open&per_page=100&page=2"
		if len(paths) != 2 || paths[1] != want {
			t.Fatalf("%s: requests = %v, want the pinned follow-up %q", test.name, paths, want)
		}
	}
}

func TestListIssuesRejectsCursorWithUnpinnedQuery(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte("[]"))
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Repository: "o/r", TokenFile: tokenFile(t, "sekrit")})
	if err != nil {
		t.Fatal(err)
	}
	cursor := server.URL + "/repos/o/r/issues?state=closed&per_page=100&page=2"
	if _, _, _, err := client.ListIssues(context.Background(), cursor); err == nil {
		t.Fatal("expected error for cursor with state=closed")
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}

func TestListIssuesRejectsUnsafeLinkHeaders(t *testing.T) {
	links := map[string]func(origin string) string{
		"malformed":       func(string) string { return `<>; rel="next"` },
		"cross origin":    func(string) string { return `<http://other.example/repos/o/r/issues?page=2>; rel="next"` },
		"unquoted rel":    func(origin string) string { return `<` + origin + `/repos/o/r/issues?page=2>; rel=next` },
		"empty section":   func(string) string { return `,` },
		"relative target": func(string) string { return `</repos/o/r/issues?page=2>; rel="next"` },
		"userinfo target": func(origin string) string {
			return `<http://user:pass@` + strings.TrimPrefix(origin, "http://") + `/repos/o/r/issues?page=2>; rel="next"`
		},
		"fragment target": func(origin string) string { return `<` + origin + `/repos/o/r/issues?page=2#frag>; rel="next"` },
		"repeated next": func(origin string) string {
			return `<` + origin + `/repositories/1300192/issues?page=2>; rel="next", <` +
				origin + `/repositories/1300192/issues?page=3>; rel="next"`
		},
	}
	for name, build := range links {
		var server *httptest.Server
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Link", build(server.URL))
			_, _ = w.Write([]byte("[]"))
		}))
		client, err := New(Config{BaseURL: server.URL, Repository: "o/r", TokenFile: tokenFile(t, "sekrit")})
		if err != nil {
			t.Fatal(err)
		}
		_, _, _, err = client.ListIssues(context.Background(), "")
		server.Close()
		if err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestListIssuesFollowsQuotedNextLinkAndIgnoresPrev(t *testing.T) {
	var nextCalls int
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "page=4") {
			nextCalls++
			_, _ = w.Write([]byte("[" + validIssueJSON + "]"))
			return
		}
		w.Header().Set("Link", strings.ReplaceAll(githubPageOnlyLink, "https://api.github.com", server.URL))
		_, _ = w.Write([]byte("[]"))
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Repository: "o/r", TokenFile: tokenFile(t, "sekrit")})
	if err != nil {
		t.Fatal(err)
	}
	_, _, cursor, err := client.ListIssues(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if cursor != "4" {
		t.Fatalf("cursor = %q, want the page the next link names", cursor)
	}
	if _, _, _, err := client.ListIssues(context.Background(), cursor); err != nil {
		t.Fatal(err)
	}
	if nextCalls != 1 {
		t.Fatalf("next page fetches = %d, want 1", nextCalls)
	}
}

func TestListIssuesPropagatesContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[]"))
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Repository: "o/r", TokenFile: tokenFile(t, "sekrit")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := client.ListIssues(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestListIssuesHonorsClientTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		server.Close()
	}()
	client, err := New(Config{BaseURL: server.URL, Repository: "o/r", TokenFile: tokenFile(t, "sekrit"), Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := client.ListIssues(context.Background(), ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestNewAcceptsProductionConfiguration(t *testing.T) {
	client, err := New(Config{BaseURL: "https://api.github.com", Repository: "o/r", TokenFile: tokenFile(t, "sekrit")})
	if err != nil {
		t.Fatal(err)
	}
	if client == nil {
		t.Fatal("client = nil, want a client for the production API host")
	}
	if client.Name() != "o/r" || client.baseURL.Scheme != "https" || client.baseURL.Host != "api.github.com" {
		t.Fatalf("name = %q base = %q", client.Name(), client.baseURL.String())
	}
}

func TestClientExposesOnlyReadOperations(t *testing.T) {
	clientType := reflect.TypeOf(&Client{})
	var exposed []string
	for i := 0; i < clientType.NumMethod(); i++ {
		exposed = append(exposed, clientType.Method(i).Name)
	}
	if !reflect.DeepEqual(exposed, []string{"ListIssues", "Name"}) {
		t.Fatalf("exposed methods = %v, want only ListIssues and Name", exposed)
	}
	var probed []string
	for _, name := range []string{"POST", "PATCH", "PUT", "DELETE", "Create", "Update", "Delete", "Comment", "Merge", "ListIssues", "Name"} {
		if _, ok := clientType.MethodByName(name); ok {
			probed = append(probed, name)
		}
	}
	if !reflect.DeepEqual(probed, []string{"ListIssues", "Name"}) {
		t.Fatalf("probed methods = %v, want only ListIssues and Name", probed)
	}
}

func TestBotCommentDoesNotCreateRevision(t *testing.T) {
	issue := `{"number":7,"title":"t","body":"","state":"open","user":{"login":"maint"},"assignees":[],"labels":[],"comments":2,` +
		`"created_at":"2026-09-24T08:00:00Z","updated_at":"2026-09-24T10:00:00Z","html_url":"https://github.com/o/r/issues/7"}`
	comments := `[{"user":{"login":"maint"},"updated_at":"2026-09-24T09:00:00Z"},{"user":{"login":"my-app[bot]"},"updated_at":"2026-09-24T10:00:00Z"}]`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/comments") {
			_, _ = w.Write([]byte(comments))
			return
		}
		_, _ = w.Write([]byte("[" + issue + "]"))
	}))
	defer server.Close()
	revision := func(botLogin string) string {
		client, err := New(Config{BaseURL: server.URL, Repository: "o/r", TokenFile: tokenFile(t, "tok\n"), BotLogin: botLogin})
		if err != nil {
			t.Fatal(err)
		}
		issues, bad, err := CollectIssues(context.Background(), client)
		if err != nil || len(bad) != 0 || len(issues) != 1 {
			t.Fatalf("collect: %v bad=%d issues=%d", err, len(bad), len(issues))
		}
		return issues[0].RevisionID()
	}
	if got := revision(""); got != "2026-09-24T10:00:00Z" {
		t.Fatalf("without bot login the revision is updated_at, got %s", got)
	}
	if got := revision("My-App[bot]"); got != "2026-09-24T09:00:00Z" {
		t.Fatalf("bot comment created a new revision: %s", got)
	}
	comments = `[{"user":{"login":"maint"},"updated_at":"2026-09-24T10:00:00Z"},{"user":{"login":"my-app[bot]"},"updated_at":"2026-09-24T09:00:00Z"}]`
	if got := revision("my-app[bot]"); got != "2026-09-24T10:00:00Z" {
		t.Fatalf("human comment must still create a revision: %s", got)
	}
}
