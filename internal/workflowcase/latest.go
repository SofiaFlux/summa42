package workflowcase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

// ErrObjectOwnedByOtherMission refuses a new case for an external object that a
// different mission still holds in a non-CLOSED case.
var ErrObjectOwnedByOtherMission = errors.New("external object is owned by a case of another mission")

// requireObjectNotOwnedByOtherMissionTx keeps one live case per (source, object)
// across missions. A replay of an existing (mission, revision) case is allowed,
// and existing rows are never touched; only a NEW case is refused.
func requireObjectNotOwnedByOtherMissionTx(ctx context.Context, tx *sql.Tx, o Observation) error {
	var existing int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM workflow_cases
		WHERE mission_id = ? AND source = ? AND object_id = ? AND revision_id = ?`,
		o.MissionID, o.Source, o.ObjectID, o.RevisionID).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var owner, caseID domain.ID
	err = tx.QueryRowContext(ctx, `SELECT case_id, mission_id FROM workflow_cases
		WHERE mission_id <> ? AND source = ? AND object_id = ? AND state <> ? LIMIT 1`,
		o.MissionID, o.Source, o.ObjectID, Closed).Scan(&caseID, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: %s %s is held by case %s of mission %s", ErrObjectOwnedByOtherMission, o.Source, o.ObjectID, caseID, owner)
}

// requireLatestRevisionTx closes the registration race inside the transaction
// that changes a case or creates its next Task. Revisions are timestamps, not
// strings: RFC3339Nano's variable fractional precision reverses byte order.
func requireLatestRevisionTx(ctx context.Context, tx *sql.Tx, c Case) error {
	current, err := time.Parse(time.RFC3339Nano, c.RevisionID)
	if err != nil {
		return fmt.Errorf("parse case revision %q: %w", c.RevisionID, err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT revision_id FROM workflow_cases
		WHERE mission_id = ? AND source = ? AND object_id = ? AND case_id <> ?`,
		c.MissionID, c.Source, c.ObjectID, c.ID)
	if err != nil {
		return fmt.Errorf("read registered revisions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		other, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return fmt.Errorf("parse registered revision %q: %w", raw, err)
		}
		if other.After(current) {
			return fmt.Errorf("case %s was superseded by revision %s", c.ID, raw)
		}
	}
	return rows.Err()
}
