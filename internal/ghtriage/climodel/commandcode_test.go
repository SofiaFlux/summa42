package climodel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/ghtriage"
)

// fakeCommandCode is an executable named cmd that records CMD_ZDR and prints
// stdout, standing in for the Command Code CLI.
func fakeCommandCode(t *testing.T, stdout string) (binary, zdrFile string) {
	t.Helper()
	dir := t.TempDir()
	zdrFile = filepath.Join(dir, "zdr")
	binary = filepath.Join(dir, "cmd")
	script := "#!/bin/sh\ncat > /dev/null\nprintf %s \"$CMD_ZDR\" > " + zdrFile + "\ncat <<'NDJSON'\n" + stdout + "\nNDJSON\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary, zdrFile
}

const reviewAnswer = `{"verdict":"ACCEPT","reason":"scope fits"}`

func resultLine(subtype, finalText, errText string) string {
	return `{"type":"event","event":{"type":"run_end"}}` + "\n" +
		`{"type":"result","subtype":"` + subtype + `","finalText":` + jsonString(finalText) + `,"error":` + jsonString(errText) + `,"usage":{"inputTokens":3,"outputTokens":1}}`
}

func jsonString(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

func TestCommandCodeAnswerIsTheSuccessfulResultText(t *testing.T) {
	binary, _ := fakeCommandCode(t, resultLine("success", reviewAnswer, ""))
	adapter, err := New(Config{RunAsUser: testUser, Binary: binary, CommandCodeModel: "reflection/beam:free", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.ReviewPlan(context.Background(), ghtriage.PlanReviewInput{Schema: ghtriage.PlanReviewVersion}); err != nil {
		t.Fatal(err)
	}
}

func TestCommandCodeFailuresFailClosed(t *testing.T) {
	for name, stdout := range map[string]string{
		"error result":   resultLine("error", "", "Rate limit exceeded"),
		"empty answer":   resultLine("success", "  ", ""),
		"no result":      `{"type":"event","event":{"type":"run_end"}}`,
		"not json":       "Error: Unable to connect to the API.",
		"error after ok": resultLine("success", reviewAnswer, "") + "\n" + resultLine("error", "", "late failure"),
	} {
		t.Run(name, func(t *testing.T) {
			binary, _ := fakeCommandCode(t, stdout)
			adapter, err := New(Config{RunAsUser: testUser, Binary: binary, CommandCodeModel: "reflection/beam:free", Timeout: 10 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := adapter.ReviewPlan(context.Background(), ghtriage.PlanReviewInput{Schema: ghtriage.PlanReviewVersion}); err == nil {
				t.Fatal("a failed Command Code run was read as an answer")
			}
		})
	}
}

func TestCommandCodeConfigurationRules(t *testing.T) {
	binary, _ := fakeCommandCode(t, "")
	plain := writeScript(t, "{}")
	for name, tc := range map[string]struct {
		config Config
		want   string
	}{
		"paid without opt-in":  {Config{Binary: binary, CommandCodeModel: "zai-org/glm-5.3"}, "not free"},
		"zdr on a free model":  {Config{Binary: binary, CommandCodeModel: "poolside/laguna-s-2.1-free", ZDR: true}, "zero data retention"},
		"zdr on another cli":   {Config{Binary: plain, ZDR: true}, "--model-commandcode"},
		"cmd without the flag": {Config{Binary: binary, Args: []string{"-p", "-m", "reflection/beam:free", "--output-format", "json"}}, "--model-commandcode"},
		"operator args":        {Config{Binary: binary, CommandCodeModel: "reflection/beam:free", Args: []string{"--yolo"}}, "--model-arg is not accepted"},
		"another binary":       {Config{Binary: plain, CommandCodeModel: "reflection/beam:free"}, "needs the Command Code binary"},
		"option as model id":   {Config{Binary: binary, CommandCodeModel: "--yolo"}, "invalid Command Code model id"},
		"installed as cmdc":    {Config{Binary: filepath.Join(filepath.Dir(binary), "cmdc")}, "--model-commandcode"},
	} {
		t.Run(name, func(t *testing.T) {
			tc.config.RunAsUser, tc.config.Timeout = testUser, time.Second
			if _, err := New(tc.config); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
	if _, err := New(Config{RunAsUser: testUser, Binary: binary, CommandCodeModel: "zai-org/glm-5.3", AllowPaid: true, ZDR: true, Timeout: time.Second}); err != nil {
		t.Fatalf("paid ZDR model with opt-in refused: %v", err)
	}
}

func TestCommandCodeZDRReachesTheModelProcess(t *testing.T) {
	binary, zdrFile := fakeCommandCode(t, resultLine("success", reviewAnswer, ""))
	adapter, err := New(Config{RunAsUser: testUser, Binary: binary, CommandCodeModel: "zai-org/glm-5.3", AllowPaid: true, ZDR: true, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.ReviewPlan(context.Background(), ghtriage.PlanReviewInput{Schema: ghtriage.PlanReviewVersion}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(zdrFile); err != nil || string(got) != "1" {
		t.Fatalf("CMD_ZDR in the model process = %q, %v", got, err)
	}
}

func TestCommandCodeArgvIsBuiltByTheAdapter(t *testing.T) {
	dir := t.TempDir()
	argvFile, cwdFile := filepath.Join(dir, "argv"), filepath.Join(dir, "cwd")
	binary := filepath.Join(dir, "cmd")
	script := "#!/bin/sh\ncat > /dev/null\nprintf '%s\\n' \"$@\" > " + argvFile + "\npwd > " + cwdFile + "\necho '" + strings.ReplaceAll(resultLine("success", reviewAnswer, ""), "\n", "'\necho '") + "'\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	adapter, err := New(Config{RunAsUser: testUser, Binary: binary, CommandCodeModel: "reflection/beam:free", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.ReviewPlan(context.Background(), ghtriage.PlanReviewInput{Schema: ghtriage.PlanReviewVersion}); err != nil {
		t.Fatal(err)
	}
	argv, _ := os.ReadFile(argvFile)
	want := strings.Join(commandCodeArgs("reflection/beam:free"), "\n") + "\n"
	if string(argv) != want {
		t.Fatalf("argv:\n%s\nwant:\n%s", argv, want)
	}
	for _, flag := range []string{"plan", "--no-session", "--safe-mode", "--no-skills", "--skip-onboarding", "-t"} {
		if !strings.Contains(want, flag) {
			t.Fatalf("argv lacks %s", flag)
		}
	}
	if cwd, _ := os.ReadFile(cwdFile); !strings.Contains(string(cwd), "/tmp/run.") {
		t.Fatalf("cmd did not run in a private scratch dir: %s", cwd)
	}
}

func TestModelOutputIsCapped(t *testing.T) {
	var b cappedBuffer
	if _, err := b.Write(make([]byte, maxModelOutput)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("x")); err == nil {
		t.Fatal("output past the cap was buffered")
	}
}

func TestProviderLimitsAreMarkedUnavailable(t *testing.T) {
	for name, stdout := range map[string]string{
		"rate limit result": resultLine("error", "", "Rate limit exceeded"),
		"withdrawn model":   resultLine("error", "", "MiniMax M3.1 Flash is no longer available."),
		"other error":       resultLine("error", "", "model crashed"),
	} {
		t.Run(name, func(t *testing.T) {
			binary, _ := fakeCommandCode(t, stdout)
			adapter, err := New(Config{RunAsUser: testUser, Binary: binary, CommandCodeModel: "reflection/beam:free", Timeout: 10 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			_, err = adapter.ReviewPlan(context.Background(), ghtriage.PlanReviewInput{Schema: ghtriage.PlanReviewVersion})
			if got, want := errors.Is(err, ghtriage.ErrModelUnavailable), name != "other error"; got != want {
				t.Fatalf("unavailable = %v, want %v (err %v)", got, want, err)
			}
		})
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "cmd")
	script := "#!/bin/sh\ncat > /dev/null\necho \"Error: You've reached today's limit on Glyph Cluster.\" >&2\nexit 5\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	adapter, err := New(Config{RunAsUser: testUser, Binary: binary, CommandCodeModel: "stealth/glyph-cluster:free", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.ReviewPlan(context.Background(), ghtriage.PlanReviewInput{Schema: ghtriage.PlanReviewVersion}); !errors.Is(err, ghtriage.ErrModelUnavailable) {
		t.Fatalf("daily limit exit not marked unavailable: %v", err)
	}
}

func TestUnfenceOnlyUnwrapsOneWholeJSONBlock(t *testing.T) {
	for in, want := range map[string]string{
		"```json\n{\"a\":1}\n```":            `{"a":1}`,
		"  ```JSON\n{\"a\":1}\n```\n":        `{"a":1}`,
		"```\n{\"a\":1}\n```":                `{"a":1}`,
		`{"a":1}`:                            `{"a":1}`,
		"```python\nx = 1\n```":              "```python\nx = 1\n```",
		"```json\n{}\n```\n```json\n{}\n```": "```json\n{}\n```\n```json\n{}\n```",
		"Here: ```json\n{}\n```":             "Here: ```json\n{}\n```",
	} {
		if got := string(unfence([]byte(in))); got != want {
			t.Errorf("unfence(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCommandCodePromptStartsWithTheNoToolsPreamble(t *testing.T) {
	dir := t.TempDir()
	stdinFile := filepath.Join(dir, "stdin")
	binary := filepath.Join(dir, "cmd")
	script := "#!/bin/sh\ncat > " + stdinFile + "\nprintf '%s\\n' '" + strings.ReplaceAll(resultLine("success", "```json\\n"+reviewAnswer+"\\n```", ""), "\n", "' '") + "'\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	adapter, err := New(Config{RunAsUser: testUser, Binary: binary, CommandCodeModel: "reflection/beam:free", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.ReviewPlan(context.Background(), ghtriage.PlanReviewInput{Schema: ghtriage.PlanReviewVersion}); err != nil {
		t.Fatalf("fenced answer not accepted: %v", err)
	}
	got, _ := os.ReadFile(stdinFile)
	if !strings.HasPrefix(string(got), commandCodePreamble+"{") {
		t.Fatalf("prompt = %.120q, want the preamble then the JSON document", got)
	}
}
