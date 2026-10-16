package workflowcase

import (
	"database/sql"
	"testing"
)

func TestCurrentWorkGuardRejectsSupersededRevision(t *testing.T) {
	svc, _, missionID, ctx := setupEnsure(t)
	initial := sampleObservation(missionID)
	initial.RevisionID = "2026-09-29T10:00:00Z"
	c, err := svc.Ensure(ctx, initial)
	if err != nil {
		t.Fatal(err)
	}
	err = svc.store.WithTx(ctx, func(tx *sql.Tx) error { _, err := svc.GuardCurrentWork(ctx, tx, c.ID, c.CurrentWorkID); return err })
	if err != nil {
		t.Fatal(err)
	}
	obs := sampleObservation(c.MissionID)
	obs.RevisionID = "2026-09-30T10:00:00Z"
	if _, err := svc.Ensure(ctx, obs); err != nil {
		t.Fatal(err)
	}
	err = svc.store.WithTx(ctx, func(tx *sql.Tx) error { _, err := svc.GuardCurrentWork(ctx, tx, c.ID, c.CurrentWorkID); return err })
	if err == nil {
		t.Fatal("superseded work authorized")
	}
}
