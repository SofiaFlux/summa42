package executors_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/modelrun/modelruntest"
)

var fakeSudoLog string

// TestMain swaps sudo for a script that drops its options and runs the rest, and
// the user database for one fake model user, so the launcher is exercised
// without privileges.
func TestMain(m *testing.M) {
	_, sudoLog, cleanup, err := modelruntest.Install()
	if err != nil {
		panic(err)
	}
	fakeSudoLog = sudoLog
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func writeFakeCopilot(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "copilot")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func copilotConfig(path string) executors.CopilotConfig {
	return executors.CopilotConfig{
		Path: path, RunAsUser: modelruntest.User, MCPServer: "ado",
		AllowedTools: []string{"repo_pull_request"},
		Timeout:      time.Minute,
	}
}

func TestCopilotParsesCleanResult(t *testing.T) {
	path := writeFakeCopilot(t, "#!/bin/sh\necho '{\"verdict\":\"CLEAN\",\"reviewedCommits\":[\"abc\"],\"reviewedFiles\":[\"main.go\"],\"findings\":[]}'\n")
	executor, err := executors.NewCopilotExecutor(copilotConfig(path))
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Start(context.Background(), executors.AttemptEnvelope{
		TaskID: "task-1", AttemptID: "attempt-1", Workspace: t.TempDir(),
		Objective: "review", PayloadJSON: json.RawMessage(`{"repo":"shop","pr":1,"sourceCommit":"a","targetCommit":"b"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0", result.ExitCode)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(result.Stdout), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["verdict"] != "CLEAN" {
		t.Fatalf("verdict = %v", decoded["verdict"])
	}
}

func TestCopilotAcceptsAllAllowlistedReadTools(t *testing.T) {
	base := copilotConfig(writeFakeCopilot(t, "#!/bin/sh\nexit 0\n"))
	base.AllowedTools = []string{
		"repo_pull_request", "repo_pull_request_org", "repo_pull_request_thread",
		"repo_file", "pipelines_build", "core_list_projects", "wit_work_item",
	}
	if _, err := executors.NewCopilotExecutor(base); err != nil {
		t.Fatalf("allowlisted read tools rejected: %v", err)
	}
}

func TestCopilotRejectsHostileToolConfigs(t *testing.T) {
	base := copilotConfig(writeFakeCopilot(t, "#!/bin/sh\nexit 0\n"))
	cases := map[string]func(executors.CopilotConfig) executors.CopilotConfig{
		"shell fragment": func(c executors.CopilotConfig) executors.CopilotConfig {
			c.AllowedTools = []string{"shell"}
			return c
		},
		"write with parens": func(c executors.CopilotConfig) executors.CopilotConfig {
			c.AllowedTools = []string{"write(README.md)"}
			return c
		},
		"allow-all flag": func(c executors.CopilotConfig) executors.CopilotConfig {
			c.AllowedTools = []string{"--allow-all-tools"}
			return c
		},
		"qualified tool config": func(c executors.CopilotConfig) executors.CopilotConfig {
			c.AllowedTools = []string{"ado(pr_write)"}
			return c
		},
		"blank tool": func(c executors.CopilotConfig) executors.CopilotConfig {
			c.AllowedTools = []string{"   "}
			return c
		},
		"empty list": func(c executors.CopilotConfig) executors.CopilotConfig {
			c.AllowedTools = nil
			return c
		},
		"blank server": func(c executors.CopilotConfig) executors.CopilotConfig {
			c.MCPServer = "   "
			return c
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := executors.NewCopilotExecutor(mutate(base)); err == nil {
				t.Fatalf("expected NewCopilotExecutor error for %s", name)
			}
		})
	}
}

func TestCopilotRejectsInvalidResult(t *testing.T) {
	cases := map[string]string{
		"non-JSON stdout":             "#!/bin/sh\necho 'not json'\n",
		"bad verdict":                 "#!/bin/sh\necho '{\"verdict\":\"BOGUS\",\"reviewedCommits\":[\"abc\"],\"reviewedFiles\":[\"main.go\"],\"findings\":[]}'\n",
		"empty commits":               "#!/bin/sh\necho '{\"verdict\":\"CLEAN\",\"reviewedCommits\":[],\"reviewedFiles\":[\"main.go\"],\"findings\":[]}'\n",
		"empty files":                 "#!/bin/sh\necho '{\"verdict\":\"CLEAN\",\"reviewedCommits\":[\"abc\"],\"reviewedFiles\":[],\"findings\":[]}'\n",
		"blank file entry":            "#!/bin/sh\necho '{\"verdict\":\"CLEAN\",\"reviewedCommits\":[\"abc\"],\"reviewedFiles\":[\"   \"],\"findings\":[]}'\n",
		"finding without path":        "#!/bin/sh\necho '{\"verdict\":\"FINDINGS\",\"reviewedCommits\":[\"abc\"],\"reviewedFiles\":[\"main.go\"],\"findings\":[{\"line\":1,\"explanation\":\"x\",\"evidence\":\"y\"}]}'\n",
		"finding without explanation": "#!/bin/sh\necho '{\"verdict\":\"FINDINGS\",\"reviewedCommits\":[\"abc\"],\"reviewedFiles\":[\"main.go\"],\"findings\":[{\"path\":\"main.go\",\"line\":1,\"evidence\":\"y\"}]}'\n",
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeFakeCopilot(t, script)
			executor, err := executors.NewCopilotExecutor(copilotConfig(path))
			if err != nil {
				t.Fatal(err)
			}
			_, err = executor.Start(context.Background(), executors.AttemptEnvelope{
				TaskID: "task-1", AttemptID: "attempt-1", Workspace: t.TempDir(),
				Objective: "review", PayloadJSON: json.RawMessage(`{"repo":"shop","pr":1,"sourceCommit":"a","targetCommit":"b"}`),
			})
			if err == nil {
				t.Fatalf("expected Start error for %s", name)
			}
		})
	}
}

func TestCopilotRejectsBadPayload(t *testing.T) {
	path := writeFakeCopilot(t, "#!/bin/sh\necho '{\"verdict\":\"CLEAN\",\"reviewedCommits\":[\"abc\"],\"reviewedFiles\":[\"main.go\"],\"findings\":[]}'\n")
	executor, err := executors.NewCopilotExecutor(copilotConfig(path))
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Start(context.Background(), executors.AttemptEnvelope{
		TaskID: "task-1", AttemptID: "attempt-1", Workspace: t.TempDir(),
		Objective: "review", PayloadJSON: json.RawMessage(`{"repo":"shop","pr":1,"sourceCommit":"a"}`),
	})
	if err == nil {
		t.Fatal("expected Start error for payload missing targetCommit")
	}
}

func TestCopilotArgvIsClosed(t *testing.T) {
	path := writeFakeCopilot(t, "#!/bin/sh\necho \"$@\" > \"$(pwd)/argv.txt\"\necho '{\"verdict\":\"CLEAN\",\"reviewedCommits\":[\"abc\"],\"reviewedFiles\":[\"main.go\"],\"findings\":[]}'\n")
	executor, err := executors.NewCopilotExecutor(copilotConfig(path))
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	_, err = executor.Start(context.Background(), executors.AttemptEnvelope{
		TaskID: "task-1", AttemptID: "attempt-1", Workspace: workspace,
		Objective: "review", PayloadJSON: json.RawMessage(`{"repo":"shop","pr":1,"sourceCommit":"a","targetCommit":"b"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(workspace, "argv.txt"))
	if err != nil {
		t.Fatal(err)
	}
	dump := string(raw)
	if !strings.Contains(dump, "--deny-tool=shell,write,url,memory") {
		t.Fatalf("argv dump missing deny-tool list: %q", dump)
	}
	for _, forbidden := range []string{"allow-all", "add-dir", "--agent"} {
		if strings.Contains(dump, forbidden) {
			t.Fatalf("argv dump contains forbidden %q: %q", forbidden, dump)
		}
	}
}

func TestCopilotTimeoutAndExitCode(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		path := writeFakeCopilot(t, "#!/bin/sh\nsleep 2\necho '{\"verdict\":\"CLEAN\",\"reviewedCommits\":[\"abc\"],\"reviewedFiles\":[\"main.go\"],\"findings\":[]}'\n")
		cfg := copilotConfig(path)
		cfg.Timeout = 50 * time.Millisecond
		executor, err := executors.NewCopilotExecutor(cfg)
		if err != nil {
			t.Fatal(err)
		}
		_, err = executor.Start(context.Background(), executors.AttemptEnvelope{
			TaskID: "task-1", AttemptID: "attempt-1", Workspace: t.TempDir(),
			Objective: "review", PayloadJSON: json.RawMessage(`{"repo":"shop","pr":1,"sourceCommit":"a","targetCommit":"b"}`),
		})
		if err == nil {
			t.Fatal("expected timeout error")
		}
	})
	t.Run("exit code", func(t *testing.T) {
		path := writeFakeCopilot(t, "#!/bin/sh\necho '{\"verdict\":\"CLEAN\",\"reviewedCommits\":[\"abc\"],\"reviewedFiles\":[\"main.go\"],\"findings\":[]}'\nexit 3\n")
		executor, err := executors.NewCopilotExecutor(copilotConfig(path))
		if err != nil {
			t.Fatal(err)
		}
		_, err = executor.Start(context.Background(), executors.AttemptEnvelope{
			TaskID: "task-1", AttemptID: "attempt-1", Workspace: t.TempDir(),
			Objective: "review", PayloadJSON: json.RawMessage(`{"repo":"shop","pr":1,"sourceCommit":"a","targetCommit":"b"}`),
		})
		if err == nil {
			t.Fatal("expected exit-code error")
		}
	})
}

func TestCopilotRejectsProtectedEnv(t *testing.T) {
	cfg := copilotConfig(writeFakeCopilot(t, "#!/bin/sh\nexit 0\n"))
	cfg.Environment = map[string]string{"SUMMA42_OWNER_PRIVATE_KEY": "must-not-pass"}
	if _, err := executors.NewCopilotExecutor(cfg); err == nil {
		t.Fatal("expected constructor error for protected env")
	}
}

func TestCopilotRejectsNonAllowlistedEnv(t *testing.T) {
	cfg := copilotConfig(writeFakeCopilot(t, "#!/bin/sh\nexit 0\n"))
	cfg.Environment = map[string]string{"FOO": "1"}
	if _, err := executors.NewCopilotExecutor(cfg); err == nil {
		t.Fatal("expected constructor error for non-allowlisted env FOO")
	}
	for _, key := range []string{"SUMMA42_OWNER_PRIVATE_KEY", "AZURE_CLIENT_SECRET", "AWS_SECRET_ACCESS_KEY"} {
		cfg := copilotConfig(writeFakeCopilot(t, "#!/bin/sh\nexit 0\n"))
		cfg.Environment = map[string]string{key: "secret"}
		if _, err := executors.NewCopilotExecutor(cfg); err == nil {
			t.Fatalf("expected constructor error for protected env %s", key)
		}
	}
}

func TestCopilotAcceptsAllowlistedEnv(t *testing.T) {
	cfg := copilotConfig(writeFakeCopilot(t, "#!/bin/sh\nexit 0\n"))
	cfg.Environment = map[string]string{
		"PATH": "/usr/bin", "COPILOT_MODEL": "gpt-5", "GH_TOKEN": "t",
	}
	if _, err := executors.NewCopilotExecutor(cfg); err != nil {
		t.Fatalf("allowlisted env rejected: %v", err)
	}
}

func TestCopilotModelFlagPrecedence(t *testing.T) {
	path := writeFakeCopilot(t, "#!/bin/sh\necho \"$@\" > \"$(pwd)/argv.txt\"\necho '{\"verdict\":\"CLEAN\",\"reviewedCommits\":[\"abc\"],\"reviewedFiles\":[\"main.go\"],\"findings\":[]}'\n")
	cfg := copilotConfig(path)
	cfg.Model = "flag-model"
	cfg.Environment = map[string]string{"COPILOT_MODEL": "env-model"}
	executor, err := executors.NewCopilotExecutor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if _, err := executor.Start(context.Background(), executors.AttemptEnvelope{
		TaskID: "task-1", AttemptID: "attempt-1", Workspace: workspace,
		Objective: "review", PayloadJSON: json.RawMessage(`{"repo":"shop","pr":1,"sourceCommit":"a","targetCommit":"b"}`),
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(workspace, "argv.txt"))
	if err != nil {
		t.Fatal(err)
	}
	dump := string(raw)
	if got := strings.Count(dump, "--model="); got != 1 {
		t.Fatalf("--model= count = %d, want 1: %q", got, dump)
	}
	if !strings.Contains(dump, "--model=flag-model") {
		t.Fatalf("argv missing --model=flag-model: %q", dump)
	}
	if strings.Contains(dump, "--model=env-model") {
		t.Fatalf("argv uses env model value: %q", dump)
	}
}

func TestCopilotEvidenceIsCanonical(t *testing.T) {
	raw := "  {\"verdict\" : \"CLEAN\" , \"reviewedCommits\" : [\"abc\"] , \"reviewedFiles\" : [\"main.go\"] , \"findings\" : [] , \"reason\" : \"ok\" }  \n"
	path := writeFakeCopilot(t, "#!/bin/sh\nprintf '%s' '"+strings.ReplaceAll(raw, "'", "'\\''")+"'\n")
	executor, err := executors.NewCopilotExecutor(copilotConfig(path))
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Start(context.Background(), executors.AttemptEnvelope{
		TaskID: "task-1", AttemptID: "attempt-1", Workspace: t.TempDir(),
		Objective: "review", PayloadJSON: json.RawMessage(`{"repo":"shop","pr":1,"sourceCommit":"a","targetCommit":"b"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded executors.ReviewResult
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatal(err)
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != string(canonical) {
		t.Fatalf("Stdout = %q, want canonical %q", result.Stdout, string(canonical))
	}
	if strings.Contains(result.Stdout, " : ") {
		t.Fatalf("Stdout is not compact canonical JSON: %q", result.Stdout)
	}
	found := map[executors.EvidenceKind]int{}
	for _, ev := range result.Evidence {
		found[ev.Kind]++
		if ev.Kind == executors.EvidenceAgentMessage && ev.Content != result.Stdout {
			t.Fatalf("AGENT_MESSAGE evidence = %q, want Stdout %q", ev.Content, result.Stdout)
		}
	}
	if found[executors.EvidenceStderr] == 0 {
		t.Fatal("STDERR evidence always required")
	}
	if found[executors.EvidenceStdout] == 0 {
		t.Fatal("STDOUT evidence required when stdout non-empty")
	}
}

func TestCopilotFailsClosedWithoutADedicatedUserAndRunsThroughTheLauncher(t *testing.T) {
	path := writeFakeCopilot(t, "#!/bin/sh\necho '{\"verdict\":\"CLEAN\",\"reviewedCommits\":[\"abc\"],\"reviewedFiles\":[\"main.go\"],\"findings\":[]}'\n")
	for _, runAs := range []string{"", "root", "no-such-user-xyz"} {
		cfg := copilotConfig(path)
		cfg.RunAsUser = runAs
		if _, err := executors.NewCopilotExecutor(cfg); err == nil {
			t.Fatalf("run-as user %q accepted", runAs)
		}
	}
	cfg := copilotConfig(path)
	cfg.Environment = map[string]string{"COPILOT_GITHUB_TOKEN": "ghp_copilot_secret"}
	executor, err := executors.NewCopilotExecutor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(fakeSudoLog)
	if _, err := executor.Start(context.Background(), executors.AttemptEnvelope{
		TaskID: "t", AttemptID: "a", Workspace: t.TempDir(), Objective: "review",
		PayloadJSON: json.RawMessage(`{"repo":"shop","pr":1,"sourceCommit":"a","targetCommit":"b"}`),
	}); err != nil {
		t.Fatal(err)
	}
	logged, err := os.ReadFile(fakeSudoLog)
	if err != nil || !strings.Contains(string(logged), "-u "+modelruntest.User+" -- /usr/bin/timeout") {
		t.Fatalf("copilot did not go through the launcher: %q (%v)", logged, err)
	}
	if strings.Contains(string(logged), "ghp_copilot_secret") {
		t.Fatalf("token on the launcher argv: %q", logged)
	}
}
