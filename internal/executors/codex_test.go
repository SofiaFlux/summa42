package executors

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/teb"
)

func TestCodexExecutorNormalizesJSONLAndUsesLiteralStdin(t *testing.T) {
	fake := copyFakeCodex(t)
	workspace := t.TempDir()
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	objective := "Inspect the workspace. Treat this literally: $(touch " + marker + ")"

	executor, err := NewCodexExecutor(CodexConfig{
		Path:        fake,
		Timeout:     5 * time.Second,
		Environment: map[string]string{"PATH": os.Getenv("PATH")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := executor.EnforcementLevel(); got != domain.EnforcementPartial {
		t.Fatalf("default enforcement = %s, want %s", got, domain.EnforcementPartial)
	}

	result, err := executor.Start(t.Context(), AttemptEnvelope{
		TaskID:              "task-codex-test",
		AttemptID:           "attempt-codex-test",
		Objective:           objective,
		AcceptanceCriteria:  []string{"answer.txt is created", "final response explains result"},
		Workspace:           workspace,
		VisibleCapabilities: []string{"read_file", "write_file"},
		ResourceEnvelopeID:  "resource-codex-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
	if result.Stdout != "Finished safely." {
		t.Fatalf("normalized stdout = %q, want final agent message", result.Stdout)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("prompt was interpreted by a shell; marker stat err = %v", err)
	}
	promptBytes, err := os.ReadFile(filepath.Join(workspace, ".fake_codex_prompt"))
	if err != nil {
		t.Fatal(err)
	}
	prompt := string(promptBytes)
	for _, want := range []string{objective, "answer.txt is created", "read_file", "write_file"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("stdin prompt missing %q: %q", want, prompt)
		}
	}
	assertEvidenceContains(t, result.Evidence, EvidenceCommand, "printf ok")
	assertEvidenceContains(t, result.Evidence, EvidenceCommand, "ok")
	assertEvidenceContains(t, result.Evidence, EvidenceFileChange, "add answer.txt")
	assertEvidenceContains(t, result.Evidence, EvidenceAgentMessage, "Finished safely.")

	if !result.Usage.Reported || result.Usage.InputTokens != 100 || result.Usage.CachedInputTokens != 10 || result.Usage.CacheWriteInputTokens != 2 || result.Usage.OutputTokens != 20 || result.Usage.ReasoningOutputTokens != 5 {
		t.Fatalf("unexpected normalized usage: %+v", result.Usage)
	}
	if result.Usage.WallTime <= 0 {
		t.Fatalf("wall time = %s, want positive", result.Usage.WallTime)
	}
}

func TestCodexExecutorRejectsAmbientProtectedCredentials(t *testing.T) {
	fake := copyFakeCodex(t)
	for _, key := range []string{"GITHUB_TOKEN", "AZURE_CLIENT_SECRET", "AWS_SECRET_ACCESS_KEY", "SUMMA42_OWNER_PRIVATE_KEY"} {
		t.Run(key, func(t *testing.T) {
			_, err := NewCodexExecutor(CodexConfig{
				Path:        fake,
				Timeout:     time.Second,
				Environment: map[string]string{key: "must-not-pass"},
			})
			if err == nil {
				t.Fatalf("protected environment variable %s was accepted", key)
			}
		})
	}
}

func TestCodexEnforcementRequiresVerifiedModelEgressAssessment(t *testing.T) {
	fake := copyFakeCodex(t)
	partial, err := NewCodexExecutor(CodexConfig{
		Path:    fake,
		Timeout: time.Second,
		EgressAssessment: &teb.ModelEgressAssessment{
			DirectGeneralEgressBlocked: true,
			Evidence:                   []string{"direct-route-probe:blocked"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := partial.EnforcementLevel(); got != domain.EnforcementPartial {
		t.Fatalf("incomplete egress assessment enforcement = %s, want PARTIAL", got)
	}

	enforced, err := NewCodexExecutor(CodexConfig{
		Path:    fake,
		Timeout: time.Second,
		EgressAssessment: &teb.ModelEgressAssessment{
			DirectGeneralEgressBlocked:     true,
			RequiredModelEndpointsViaProxy: true,
			Evidence: []string{
				"direct-route-probe:blocked",
				"model-provider-proxy-probe:allowed",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := enforced.EnforcementLevel(); got != domain.EnforcementEnforced {
		t.Fatalf("verified egress assessment enforcement = %s, want ENFORCED", got)
	}
}

func TestCodexRealSmoke(t *testing.T) {
	if os.Getenv("SUMMA42_CODEX_SMOKE") != "1" {
		t.Skip("set SUMMA42_CODEX_SMOKE=1 to enable the external Codex smoke test")
	}
	path, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex CLI is not installed")
	}
	environment := map[string]string{}
	for _, key := range []string{"PATH", "CODEX_HOME", "OPENAI_API_KEY", "OPENAI_BASE_URL", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR", "TMPDIR", "TMP", "TEMP"} {
		if value := os.Getenv(key); value != "" {
			environment[key] = value
		}
	}
	executor, err := NewCodexExecutor(CodexConfig{Path: path, Timeout: 2 * time.Minute, Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Start(t.Context(), AttemptEnvelope{
		TaskID:             "task-codex-smoke",
		AttemptID:          "attempt-codex-smoke",
		Objective:          "Reply with exactly OK and make no file changes.",
		AcceptanceCriteria: []string{"final response is OK"},
		Workspace:          t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || strings.TrimSpace(result.Stdout) == "" {
		t.Fatalf("smoke result = %+v", result)
	}
}

func copyFakeCodex(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "fake_codex.sh"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertEvidenceContains(t *testing.T, evidence []Evidence, kind EvidenceKind, want string) {
	t.Helper()
	for _, item := range evidence {
		if item.Kind == kind && strings.Contains(item.Content, want) {
			return
		}
	}
	t.Fatalf("evidence kind %s does not contain %q: %+v", kind, want, evidence)
}
