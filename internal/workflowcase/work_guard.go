package workflowcase

import (
	"context"
	"database/sql"
	"errors"
	"github.com/SofiaFlux/summa42/internal/domain"
)

// GuardCurrentWork checks active work, latest revision and purpose inside a caller transaction.
func (s *Service) GuardCurrentWork(ctx context.Context, tx *sql.Tx, caseID, workID domain.ID) (Case, error) {
	if s == nil || tx == nil {
		return Case{}, errors.New("case service and transaction required")
	}
	current, _, err := scanCase(tx.QueryRowContext(ctx, `SELECT case_id, mission_id, source, object_id, revision_id,
 observation_evidence_id, state, current_work_id, next_work_json, grant_json,
 completed_steps, max_steps, remaining_budget, progress_signature, initial_request_json
 FROM workflow_cases WHERE case_id=?`, caseID))
	if err != nil {
		return Case{}, err
	}
	if current.State != Active || current.CurrentWorkID != workID {
		return Case{}, errors.New("work is not active")
	}
	if err := requireLatestRevisionTx(ctx, tx, current); err != nil {
		return Case{}, err
	}
	if err := s.purposes.ValidatePurposeTx(ctx, tx, domain.PurposeRef{Kind: domain.PurposeMission, ID: current.MissionID}); err != nil {
		return Case{}, err
	}
	return current, nil
}
