package main

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/ghtriage/climodel"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
)

func TestParseWorkerFlagsDefaults(t *testing.T) {
	poll, lease, model, err := parseWorkerFlags([]string{})
	if err != nil {
		t.Fatal(err)
	}
	if poll != 30*time.Second || lease != 0 {
		t.Fatalf("flags = %v %v, want 30s 0", poll, lease)
	}
	if model.Binary != "codex" || model.Timeout != 60*time.Second {
		t.Fatalf("model = %+v, want codex 60s", model)
	}
}

func TestParseWorkerFlagsRejectsNonPositivePoll(t *testing.T) {
	if _, _, _, err := parseWorkerFlags([]string{"--poll-interval=0"}); err == nil {
		t.Fatal("expected error for zero poll interval")
	}
}

func TestParseWorkerFlagsAcceptsOverrides(t *testing.T) {
	poll, lease, _, err := parseWorkerFlags([]string{"--poll-interval=5s", "--lease-duration=2m"})
	if err != nil {
		t.Fatal(err)
	}
	if poll != 5*time.Second || lease != 2*time.Minute {
		t.Fatalf("flags = %v %v, want 5s 2m", poll, lease)
	}
	_, _, overridden, err := parseWorkerFlags([]string{"--model-binary=/usr/bin/custom", "--model-timeout=5s"})
	if err != nil {
		t.Fatal(err)
	}
	if overridden.Binary != "/usr/bin/custom" || overridden.Timeout != 5*time.Second {
		t.Fatalf("model = %+v, want /usr/bin/custom 5s", overridden)
	}
}

// A model flag that cannot be honoured is a misconfiguration and stays a startup
// error. Degrading a missing model binary must not start swallowing these, or a
// typo would silently turn a box's triage executor off.
func TestParseWorkerFlagsRejectsUnusableModelValues(t *testing.T) {
	if _, _, _, err := parseWorkerFlags([]string{"--model-timeout=nope"}); err == nil {
		t.Fatal("expected error for an unparseable --model-timeout")
	} else if !strings.Contains(err.Error(), "--model-timeout") {
		t.Fatalf("error = %q, want it to name --model-timeout", err)
	}
	if _, _, _, err := parseWorkerFlags([]string{"--model-timeout=0s"}); err == nil {
		t.Fatal("expected error for a non-positive --model-timeout")
	}
	if _, _, _, err := parseWorkerFlags([]string{"--model-binary="}); err == nil {
		t.Fatal("expected error for a blank --model-binary")
	} else if !strings.Contains(err.Error(), "--model-binary") {
		t.Fatalf("error = %q, want it to name --model-binary", err)
	}
}

// parseWorkerFlags parses the model and timing flags with a dummy workspace root.
func parseWorkerFlags(args []string) (time.Duration, time.Duration, climodel.Config, error) {
	_, _, _, poll, lease, model, err := parseWorkerArgs(append([]string{"--workspace-root=/ws"}, args...))
	return poll, lease, model, err
}

func TestWorkerOnceInsideModelArgIsPreserved(t *testing.T) {
	once, root, _, _, _, model, err := parseWorkerArgs([]string{"--workspace-root", "/ws", "--model-arg=exec", "--model-arg", "--once"})
	if err != nil {
		t.Fatal(err)
	}
	if once {
		t.Fatal("--once consumed from inside a --model-arg value")
	}
	if root != "/ws" || len(model.Args) != 2 || model.Args[1] != "--once" {
		t.Fatalf("root=%q model args=%v, want /ws and [exec --once]", root, model.Args)
	}
	once, _, _, _, _, _, err = parseWorkerArgs([]string{"--once", "--workspace-root=/ws"})
	if err != nil || !once {
		t.Fatalf("once=%v err=%v, want true", once, err)
	}
	if _, _, _, _, _, _, err = parseWorkerArgs([]string{"--once"}); err == nil {
		t.Fatal("missing --workspace-root accepted")
	}
}

func TestWorkerCapacityEmptyRegistryErrors(t *testing.T) {
	empty := &summa42runtime.Box{Executors: map[string]executors.Executor{}}
	if _, err := workerCapacity(empty); err == nil {
		t.Fatal("expected explicit error for empty executor registry")
	} else if !strings.Contains(err.Error(), "no schedulable capabilities") {
		t.Fatalf("error = %q, want explicit empty-registry message", err)
	}
}

func TestWorkerCredentialFileFlag(t *testing.T) {
	_, _, cred, _, _, _, err := parseWorkerArgs([]string{"--workspace-root=/ws", "--credential-file=/tok"})
	if err != nil || cred != "/tok" {
		t.Fatalf("cred=%q err=%v", cred, err)
	}
}

func TestFeedbackTokenFilePrefersFlagOverDeprecatedEnv(t *testing.T) {
	t.Setenv("SUMMA42_FEEDBACK_GITHUB_TOKEN_FILE", "/env")
	if got := feedbackTokenFile(" /flag "); got != "/flag" {
		t.Fatalf("got %q, want /flag", got)
	}
	if got := feedbackTokenFile(""); got != "/env" {
		t.Fatalf("got %q, want /env alias", got)
	}
}

func TestGHIntakeCredentialFilePreferredOverEnv(t *testing.T) {
	t.Setenv("SUMMA42_GITHUB_TOKEN_FILE", "/env")
	if got := ghIntakeClientConfig("o/r", "", "/flag").TokenFile; got != "/flag" {
		t.Fatalf("token file = %q, want /flag", got)
	}
}

func TestFeedbackBotLoginWarningAndEnvDeprecation(t *testing.T) {
	var buf strings.Builder
	oldW, oldOnce := diagWriter, &feedbackWarnOnce
	diagWriter = &buf
	feedbackWarnOnce = sync.Once{}
	deprecationNotices.Delete("SUMMA42_GITHUB_TOKEN_FILE")
	defer func() { diagWriter = oldW; _ = oldOnce }()
	t.Setenv("SUMMA42_FEEDBACK_GITHUB_BOT_LOGIN", "")
	t.Setenv("SUMMA42_FEEDBACK_GITHUB_TOKEN_FILE", "/tok")
	cfg := localconfig.Config{FieldFeedback: localconfig.FieldFeedbackConfig{
		Enabled: true, Mode: localconfig.FeedbackModeAutoIfAllowed, Provider: "github", Destination: "owner/repo",
	}}
	for i := 0; i < 2; i++ {
		if _, err := buildFeedbackSink(cfg, ""); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(buf.String(), "SUMMA42_FEEDBACK_GITHUB_BOT_LOGIN is not set"); n != 1 {
		t.Fatalf("bot-login warning logged %d times, want 1: %q", n, buf.String())
	}
	t.Setenv("SUMMA42_GITHUB_TOKEN_FILE", "/env")
	for i := 0; i < 2; i++ {
		ghIntakeClientConfig("o/r", "", "")
	}
	if n := strings.Count(buf.String(), "SUMMA42_GITHUB_TOKEN_FILE is deprecated"); n != 1 {
		t.Fatalf("intake env deprecation logged %d times, want 1", n)
	}
}
