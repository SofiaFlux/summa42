package ghtriage_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

// The claim the interface makes - two concurrent ticks may both pay for a model
// call, but only one verdict is linked and reported - is carried by the table's
// primary key and by the ON CONFLICT clause of the insert, and neither is
// observable from the reviewer's own tests: a table with no key accepts the
// second row silently, and a caller that ignores the bool cannot tell. So it is
// pinned here, on the index alone, where the two are the only thing under test.
func TestLinkReviewKeepsOneVerdictPerDecisionVersionAndState(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC))
	evidenceStore, err := evidence.New(store, t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	decision := putIndexEvidence(t, ctx, evidenceStore, ghtriage.KindDecision, "a decision")
	first := putIndexEvidence(t, ctx, evidenceStore, ghtriage.KindReview, "the first verdict")
	second := putIndexEvidence(t, ctx, evidenceStore, ghtriage.KindReview, "the second verdict")
	index := ghtriage.NewReviewIndex(store, clk)

	linked, err := index.LinkReview(ctx, decision, ghtriage.ReviewerVersion, "ACTIVE|none", first)
	if err != nil {
		t.Fatal(err)
	}
	if !linked {
		t.Fatal("the first link reported that it stored nothing")
	}
	if linked, err := index.ReviewLinked(ctx, decision, ghtriage.ReviewerVersion, "ACTIVE|none"); err != nil {
		t.Fatal(err)
	} else if !linked {
		t.Fatal("the first link is not visible to the lookup that skips the work")
	}

	// The same key, a different verdict document: this is the loser's row. A
	// table without the primary key takes it, and the index then names two
	// verdicts for one decision in one state; an insert without ON CONFLICT
	// refuses it with a constraint error, and the loser is a per-case failure the
	// operator reads for ever.
	clk.Advance(time.Second)
	linked, err = index.LinkReview(ctx, decision, ghtriage.ReviewerVersion, "ACTIVE|none", second)
	if err != nil {
		t.Fatal(err)
	}
	if linked {
		t.Fatal("a second link on the same decision, version and state reported that it stored a row")
	}
	if got := reviewRowCount(t, ctx, store); got != 1 {
		t.Fatalf("review index rows = %d, want 1", got)
	}
	if got := verdictOf(t, ctx, store, decision, ghtriage.ReviewerVersion, "ACTIVE|none"); got != string(first) {
		t.Fatalf("linked verdict = %s, want the winner's %s: the loser's document must not replace it", got, first)
	}

	// A different observed state is a different key, so the same decision is
	// reviewable again once the case has moved. Without that, a case that
	// changed state would never be looked at twice.
	clk.Advance(time.Second)
	linked, err = index.LinkReview(ctx, decision, ghtriage.ReviewerVersion, "BLOCKED|assessment_1", second)
	if err != nil {
		t.Fatal(err)
	}
	if !linked {
		t.Fatal("a link on a new state reported that it stored nothing")
	}
	if got := reviewRowCount(t, ctx, store); got != 2 {
		t.Fatalf("review index rows = %d, want 2: one per observed state", got)
	}

	// The third column of the key, and the only thing that lets a changed
	// reviewer look at a decision again. Without it in the key this link lands on
	// the first row and reports false for ever, and the failure is a permanent
	// silent skip rather than an error: the tick pays for the model call, writes
	// the verdict document, is told it stored nothing, and counts the case as
	// skipped on every tick until the end of the mission.
	clk.Advance(time.Second)
	const nextVersion = ghtriage.ReviewerVersion + "-next"
	linked, err = index.LinkReview(ctx, decision, nextVersion, "ACTIVE|none", second)
	if err != nil {
		t.Fatal(err)
	}
	if !linked {
		t.Fatal("a link under a new reviewer version reported that it stored nothing: " +
			"every later reviewer would skip this decision for ever")
	}
	if got := reviewRowCount(t, ctx, store); got != 3 {
		t.Fatalf("review index rows = %d, want 3: one per decision, version and state", got)
	}
	if linked, err := index.ReviewLinked(ctx, decision, nextVersion, "ACTIVE|none"); err != nil {
		t.Fatal(err)
	} else if !linked {
		t.Fatal("the new version's link is not visible to its own lookup")
	}
	// And the old version's row is still its own: the two versions are separate
	// keys, so a new reviewer does not evict what the previous one recorded.
	if got := verdictOf(t, ctx, store, decision, ghtriage.ReviewerVersion, "ACTIVE|none"); got != string(first) {
		t.Fatalf("linked verdict under the original version = %s, want %s", got, first)
	}
}

func putIndexEvidence(t *testing.T, ctx context.Context, store *evidence.Store, kind, body string) domain.ID {
	t.Helper()
	object, err := store.Put(ctx, strings.NewReader(body), evidence.Metadata{
		MediaType: ghtriage.DecisionMediaType, Kind: kind,
	})
	if err != nil {
		t.Fatal(err)
	}
	return object.ID
}

func reviewRowCount(t *testing.T, ctx context.Context, store *state.Store) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM github_issue_triage_reviews`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func verdictOf(t *testing.T, ctx context.Context, store *state.Store, decisionID domain.ID, reviewerVersion, fingerprint string) string {
	t.Helper()
	var id string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT verdict_evidence_id FROM github_issue_triage_reviews
		 WHERE decision_evidence_id = ? AND reviewer_version = ? AND state_fingerprint = ?`,
		decisionID, reviewerVersion, fingerprint).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
