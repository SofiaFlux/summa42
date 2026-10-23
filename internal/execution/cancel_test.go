package execution_test

import (
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
)

func TestCancelEligibleTaskOnlyCancelsEligibleTasks(t *testing.T) {
	store, svc, ctx := newTriageService(t)
	task := newTriageTask(t, store, svc, ctx, "cancel-me")
	if task.State != domain.TaskEligible {
		t.Fatalf("fixture task is %s, want ELIGIBLE", task.State)
	}
	changed, err := svc.CancelEligibleTask(ctx, task.ID)
	if err != nil || !changed {
		t.Fatalf("cancel = %v, %v", changed, err)
	}
	if n := countRows(t, store, ctx, `SELECT COUNT(*) FROM tasks WHERE task_id = ? AND state = 'CANCELLED'`, task.ID); n != 1 {
		t.Fatal("task not CANCELLED")
	}
	if n := countRows(t, store, ctx, `SELECT COUNT(*) FROM execution_events WHERE task_id = ? AND event_type = 'TASK_CANCELLED'`, task.ID); n != 1 {
		t.Fatal("no TASK_CANCELLED event")
	}
	if changed, err := svc.CancelEligibleTask(ctx, task.ID); err != nil || changed {
		t.Fatalf("second cancel = %v, %v; want no change", changed, err)
	}
}
