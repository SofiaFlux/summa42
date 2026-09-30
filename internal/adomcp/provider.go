// Package adomcp exposes a small, explicit read-only subset of Azure DevOps MCP.
package adomcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/capabilities"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const providerName = "ado-mcp"

var organizationPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,49}$`)

type Config struct {
	Command      string
	Organization string
	Timeout      time.Duration
	// DefaultProject scopes ado.pr.org_active. Azure DevOps has no endpoint
	// that lists active pull requests across an entire organization: the
	// underlying tool (repo_pull_request, action=list) requires a project
	// or repositoryId (confirmed live: the server rejects a scopeless list
	// call with "Either repositoryId or project must be provided", and the
	// server's own ado_mcp_project default env var does not apply to this
	// tool). DefaultProject is summa42's own, explicit stand-in: when set,
	// org_active lists active PRs in this one project; when unset,
	// org_active fails closed with an actionable error instead of a
	// confusing remote rejection.
	DefaultProject string
}

type mcpSession interface {
	ListTools(context.Context, *mcp.ListToolsParams) (*mcp.ListToolsResult, error)
	CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error)
	Close() error
}

type Provider struct {
	config Config
	dial   func(context.Context) (mcpSession, error)
}

func New(config Config) (*Provider, error) {
	config.Command = strings.TrimSpace(config.Command)
	config.Organization = strings.TrimSpace(config.Organization)
	config.DefaultProject = strings.TrimSpace(config.DefaultProject)
	if config.Command == "" || !organizationPattern.MatchString(config.Organization) {
		return nil, errors.New("ADO MCP requires an executable and a valid organization name")
	}
	if config.Timeout <= 0 {
		config.Timeout = 15 * time.Second
	}
	p := &Provider{config: config}
	p.dial = p.connect
	return p, nil
}

func (p *Provider) Name() string { return providerName }

func (p *Provider) Advertise(context.Context) ([]capabilities.Definition, error) {
	return []capabilities.Definition{
		p.definition("ado.projects.list", "core_list_projects"),
		p.definition("ado.work_item.read", "wit_work_item"),
		p.definition("ado.pr.list", "repo_pull_request"),
		p.definition("ado.pr.get", "repo_pull_request"),
		p.definition("ado.pr.org_active", "repo_pull_request"),
		p.definition("ado.pr.threads", "repo_pull_request_thread"),
		p.definition("ado.pr.file", "repo_file"),
		p.definition("ado.build.status", "pipelines_build"),
	}, nil
}

func (p *Provider) definition(name, tool string) capabilities.Definition {
	return capabilities.Definition{
		ID:          domain.ID("cap_" + strings.ReplaceAll(name, ".", "_") + "_v1"),
		Skill:       capabilities.Skill{Name: name, Version: "v1"},
		Access:      capabilities.Access{Provider: p.Name(), Context: "ado:" + p.config.Organization + ":" + tool},
		Authority:   capabilities.AuthorityRequirement{Capabilities: []string{name}},
		Environment: capabilities.EnvironmentRequirement{MinimumEnforcement: domain.EnforcementPartial},
	}
}

func (p *Provider) Probe(ctx context.Context, definition capabilities.Definition) (capabilities.ProbeResult, error) {
	tool, err := toolFor(definition.Skill.Name)
	if err != nil {
		return capabilities.ProbeResult{}, err
	}
	available, err := p.hasTool(ctx, tool)
	if err != nil {
		return capabilities.ProbeResult{}, err
	}
	health := capabilities.HealthHealthy
	if !available {
		health = capabilities.HealthUnhealthy
	}
	return capabilities.ProbeResult{
		Enforcement:  domain.EnforcementPartial,
		Evidence:     []string{"ado-mcp:tool-present:" + tool + ":" + fmt.Sprint(available)},
		CostMetadata: map[string]any{"unit": "mcp-call", "class": "external-read"},
		Health:       health,
		Available:    available,
	}, nil
}

func (p *Provider) Call(ctx context.Context, capability string, request any) (any, error) {
	tool, err := toolFor(capability)
	if err != nil {
		return nil, err
	}
	args := map[string]any{}
	switch capability {
	case "ado.projects.list":
		if request != nil {
			m, ok := request.(map[string]any)
			if !ok || len(m) != 0 {
				return nil, errors.New("project listing accepts no parameters")
			}
		}
	case "ado.work_item.read":
		m, ok := request.(map[string]any)
		if !ok {
			return nil, errors.New("work item request must be an object")
		}
		action, ok := m["action"].(string)
		if !ok || !allowedAction(capability, action) {
			return nil, fmt.Errorf("work item action %q is not read-only", action)
		}
		if _, bypass := m["tool"]; bypass {
			return nil, errors.New("MCP tool override is forbidden")
		}
		for key, value := range m {
			args[key] = value
		}
	case "ado.pr.list", "ado.pr.get":
		m, ok := request.(map[string]any)
		if !ok {
			return nil, errors.New("pull request request must be an object")
		}
		action, ok := m["action"].(string)
		if !ok || !allowedAction(capability, action) {
			return nil, fmt.Errorf("pull request action %q is not allowed for %q", action, capability)
		}
		if _, bypass := m["tool"]; bypass {
			return nil, errors.New("MCP tool override is forbidden")
		}
		for key, value := range m {
			args[key] = value
		}
	case "ado.pr.org_active":
		if request != nil {
			m, ok := request.(map[string]any)
			if !ok {
				return nil, errors.New("org PR request must be an object")
			}
			if _, has := m["action"]; has {
				return nil, errors.New("org PR listing accepts no action")
			}
			if _, bypass := m["tool"]; bypass {
				return nil, errors.New("MCP tool override is forbidden")
			}
			for key, value := range m {
				args[key] = value
			}
		}
		// Azure DevOps has no organization-wide active-PR listing: the real
		// tool always requires a project or repositoryId scope (confirmed
		// live against @azure-devops/mcp: a scopeless list call is rejected
		// with "Either repositoryId or project must be provided"). Fail
		// closed here, before dialing, rather than forwarding a call the
		// remote server can only reject.
		if _, hasProject := args["project"]; !hasProject {
			if _, hasRepo := args["repositoryId"]; !hasRepo {
				if p.config.DefaultProject == "" {
					return nil, errors.New("ado.pr.org_active requires SUMMA42_ADO_DEFAULT_PROJECT: Azure DevOps has no true org-wide PR listing")
				}
				args["project"] = p.config.DefaultProject
			}
		}
		args["action"] = "list"
		return p.listAndEnrichPRs(ctx, args)
	case "ado.pr.threads", "ado.pr.file", "ado.build.status":
		m, ok := request.(map[string]any)
		if !ok {
			return nil, errors.New("request must be an object")
		}
		action, ok := m["action"].(string)
		if !ok || !allowedAction(capability, action) {
			return nil, fmt.Errorf("action %q is not allowed for %q", action, capability)
		}
		if _, bypass := m["tool"]; bypass {
			return nil, errors.New("MCP tool override is forbidden")
		}
		for key, value := range m {
			args[key] = value
		}
	}
	if capability == "ado.pr.list" && args["action"] == "list" {
		return p.listAndEnrichPRs(ctx, args)
	}
	callCtx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	session, err := p.dial(callCtx)
	if err != nil {
		return nil, fmt.Errorf("connect ADO MCP: %w", err)
	}
	defer session.Close()
	result, err := session.CallTool(callCtx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("ADO MCP %s: %w", tool, err)
	}
	if result == nil || result.IsError {
		return nil, fmt.Errorf("ADO MCP %s returned a tool error", tool)
	}
	decoded, err := decodeToolResult(result, tool)
	if err != nil {
		return nil, err
	}
	switch capability {
	case "ado.pr.get":
		return enrichPRGet(decoded), nil
	case "ado.pr.threads":
		if arr, ok := decoded.([]any); ok {
			return map[string]any{"threads": arr}, nil
		}
	}
	return decoded, nil
}

// maxPRDetailFetchesPerCall bounds the per-item repo_pull_request "get" calls
// a single ado.pr.list/ado.pr.org_active invocation will issue (the "list"
// action's response omits reviewers and merge-commit SHAs, so each active PR
// needs one enrichment "get"). A poll tick failing loudly beyond this cap is
// preferable to an unbounded, silent burst of MCP calls; narrow with
// --repository or a smaller project.
const maxPRDetailFetchesPerCall = 200

// listAndEnrichPRs pages the real repo_pull_request "list" action (skip/top;
// Azure DevOps has no continuation-token concept here) and, since "list"
// omits reviewers and merge-commit SHAs, issues one "get" per returned PR to
// build the canonical shape internal/adoreview.ParsePR expects. The
// returned map's "prs" key intentionally carries no continuationToken: this
// call already drained every page, matching the {"prs": [...]} contract
// PRCaller implementations are expected to return.
func (p *Provider) listAndEnrichPRs(ctx context.Context, listArgs map[string]any) (any, error) {
	const pageSize = 100
	project, _ := listArgs["project"].(string)
	repositoryID, _ := listArgs["repositoryId"].(string)
	var summaries []map[string]any
	for skip := 0; ; skip += pageSize {
		pageArgs := map[string]any{"action": "list", "top": pageSize, "skip": skip}
		if project != "" {
			pageArgs["project"] = project
		}
		if repositoryID != "" {
			pageArgs["repositoryId"] = repositoryID
		}
		for key, value := range listArgs {
			switch key {
			case "action", "project", "repositoryId", "top", "skip":
				continue
			default:
				pageArgs[key] = value
			}
		}
		page, err := p.callTool(ctx, "repo_pull_request", pageArgs)
		if err != nil {
			return nil, err
		}
		items, ok := page.([]any)
		if !ok {
			return nil, errors.New("ADO MCP repo_pull_request list did not return an array")
		}
		for _, item := range items {
			if m, ok := item.(map[string]any); ok {
				summaries = append(summaries, m)
			}
		}
		if len(summaries) > maxPRDetailFetchesPerCall {
			return nil, fmt.Errorf("ado.pr.list: more than %d active PRs to enrich in one poll; narrow with a --repository", maxPRDetailFetchesPerCall)
		}
		if len(items) < pageSize {
			break
		}
	}
	prs := make([]any, 0, len(summaries))
	for _, summary := range summaries {
		enriched, err := p.enrichPRSummary(ctx, summary)
		if err != nil {
			// One PR's detail "get" can fail transiently (confirmed live:
			// Azure DevOps intermittently returns an empty "Error with pull
			// request operation" for an otherwise-healthy PR) without the
			// whole org/project having a problem. Degrade to the summary-only
			// fields instead of aborting discovery for every other PR in this
			// poll: summary-only objects are missing sourceCommit/
			// targetCommit, so ParsePR's existing required-field validation
			// naturally buckets this one PR as Unparseable (with a reason)
			// for adoreview to retry next tick, exactly like any other
			// per-item failure the design already accounts for.
			enriched = map[string]any{
				"repository": summary["repository"],
				"number":     summary["pullRequestId"],
				"title":      summary["title"],
				"isDraft":    summary["isDraft"],
			}
		}
		prs = append(prs, enriched)
	}
	return map[string]any{"prs": prs}, nil
}

// enrichPRSummary fetches full PR detail (reviewers, merge-commit SHAs) for
// one "list" result item and returns the canonical object
// internal/adoreview.ParsePR expects: repository (string), number, title,
// isDraft, author (raw createdBy, ParsePR reads its "id"), sourceCommit,
// targetCommit, reviewers (raw array; each carries id/uniqueName/isContainer).
func (p *Provider) enrichPRSummary(ctx context.Context, summary map[string]any) (map[string]any, error) {
	repository, _ := summary["repository"].(string)
	project, _ := summary["project"].(string)
	pullRequestID, hasID := stringOrNumberFromAny(summary["pullRequestId"])
	if repository == "" || !hasID {
		return nil, errors.New("ADO MCP repo_pull_request list item is missing repository or pullRequestId")
	}
	getArgs := map[string]any{"action": "get", "repositoryId": repository, "pullRequestId": pullRequestID}
	if project != "" {
		getArgs["project"] = project
	}
	raw, err := p.callTool(ctx, "repo_pull_request", getArgs)
	if err != nil {
		return nil, err
	}
	full, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("ADO MCP repo_pull_request get did not return an object")
	}
	full = enrichPRGet(full)
	canonical := map[string]any{
		"repository":   repository,
		"number":       summary["pullRequestId"],
		"title":        summary["title"],
		"isDraft":      summary["isDraft"],
		"author":       full["createdBy"],
		"sourceCommit": full["sourceCommit"],
		"targetCommit": full["targetCommit"],
		"reviewers":    full["reviewers"],
	}
	return canonical, nil
}

// enrichPRGet adds flat, canonical aliases on top of a raw repo_pull_request
// "get" response without removing or renaming any original key (callers
// such as internal/adoeffects.reviewersContainVote still need the raw
// reviewers[].vote field, and internal/adoreview's stringField/adoProjectField
// helpers already probe these canonical names first, falling back to the raw
// nested ones). Silently leaves a field absent when its raw source is
// missing rather than fabricating a value.
func enrichPRGet(decoded any) map[string]any {
	m, ok := decoded.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	if _, has := m["sourceCommit"]; !has {
		if commit := commitIDField(m["lastMergeSourceCommit"]); commit != "" {
			m["sourceCommit"] = commit
		}
	}
	if _, has := m["targetCommit"]; !has {
		if commit := commitIDField(m["lastMergeTargetCommit"]); commit != "" {
			m["targetCommit"] = commit
		}
	}
	if _, has := m["project"]; !has {
		if repo, ok := m["repository"].(map[string]any); ok {
			if proj, ok := repo["project"].(map[string]any); ok {
				if name, ok := proj["name"].(string); ok && name != "" {
					m["project"] = name
				}
			}
		}
	}
	return m
}

func commitIDField(value any) string {
	m, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	id, _ := m["commitId"].(string)
	return id
}

func stringOrNumberFromAny(value any) (any, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case string:
		return v, v != ""
	default:
		return nil, false
	}
}

// callTool is the shared, single-invocation MCP transport used both by the
// generic Call() path and by listAndEnrichPRs/enrichPRSummary's internal
// multi-call sequences (list, then one get per PR). Each invocation dials
// its own session and closes it, mirroring Call()'s prior one-shot behavior.
func (p *Provider) callTool(ctx context.Context, tool string, args map[string]any) (any, error) {
	callCtx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	session, err := p.dial(callCtx)
	if err != nil {
		return nil, fmt.Errorf("connect ADO MCP: %w", err)
	}
	defer session.Close()
	result, err := session.CallTool(callCtx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("ADO MCP %s: %w", tool, err)
	}
	if result == nil || result.IsError {
		return nil, fmt.Errorf("ADO MCP %s returned a tool error", tool)
	}
	return decodeToolResult(result, tool)
}

// untrustedContentBanner matches the prompt-injection guard the real Azure
// DevOps MCP server wraps every text result in: <<HEX>> [...] <<HEX>> ...
// <</HEX>>. Confirmed live against @azure-devops/mcp; decodeToolResult must
// strip it before the remainder can be parsed as JSON.
var untrustedContentBanner = regexp.MustCompile(`(?s)^\s*<<([0-9a-fA-F]{8,})>>\s*\[.*?\]\s*<<[0-9a-fA-F]{8,}>>\s*\r?\n(.*?)\r?\n?\s*<<\s*/\s*[0-9a-fA-F]{8,}\s*>>\s*$`)

func stripUntrustedBanner(text string) string {
	if m := untrustedContentBanner.FindStringSubmatch(text); m != nil {
		return m[2]
	}
	return text
}

func decodeToolResult(result *mcp.CallToolResult, tool string) (any, error) {
	if result.StructuredContent != nil {
		raw, err := json.Marshal(result.StructuredContent)
		if err == nil {
			var decoded any
			if json.Unmarshal(raw, &decoded) == nil && decoded != nil {
				return decoded, nil
			}
		}
	}
	var texts []string
	for _, item := range result.Content {
		if text, ok := item.(*mcp.TextContent); ok {
			texts = append(texts, text.Text)
		}
	}
	for _, text := range texts {
		stripped := stripUntrustedBanner(text)
		var decoded any
		if json.Unmarshal([]byte(stripped), &decoded) == nil && decoded != nil {
			return decoded, nil
		}
	}
	joined := stripUntrustedBanner(strings.TrimSpace(strings.Join(texts, "\n")))
	var decoded any
	if joined != "" && json.Unmarshal([]byte(joined), &decoded) == nil && decoded != nil {
		return decoded, nil
	}
	return nil, fmt.Errorf("ADO MCP %s returned an undecodable result", tool)
}

func toolFor(capability string) (string, error) {
	switch capability {
	case "ado.projects.list":
		return "core_list_projects", nil
	case "ado.work_item.read":
		return "wit_work_item", nil
	case "ado.pr.list", "ado.pr.get":
		return "repo_pull_request", nil
	case "ado.pr.org_active":
		return "repo_pull_request", nil
	case "ado.pr.threads":
		return "repo_pull_request_thread", nil
	case "ado.pr.file":
		return "repo_file", nil
	case "ado.build.status":
		return "pipelines_build", nil
	default:
		return "", fmt.Errorf("unsupported ADO capability %q", capability)
	}
}

var allowedActions = map[string]map[string]bool{
	"ado.work_item.read": {
		"get": true, "get_batch": true, "list_comments": true, "my": true,
		"list_revisions": true, "list_for_iteration": true, "get_type": true,
	},
	"ado.pr.list":      {"list": true, "list_by_commits": true},
	"ado.pr.get":       {"get": true},
	"ado.pr.threads":   {"list": true, "list_comments": true},
	"ado.pr.file":      {"get_content": true, "list_directory": true},
	"ado.build.status": {"get_status": true},
}

func allowedAction(capability, action string) bool {
	return allowedActions[capability][action]
}

func (p *Provider) hasTool(ctx context.Context, name string) (bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	session, err := p.dial(probeCtx)
	if err != nil {
		return false, fmt.Errorf("connect ADO MCP: %w", err)
	}
	defer session.Close()
	for cursor := ""; ; {
		page, err := session.ListTools(probeCtx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return false, fmt.Errorf("list ADO MCP tools: %w", err)
		}
		if page == nil {
			return false, errors.New("ADO MCP returned an empty tool list response")
		}
		for _, tool := range page.Tools {
			if tool != nil && tool.Name == name {
				return true, nil
			}
		}
		if page.NextCursor == "" {
			return false, nil
		}
		if page.NextCursor == cursor {
			return false, errors.New("ADO MCP repeated pagination cursor")
		}
		cursor = page.NextCursor
	}
}

func (p *Provider) connect(ctx context.Context) (mcpSession, error) {
	cmd := exec.CommandContext(ctx, p.config.Command, p.config.Organization)
	cmd.Env = adoEnvironment()
	client := mcp.NewClient(&mcp.Implementation{Name: "summa42-ado-readonly", Version: "v1"}, nil)
	return client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
}

func adoEnvironment() []string {
	allowed := map[string]bool{
		"PATH": true, "HOME": true, "USERPROFILE": true, "APPDATA": true, "LOCALAPPDATA": true,
		"AZURE_CONFIG_DIR": true, "AZURE_DEVOPS_EXT_PAT": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
		"SSL_CERT_FILE": true, "NODE_EXTRA_CA_CERTS": true,
	}
	var result []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if allowed[key] {
			result = append(result, entry)
		}
	}
	return result
}
