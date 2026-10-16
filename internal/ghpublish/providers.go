// Package ghpublish mediates create-only publication of an exact reviewed candidate.
package ghpublish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/SofiaFlux/summa42/internal/repoworkspace"
	"github.com/SofiaFlux/summa42/internal/resources"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const BranchProviderName = "github-candidate-branch"
const PRProviderName = "github-candidate-pr"

type CredentialSource interface {
	Token(context.Context) (string, error)
}
type Config struct {
	APIBaseURL, Repository string
	CredentialSource       CredentialSource
	HTTPClient             *http.Client
}
type BranchIntent struct {
	Repository, BaseBranch, HeadBranch string
	Export                             repoworkspace.Export
}

func (BranchIntent) DescriptorType() string { return "github.candidate.branch.v1" }

type PRIntent struct{ Repository, BaseBranch, HeadBranch, BaseSHA, HeadSHA, Title, Body, Marker string }

func (PRIntent) DescriptorType() string { return "github.candidate.pr.v1" }

type client struct {
	base, repo  string
	credentials CredentialSource
	http        *http.Client
}

var repository = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]*/[A-Za-z0-9_-][A-Za-z0-9_.-]*$`)
var refName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./-]{0,199}$`)
var managedHead = regexp.MustCompile(`^summa42/issue-[1-9][0-9]*-[0-9a-f]{16}$`)
var marker = regexp.MustCompile(`^summa42:publication:[0-9a-f]{64}$`)

func newClient(c Config) (*client, error) {
	if c.APIBaseURL == "" {
		c.APIBaseURL = "https://api.github.com"
	}
	u, err := url.Parse(c.APIBaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()) != nil && net.ParseIP(u.Hostname()).IsLoopback()))) || !repository.MatchString(c.Repository) || c.CredentialSource == nil {
		return nil, errors.New("invalid GitHub publication configuration")
	}
	h := http.Client{}
	if c.HTTPClient != nil {
		h = *c.HTTPClient
	}
	h.Timeout = 20 * time.Second
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client{strings.TrimRight(c.APIBaseURL, "/"), c.Repository, c.CredentialSource, &h}, nil
}
func validRef(s string) bool {
	return refName.MatchString(s) && !strings.Contains(s, "..") && !strings.Contains(s, "//") && !strings.HasSuffix(s, "/") && !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, ".lock")
}
func (c *client) request(ctx context.Context, method, path string, in, out any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/repos/"+c.repo+path, body)
	if err != nil {
		return 0, errors.New("invalid GitHub request")
	}
	token, err := c.credentials.Token(ctx)
	if err != nil {
		return 0, errors.New("GitHub credential unavailable")
	}
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return 0, errors.New("invalid GitHub credential")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return 0, errors.New("GitHub transport failed")
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return res.StatusCode, errors.New("invalid GitHub response size")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return res.StatusCode, fmt.Errorf("GitHub status %d", res.StatusCode)
	}
	if out != nil {
		d := json.NewDecoder(bytes.NewReader(raw))
		if d.Decode(out) != nil {
			return res.StatusCode, errors.New("invalid GitHub JSON")
		}
		if d.Decode(new(any)) != io.EOF {
			return res.StatusCode, errors.New("trailing GitHub JSON")
		}
	}
	return res.StatusCode, nil
}
func (c *client) ref(ctx context.Context, name string) (string, int, error) {
	var v struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	status, err := c.request(ctx, "GET", "/git/ref/heads/"+name, nil, &v)
	return v.Object.SHA, status, err
}
func decode(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return errors.New("invalid canonical publication intent")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing publication intent")
	}
	return nil
}
func unknown() operations.ProviderOutcome {
	return operations.ProviderOutcome{State: domain.OperationOutcomeUnknown}
}

type BranchProvider struct{ c *client }
type PRProvider struct{ c *client }

func NewBranchProvider(c Config) (*BranchProvider, error) {
	x, e := newClient(c)
	return &BranchProvider{x}, e
}
func NewPRProvider(c Config) (*PRProvider, error)                 { x, e := newClient(c); return &PRProvider{x}, e }
func (*BranchProvider) Name() string                              { return BranchProviderName }
func (*PRProvider) Name() string                                  { return PRProviderName }
func (*BranchProvider) Capability() string                        { return "github.repo.publish" }
func (*PRProvider) Capability() string                            { return "github.pr.create" }
func (*BranchProvider) EnforcementLevel() domain.EnforcementLevel { return domain.EnforcementEnforced }
func (*PRProvider) EnforcementLevel() domain.EnforcementLevel     { return domain.EnforcementEnforced }
func (*BranchProvider) AdapterVersion() string                    { return "1" }
func (*PRProvider) AdapterVersion() string                        { return "1" }
func (*BranchProvider) AdapterVersionSemanticallyRelevant() bool  { return true }
func (*PRProvider) AdapterVersionSemanticallyRelevant() bool      { return true }
func (p *BranchProvider) validate(v BranchIntent) error {
	if v.Repository != p.c.repo || !validRef(v.BaseBranch) || !managedHead.MatchString(v.HeadBranch) || v.BaseBranch == v.HeadBranch {
		return errors.New("invalid branch publication intent")
	}
	return v.Export.Validate()
}
func (p *PRProvider) validate(v PRIntent) error {
	if v.Repository != p.c.repo || !validRef(v.BaseBranch) || !managedHead.MatchString(v.HeadBranch) || v.BaseBranch == v.HeadBranch || !repoworkspace.FullCommit(v.BaseSHA) || !repoworkspace.FullCommit(v.HeadSHA) || len(v.Title) == 0 || len(v.Title) > 256 || len(v.Body) > 16384 || !utf8.ValidString(v.Title+v.Body) || strings.ContainsRune(v.Title+v.Body, 0) || !marker.MatchString(v.Marker) || !strings.HasSuffix(v.Body, "<!-- "+v.Marker+" -->") {
		return errors.New("invalid draft publication intent")
	}
	return nil
}
func (p *BranchProvider) CanonicalIntent(d operations.IntentDescriptor) ([]byte, error) {
	v, ok := d.(BranchIntent)
	if !ok {
		return nil, errors.New("branch descriptor required")
	}
	if err := p.validate(v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
func (p *PRProvider) CanonicalIntent(d operations.IntentDescriptor) ([]byte, error) {
	v, ok := d.(PRIntent)
	if !ok {
		return nil, errors.New("PR descriptor required")
	}
	if err := p.validate(v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
func (p *BranchProvider) CostProfile(d operations.IntentDescriptor) (operations.CostProfile, error) {
	_, e := p.CanonicalIntent(d)
	return operations.CostProfile{MaxExposure: 3, Enforceability: resources.Enforceability{CostControl: resources.CostTechnicallyCapped, RequireHardCap: true, Source: "bounded GitHub write-request units"}}, e
}
func (p *PRProvider) CostProfile(d operations.IntentDescriptor) (operations.CostProfile, error) {
	_, e := p.CanonicalIntent(d)
	return operations.CostProfile{MaxExposure: 1, Enforceability: resources.Enforceability{CostControl: resources.CostTechnicallyCapped, RequireHardCap: true, Source: "bounded GitHub write-request units"}}, e
}
func (p *BranchProvider) Dispatch(ctx context.Context, r operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	var v BranchIntent
	if e := decode(r.CanonicalIntent, &v); e != nil {
		return unknown(), e
	}
	if e := p.validate(v); e != nil {
		return unknown(), e
	}
	base, _, e := p.c.ref(ctx, v.BaseBranch)
	if e != nil || base != v.Export.Candidate.BaseSHA {
		return unknown(), errors.New("publication base moved or unavailable")
	}
	head, status, e := p.c.ref(ctx, v.HeadBranch)
	if e == nil {
		if head == v.Export.Candidate.CandidateSHA {
			return p.LookupOutcome(ctx, r)
		}
		return unknown(), errors.New("managed branch already has different commit")
	}
	if status != 404 {
		return unknown(), e
	}
	var b struct {
		SHA  string `json:"sha"`
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if _, e = p.c.request(ctx, "GET", "/git/commits/"+base, nil, &b); e != nil || b.SHA != base || b.Tree.SHA != v.Export.BaseTreeSHA {
		return unknown(), errors.New("base tree mismatch")
	}
	tree := []map[string]any{}
	for _, f := range v.Export.Files {
		x := map[string]any{"path": f.Path, "mode": f.Mode, "type": "blob"}
		if f.Delete {
			x["sha"] = nil
		} else {
			x["content"] = f.Content
		}
		tree = append(tree, x)
	}
	var sha struct {
		SHA string `json:"sha"`
	}
	if _, e = p.c.request(ctx, "POST", "/git/trees", map[string]any{"base_tree": v.Export.BaseTreeSHA, "tree": tree}, &sha); e != nil {
		return unknown(), e
	}
	if sha.SHA != v.Export.Candidate.TreeSHA {
		return unknown(), errors.New("GitHub tree differs from reviewed candidate")
	}
	if _, e = p.c.request(ctx, "POST", "/git/commits", map[string]any{"tree": sha.SHA, "parents": []string{base}, "message": v.Export.Message, "author": v.Export.Author, "committer": v.Export.Committer}, &sha); e != nil {
		return unknown(), e
	}
	if sha.SHA != v.Export.Candidate.CandidateSHA {
		return unknown(), errors.New("GitHub commit differs from reviewed candidate")
	}
	if _, e = p.c.request(ctx, "POST", "/git/refs", map[string]string{"ref": "refs/heads/" + v.HeadBranch, "sha": sha.SHA}, nil); e != nil {
		return unknown(), e
	}
	return p.LookupOutcome(ctx, r)
}
func (p *BranchProvider) LookupOutcome(ctx context.Context, r operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	var v BranchIntent
	if e := decode(r.CanonicalIntent, &v); e != nil {
		return unknown(), e
	}
	if e := p.validate(v); e != nil {
		return unknown(), e
	}
	base, _, e := p.c.ref(ctx, v.BaseBranch)
	if e != nil || base != v.Export.Candidate.BaseSHA {
		return unknown(), nil
	}
	head, _, e := p.c.ref(ctx, v.HeadBranch)
	if e != nil || head != v.Export.Candidate.CandidateSHA {
		return unknown(), nil
	}
	return operations.ProviderOutcome{State: domain.OperationConfirmedEffect, ProviderReference: "https://github.com/" + v.Repository + "/tree/" + v.HeadBranch, ActualCost: 3}, nil
}

type pull struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Draft  bool     `json:"draft"`
	Head   endpoint `json:"head"`
	Base   endpoint `json:"base"`
}
type endpoint struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}

func (p *PRProvider) find(ctx context.Context, v PRIntent) (operations.ProviderOutcome, error) {
	found := 0
	number := 0
	for page := 1; page <= 100; page++ {
		var prs []pull
		q := url.Values{"state": {"all"}, "head": {strings.Split(v.Repository, "/")[0] + ":" + v.HeadBranch}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if _, e := p.c.request(ctx, "GET", "/pulls?"+q.Encode(), nil, &prs); e != nil {
			return unknown(), e
		}
		if prs == nil {
			return unknown(), errors.New("GitHub PR page must be a non-null array")
		}
		for _, pr := range prs {
			if !strings.Contains(pr.Body, v.Marker) {
				continue
			}
			found++
			if pr.Number <= 0 || pr.Title != v.Title || pr.Body != v.Body || !pr.Draft || pr.Head.Ref != v.HeadBranch || pr.Head.SHA != v.HeadSHA || pr.Head.Repo.FullName != v.Repository || pr.Base.Ref != v.BaseBranch || pr.Base.SHA != v.BaseSHA || pr.Base.Repo.FullName != v.Repository {
				return unknown(), errors.New("publication marker has conflicting PR")
			}
			number = pr.Number
		}
		if len(prs) < 100 {
			if found == 1 {
				return operations.ProviderOutcome{State: domain.OperationConfirmedEffect, ProviderReference: fmt.Sprintf("https://github.com/%s/pull/%d", v.Repository, number), ActualCost: 1}, nil
			}
			if found > 1 {
				return unknown(), errors.New("ambiguous publication marker")
			}
			return unknown(), nil
		}
	}
	return unknown(), errors.New("GitHub PR pagination limit")
}
func (p *PRProvider) pinned(ctx context.Context, v PRIntent) bool {
	base, _, e := p.c.ref(ctx, v.BaseBranch)
	if e != nil || base != v.BaseSHA {
		return false
	}
	head, _, e := p.c.ref(ctx, v.HeadBranch)
	return e == nil && head == v.HeadSHA
}
func (p *PRProvider) Dispatch(ctx context.Context, r operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	var v PRIntent
	if e := decode(r.CanonicalIntent, &v); e != nil {
		return unknown(), e
	}
	if e := p.validate(v); e != nil {
		return unknown(), e
	}
	if !p.pinned(ctx, v) {
		return unknown(), errors.New("publication refs moved or unavailable")
	}
	out, e := p.find(ctx, v)
	if e != nil || out.State == domain.OperationConfirmedEffect {
		return out, e
	}
	if _, e = p.c.request(ctx, "POST", "/pulls", map[string]any{"head": v.HeadBranch, "base": v.BaseBranch, "title": v.Title, "body": v.Body, "draft": true}, nil); e != nil {
		return unknown(), e
	}
	return p.LookupOutcome(ctx, r)
}
func (p *PRProvider) LookupOutcome(ctx context.Context, r operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	var v PRIntent
	if e := decode(r.CanonicalIntent, &v); e != nil {
		return unknown(), e
	}
	if e := p.validate(v); e != nil {
		return unknown(), e
	}
	if !p.pinned(ctx, v) {
		return unknown(), nil
	}
	out, e := p.find(ctx, v)
	if e != nil {
		return unknown(), nil
	}
	return out, nil
}
