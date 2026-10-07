package executors

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/teb"
)

type CodexConfig struct {
	Path             string
	Environment      map[string]string
	Timeout          time.Duration
	EgressAssessment *teb.ModelEgressAssessment
}

type CodexExecutor struct {
	path        string
	environment map[string]string
	timeout     time.Duration
	enforcement domain.EnforcementLevel
}

var codexEnvironmentAllowlist = map[string]struct{}{
	"PATH":            {},
	"CODEX_HOME":      {},
	"OPENAI_API_KEY":  {},
	"OPENAI_BASE_URL": {},
	"HTTP_PROXY":      {},
	"HTTPS_PROXY":     {},
	"NO_PROXY":        {},
	"SSL_CERT_FILE":   {},
	"SSL_CERT_DIR":    {},
	"TMPDIR":          {},
	"TMP":             {},
	"TEMP":            {},
}

var codexProtectedEnvironment = map[string]struct{}{
	"GITHUB_TOKEN":              {},
	"AZURE_CLIENT_SECRET":       {},
	"AWS_SECRET_ACCESS_KEY":     {},
	"SUMMA42_OWNER_PRIVATE_KEY": {},
}

func NewCodexExecutor(config CodexConfig) (*CodexExecutor, error) {
	config.Path = strings.TrimSpace(config.Path)
	if config.Path == "" {
		return nil, errors.New("codex path is required")
	}
	if config.Timeout <= 0 {
		return nil, errors.New("codex timeout must be positive")
	}
	for key := range config.Environment {
		if _, protected := codexProtectedEnvironment[key]; protected {
			return nil, fmt.Errorf("protected environment variable %q cannot be passed to Codex", key)
		}
		if _, allowed := codexEnvironmentAllowlist[key]; !allowed {
			return nil, fmt.Errorf("environment variable %q is not in the Codex allowlist", key)
		}
	}

	enforcement := domain.EnforcementPartial
	if config.EgressAssessment.Verified() {
		enforcement = domain.EnforcementEnforced
	}
	return &CodexExecutor{
		path:        config.Path,
		environment: cloneEnvironment(config.Environment),
		timeout:     config.Timeout,
		enforcement: enforcement,
	}, nil
}

func (e *CodexExecutor) EnforcementLevel() domain.EnforcementLevel {
	if e == nil {
		return domain.EnforcementUnenforced
	}
	return e.enforcement
}

func (e *CodexExecutor) Start(ctx context.Context, envelope AttemptEnvelope) (ExecutionResult, error) {
	if e == nil {
		return ExecutionResult{}, errors.New("codex executor is not configured")
	}
	workspace := strings.TrimSpace(envelope.Workspace)
	if workspace == "" {
		return ExecutionResult{}, errors.New("attempt workspace is required")
	}
	if strings.TrimSpace(envelope.Objective) == "" {
		return ExecutionResult{}, errors.New("attempt objective is required")
	}

	runCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, e.path, "exec", "--ephemeral", "--json", "--cd", workspace, "-")
	cmd.Dir = workspace
	cmd.Env = environmentList(e.environment)
	cmd.Stdin = strings.NewReader(codexPrompt(envelope))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	started := time.Now()
	runErr := cmd.Run()
	wall := time.Since(started)

	finalMessage, eventEvidence, tokenUsage, parseErr := parseCodexJSONL(stdout.Bytes())
	result := ExecutionResult{
		ExitCode: exitCode(runErr),
		Stdout:   finalMessage,
		Stderr:   stderr.String(),
		Evidence: append(eventEvidence, Evidence{Kind: EvidenceStderr, Content: stderr.String()}),
		Usage: Usage{
			Reported:              tokenUsage.Reported,
			WallTime:              wall,
			InputTokens:           tokenUsage.InputTokens,
			CachedInputTokens:     tokenUsage.CachedInputTokens,
			CacheWriteInputTokens: tokenUsage.CacheWriteInputTokens,
			OutputTokens:          tokenUsage.OutputTokens,
			ReasoningOutputTokens: tokenUsage.ReasoningOutputTokens,
		},
	}
	if finalMessage != "" {
		result.Evidence = append(result.Evidence, Evidence{Kind: EvidenceStdout, Content: finalMessage})
	}
	if runCtx.Err() != nil {
		return result, runCtx.Err()
	}
	if runErr != nil {
		return result, fmt.Errorf("codex execution failed: %w", runErr)
	}
	if parseErr != nil {
		return result, parseErr
	}
	return result, nil
}

func codexPrompt(envelope AttemptEnvelope) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Task ID: %s\nAttempt ID: %s\nObjective:\n%s\n", envelope.TaskID, envelope.AttemptID, envelope.Objective)
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
	return b.String()
}

type codexTokenUsage struct {
	Reported              bool  `json:"-"`
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
}

type codexEvent struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id"`
	Item     json.RawMessage `json:"item"`
	Usage    codexTokenUsage `json:"usage"`
	Message  string          `json:"message"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type codexItem struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	Command          string `json:"command"`
	AggregatedOutput string `json:"aggregated_output"`
	Status           string `json:"status"`
	Text             string `json:"text"`
	Message          string `json:"message"`
	Changes          []struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
	} `json:"changes"`
}

func parseCodexJSONL(data []byte) (string, []Evidence, codexTokenUsage, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var (
		finalMessage string
		evidence     []Evidence
		usage        codexTokenUsage
		sawThread    bool
		sawCompleted bool
	)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event codexEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return finalMessage, evidence, usage, fmt.Errorf("decode Codex JSONL event: %w", err)
		}
		switch event.Type {
		case "thread.started":
			if strings.TrimSpace(event.ThreadID) == "" {
				return finalMessage, evidence, usage, errors.New("Codex thread.started event is missing thread_id")
			}
			sawThread = true
		case "item.completed":
			var item codexItem
			if err := json.Unmarshal(event.Item, &item); err != nil {
				return finalMessage, evidence, usage, fmt.Errorf("decode Codex completed item: %w", err)
			}
			switch item.Type {
			case "command_execution":
				content := "command: " + item.Command
				if item.AggregatedOutput != "" {
					content += "\noutput:\n" + item.AggregatedOutput
				}
				evidence = append(evidence, Evidence{Kind: EvidenceCommand, Content: content})
			case "file_change":
				for _, change := range item.Changes {
					evidence = append(evidence, Evidence{Kind: EvidenceFileChange, Content: strings.TrimSpace(change.Kind + " " + change.Path)})
				}
			case "agent_message":
				finalMessage = item.Text
				evidence = append(evidence, Evidence{Kind: EvidenceAgentMessage, Content: item.Text})
			case "error":
				return finalMessage, evidence, usage, fmt.Errorf("Codex item error: %s", item.Message)
			}
		case "turn.completed":
			usage = event.Usage
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(line, &fields); err == nil {
				report, present := fields["usage"]
				usage.Reported = present && !bytes.Equal(bytes.TrimSpace(report), []byte("null"))
			}
			sawCompleted = true
		case "turn.failed":
			if event.Error != nil && event.Error.Message != "" {
				return finalMessage, evidence, usage, fmt.Errorf("Codex turn failed: %s", event.Error.Message)
			}
			return finalMessage, evidence, usage, errors.New("Codex turn failed")
		case "error":
			if event.Message != "" {
				return finalMessage, evidence, usage, fmt.Errorf("Codex stream error: %s", event.Message)
			}
			return finalMessage, evidence, usage, errors.New("Codex stream error")
		}
	}
	if err := scanner.Err(); err != nil {
		return finalMessage, evidence, usage, fmt.Errorf("read Codex JSONL: %w", err)
	}
	if !sawThread {
		return finalMessage, evidence, usage, errors.New("Codex JSONL did not contain thread.started")
	}
	if !sawCompleted {
		return finalMessage, evidence, usage, errors.New("Codex JSONL did not contain turn.completed")
	}
	if strings.TrimSpace(finalMessage) == "" {
		return finalMessage, evidence, usage, errors.New("Codex JSONL did not contain a final agent message")
	}
	return finalMessage, evidence, usage, nil
}
