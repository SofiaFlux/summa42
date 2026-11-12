//go:build unix

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SofiaFlux/summa42/internal/modelrun/modelruntest"
)

// The start script and the example environment in deploy/ are what the
// runbook tells the Owner to install. This runs the real script with the
// example file's values (paths and user names replaced by ones that exist
// here) and gives the arguments it would start summa42-box with to serve's
// own flag checks, so the documented way to start cannot drift from the code.
func TestDeployedServeScriptProducesFlagsServeAccepts(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	_, _, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	work := t.TempDir()
	source := filepath.Join(work, "source")
	git := func(arguments ...string) string {
		command := exec.Command("git", arguments...)
		command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", arguments, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "-q", "-b", "main", source)
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("-C", source, "add", "-A")
	git("-C", source, "commit", "-q", "-m", "base")
	head := git("-C", source, "rev-parse", "HEAD")

	token := filepath.Join(work, "github_token")
	if err := os.WriteFile(token, []byte("not-a-real-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A stand-in for summa42-box that records its arguments, one per line.
	recorded := filepath.Join(work, "args")
	stub := filepath.Join(work, "box-stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > "+recorded+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	example, err := os.ReadFile(filepath.Join(root, "deploy", "serve.env.example"))
	if err != nil {
		t.Fatal(err)
	}
	override := map[string]string{
		"SUMMA42_HOME":              filepath.Join(work, "home"),
		"SUMMA42_SOURCE_REPO":       source,
		"SUMMA42_WORKSPACE_ROOT":    filepath.Join(work, "attempts"),
		"SUMMA42_GITHUB_TOKEN_FILE": token,
		"SUMMA42_MODEL_USER":        modelruntest.User,
		"SUMMA42_VALIDATION_USER":   modelruntest.ValidationUser,
		"SUMMA42_BOX_BIN":           stub,
	}
	environment := []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_SYSTEM=" + os.DevNull}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(example), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("serve.env.example: not NAME=value: %q", line)
		}
		if replacement, ok := override[name]; ok {
			value = replacement
		}
		seen[name] = true
		environment = append(environment, name+"="+value)
		t.Setenv(name, value) // the flag parsers read some defaults from the environment
	}
	for name := range override {
		if !seen[name] {
			t.Fatalf("serve.env.example does not set %s", name)
		}
	}

	script := exec.Command("/bin/sh", filepath.Join(root, "deploy", "summa42-serve.sh"))
	script.Env = environment
	var stderr bytes.Buffer
	script.Stderr = &stderr
	if err := script.Run(); err != nil {
		t.Fatalf("summa42-serve.sh: %v\n%s", err, stderr.String())
	}
	raw, err := os.ReadFile(recorded)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(args) == 0 || args[0] != "serve" {
		t.Fatalf("the script did not start `summa42-box serve`: %v", args)
	}
	if !strings.Contains(string(raw), "--source-sha\n"+head+"\n") {
		t.Fatalf("the script did not pin the base branch's commit %s:\n%s", head, raw)
	}

	own, sections, err := splitServeArgs(args[1:])
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 0 {
		t.Fatalf("unexpected serve flags before the first section: %v", own)
	}
	loops := serveLoops(sections)
	if len(loops) != 3 || loops[0].name != "control" || loops[1].name != "intake" || loops[2].name != "pipeline" {
		t.Fatalf("loops = %+v, want control, intake and pipeline", loops)
	}
	for _, loop := range loops {
		if err := loop.validate(loop.args); err != nil {
			t.Errorf("%s flags from the deployed script are refused: %v\n%v", loop.name, err, loop.args)
		}
	}
	// With a webhook file set the script adds the notifier.
	webhook := filepath.Join(work, "discord_webhook")
	if err := os.WriteFile(webhook, []byte("https://discord.com/api/webhooks/1/token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withWebhook := exec.Command("/bin/sh", filepath.Join(root, "deploy", "summa42-serve.sh"))
	withWebhook.Env = append(environment, "SUMMA42_DISCORD_WEBHOOK_FILE="+webhook)
	if output, err := withWebhook.CombinedOutput(); err != nil {
		t.Fatalf("summa42-serve.sh with a webhook: %v\n%s", err, output)
	}
	raw, err = os.ReadFile(recorded)
	if err != nil {
		t.Fatal(err)
	}
	_, sections, err = splitServeArgs(strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")[1:])
	if err != nil {
		t.Fatal(err)
	}
	loops = serveLoops(sections)
	if len(loops) != 4 || loops[3].name != "notifier" {
		t.Fatalf("loops with a webhook = %+v, want the notifier as well", loops)
	}
	if err := loops[3].validate(loops[3].args); err != nil {
		t.Errorf("notifier flags from the deployed script are refused: %v", err)
	}
}

func TestDeployedServeScriptNamesAMissingSetting(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	script := exec.Command("/bin/sh", filepath.Join(root, "deploy", "summa42-serve.sh"))
	script.Env = []string{"PATH=" + os.Getenv("PATH"), "SUMMA42_MISSION=m"}
	output, err := script.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "SUMMA42_ENVELOPE is not set") {
		t.Fatalf("script with a missing setting: %v\n%s", err, output)
	}
}
