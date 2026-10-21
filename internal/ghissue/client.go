package ghissue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAPIBaseURL = "https://api.github.com"
	defaultAPIHost    = "api.github.com"
	defaultTimeout    = 20 * time.Second
	defaultMaxBytes   = 16 << 20
	userAgent         = "summa42-ghissue/1"
	pageSize          = 100
)

// rejectRedirect keeps bearer credentials pinned to the configured origin:
// any 3xx is surfaced as a non-200 response instead of being followed.
func rejectRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Config configures the read-only client. Zero Timeout or MaxResponseBytes
// falls back to package defaults.
type Config struct {
	BaseURL          string
	Repository       string
	TokenFile        string
	Timeout          time.Duration
	MaxResponseBytes int
	// BotLogin is the GitHub App bot login (for example "my-app[bot]"). When set, an
	// issue whose latest update is only a comment by this login keeps the revision it
	// had before that comment. Empty keeps the issue's updated_at as the revision.
	BotLogin string
}

// Client performs GET requests only; no write method exists on the type.
type Client struct {
	baseURL          *url.URL
	repository       string
	tokenFile        string
	http             *http.Client
	maxResponseBytes int
	botLogin         string
}

func New(cfg Config) (*Client, error) {
	repository := strings.TrimSpace(cfg.Repository)
	if !repositoryPattern.MatchString(repository) {
		return nil, errors.New("GitHub issues repository must be owner/name")
	}
	for _, part := range strings.Split(repository, "/") {
		if part == "." || part == ".." {
			return nil, errors.New("GitHub issues repository must be owner/name")
		}
	}
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = defaultAPIBaseURL
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return nil, errors.New("GitHub API base URL must be an absolute HTTP(S) URL without user info")
	}
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, errors.New("GitHub API base URL must not carry a path, query, or fragment")
	}
	parsed.Path = ""
	host := strings.TrimSpace(parsed.Hostname())
	loopback := strings.EqualFold(host, "localhost") || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
	if parsed.Scheme != "https" {
		if parsed.Scheme != "http" || !loopback {
			return nil, errors.New("GitHub API base URL must use HTTPS except for loopback test endpoints")
		}
	} else if !loopback {
		if !strings.EqualFold(host, defaultAPIHost) {
			return nil, errors.New("GitHub API base URL host must be api.github.com")
		}
		if port := parsed.Port(); port != "" && port != "443" {
			return nil, errors.New("GitHub API base URL must not carry a non-standard port")
		}
	}
	tokenFile := strings.TrimSpace(cfg.TokenFile)
	if tokenFile == "" {
		return nil, errors.New("GitHub issues token file path is required")
	}
	if _, err := readTokenFile(tokenFile); err != nil {
		return nil, err
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	limit := cfg.MaxResponseBytes
	if limit <= 0 {
		limit = defaultMaxBytes
	}
	return &Client{
		baseURL:          parsed,
		repository:       repository,
		tokenFile:        tokenFile,
		http:             &http.Client{Timeout: timeout, CheckRedirect: rejectRedirect},
		maxResponseBytes: limit,
		botLogin:         strings.TrimSpace(cfg.BotLogin),
	}, nil
}

// Name returns the configured repository, used to qualify object IDs.
func (c *Client) Name() string {
	return c.repository
}

func (c *Client) ListIssues(ctx context.Context, cursor string) ([]any, []Unparseable, string, error) {
	if ctx == nil {
		return nil, nil, "", errors.New("GitHub issues context is required")
	}
	target, err := c.pageURL(cursor)
	if err != nil {
		return nil, nil, "", err
	}
	token, err := readTokenFile(c.tokenFile)
	if err != nil {
		return nil, nil, "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, nil, "", fmt.Errorf("build GitHub issues request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("User-Agent", userAgent)
	response, err := c.http.Do(request)
	if err != nil {
		return nil, nil, "", fmt.Errorf("list GitHub issues: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, nil, "", fmt.Errorf("list GitHub issues: unexpected status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(c.maxResponseBytes)+1))
	if err != nil {
		return nil, nil, "", fmt.Errorf("read GitHub issues response: %w", err)
	}
	if len(body) > c.maxResponseBytes {
		return nil, nil, "", fmt.Errorf("GitHub issues response exceeds %d bytes", c.maxResponseBytes)
	}
	var raw []any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, "", fmt.Errorf("decode GitHub issues response: %w", err)
	}
	if raw == nil {
		return nil, nil, "", errors.New("decode GitHub issues response: expected a JSON array")
	}
	next, err := c.nextCursor(response.Header.Get("Link"))
	if err != nil {
		return nil, nil, "", err
	}
	items := make([]any, 0, len(raw))
	var unparseable []Unparseable
	for _, item := range raw {
		if err := classifyWireItem(item); err != nil {
			unparseable = append(unparseable, Unparseable{Raw: item})
			continue
		}
		if c.botLogin != "" {
			if err := c.discountBotComments(ctx, token, item.(map[string]any)); err != nil {
				return nil, nil, "", err
			}
		}
		items = append(items, item)
	}
	return items, unparseable, next, nil
}

// issuesURL builds one request URL from the client's own pinned base URL,
// repository and filter query. The cursor contributes nothing but the page
// number, so no part of the outgoing path or query is server-influenceable.
func (c *Client) issuesURL(page int) string {
	return c.baseURL.String() + "/repos/" + c.repository + "/issues?state=open&per_page=" +
		strconv.Itoa(pageSize) + "&page=" + strconv.Itoa(page)
}

// pageURL turns a cursor back into a request URL. The cursor is the page number
// the client derived from the last Link header, never a URL, so there is no
// server-supplied origin, path, filter or fragment left to validate.
func (c *Client) pageURL(cursor string) (string, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return c.issuesURL(1), nil
	}
	page, err := strconv.Atoi(cursor)
	if err != nil || page < 1 {
		return "", errors.New("GitHub issues pagination cursor is not a page number")
	}
	return c.issuesURL(page), nil
}

func (c *Client) nextCursor(link string) (string, error) {
	link = strings.TrimSpace(link)
	if link == "" {
		return "", nil
	}
	next := ""
	for _, section := range strings.Split(link, ",") {
		section = strings.TrimSpace(section)
		if section == "" {
			return "", errors.New("GitHub issues Link header has an empty section")
		}
		open := strings.Index(section, "<")
		closing := strings.Index(section, ">")
		if open != 0 || closing <= open+1 {
			return "", errors.New("GitHub issues Link header is malformed")
		}
		target := section[open+1 : closing]
		params := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(section[closing+1:]), ";"))
		isNext := false
		if params != "" {
			for _, param := range strings.Split(params, ";") {
				key, value, found := strings.Cut(strings.TrimSpace(param), "=")
				if !found || len(value) < 2 || !strings.HasPrefix(value, `"`) || !strings.HasSuffix(value, `"`) {
					return "", errors.New("GitHub issues Link header parameter is malformed")
				}
				if strings.TrimSpace(key) == "rel" && value == `"next"` {
					isNext = true
				}
			}
		}
		if !isNext {
			continue
		}
		parsed, err := url.Parse(target)
		if err != nil || !parsed.IsAbs() {
			return "", errors.New("GitHub issues next link is malformed")
		}
		if parsed.Scheme != c.baseURL.Scheme || parsed.Host != c.baseURL.Host {
			return "", errors.New("GitHub issues next link is cross-origin")
		}
		if parsed.User != nil || parsed.Fragment != "" {
			return "", errors.New("GitHub issues next link is malformed")
		}
		page, err := nextPageQuery(parsed)
		if err != nil {
			return "", err
		}
		if next != "" {
			return "", errors.New("GitHub issues Link header repeats rel=next")
		}
		next = strconv.Itoa(page)
	}
	return next, nil
}

// nextPageQuery reads the page number a next link advances to. GitHub sends the
// numeric form carrying only the pagination parameters, so any other key is
// rejected rather than interpreted, and a next link that cannot advance — no
// page, a non-numeric one, or page 1 — is malformed. A per_page is accepted
// only when it is the pinned page size, because the next request is rebuilt
// with that size: a disagreeing one would make the page number a different
// window of issues.
func nextPageQuery(parsed *url.URL) (int, error) {
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return 0, errors.New("GitHub issues page query is malformed")
	}
	for key := range query {
		if key != "page" && key != "per_page" {
			return 0, errors.New("GitHub issues page query has an unexpected parameter")
		}
	}
	if len(query["per_page"]) > 1 {
		return 0, errors.New("GitHub issues page query repeats the per_page parameter")
	}
	if len(query["per_page"]) == 1 {
		size, err := strconv.Atoi(query["per_page"][0])
		if err != nil || size < 1 {
			return 0, errors.New("GitHub issues page query has an invalid per_page parameter")
		}
		if size != pageSize {
			return 0, errors.New("GitHub issues page query per_page disagrees with the pinned page size")
		}
	}
	if len(query["page"]) != 1 {
		return 0, errors.New("GitHub issues page query must carry exactly one page parameter")
	}
	number, err := strconv.Atoi(query["page"][0])
	if err != nil || number < 2 {
		return 0, errors.New("GitHub issues page query has an invalid page parameter")
	}
	return number, nil
}

func readTokenFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("stat GitHub issues credential file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("GitHub issues credential path must be a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("GitHub issues credential file must not be group/world accessible")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read GitHub issues credential file: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", errors.New("GitHub issues credential file is empty")
	}
	return token, nil
}

// maxCommentPages bounds the comment scan for one issue; beyond it the issue's own
// updated_at is kept (the conservative, pre-existing behaviour).
const maxCommentPages = 10

// discountBotComments rewrites item["updated_at"] when the issue's latest update is
// exactly the latest comment and that comment was written by the configured bot: the
// revision then falls back to the latest non-bot comment, or the issue's creation time.
// ponytail: a human edit made after the last human comment and before the bot's reply
// is not seen as a new revision; the bot only replies to an already observed revision.
func (c *Client) discountBotComments(ctx context.Context, token string, item map[string]any) error {
	if n, ok := item["comments"].(float64); ok && n == 0 {
		return nil
	}
	number, err := numberOf(item)
	if err != nil {
		return err
	}
	updatedRaw, _ := item["updated_at"].(string)
	updated, err := time.Parse(time.RFC3339, updatedRaw)
	if err != nil {
		return nil
	}
	created, err := time.Parse(time.RFC3339, fmt.Sprint(item["created_at"]))
	if err != nil {
		return nil
	}
	var latest, latestHuman time.Time
	latestByBot := false
	for page := 1; ; page++ {
		if page > maxCommentPages {
			return nil
		}
		target := c.baseURL.String() + "/repos/" + c.repository + "/issues/" + strconv.FormatInt(number, 10) +
			"/comments?per_page=" + strconv.Itoa(pageSize) + "&page=" + strconv.Itoa(page)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return fmt.Errorf("build GitHub issue comments request: %w", err)
		}
		request.Header.Set("Accept", "application/vnd.github+json")
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("User-Agent", userAgent)
		response, err := c.http.Do(request)
		if err != nil {
			return fmt.Errorf("list GitHub issue comments: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, int64(c.maxResponseBytes)+1))
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("list GitHub issue comments: unexpected status %d", response.StatusCode)
		}
		if readErr != nil || len(body) > c.maxResponseBytes {
			return errors.New("read GitHub issue comments response failed or exceeded the size limit")
		}
		var comments []struct {
			User struct {
				Login string `json:"login"`
			} `json:"user"`
			UpdatedAt string `json:"updated_at"`
		}
		if err := json.Unmarshal(body, &comments); err != nil {
			return fmt.Errorf("decode GitHub issue comments: %w", err)
		}
		for _, comment := range comments {
			at, err := time.Parse(time.RFC3339, comment.UpdatedAt)
			if err != nil {
				return nil
			}
			bot := strings.EqualFold(comment.User.Login, c.botLogin)
			if !at.Before(latest) {
				latest, latestByBot = at, bot
			}
			if !bot && at.After(latestHuman) {
				latestHuman = at
			}
		}
		if len(comments) < pageSize {
			break
		}
	}
	if latest.IsZero() || !latestByBot || !latest.Equal(updated) {
		return nil
	}
	effective := created
	if latestHuman.After(effective) {
		effective = latestHuman
	}
	item["updated_at"] = effective.UTC().Format(time.RFC3339)
	return nil
}
