package climodel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/capacity"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
)

// memLedger is an in-memory capacity.Ledger. A pool is exhausted while its
// latest refusal has a reset in the future and no success came after it.
type memLedger struct {
	mu    sync.Mutex
	calls []capacity.Call
}

func (l *memLedger) RecordCall(_ context.Context, c capacity.Call) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, c)
	return nil
}

func (l *memLedger) PoolState(_ context.Context, pool string, now time.Time) (capacity.PoolState, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	state := capacity.PoolState{Pool: pool}
	for i := len(l.calls) - 1; i >= 0; i-- {
		c := l.calls[i]
		if c.Pool != pool {
			continue
		}
		if c.Outcome == capacity.OutcomeOK {
			return state, nil
		}
		if c.Outcome == capacity.OutcomeRefused && c.ResetAt.After(now) {
			state.Exhausted, state.ResetAt = true, c.ResetAt
		}
		return state, nil
	}
	return state, nil
}

// countingCmd is a fake cmd that counts its runs and fails like a provider at
// its weekly limit.
func countingCmd(t *testing.T, stderr string) (binary, counter string) {
	t.Helper()
	dir := t.TempDir()
	binary = filepath.Join(dir, "cmd")
	counter = filepath.Join(dir, "runs")
	script := "#!/bin/sh\ncat > /dev/null\necho run >> " + counter + "\necho \"" + stderr + "\" >&2\nexit 5\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary, counter
}

func TestRefusalIsLedgeredAndGatesTheNextCall(t *testing.T) {
	binary, counter := countingCmd(t, "Error: You've reached your weekly usage limit. Resets in 2d 1h (Thu 10:23 AM).")
	ledger := &memLedger{}
	adapter, err := New(Config{RunAsUser: testUser, Binary: binary, CommandCodeModel: "reflection/beam:free", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	adapter = adapter.WithLedger(ledger, "plan-review", "case_1")
	review := ghtriage.PlanReviewInput{Schema: ghtriage.PlanReviewVersion}

	if _, err := adapter.ReviewPlan(context.Background(), review); !errors.Is(err, ghtriage.ErrModelUnavailable) {
		t.Fatalf("first call: %v, want unavailable", err)
	}
	if len(ledger.calls) != 1 {
		t.Fatalf("ledger rows = %d, want 1", len(ledger.calls))
	}
	row := ledger.calls[0]
	if row.Outcome != capacity.OutcomeRefused || row.ResetSource != "parsed" || row.Pool != "commandcode:free:reflection/beam:free" {
		t.Fatalf("refusal row = %+v", row)
	}
	if d := time.Until(row.ResetAt); d < 47*time.Hour || d > 50*time.Hour {
		t.Fatalf("reset %v is not about 49h away", d)
	}

	_, err = adapter.ReviewPlan(context.Background(), review)
	if !errors.Is(err, ghtriage.ErrModelUnavailable) || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("second call: %v, want a gated unavailable", err)
	}
	if runs, _ := os.ReadFile(counter); strings.Count(string(runs), "run") != 1 {
		t.Fatalf("the binary ran %q times; the gate should stop the second call before exec", runs)
	}
}

func TestRefusalWithoutAResetTimeDefaultsToAnHour(t *testing.T) {
	binary, _ := countingCmd(t, "Error: rate limit exceeded")
	ledger := &memLedger{}
	adapter, err := New(Config{RunAsUser: testUser, Binary: binary, CommandCodeModel: "reflection/beam:free", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	adapter = adapter.WithLedger(ledger, "plan-review", "case_1")
	_, _ = adapter.ReviewPlan(context.Background(), ghtriage.PlanReviewInput{Schema: ghtriage.PlanReviewVersion})
	if len(ledger.calls) != 1 {
		t.Fatalf("ledger rows = %d, want 1", len(ledger.calls))
	}
	if row := ledger.calls[0]; row.ResetSource != "default" || time.Until(row.ResetAt) < 59*time.Minute || time.Until(row.ResetAt) > 61*time.Minute {
		t.Fatalf("default reset row = %+v", row)
	}
}

func TestCommandCodeTokensReachTheRow(t *testing.T) {
	binary, _ := fakeCommandCode(t, resultLine("success", reviewAnswer, ""))
	ledger := &memLedger{}
	adapter, err := New(Config{RunAsUser: testUser, Binary: binary, CommandCodeModel: "reflection/beam:free", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	adapter = adapter.WithLedger(ledger, "plan-review", "case_1")
	if _, err := adapter.ReviewPlan(context.Background(), ghtriage.PlanReviewInput{Schema: ghtriage.PlanReviewVersion}); err != nil {
		t.Fatal(err)
	}
	row := ledger.calls[0]
	if row.Outcome != capacity.OutcomeOK || row.InputTokens == nil || *row.InputTokens != 3 || row.OutputTokens == nil || *row.OutputTokens != 1 {
		t.Fatalf("success row = %+v, want ok with tokens 3/1", row)
	}
}

// Every failed call keeps its message, not only refusals, so a timeout and a
// crash can be told apart from the ledger.
func TestFailedCallKeepsItsMessage(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "cmd")
	script := "#!/bin/sh\ncat > /dev/null\necho 'segmentation fault in model host' >&2\nexit 1\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ledger := &memLedger{}
	adapter, err := New(Config{RunAsUser: testUser, Binary: binary, CommandCodeModel: "reflection/beam:free", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	adapter = adapter.WithLedger(ledger, "plan-review", "case_1")
	_, _ = adapter.ReviewPlan(context.Background(), ghtriage.PlanReviewInput{Schema: ghtriage.PlanReviewVersion})
	if len(ledger.calls) != 1 {
		t.Fatalf("ledger rows = %d, want 1", len(ledger.calls))
	}
	if row := ledger.calls[0]; row.Outcome != capacity.OutcomeError || !strings.Contains(row.Message, "segmentation fault") {
		t.Fatalf("error row = %+v, want the error text", row)
	}
}
