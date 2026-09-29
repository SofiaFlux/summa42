package ghtriage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

// ReviewIndexStore schedules reviews by (decision, reviewer version, observed
// case state).
//
// The store answers the second of two ticks that reach the same decision in the
// same state with false for LinkReview and true for ReviewLinked, so the first
// verdict linked is the one both a reader and every later tick find; a loser
// that asks anyway is told nothing was stored and reports no verdict. That is
// what the schema guarantees: the primary key makes the second row impossible.
// Whether two ticks really do reach the link together is not exercised here - the
// reviewer's test drives it through an index that answers as the losing tick
// finds it - so this is a statement about the store's answers, not a measurement
// of the race.
type ReviewIndexStore interface {
	ReviewLinked(ctx context.Context, decisionID domain.ID, reviewerVersion, fingerprint string) (bool, error)
	LinkReview(ctx context.Context, decisionID domain.ID, reviewerVersion, fingerprint string, verdictID domain.ID) (bool, error)
}

type reviewIndex struct {
	store *state.Store
	clock clock.Clock
}

func NewReviewIndex(store *state.Store, clk clock.Clock) ReviewIndexStore {
	return &reviewIndex{store: store, clock: clk}
}

func (r *reviewIndex) ReviewLinked(ctx context.Context, decisionID domain.ID, reviewerVersion, fingerprint string) (bool, error) {
	if r == nil || r.store == nil {
		return false, errors.New("review index is not configured")
	}
	var one int
	err := r.store.DB().QueryRowContext(ctx,
		`SELECT 1 FROM github_issue_triage_reviews
		 WHERE decision_evidence_id = ? AND reviewer_version = ? AND state_fingerprint = ?`,
		decisionID, reviewerVersion, fingerprint).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up triage review link: %w", err)
	}
	return true, nil
}

func (r *reviewIndex) LinkReview(ctx context.Context, decisionID domain.ID, reviewerVersion, fingerprint string, verdictID domain.ID) (bool, error) {
	if r == nil || r.store == nil {
		return false, errors.New("review index is not configured")
	}
	now := r.clock.Now().UTC().Format(time.RFC3339Nano)
	result, err := r.store.DB().ExecContext(ctx,
		`INSERT INTO github_issue_triage_reviews
		 (decision_evidence_id, reviewer_version, state_fingerprint, verdict_evidence_id, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(decision_evidence_id, reviewer_version, state_fingerprint) DO NOTHING`,
		decisionID, reviewerVersion, fingerprint, verdictID, now)
	if err != nil {
		return false, fmt.Errorf("link triage review: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed == 1, nil
}
