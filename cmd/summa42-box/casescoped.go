package main

import (
	"context"
	"database/sql"
	"errors"
	"log"

	"github.com/SofiaFlux/summa42/internal/capacity"
	"github.com/SofiaFlux/summa42/internal/executors"
)

// caseScopedExecutor puts the case a Task belongs to in the context of its
// attempt, so the model calls the executor makes are ledgered under that case.
// The worker's one adapter serves every case, and the Task carries only its
// own ID: a Task is keyed by the work it was made for, the case's work ID, which
// is how the case report finds a case's Tasks too.
type caseScopedExecutor struct {
	executors.Executor
	db *sql.DB
}

func (e caseScopedExecutor) ExecutionContract() executors.Contract {
	return executors.Describe("", e.Executor)
}

func (e caseScopedExecutor) Start(ctx context.Context, envelope executors.AttemptEnvelope) (executors.ExecutionResult, error) {
	var caseID string
	err := e.db.QueryRowContext(ctx, `
		SELECT c.case_id FROM tasks t
		JOIN workflow_cases c ON t.idempotency_key = c.current_work_id OR t.idempotency_key LIKE '%:' || c.current_work_id
		WHERE t.task_id = ? LIMIT 1`, envelope.TaskID).Scan(&caseID)
	if err == nil {
		ctx = capacity.WithCaseID(ctx, caseID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		// The ledger is best effort: an unlabelled call is better than a failed attempt.
		log.Printf("capacity ledger: find the case of task %s: %v", envelope.TaskID, err)
	}
	return e.Executor.Start(ctx, envelope)
}
