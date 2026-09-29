package ghtriage

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/testutil"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

// newGuardFixture writes one ACTIVE case with a current work, and the driver
// that would close it. Only the case service is wired, because block and
// applyDisposition reach nothing else: a fixture that also built a mission, an
// executor and an evidence store would be testing more machinery than the
// guards these two tests exist for.
func newGuardFixture(t *testing.T) (*Driver, workflowcase.Case) {
	t.Helper()
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC))
	now := clk.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO missions(mission_id, statement, active, created_at) VALUES ('mission-guards', 'triage issues', 1, ?)`,
		now,
	); err != nil {
		t.Fatal(err)
	}
	inserted, work := domain.NewID("case"), domain.NewID("work")
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO workflow_cases(case_id, mission_id, source, object_id, revision_id,
			observation_evidence_id, initial_request_json, grant_json, state, current_work_id,
			next_work_json, completed_steps, max_steps, remaining_budget, progress_signature,
			created_at, updated_at)
		 VALUES (?, 'mission-guards', 'github', 'o/r#42', '2026-09-28T10:00:00Z', ?,
			'{}', '{}', 'ACTIVE', ?, '{}', 0, 3, 10, '', ?, ?)`,
		inserted, domain.NewID("evidence"), work, now, now,
	); err != nil {
		t.Fatal(err)
	}
	cases := workflowcase.New(store, clk, purpose.New(store, clk))
	created, err := cases.Get(ctx, inserted)
	if err != nil {
		t.Fatal(err)
	}
	return NewDriver(cases, nil, nil, nil, nil, clk), created
}

// A blank evidence ID is refused before Assess is handed one. workflow.Decide
// rejects one, and the rejection would repeat identically on every tick with
// the case stuck ACTIVE and the task already changed, so the refusal is here
// rather than left to Decide. No driver path reaches it today - every caller
// supplies an evidence ID of its own, or refuses first - which is exactly why
// the black-box suite does not cover it and this one has to.
func TestBlockRefusesToCloseACaseWithoutAnEvidenceID(t *testing.T) {
	ctx := context.Background()
	driver, c := newGuardFixture(t)

	for _, blank := range []domain.ID{"", "   "} {
		blocked, err := driver.block(ctx, c, c.CurrentWorkID, "not-actionable", blank)
		if err == nil {
			t.Fatalf("block with evidence ID %q returned no error, blocked = %t", blank, blocked)
		}
		if blocked {
			t.Fatalf("block with evidence ID %q reported a transition it did not make", blank)
		}
		if !strings.Contains(err.Error(), string(c.ID)) {
			t.Fatalf("error = %q, want it to name the case it refused to close", err)
		}
		// The refusal has to leave the case exactly as it was: a guard that
		// reported the problem after half-assessing would be worse than silent.
		current, err := driver.cases.Get(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State != workflowcase.Active || current.CurrentWorkID != c.CurrentWorkID {
			t.Fatalf("case = %+v, want it untouched and still ACTIVE on %s", current, c.CurrentWorkID)
		}
	}
}

// Assessed is what a caller reads to learn that work was done, so it may only
// count transitions that happened. block short-circuits on a case that is no
// longer ACTIVE or whose current work is not the one the caller holds - a
// racing lease, a supersession that closed the case a moment earlier - and a
// counter that counted those anyway would report assessments the store does not
// contain, which is the same overclaim as the one the failure list used to make
// about a successful block.
func TestApplyDispositionCountsOnlyTheBlocksItPerformed(t *testing.T) {
	ctx := context.Background()
	driver, c := newGuardFixture(t)
	decision := Decision{Stage3: &Stage3Result{Disposition: DispositionNotActionable}}

	var first DriverResult
	if err := driver.applyDisposition(ctx, c, c.CurrentWorkID, decision, domain.NewID("evidence"), &first); err != nil {
		t.Fatal(err)
	}
	if first.Assessed != 1 {
		t.Fatalf("first disposition = %+v, want the assessment it performed", first)
	}

	// The case is BLOCKED now, on the work ID Assess cleared, so the same
	// request against the reloaded case is the short-circuit: no assessment
	// happens and none may be counted.
	closed, err := driver.cases.Get(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	var second DriverResult
	if err := driver.applyDisposition(ctx, closed, closed.CurrentWorkID, decision, domain.NewID("evidence"), &second); err != nil {
		t.Fatal(err)
	}
	if second.Assessed != 0 {
		t.Fatalf("second disposition = %+v, want no assessment counted for a case that was already closed", second)
	}
	if closed.State != workflowcase.Blocked {
		t.Fatalf("case state = %q, want BLOCKED", closed.State)
	}
}
