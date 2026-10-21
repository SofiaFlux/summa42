package scheduler

import (
	"context"
	"testing"
	"time"
)

func TestAttemptBudgetIsMinOfLeaseAndCallerMinusMargin(t *testing.T) {
	if got := attemptBudget(context.Background(), 5*time.Minute); got != 5*time.Minute-attemptMargin {
		t.Fatalf("no caller deadline: %s", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	got := attemptBudget(ctx, 5*time.Minute)
	if got > time.Minute-attemptMargin || got < time.Minute-attemptMargin-5*time.Second {
		t.Fatalf("caller deadline did not bound the budget: %s", got)
	}
}
