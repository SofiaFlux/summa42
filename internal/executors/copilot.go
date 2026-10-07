package executors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SofiaFlux/summa42/internal/domain"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

type CopilotConfig struct {
	Path         string
	Model        string
	MCPServer    string
	AllowedTools []string
	Timeout      time.Duration
	Environment  map[string]string
}

type CopilotExecutor struct {
	path        string
	model       string
	server      string
	tools       []string
	timeout     time.Duration
	environment map[string]string
}

func (e *CopilotExecutor) ExecutionContract() Contract {
	return Contract{TaskClasses: []string{"ado.pr.review"}, Capabilities: []string{"copilot", "ado.pr.read"}, Enforcement: domain.EnforcementPartial}
}

type ReviewVerdict string

const (
	ReviewClean     ReviewVerdict = "CLEAN"
	ReviewFindings  ReviewVerdict = "FINDINGS"
	ReviewUncertain ReviewVerdict = "UNCERTAIN"
)

type ReviewFinding struct {
	Path        string `json:"path"`
	Line        int64  `json:"line"`
	Explanation string `json:"explanation"`
	Evidence    string `json:"evidence"`
}

type ReviewResult struct {
	Verdict         ReviewVerdict   `json:"verdict"`
	ReviewedCommits []string        `json:"reviewedCommits"`
	ReviewedFiles   []string        `json:"reviewedFiles"`
	Findings        []ReviewFinding `json:"findings"`
	Reason          string          `json:"reason"`
}

type CopilotReviewPayload struct {
	Repo         string `json:"repo"`
	PR           int64  `json:"pr"`
	SourceCommit string `json:"sourceCommit"`
	TargetCommit string `json:"targetCommit"`
}

var copilotReadTools = map[string]struct{}{
	"repo_pull_request": {}, "repo_pull_request_org": {}, "repo_pull_request_thread": {},
	"repo_file": {}, "pipelines_build": {}, "core_list_projects": {}, "wit_work_item": {},
}

// Note: "read" is deliberately absent here. Exact allowlist membership above
// already rejects bare file-read tools ("read", "glob"); a "read" substring
// check would false-positive on the allowlisted "repo_pull_request_thread"
// ("thread" contains "read").
var copilotForbiddenToolFragments = []string{"shell", "write", "url", "memory", "allow-all"}

var copilotEnvironmentAllowlist = map[string]struct{}{
	"PATH": {}, "COPILOT_MODEL": {}, "COPILOT_GITHUB_TOKEN": {}, "GH_TOKEN": {}, "GITHUB_TOKEN": {},
	"HTTP_PROXY": {}, "HTTPS_PROXY": {}, "NO_PROXY": {},
	"SSL_CERT_FILE": {}, "SSL_CERT_DIR": {}, "TMPDIR": {}, "TMP": {}, "TEMP": {},
}

var copilotProtectedEnvironment = map[string]struct{}{
	"SUMMA42_OWNER_PRIVATE_KEY": {}, "AZURE_CLIENT_SECRET": {}, "AWS_SECRET_ACCESS_KEY": {},
}

var copilotServerPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func NewCopilotExecutor(config CopilotConfig) (*CopilotExecutor, error) {
	config.Path = strings.TrimSpace(config.Path)
	if config.Path == "" {
		return nil, errors.New("copilot path is required")
	}
	if config.Timeout <= 0 {
		return nil, errors.New("copilot timeout must be positive")
	}
	server := strings.TrimSpace(config.MCPServer)
	if !copilotServerPattern.MatchString(server) {
		return nil, fmt.Errorf("copilot MCP server %q is invalid", config.MCPServer)
	}
	if len(config.AllowedTools) == 0 {
		return nil, errors.New("copilot allowed tools must not be empty")
	}
	qualified := make([]string, 0, len(config.AllowedTools))
	for _, tool := range config.AllowedTools {
		trimmed := strings.TrimSpace(tool)
		if trimmed == "" {
			return nil, errors.New("copilot allowed tool must not be blank")
		}
		if strings.Contains(trimmed, "(") {
			return nil, fmt.Errorf("copilot allowed tool %q must be an unqualified tool name", tool)
		}
		if _, ok := copilotReadTools[trimmed]; !ok {
			return nil, fmt.Errorf("copilot allowed tool %q is not a permitted read-only tool", tool)
		}
		lowered := strings.ToLower(trimmed)
		// Exact allowlist membership is checked first and subsumes the fragment
		// scan (fragments are future-proofing for qualified Server(tool) forms).
		for _, fragment := range copilotForbiddenToolFragments {
			if strings.Contains(lowered, fragment) {
				return nil, fmt.Errorf("copilot allowed tool %q contains forbidden fragment %q", tool, fragment)
			}
		}
		qualified = append(qualified, server+"("+trimmed+")")
	}
	for key := range config.Environment {
		if _, protected := copilotProtectedEnvironment[key]; protected {
			return nil, fmt.Errorf("protected environment variable %q cannot be passed to Copilot", key)
		}
		if _, allowed := copilotEnvironmentAllowlist[key]; !allowed {
			return nil, fmt.Errorf("environment variable %q is not in the Copilot allowlist", key)
		}
	}
	return &CopilotExecutor{
		path:        config.Path,
		model:       strings.TrimSpace(config.Model),
		server:      server,
		tools:       qualified,
		timeout:     config.Timeout,
		environment: cloneEnvironment(config.Environment),
	}, nil
}

func (e *CopilotExecutor) Start(ctx context.Context, envelope AttemptEnvelope) (ExecutionResult, error) {
	if e == nil {
		return ExecutionResult{}, errors.New("copilot executor is not configured")
	}
	workspace := strings.TrimSpace(envelope.Workspace)
	if workspace == "" {
		return ExecutionResult{}, errors.New("attempt workspace is required")
	}
	if strings.TrimSpace(envelope.Objective) == "" {
		return ExecutionResult{}, errors.New("attempt objective is required")
	}
	var payload CopilotReviewPayload
	if err := json.Unmarshal(envelope.PayloadJSON, &payload); err != nil {
		return ExecutionResult{}, fmt.Errorf("decode Copilot payload: %w", err)
	}
	if strings.TrimSpace(payload.Repo) == "" {
		return ExecutionResult{}, errors.New("copilot payload repo is required")
	}
	if payload.PR <= 0 {
		return ExecutionResult{}, errors.New("copilot payload pr must be positive")
	}
	if strings.TrimSpace(payload.SourceCommit) == "" {
		return ExecutionResult{}, errors.New("copilot payload sourceCommit is required")
	}
	if strings.TrimSpace(payload.TargetCommit) == "" {
		return ExecutionResult{}, errors.New("copilot payload targetCommit is required")
	}

	prompt := copilotPrompt(envelope, payload)
	args := []string{"-p", prompt, "-s", "--no-ask-user",
		"--available-tools=" + strings.Join(e.tools, ","),
		"--allow-tool=" + strings.Join(e.tools, ","),
		"--deny-tool=shell,write,url,memory"}
	if e.model != "" {
		args = append(args, "--model="+e.model)
	}

	runCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, e.path, args...)
	cmd.Dir = workspace
	cmd.Env = environmentList(e.environment)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	started := time.Now()
	runErr := cmd.Run()
	wall := time.Since(started)

	canonical, validateErr := validateCopilotResult(stdout.Bytes())
	result := ExecutionResult{
		ExitCode: exitCode(runErr),
		Stdout:   canonical,
		Stderr:   stderr.String(),
		Evidence: []Evidence{
			{Kind: EvidenceAgentMessage, Content: canonical},
			{Kind: EvidenceStderr, Content: stderr.String()},
		},
		Usage: Usage{WallTime: wall},
	}
	if canonical != "" {
		result.Evidence = append(result.Evidence, Evidence{Kind: EvidenceStdout, Content: canonical})
	}
	if runCtx.Err() != nil {
		return result, runCtx.Err()
	}
	if runErr != nil {
		return result, fmt.Errorf("copilot execution failed: %w", runErr)
	}
	if validateErr != nil {
		return result, validateErr
	}
	return result, nil
}

func validateCopilotResult(data []byte) (string, error) {
	var result ReviewResult
	if err := json.Unmarshal(bytes.TrimSpace(data), &result); err != nil {
		return "", fmt.Errorf("decode Copilot result: %w", err)
	}
	switch result.Verdict {
	case ReviewClean, ReviewFindings, ReviewUncertain:
	default:
		return "", fmt.Errorf("copilot result has invalid verdict %q", result.Verdict)
	}
	if len(result.ReviewedCommits) == 0 {
		return "", errors.New("copilot result reviewedCommits is required")
	}
	for i, commit := range result.ReviewedCommits {
		if strings.TrimSpace(commit) == "" {
			return "", fmt.Errorf("copilot result reviewedCommits[%d] must not be blank", i)
		}
	}
	if len(result.ReviewedFiles) == 0 {
		return "", errors.New("copilot result reviewedFiles is required")
	}
	for i, file := range result.ReviewedFiles {
		if strings.TrimSpace(file) == "" {
			return "", fmt.Errorf("copilot result reviewedFiles[%d] must not be blank", i)
		}
	}
	for i, finding := range result.Findings {
		if strings.TrimSpace(finding.Path) == "" {
			return "", fmt.Errorf("copilot result findings[%d] is missing path", i)
		}
		if strings.TrimSpace(finding.Explanation) == "" {
			return "", fmt.Errorf("copilot result findings[%d] is missing explanation", i)
		}
	}
	canonical, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("encode Copilot result: %w", err)
	}
	return string(canonical), nil
}

func copilotPrompt(envelope AttemptEnvelope, payload CopilotReviewPayload) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Task ID: %s\nAttempt ID: %s\nObjective:\n%s\n", envelope.TaskID, envelope.AttemptID, envelope.Objective)
	fmt.Fprintf(&b, "Repository: %s\nPull request: %d\nSource commit: %s\nTarget commit: %s\n",
		payload.Repo, payload.PR, payload.SourceCommit, payload.TargetCommit)
	if len(envelope.AcceptanceCriteria) > 0 {
		b.WriteString("Acceptance criteria:\n")
		for _, criterion := range envelope.AcceptanceCriteria {
			fmt.Fprintf(&b, "- %s\n", criterion)
		}
	}
	if len(envelope.VisibleCapabilities) > 0 {
		b.WriteString("Visible capabilities:\n")
		for _, capability := range envelope.VisibleCapabilities {
			fmt.Fprintf(&b, "- %s\n", capability)
		}
	}
	if envelope.ResourceEnvelopeID != "" {
		fmt.Fprintf(&b, "Resource envelope: %s\n", envelope.ResourceEnvelopeID)
	}
	b.WriteString("Respond with ONLY the review JSON document, no prose.\n")
	b.WriteString(`Schema: {"verdict":"CLEAN|FINDINGS|UNCERTAIN","reviewedCommits":["<sha>",...],"reviewedFiles":["<path>",...],"findings":[{"path":"<path>","line":<n>,"explanation":"<why>","evidence":"<quote>"}],"reason":"<summary>"}` + "\n")
	return b.String()
}
