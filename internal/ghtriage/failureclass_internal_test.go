package ghtriage

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

func TestFailureSignatureClasses(t *testing.T) {
	_, parseErr := ParsePlanReviewOutput([]byte("not json"))
	exitErr := exec.Command("false").Run()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"deadline", context.DeadlineExceeded, "timeout"},
		{"wrapped deadline", fmt.Errorf("model invocation timed out: %w", context.DeadlineExceeded), "timeout"},
		{"exit", fmt.Errorf("model invocation failed: %w", exitErr), "model-exit"},
		{"parse", parseErr, "parse"},
		{"validation", &planFailure{"validation", errors.New("bad")}, "validation"},
		{"internal", errors.New("disk"), "internal"},
	}
	for _, c := range cases {
		if got, want := failureSignature("plan-review", "work_1", c.err), "plan-review:"+c.want+":work_1"; got != want {
			t.Errorf("%s: %q, want %q", c.name, got, want)
		}
	}
}
