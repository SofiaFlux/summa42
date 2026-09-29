package execution_test

import (
	"context"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

// The reason a task is CHALLENGED is the only account of why its work stopped,
// so reading it back is not decoration: it is what lets a caller that finds a
// challenged task finish the transition the challenge began instead of inventing
// a reason of its own. The whole row comes back, because the scope and the
// evidence are part of that account and a caller that has to go to SQL for them
// is a caller that will.
func TestChallengeReadsBackTheChallengeThatInertedTheTask(t *testing.T) {
	store, svc, ctx := newTriageService(t)
	task := newTriageTask(t, store, svc, ctx, "work-read")
	evidenceID := domain.NewID("evidence")

	if _, err := svc.ChallengeTaskIfInStates(ctx, task.ID,
		[]domain.TaskState{domain.TaskEligible, domain.TaskExecuting},
		domain.ChallengeTask, "superseded-by:2026-09-28T11:00:00Z",
		[]domain.ID{evidenceID}); err != nil {
		t.Fatal(err)
	}

	challenge, found, err := svc.Challenge(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("a challenged task reported no challenge")
	}
	if challenge.TaskID != task.ID {
		t.Fatalf("task id = %q, want %q", challenge.TaskID, task.ID)
	}
	if challenge.Scope != domain.ChallengeTask {
		t.Fatalf("scope = %q, want TASK", challenge.Scope)
	}
	if challenge.Reason != "superseded-by:2026-09-28T11:00:00Z" {
		t.Fatalf("reason = %q", challenge.Reason)
	}
	if len(challenge.EvidenceIDs) != 1 || challenge.EvidenceIDs[0] != evidenceID {
		t.Fatalf("evidence = %v, want [%s]", challenge.EvidenceIDs, evidenceID)
	}
	if challenge.CreatedAt.IsZero() {
		t.Fatal("created_at was not read back")
	}
}

// Challenge rows are never rewritten, so a task challenged twice has two of them
// and only the later one explains the state it is in now. The clock is advanced
// between the two so "later" is a fact about time rather than about the tiebreak.
func TestChallengeReturnsTheMostRecentOfSeveral(t *testing.T) {
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC))
	svc := execution.New(store, clk, purpose.New(store, clk))
	ctx := context.Background()
	task := newTriageTask(t, store, svc, ctx, "work-recent")
	if err := svc.ChallengeTask(ctx, task.ID, domain.ChallengeTask, "first", nil); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second)
	evidenceID := domain.NewID("evidence")
	if err := svc.ChallengeTask(ctx, task.ID, domain.ChallengeGoal, "second", []domain.ID{evidenceID}); err != nil {
		t.Fatal(err)
	}

	challenge, found, err := svc.Challenge(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("a challenged task reported no challenge")
	}
	if challenge.Reason != "second" || challenge.Scope != domain.ChallengeGoal {
		t.Fatalf("challenge = %+v, want the later one", challenge)
	}
	if len(challenge.EvidenceIDs) != 1 || challenge.EvidenceIDs[0] != evidenceID {
		t.Fatalf("evidence = %v, want [%s]", challenge.EvidenceIDs, evidenceID)
	}
	// Both rows are still there. The accessor answers a question about the state
	// a task is in; it is not a way of discarding what it did before.
	if rows := countRows(t, store, ctx, `SELECT count(*) FROM task_challenges WHERE task_id = ?`, task.ID); rows != 2 {
		t.Fatalf("challenge rows = %d, want 2", rows)
	}
}

// Two challenges written inside the same instant are ordered by challenge ID
// rather than by insertion, because the store has no insertion order to order by.
// The IDs are random, so that tie is arbitrary - it is pinned here only so it is
// a chosen tie and not an accident: the answer is one of the task's own
// challenges, and the same task always answers the same way.
func TestChallengeOrdersTwoChallengesOfOneInstantDeterministically(t *testing.T) {
	store, svc, ctx := newTriageService(t)
	task := newTriageTask(t, store, svc, ctx, "work-tie")
	for _, reason := range []string{"first", "second"} {
		if err := svc.ChallengeTask(ctx, task.ID, domain.ChallengeTask, reason, nil); err != nil {
			t.Fatal(err)
		}
	}

	first, found, err := svc.Challenge(ctx, task.ID)
	if err != nil || !found {
		t.Fatalf("Challenge = %+v, %v, %v; want one challenge", first, found, err)
	}
	second, found, err := svc.Challenge(ctx, task.ID)
	if err != nil || !found {
		t.Fatalf("Challenge = %+v, %v, %v; want one challenge", second, found, err)
	}
	if first.ID != second.ID {
		t.Fatalf("the same task answered %s and then %s, want the same challenge", first.ID, second.ID)
	}
	if first.Reason != "first" && first.Reason != "second" {
		t.Fatalf("reason = %q, want one of the two challenges recorded", first.Reason)
	}
	// Whichever one it picks, it picks the greatest challenge ID, which is the
	// order the query asks for and the only tiebreak a caller can rely on.
	var greatest string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT max(challenge_id) FROM task_challenges WHERE task_id = ?`, task.ID).Scan(&greatest); err != nil {
		t.Fatal(err)
	}
	if string(first.ID) != greatest {
		t.Fatalf("challenge = %s, want the greatest challenge ID %s", first.ID, greatest)
	}
}

// The not-found and the refused cases, which a caller has to be able to tell
// apart: a task nobody challenged is a legitimate answer, a blank task ID is a
// programming error, and an unconfigured service is neither.
func TestChallengeOnATaskNobodyChallengedAndOnBadInput(t *testing.T) {
	store, svc, ctx := newTriageService(t)
	task := newTriageTask(t, store, svc, ctx, "work-unchallenged")

	challenge, found, err := svc.Challenge(ctx, task.ID)
	if err != nil || found {
		t.Fatalf("Challenge = %+v, %v, %v; want no challenge and no error", challenge, found, err)
	}
	if _, _, err := svc.Challenge(ctx, "   "); err == nil {
		t.Fatal("a blank task id was accepted")
	}
	var missing *execution.Service
	if _, _, err := missing.Challenge(ctx, task.ID); err == nil {
		t.Fatal("an unconfigured service answered a challenge lookup")
	}
	if _, _, err := (&execution.Service{}).Challenge(ctx, task.ID); err == nil {
		t.Fatal("a service with no store answered a challenge lookup")
	}
}

// A challenge whose evidence column is not a JSON array of strings is reported,
// not read as "recorded against nothing": a caller deciding whether it may close
// a case on this challenge would otherwise treat a corrupted column as licence to
// cite no evidence at all.
func TestChallengeReportsEvidenceItCannotDecode(t *testing.T) {
	store, svc, ctx := newTriageService(t)
	task := newTriageTask(t, store, svc, ctx, "work-corrupt")
	if err := svc.ChallengeTask(ctx, task.ID, domain.ChallengeTask, "superseded", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE task_challenges SET evidence_ids_json = 'not json' WHERE task_id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, found, err := svc.Challenge(ctx, task.ID); err == nil || found {
		t.Fatalf("Challenge = %v, %v; want an error and no challenge", found, err)
	}
}

// A challenge raised with no evidence is a challenge that exists and named
// nothing, and a caller deciding whether it may close a case on this challenge
// has to see that difference: a challenge is there to be read, and the absence
// of evidence in it is a reason not to act rather than a reason to read nothing.
func TestChallengeWithNoEvidenceIsAnEmptyList(t *testing.T) {
	store, svc, ctx := newTriageService(t)
	task := newTriageTask(t, store, svc, ctx, "work-bare")
	if err := svc.ChallengeTask(ctx, task.ID, domain.ChallengeTask, "stopped", nil); err != nil {
		t.Fatal(err)
	}
	challenge, found, err := svc.Challenge(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("a challenged task reported no challenge")
	}
	if len(challenge.EvidenceIDs) != 0 {
		t.Fatalf("evidence = %v, want none", challenge.EvidenceIDs)
	}
}
