package feedbackgithub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/fieldfeedback"
)

const (
	defaultAPIBaseURL = "https://api.github.com"
	maxResponseBytes  = 1 << 20
	userAgent         = "summa42-feedback/1"
	// maxReconcilePages bounds findByMarker's walk of the issue history.
	maxReconcilePages = 10
)

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

type CredentialSource interface {
	Token(context.Context) (string, error)
}

type FileCredentialSource struct {
	Path string
}

func (s FileCredentialSource) Token(_ context.Context) (string, error) {
	path := strings.TrimSpace(s.Path)
	if path == "" {
		return "", errors.New("GitHub credential file path is required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("stat GitHub credential file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("GitHub credential path must be a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("GitHub credential file must not be group/world accessible")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read GitHub credential file: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", errors.New("GitHub credential file is empty")
	}
	return token, nil
}

type Config struct {
	APIBaseURL       string
	Repository       string
	CredentialSource CredentialSource
	HTTPClient       *http.Client
	// BotLogin, when set, restricts reconciliation matches to issues it authored.
	BotLogin string
}

type client struct {
	baseURL    *url.URL
	repository string
	credential CredentialSource
	http       *http.Client
	botLogin   string
}

type issueResponse struct {
	Number  int64  `json:"number"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
}

func newClient(cfg Config) (*client, error) {
	repository := strings.TrimSpace(cfg.Repository)
	if !repositoryPattern.MatchString(repository) {
		return nil, errors.New("GitHub feedback repository must be owner/name")
	}
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
		return nil, errors.New("GitHub feedback repository must be owner/name")
	}
	base := strings.TrimSpace(cfg.APIBaseURL)
	if base == "" {
		base = defaultAPIBaseURL
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return nil, errors.New("GitHub API base URL must be an absolute HTTP(S) URL without user info")
	}
	if parsed.Scheme != "https" {
		host := strings.TrimSpace(parsed.Hostname())
		ip := net.ParseIP(host)
		if parsed.Scheme != "http" || (host != "localhost" && (ip == nil || !ip.IsLoopback())) {
			return nil, errors.New("GitHub API base URL must use HTTPS except for explicit loopback test endpoints")
		}
	}
	if cfg.CredentialSource == nil {
		return nil, errors.New("GitHub credential source is required")
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	return &client{
		baseURL: parsed, repository: repository,
		credential: cfg.CredentialSource, http: httpClient,
		botLogin: strings.TrimSpace(cfg.BotLogin),
	}, nil
}

func (c *client) createIssue(ctx context.Context, payload fieldfeedback.IssuePayload) (string, int64, error) {
	body := issueCreateRequest(payload)
	endpoint := c.resolve("/repos/" + c.repository + "/issues")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("create GitHub issue request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	var response issueResponse
	status, err := c.doJSON(req, &response)
	if err != nil {
		return "", 0, err
	}
	if status != http.StatusCreated {
		return "", 0, sanitizedStatusError("create issue", status)
	}
	reference := strings.TrimSpace(response.HTMLURL)
	if reference == "" && response.Number > 0 {
		reference = fmt.Sprintf("github:%s#%d", c.repository, response.Number)
	}
	if reference == "" {
		return "", 0, errors.New("GitHub create issue response lacks issue reference")
	}
	return reference, 0, nil
}

func (c *client) findByMarker(ctx context.Context, marker string) (string, bool, error) {
	marker = strings.TrimSpace(marker)
	if marker == "" {
		return "", false, errors.New("GitHub reconciliation marker is required")
	}
	endpoint := c.resolve("/repos/" + c.repository + "/issues")
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", false, err
	}
	query := parsed.Query()
	query.Set("state", "all")
	query.Set("per_page", "100")
	query.Set("sort", "created")
	// Ascending creation order keeps page boundaries stable when new issues are
	// created while reconciliation is walking the repository history.
	query.Set("direction", "asc")
	exact := "<!-- " + marker + " -->"
	for page := 1; page <= maxReconcilePages; page++ {
		query.Set("page", fmt.Sprintf("%d", page))
		parsed.RawQuery = query.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
		if err != nil {
			return "", false, fmt.Errorf("create GitHub reconcile request: %w", err)
		}
		var issues []issueResponse
		status, err := c.doJSON(req, &issues)
		if err != nil {
			return "", false, err
		}
		if status != http.StatusOK {
			return "", false, sanitizedStatusError("reconcile issue", status)
		}
		for _, issue := range issues {
			if !strings.Contains(issue.Body, exact) {
				continue
			}
			if c.botLogin != "" && !strings.EqualFold(issue.User.Login, c.botLogin) {
				continue
			}
			reference := strings.TrimSpace(issue.HTMLURL)
			if reference == "" && issue.Number > 0 {
				reference = fmt.Sprintf("github:%s#%d", c.repository, issue.Number)
			}
			if reference == "" {
				return "", false, errors.New("GitHub reconciliation found marker without issue reference")
			}
			return reference, true, nil
		}
		if len(issues) < 100 {
			return "", false, nil
		}
	}
	// ponytail: a repository with more than maxReconcilePages*100 issues cannot be
	// walked; fail closed rather than report "not found" and risk a duplicate.
	return "", false, fmt.Errorf("GitHub reconciliation did not finish within %d pages", maxReconcilePages)
}

func (c *client) doJSON(req *http.Request, out any) (int, error) {
	token, err := c.credential.Token(req.Context())
	if err != nil {
		return 0, fmt.Errorf("load GitHub credential: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent)
	response, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("GitHub request failed before a trustworthy outcome was known: %w", err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maxResponseBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return response.StatusCode, errors.New("read GitHub response failed")
	}
	if len(raw) > maxResponseBytes {
		return response.StatusCode, errors.New("GitHub response exceeded safe size limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, sanitizedStatusError("GitHub API", response.StatusCode)
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return response.StatusCode, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(out); err != nil {
		return response.StatusCode, errors.New("decode GitHub response failed")
	}
	return response.StatusCode, nil
}

func (c *client) resolve(path string) string {
	base := *c.baseURL
	base.Path = strings.TrimRight(base.Path, "/") + path
	base.RawQuery = ""
	base.Fragment = ""
	return base.String()
}

func sanitizedStatusError(operation string, status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%s rejected by GitHub with status %d", operation, status)
	default:
		return fmt.Errorf("%s failed with GitHub status %d", operation, status)
	}
}

func issueCreateRequest(payload fieldfeedback.IssuePayload) []byte {
	body := strings.TrimSpace(payload.Body)
	marker := strings.TrimSpace(payload.Marker)
	if marker != "" {
		if body != "" {
			body += "\n\n"
		}
		body += "<!-- " + marker + " -->"
	}
	encoded, _ := json.Marshal(map[string]string{
		"title": strings.TrimSpace(payload.Title),
		"body":  body,
	})
	return encoded
}
