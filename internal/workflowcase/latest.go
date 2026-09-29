package workflowcase

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

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
