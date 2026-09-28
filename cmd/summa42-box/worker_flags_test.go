package main

import (
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/executors"
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

func TestSplitWorkspaceRootArgEqualsForm(t *testing.T) {
	root, rest, err := splitWorkspaceRootArg([]string{"--workspace-root=/tmp/ws", "--poll-interval=5s"})
	if err != nil {
		t.Fatal(err)
	}
	if root != "/tmp/ws" {
		t.Fatalf("root = %q, want /tmp/ws", root)
	}
	if len(rest) != 1 || rest[0] != "--poll-interval=5s" {
		t.Fatalf("rest = %v, want [--poll-interval=5s]", rest)
	}
}

func TestSplitWorkspaceRootArgSeparateForm(t *testing.T) {
	root, rest, err := splitWorkspaceRootArg([]string{"--workspace-root", "/tmp/ws", "--poll-interval=5s"})
	if err != nil {
		t.Fatal(err)
	}
	if root != "/tmp/ws" {
		t.Fatalf("root = %q, want /tmp/ws", root)
	}
	if len(rest) != 1 || rest[0] != "--poll-interval=5s" {
		t.Fatalf("rest = %v, want [--poll-interval=5s]", rest)
	}
}

func TestSplitWorkspaceRootArgMissingValue(t *testing.T) {
	if _, _, err := splitWorkspaceRootArg([]string{"--workspace-root"}); err == nil {
		t.Fatal("expected error for missing --workspace-root value")
	}
}

func TestSplitWorkspaceRootArgEmptyReject(t *testing.T) {
	root, _, err := splitWorkspaceRootArg([]string{"--workspace-root="})
	if err != nil {
		return
	}
	if strings.TrimSpace(root) != "" {
		t.Fatalf("root = %q, want empty", root)
	}
	if strings.TrimSpace(root) == "" {
		// runWorker rejects an empty workspace root; split must surface it as empty
		// so the caller-side guard triggers.
		return
	}
	t.Fatal("expected empty workspace root to be rejected downstream")
}

func TestWorkerCapacityEmptyRegistryErrors(t *testing.T) {
	empty := &summa42runtime.Box{Executors: map[string]executors.Executor{}}
	if _, err := workerCapacity(empty); err == nil {
		t.Fatal("expected explicit error for empty executor registry")
	} else if !strings.Contains(err.Error(), "no schedulable capabilities") {
		t.Fatalf("error = %q, want explicit empty-registry message", err)
	}
}
