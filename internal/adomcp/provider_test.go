package adomcp

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type fakeSession struct {
	tools  []string
	called []string
	args   []any
	result *mcp.CallToolResult
}

func (s *fakeSession) ListTools(_ context.Context, _ *mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	tools := make([]*mcp.Tool, 0, len(s.tools))
	for _, name := range s.tools {
		tools = append(tools, &mcp.Tool{Name: name})
	}
	return &mcp.ListToolsResult{Tools: tools}, nil
}
func (s *fakeSession) CallTool(_ context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	s.called = append(s.called, params.Name)
	s.args = append(s.args, params.Arguments)
	if s.result != nil {
		return s.result, nil
	}
	return &mcp.CallToolResult{StructuredContent: map[string]any{}}, nil
}
func (s *fakeSession) Close() error { return nil }

func structuredCallResult(content map[string]any) *mcp.CallToolResult {
	return &mcp.CallToolResult{StructuredContent: content}
}

func textCallResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func garbageCallResult() *mcp.CallToolResult {
	return textCallResult("not JSON")
}

func TestCallDecodesStructuredAndTextResults(t *testing.T) {
	p, _ := New(Config{Command: "/bin/true", Organization: "Contoso"})
	session := &fakeSession{tools: []string{"repo_pull_request"}}
	p.dial = func(context.Context) (mcpSession, error) { return session, nil }
	session.result = structuredCallResult(map[string]any{"status": "active"})
	got, err := p.Call(t.Context(), "ado.pr.get", map[string]any{"action": "get"})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := got.(map[string]any)
	if !ok || m["status"] != "active" {
		t.Fatalf("got %#v", got)
	}
	session.result = textCallResult(`{"status":"text"}`)
	got, err = p.Call(t.Context(), "ado.pr.get", map[string]any{"action": "get"})
	if err != nil {
		t.Fatal(err)
	}
	m, _ = got.(map[string]any)
	if m == nil || m["status"] != "text" {
		t.Fatalf("got %#v", got)
	}
	session.result = garbageCallResult()
	if _, err := p.Call(t.Context(), "ado.pr.get", map[string]any{"action": "get"}); err == nil {
		t.Fatal("accepted undecodable result")
	}
}

func TestProviderAdvertisesOnlyExplicitReadCapabilities(t *testing.T) {
	p, err := New(Config{Command: "/bin/true", Organization: "Contoso"})
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := p.Advertise(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 8 {
		t.Fatalf("got %d definitions, want 8", len(definitions))
	}
	want := []string{"ado.projects.list", "ado.work_item.read", "ado.pr.list", "ado.pr.get", "ado.pr.org_active", "ado.pr.threads", "ado.pr.file", "ado.build.status"}
	for i, name := range want {
		if definitions[i].Skill.Name != name {
			t.Fatalf("definition[%d] = %q, want %q", i, definitions[i].Skill.Name, name)
		}
	}
	for _, definition := range definitions {
		if definition.Access.Provider != p.Name() || len(definition.Authority.Capabilities) != 1 || definition.Authority.Capabilities[0] != definition.Skill.Name {
			t.Fatalf("unscoped definition: %+v", definition)
		}
	}
}

func TestProviderRejectsUnknownAndMutatingActionsBeforeDial(t *testing.T) {
	p, _ := New(Config{Command: "/bin/true", Organization: "Contoso"})
	dials := 0
	p.dial = func(context.Context) (mcpSession, error) { dials++; return &fakeSession{}, nil }
	for _, tc := range []struct {
		name    string
		request any
	}{
		{"ado.unknown", nil},
		{"ado.work_item.read", map[string]any{"action": "update", "id": 42}},
		{"ado.work_item.read", map[string]any{"action": "get", "tool": "wit_work_item_write"}},
		{"ado.projects.list", map[string]any{"action": "delete"}},
	} {
		if _, err := p.Call(t.Context(), tc.name, tc.request); err == nil {
			t.Fatalf("accepted %s: %#v", tc.name, tc.request)
		}
	}
	if dials != 0 {
		t.Fatalf("dialed %d times for rejected calls", dials)
	}
}

func TestProviderCallsReadToolAndPropagatesToolError(t *testing.T) {
	p, _ := New(Config{Command: "/bin/true", Organization: "Contoso"})
	session := &fakeSession{tools: []string{"core_list_projects", "wit_work_item"}}
	p.dial = func(context.Context) (mcpSession, error) { return session, nil }
	if _, err := p.Call(t.Context(), "ado.work_item.read", map[string]any{"action": "get", "id": 42}); err != nil {
		t.Fatal(err)
	}
	if len(session.called) != 1 || session.called[0] != "wit_work_item" {
		t.Fatalf("called tools: %v", session.called)
	}
	forwarded, ok := session.args[0].(map[string]any)
	if !ok || forwarded["action"] != "get" || forwarded["id"] != 42 {
		t.Fatalf("work item arguments: %#v", session.args[0])
	}
	session.result = &mcp.CallToolResult{IsError: true}
	if _, err := p.Call(t.Context(), "ado.projects.list", nil); err == nil {
		t.Fatal("MCP tool error was ignored")
	}
}

func TestProviderProbeMarksMissingToolUnavailable(t *testing.T) {
	p, _ := New(Config{Command: "/bin/true", Organization: "Contoso"})
	p.dial = func(context.Context) (mcpSession, error) {
		return &fakeSession{tools: []string{"core_list_projects"}}, nil
	}
	definitions, _ := p.Advertise(t.Context())
	projects, err := p.Probe(t.Context(), definitions[0])
	if err != nil || !projects.Available {
		t.Fatalf("projects probe: %+v, %v", projects, err)
	}
	items, err := p.Probe(t.Context(), definitions[1])
	if err != nil || items.Available {
		t.Fatalf("work item probe: %+v, %v", items, err)
	}
	p.dial = func(context.Context) (mcpSession, error) { return nil, errors.New("offline") }
	if _, err := p.Probe(t.Context(), definitions[0]); err == nil {
		t.Fatal("transport failure ignored")
	}
}

func TestProviderAdvertisesPRListAndGet(t *testing.T) {
	p, err := New(Config{Command: "/bin/true", Organization: "Contoso"})
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := p.Advertise(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 8 {
		t.Fatalf("got %d definitions, want 8", len(definitions))
	}
	want := []string{"ado.projects.list", "ado.work_item.read", "ado.pr.list", "ado.pr.get", "ado.pr.org_active", "ado.pr.threads", "ado.pr.file", "ado.build.status"}
	for i, name := range want {
		if definitions[i].Skill.Name != name {
			t.Fatalf("definition[%d] = %q, want %q", i, definitions[i].Skill.Name, name)
		}
	}
}

func TestProviderEnforcesPerCapabilityActions(t *testing.T) {
	p, _ := New(Config{Command: "/bin/true", Organization: "Contoso"})
	dials := 0
	session := &fakeSession{tools: []string{"repo_pull_request"}}
	p.dial = func(context.Context) (mcpSession, error) { dials++; return session, nil }
	for _, tc := range []struct {
		name    string
		request any
	}{
		{"ado.pr.get", map[string]any{"action": "list"}},
		{"ado.pr.list", map[string]any{"action": "get"}},
		{"ado.pr.list", map[string]any{"action": "update"}},
		{"ado.pr.get", map[string]any{"action": "get", "tool": "repo_pull_request_write"}},
	} {
		if _, err := p.Call(t.Context(), tc.name, tc.request); err == nil {
			t.Fatalf("accepted %s: %#v", tc.name, tc.request)
		}
	}
	if dials != 0 {
		t.Fatalf("dialed %d times for rejected calls", dials)
	}
	if _, err := p.Call(t.Context(), "ado.pr.get", map[string]any{"action": "get", "pullRequestId": 42}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Call(t.Context(), "ado.pr.list", map[string]any{"action": "list_by_commits"}); err != nil {
		t.Fatal(err)
	}
	if len(session.called) != 2 || session.called[0] != "repo_pull_request" || session.called[1] != "repo_pull_request" {
		t.Fatalf("called tools: %v", session.called)
	}
}

func TestProviderAdvertisesAllEightCapabilities(t *testing.T) {
	p, err := New(Config{Command: "/bin/true", Organization: "Contoso"})
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := p.Advertise(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ado.projects.list", "ado.work_item.read", "ado.pr.list", "ado.pr.get", "ado.pr.org_active", "ado.pr.threads", "ado.pr.file", "ado.build.status"}
	if len(definitions) != len(want) {
		t.Fatalf("got %d definitions, want %d", len(definitions), len(want))
	}
	for i, name := range want {
		if definitions[i].Skill.Name != name {
			t.Fatalf("definition[%d] = %q, want %q", i, definitions[i].Skill.Name, name)
		}
		if definitions[i].Access.Provider != p.Name() || len(definitions[i].Authority.Capabilities) != 1 || definitions[i].Authority.Capabilities[0] != name {
			t.Fatalf("unscoped definition: %+v", definitions[i])
		}
	}
}

func TestProviderOrgActiveRequiresDefaultProjectFailClosed(t *testing.T) {
	// Azure DevOps has no organization-wide active-PR listing tool (confirmed
	// live: repo_pull_request_org does not exist, and repo_pull_request's
	// list action rejects a scopeless call). Without a configured
	// DefaultProject, org_active must fail closed before ever dialing.
	p, _ := New(Config{Command: "/bin/true", Organization: "Contoso"})
	dials := 0
	p.dial = func(context.Context) (mcpSession, error) { dials++; return &fakeSession{}, nil }
	if _, err := p.Call(t.Context(), "ado.pr.org_active", nil); err == nil {
		t.Fatal("accepted org_active without a configured default project")
	}
	if dials != 0 {
		t.Fatalf("dialed %d times for a fail-closed call", dials)
	}
}

func TestProviderOrgActiveListsAndEnrichesEachPR(t *testing.T) {
	p, _ := New(Config{Command: "/bin/true", Organization: "Contoso", DefaultProject: "Contoso-Proj"})
	session := &fakeResponder{
		byTool: map[string][]*mcp.CallToolResult{
			"repo_pull_request": {
				textCallResult(`[{"pullRequestId":1,"repository":"shop","project":"Contoso-Proj","title":"Fix","isDraft":false}]`),
				textCallResult(`{"pullRequestId":1,"createdBy":{"id":"author-1"},"lastMergeSourceCommit":{"commitId":"abc"},"lastMergeTargetCommit":{"commitId":"def"},"reviewers":[{"id":"rev-1","uniqueName":"a@example.com","vote":0}]}`),
			},
		},
	}
	p.dial = func(context.Context) (mcpSession, error) { return session, nil }
	got, err := p.Call(t.Context(), "ado.pr.org_active", nil)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("got %#v, want a map", got)
	}
	prs, ok := m["prs"].([]any)
	if !ok || len(prs) != 1 {
		t.Fatalf("prs = %#v, want one enriched PR", m["prs"])
	}
	pr, ok := prs[0].(map[string]any)
	if !ok {
		t.Fatalf("pr[0] = %#v, want an object", prs[0])
	}
	if pr["repository"] != "shop" || pr["sourceCommit"] != "abc" || pr["targetCommit"] != "def" {
		t.Fatalf("pr = %#v, want repository=shop sourceCommit=abc targetCommit=def", pr)
	}
	reviewers, ok := pr["reviewers"].([]any)
	if !ok || len(reviewers) != 1 {
		t.Fatalf("reviewers = %#v, want one entry", pr["reviewers"])
	}
	if len(session.called) != 2 || session.called[0] != "repo_pull_request" || session.called[1] != "repo_pull_request" {
		t.Fatalf("called tools: %v, want [repo_pull_request repo_pull_request] (one list, one get)", session.called)
	}
	if listArgs, ok := session.args[0].(map[string]any); !ok || listArgs["project"] != "Contoso-Proj" {
		t.Fatalf("list args: %#v, want project=Contoso-Proj (the configured default)", session.args[0])
	}
}

// fakeResponder returns queued CallTool results per tool name, so a test can
// script a list call followed by one or more per-item get calls.
type fakeResponder struct {
	byTool map[string][]*mcp.CallToolResult
	called []string
	args   []any
}

func (f *fakeResponder) ListTools(_ context.Context, _ *mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	return &mcp.ListToolsResult{}, nil
}

func (f *fakeResponder) CallTool(_ context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	f.called = append(f.called, params.Name)
	f.args = append(f.args, params.Arguments)
	queue := f.byTool[params.Name]
	if len(queue) == 0 {
		return &mcp.CallToolResult{IsError: true}, nil
	}
	f.byTool[params.Name] = queue[1:]
	return queue[0], nil
}

func (f *fakeResponder) Close() error { return nil }

func TestProviderOrgActiveRejectsActionAndToolOverride(t *testing.T) {
	p, _ := New(Config{Command: "/bin/true", Organization: "Contoso", DefaultProject: "Contoso-Proj"})
	dials := 0
	p.dial = func(context.Context) (mcpSession, error) { dials++; return &fakeSession{}, nil }
	for _, bad := range []any{
		map[string]any{"action": "list"},
		map[string]any{"tool": "repo_pull_request"},
	} {
		if _, err := p.Call(t.Context(), "ado.pr.org_active", bad); err == nil {
			t.Fatalf("accepted org_active: %#v", bad)
		}
	}
	if dials != 0 {
		t.Fatalf("dialed %d times for rejected org_active requests", dials)
	}
}

func TestProviderOrgActiveDegradesOnPerPRDetailFailure(t *testing.T) {
	// One PR's enrichment "get" can fail transiently without the whole
	// project/org being unhealthy (confirmed live: Azure DevOps intermittently
	// rejects a single otherwise-healthy PR's "get" call). That single PR
	// must not abort discovery of the rest of the page.
	p, _ := New(Config{Command: "/bin/true", Organization: "Contoso", DefaultProject: "Contoso-Proj"})
	session := &fakeResponder{
		byTool: map[string][]*mcp.CallToolResult{
			"repo_pull_request": {
				textCallResult(`[{"pullRequestId":1,"repository":"shop","project":"Contoso-Proj","title":"Fix","isDraft":false},{"pullRequestId":2,"repository":"shop","project":"Contoso-Proj","title":"Flaky","isDraft":false}]`),
				textCallResult(`{"pullRequestId":1,"createdBy":{"id":"author-1"},"lastMergeSourceCommit":{"commitId":"abc"},"lastMergeTargetCommit":{"commitId":"def"},"reviewers":[]}`),
				{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "Error with pull request operation: "}}},
			},
		},
	}
	p.dial = func(context.Context) (mcpSession, error) { return session, nil }
	got, err := p.Call(t.Context(), "ado.pr.org_active", nil)
	if err != nil {
		t.Fatal(err)
	}
	prs := got.(map[string]any)["prs"].([]any)
	if len(prs) != 2 {
		t.Fatalf("prs = %#v, want 2 (one enriched, one degraded)", prs)
	}
	healthy := prs[0].(map[string]any)
	if healthy["sourceCommit"] != "abc" {
		t.Fatalf("healthy PR = %#v, want sourceCommit=abc", healthy)
	}
	degraded := prs[1].(map[string]any)
	if _, has := degraded["sourceCommit"]; has {
		t.Fatalf("degraded PR = %#v, want no sourceCommit (so ParsePR buckets it Unparseable)", degraded)
	}
	if degraded["repository"] != "shop" || degraded["number"] != float64(2) {
		t.Fatalf("degraded PR = %#v, want repository=shop number=2", degraded)
	}
}

func TestProviderForwardsThreadFileAndBuildCalls(t *testing.T) {
	p, _ := New(Config{Command: "/bin/true", Organization: "Contoso"})
	session := &fakeSession{tools: []string{"repo_pull_request_thread", "repo_file", "pipelines_build"}}
	p.dial = func(context.Context) (mcpSession, error) { return session, nil }
	calls := []struct {
		capability string
		request    map[string]any
		tool       string
	}{
		{"ado.pr.threads", map[string]any{"action": "list_comments", "pullRequestId": 7, "threadId": 3}, "repo_pull_request_thread"},
		{"ado.pr.file", map[string]any{"action": "get_content", "path": "main.go"}, "repo_file"},
		{"ado.build.status", map[string]any{"action": "get_status", "buildId": 9}, "pipelines_build"},
	}
	for _, call := range calls {
		if _, err := p.Call(t.Context(), call.capability, call.request); err != nil {
			t.Fatalf("%s: %v", call.capability, err)
		}
	}
	if len(session.called) != len(calls) {
		t.Fatalf("called tools: %v", session.called)
	}
	for i, call := range calls {
		if session.called[i] != call.tool {
			t.Fatalf("call[%d] tool = %q, want %q", i, session.called[i], call.tool)
		}
	}
	for _, bad := range []struct {
		capability string
		request    map[string]any
	}{
		{"ado.pr.threads", map[string]any{"action": "create"}},
		{"ado.pr.file", map[string]any{"action": "delete"}},
		{"ado.build.status", map[string]any{"action": "list"}},
		{"ado.build.status", map[string]any{"action": "get_status", "tool": "pipelines_write"}},
	} {
		if _, err := p.Call(t.Context(), bad.capability, bad.request); err == nil {
			t.Fatalf("accepted %s: %#v", bad.capability, bad.request)
		}
	}
}
