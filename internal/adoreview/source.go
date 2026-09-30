// Package adoreview observes reviewer-assigned Azure DevOps pull requests and
// turns new revisions into durable review Tasks. It performs no external writes.
package adoreview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	ReasonDraft                 = "draft"
	ReasonSelfAuthored          = "self-authored"
	ReasonGroupOnly             = "group-only-assignment"
	ReasonUnconfirmedAssignment = "cannot-confirm-direct-assignment"
	ReasonUnparseable           = "unparseable-pr"
)

type PRCaller interface {
	Call(ctx context.Context, capability string, request any) (any, error)
}

type Reviewer struct {
	ID      string
	IsGroup bool
}

type PullRequest struct {
	Repository   string
	Number       int64
	Title        string
	IsDraft      bool
	AuthorID     string
	Reviewers    []Reviewer
	SourceCommit string
	TargetCommit string
}

type Unparseable struct {
	Raw any
}

type ExcludedPR struct {
	PR     PullRequest
	Reason string
	Detail string
}

func (p PullRequest) ObjectID() string   { return p.Repository + "#" + fmt.Sprint(p.Number) }
func (p PullRequest) RevisionID() string { return p.SourceCommit + ":" + p.TargetCommit }

func ListPRs(ctx context.Context, caller PRCaller, project, repository string) ([]PullRequest, []Unparseable, error) {
	if caller == nil {
		return nil, nil, errors.New("PR caller is required")
	}
	var out []PullRequest
	var bad []Unparseable
	token := ""
	for {
		items, next, err := listPage(ctx, caller, project, repository, token)
		if err != nil {
			return nil, nil, err
		}
		for _, item := range items {
			pr, err := ParsePR(item)
			if err != nil {
				bad = append(bad, Unparseable{Raw: item})
				continue
			}
			out = append(out, pr)
		}
		if len(items) == 0 || next == "" {
			return out, bad, nil
		}
		token = next
	}
}

func listPage(ctx context.Context, caller PRCaller, project, repository, token string) ([]any, string, error) {
	var capability string
	args := map[string]any{}
	if project != "" || repository != "" {
		capability = "ado.pr.list"
		args["action"] = "list"
		if project != "" {
			args["project"] = project
		}
		if repository != "" {
			// Azure DevOps' real repo_pull_request "list" action names this
			// parameter repositoryId, not repository (confirmed against the
			// live tool schema); adomcp forwards this map as-is.
			args["repositoryId"] = repository
		}
	} else {
		capability = "ado.pr.org_active"
	}
	if token != "" {
		args["continuationToken"] = token
	}
	var request any
	if len(args) > 0 {
		request = args
	}
	raw, err := caller.Call(ctx, capability, request)
	if err != nil {
		return nil, "", err
	}
	return pageItems(raw), pageToken(raw), nil
}

func pageItems(raw any) []any {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range []string{"prs", "value"} {
		if items, ok := m[key].([]any); ok {
			return items
		}
	}
	return nil
}

func pageToken(raw any) string {
	m, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range []string{"continuationToken", "nextPageToken"} {
		if token, ok := m[key].(string); ok && strings.TrimSpace(token) != "" {
			return token
		}
	}
	return ""
}

func ParsePR(item any) (PullRequest, error) {
	m, ok := item.(map[string]any)
	if !ok {
		return PullRequest{}, errors.New("PR must be an object")
	}
	var pr PullRequest
	repo, _ := m["repository"].(string)
	if strings.TrimSpace(repo) == "" {
		return PullRequest{}, errors.New("PR repository is required")
	}
	pr.Repository = repo
	switch number := m["number"].(type) {
	case float64:
		pr.Number = int64(number)
	case int64:
		pr.Number = number
	case int:
		pr.Number = int64(number)
	case json.Number:
		n, err := number.Int64()
		if err != nil {
			return PullRequest{}, fmt.Errorf("PR number is invalid: %v", m["number"])
		}
		pr.Number = n
	default:
		return PullRequest{}, errors.New("PR number is required")
	}
	if pr.Number <= 0 {
		return PullRequest{}, errors.New("PR number is required")
	}
	source, _ := m["sourceCommit"].(string)
	if strings.TrimSpace(source) == "" {
		return PullRequest{}, errors.New("PR sourceCommit is required")
	}
	target, _ := m["targetCommit"].(string)
	if strings.TrimSpace(target) == "" {
		return PullRequest{}, errors.New("PR targetCommit is required")
	}
	pr.SourceCommit, pr.TargetCommit = source, target
	pr.Title, _ = m["title"].(string)
	if draft, ok := m["isDraft"].(bool); ok {
		pr.IsDraft = draft
	} else if draft, ok := m["draft"].(bool); ok {
		pr.IsDraft = draft
	}
	switch author := m["author"].(type) {
	case string:
		pr.AuthorID = author
	case map[string]any:
		pr.AuthorID, _ = author["id"].(string)
	}
	if reviewers, ok := m["reviewers"].([]any); ok {
		for _, r := range reviewers {
			switch reviewer := r.(type) {
			case string:
				pr.Reviewers = append(pr.Reviewers, Reviewer{ID: reviewer})
			case map[string]any:
				id, _ := reviewer["id"].(string)
				isGroup, _ := reviewer["isGroup"].(bool)
				if !isGroup {
					isGroup, _ = reviewer["group"].(bool)
				}
				if !isGroup {
					// Azure DevOps' real reviewer object marks team/group
					// reviewers with isContainer, not isGroup/group
					// (confirmed against a live PR's get response).
					isGroup, _ = reviewer["isContainer"].(bool)
				}
				if strings.TrimSpace(id) != "" {
					pr.Reviewers = append(pr.Reviewers, Reviewer{ID: id, IsGroup: isGroup})
				}
			}
		}
	}
	return pr, nil
}

func FilterPRs(prs []PullRequest, reviewerID string) (kept []PullRequest, excluded []ExcludedPR) {
	for _, pr := range prs {
		switch {
		case pr.IsDraft:
			excluded = append(excluded, ExcludedPR{PR: pr, Reason: ReasonDraft})
		case pr.AuthorID != "" && pr.AuthorID == reviewerID:
			excluded = append(excluded, ExcludedPR{PR: pr, Reason: ReasonSelfAuthored})
		case hasDirectReviewer(pr, reviewerID):
			kept = append(kept, pr)
		case hasGroupReviewer(pr, reviewerID):
			excluded = append(excluded, ExcludedPR{PR: pr, Reason: ReasonGroupOnly})
		default:
			excluded = append(excluded, ExcludedPR{PR: pr, Reason: ReasonUnconfirmedAssignment})
		}
	}
	return kept, excluded
}

func hasDirectReviewer(pr PullRequest, reviewerID string) bool {
	for _, r := range pr.Reviewers {
		if !r.IsGroup && r.ID == reviewerID {
			return true
		}
	}
	return false
}

func hasGroupReviewer(pr PullRequest, _ string) bool {
	// Fail-closed intent: any group reviewer counts as group-only (conservative
	// exclusion direction) even when the reviewer identity is not checked here.
	for _, r := range pr.Reviewers {
		if r.IsGroup {
			return true
		}
	}
	return false
}
