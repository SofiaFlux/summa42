package climodel

import (
	"context"
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

func commandCodeArgs(model string) []string {
	return []string{"-p", "-m", model, "--output-format", "json", "--permission-mode", "plan"}
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
	adapter, err := New(Config{RunAsUser: testUser, Binary: binary, Args: commandCodeArgs("reflection/beam:free"), Timeout: 10 * time.Second})
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
			adapter, err := New(Config{RunAsUser: testUser, Binary: binary, Args: commandCodeArgs("reflection/beam:free"), Timeout: 10 * time.Second})
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
		"paid without opt-in": {Config{Binary: binary, Args: commandCodeArgs("zai-org/glm-5.3")}, "not free"},
		"zdr on a free model": {Config{Binary: binary, Args: commandCodeArgs("poolside/laguna-s-2.1-free"), ZDR: true}, "zero data retention"},
		"zdr on another cli":  {Config{Binary: plain, ZDR: true}, "not cmd"},
		"no model":            {Config{Binary: binary, Args: []string{"-p", "--output-format", "json"}}, "-m <model id>"},
		"no json output":      {Config{Binary: binary, Args: []string{"-p", "-m", "reflection/beam:free"}}, "--output-format json"},
	} {
		t.Run(name, func(t *testing.T) {
			tc.config.RunAsUser, tc.config.Timeout = testUser, time.Second
			if _, err := New(tc.config); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
	if _, err := New(Config{RunAsUser: testUser, Binary: binary, Args: commandCodeArgs("zai-org/glm-5.3"), AllowPaid: true, ZDR: true, Timeout: time.Second}); err != nil {
		t.Fatalf("paid ZDR model with opt-in refused: %v", err)
	}
}

func TestCommandCodeZDRReachesTheModelProcess(t *testing.T) {
	binary, zdrFile := fakeCommandCode(t, resultLine("success", reviewAnswer, ""))
	adapter, err := New(Config{RunAsUser: testUser, Binary: binary, Args: commandCodeArgs("zai-org/glm-5.3"), AllowPaid: true, ZDR: true, Timeout: 10 * time.Second})
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
