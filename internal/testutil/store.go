package testutil

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

func OpenStore(t *testing.T) *state.Store {
	t.Helper()
	store, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.DB().Close()
	})
	return store
}

// AnyTaskState lists every state a test may challenge a task from; it lets
// tests call execution.ChallengeTaskIfInStates without a state guard.
var AnyTaskState = []domain.TaskState{
	domain.TaskCreated, domain.TaskEligible, domain.TaskExecuting, domain.TaskAwaitingVerification,
	domain.TaskSucceeded, domain.TaskFailed, domain.TaskBlocked, domain.TaskCancelled, domain.TaskChallenged, domain.TaskExpired,
}
