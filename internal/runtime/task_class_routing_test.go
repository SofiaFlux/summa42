package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/localconfig"
)

// stubExecutorPreference stands in for the learned preference so the chain a Box
// builds can be read directly: it always claims a task, and it would happily
// claim a triage Task if it were consulted first.
type stubExecutorPreference struct{ kind string }

func (s stubExecutorPreference) PreferredExecutor(context.Context, domain.Task, []string) (string, bool, error) {
	return s.kind, true, nil
}

// The chain a routing Box builds: task-class routing first, then the
// preference. A mapped class must land on the triage executor even though the
// preference sorts earlier than nothing and would claim it too, and an unmapped
// class must still reach the preference instead of stopping at the routing entry.
func TestBoxRoutesMappedTaskClassAheadOfTheLearnedPreference(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	box, err := Open(ctx, Config{
		StatePath:          filepath.Join(root, "state.db"),
		EvidencePath:       filepath.Join(root, "evidence"),
		CollectiveID:       "collective-1",
		OwnerPrincipalID:   "owner-1",
		PolicyEngine:       compositionPolicy{},
		TaskClassRouting:   map[string]string{ghtriage.TaskClass: ghtriage.ExecutorKind},
		ExecutorPreference: stubExecutorPreference{kind: "learned-executor"},
		FieldFeedback: localconfig.FieldFeedbackConfig{
			Enabled: false, Mode: localconfig.FeedbackModeLocalOnly,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()

	eligible := []string{"copilot", ghtriage.ExecutorKind, "learned-executor"}
	chosen, err := box.Scheduler.ChooseExecutor(ctx,
		domain.Task{ID: "task-triage", TaskClass: ghtriage.TaskClass}, eligible)
	if err != nil {
		t.Fatal(err)
	}
	if chosen != ghtriage.ExecutorKind {
		t.Fatalf("mapped task class chose %q, want %q: routing is consulted before the preference, and copilot is the alphabetical fallback",
			chosen, ghtriage.ExecutorKind)
	}
	unmapped, err := box.Scheduler.ChooseExecutor(ctx,
		domain.Task{ID: "task-review", TaskClass: "repo.review"}, eligible)
	if err != nil {
		t.Fatal(err)
	}
	if unmapped != "learned-executor" {
		t.Fatalf("unmapped task class chose %q, want the preference behind the routing entry", unmapped)
	}
}

// Without TaskClassRouting the Box builds the single-preference chain it built
// before routing existed, so a class nothing maps to is the preference's answer.
func TestBoxWithoutTaskClassRoutingLeavesEveryClassToThePreference(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	box, err := Open(ctx, Config{
		StatePath:          filepath.Join(root, "state.db"),
		EvidencePath:       filepath.Join(root, "evidence"),
		CollectiveID:       "collective-1",
		OwnerPrincipalID:   "owner-1",
		PolicyEngine:       compositionPolicy{},
		ExecutorPreference: stubExecutorPreference{kind: "learned-executor"},
		FieldFeedback: localconfig.FieldFeedbackConfig{
			Enabled: false, Mode: localconfig.FeedbackModeLocalOnly,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()

	eligible := []string{"copilot", ghtriage.ExecutorKind, "learned-executor"}
	for _, taskClass := range []string{ghtriage.TaskClass, "repo.review"} {
		chosen, err := box.Scheduler.ChooseExecutor(ctx, domain.Task{ID: "task-1", TaskClass: taskClass}, eligible)
		if err != nil {
			t.Fatal(err)
		}
		if chosen != "learned-executor" {
			t.Fatalf("task class %q chose %q, want the only preference in the chain", taskClass, chosen)
		}
	}
}
