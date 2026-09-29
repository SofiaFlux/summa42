package workflowcase

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/workflow"
)

type AssessmentRequest struct {
	CaseID, WorkID        domain.ID
	Assessment            workflow.Assessment
	RemainingBudget       int64
	ProgressSignature     string
	RequireLatestRevision bool `json:"RequireLatestRevision,omitempty"`
}

type AssessmentResult struct {
	Decision workflow.Decision
	Case     Case
}

func (s *Service) Assess(ctx context.Context, request AssessmentRequest) (AssessmentResult, error) {
	if s == nil || s.store == nil || s.clock == nil || s.purposes == nil {
		return AssessmentResult{}, errors.New("workflow case service is not configured")
	}
	if strings.TrimSpace(string(request.CaseID)) == "" || strings.TrimSpace(string(request.WorkID)) == "" {
		return AssessmentResult{}, errors.New("case and work IDs are required")
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return AssessmentResult{}, fmt.Errorf("encode assessment request: %w", err)
	}
	var result AssessmentResult
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var storedRequest, storedResult string
		err := tx.QueryRowContext(ctx, `SELECT request_json, result_json FROM workflow_assessments WHERE case_id = ? AND work_id = ?`, request.CaseID, request.WorkID).Scan(&storedRequest, &storedResult)
		if err == nil {
			if storedRequest != string(requestJSON) {
				return errors.New("work already assessed with a different request")
			}
			return json.Unmarshal([]byte(storedResult), &result)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		row := tx.QueryRowContext(ctx, `SELECT case_id, mission_id, source, object_id, revision_id,
			observation_evidence_id, state, current_work_id, next_work_json, grant_json,
			completed_steps, max_steps, remaining_budget, progress_signature, initial_request_json
			FROM workflow_cases WHERE case_id = ?`, request.CaseID)
		current, _, err := scanCase(row)
		if err != nil {
			return err
		}
		if current.State != Active || current.CurrentWorkID != request.WorkID {
			return errors.New("work is not the active work for this case")
		}
		if request.RequireLatestRevision {
			if err := requireLatestRevisionTx(ctx, tx, current); err != nil {
				return err
			}
		}
		if err := s.purposes.ValidatePurposeTx(ctx, tx, domain.PurposeRef{Kind: domain.PurposeMission, ID: current.MissionID}); err != nil {
			return err
		}
		if request.RemainingBudget < 0 || request.RemainingBudget > current.RemainingBudget {
			return errors.New("remaining budget must be nonnegative and cannot increase")
		}
		decision, err := workflow.Decide(workflow.Input{
			Assessment: request.Assessment, Grant: current.Grant,
			Limits:                    workflow.Limits{MaxSteps: current.MaxSteps, RemainingBudget: request.RemainingBudget},
			CompletedSteps:            current.CompletedSteps + 1,
			ProgressSignature:         request.ProgressSignature,
			PreviousProgressSignature: current.ProgressSignature,
		})
		if err != nil {
			return err
		}
		current.CompletedSteps++
		current.RemainingBudget = request.RemainingBudget
		current.ProgressSignature = request.ProgressSignature
		workJSON := "{}"
		switch decision.Outcome {
		case workflow.OutcomeContinue:
			current.State = Active
			current.CurrentWorkID = domain.NewID("work")
			current.NextWork = *decision.Next
			encoded, err := json.Marshal(current.NextWork)
			if err != nil {
				return err
			}
			workJSON = string(encoded)
		case workflow.OutcomeBlocked:
			current.State = Blocked
			current.CurrentWorkID = ""
			current.NextWork = workflow.WorkProposal{}
		case workflow.OutcomeReady:
			current.State = ReadyForVerification
			current.CurrentWorkID = ""
			current.NextWork = workflow.WorkProposal{}
		default:
			return fmt.Errorf("unexpected workflow outcome %q", decision.Outcome)
		}
		result = AssessmentResult{Decision: decision, Case: current}
		resultJSON, err := json.Marshal(result)
		if err != nil {
			return err
		}
		now := s.clock.Now().UTC().Format(time.RFC3339Nano)
		_, err = tx.ExecContext(ctx, `UPDATE workflow_cases SET state=?, current_work_id=?, next_work_json=?, completed_steps=?, remaining_budget=?, progress_signature=?, updated_at=? WHERE case_id=?`,
			current.State, current.CurrentWorkID, workJSON, current.CompletedSteps, current.RemainingBudget, current.ProgressSignature, now, current.ID)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO workflow_assessments (assessment_id, case_id, work_id, request_json, result_json, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			domain.NewID("assessment"), current.ID, request.WorkID, string(requestJSON), string(resultJSON), now)
		return err
	})
	if err != nil {
		return AssessmentResult{}, fmt.Errorf("assess workflow case: %w", err)
	}
	return result, nil
}
