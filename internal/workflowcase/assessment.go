package workflowcase

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/workflow"
)

type AssessmentRequest struct {
	CaseID, WorkID        domain.ID
	Assessment            workflow.Assessment
	RemainingBudget       int64
	ProgressSignature     string
	RequireLatestRevision bool `json:"RequireLatestRevision,omitempty"`
	// PlanReviewVerdict is the typed plan-review verdict ("ACCEPT") that authorizes
	// implementation. Rows written before this field existed carry only the reason
	// prefix, which readers still accept when the field is absent.
	PlanReviewVerdict string `json:"PlanReviewVerdict,omitempty"`
	Phase             Phase  `json:"Phase,omitempty"`
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

		row := tx.QueryRowContext(ctx, `SELECT `+caseColumns+` FROM workflow_cases WHERE case_id = ?`, request.CaseID)
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
		for _, id := range request.Assessment.EvidenceIDs {
			var found int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM evidence_objects WHERE evidence_id = ?`, id).Scan(&found); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("assessment cites unknown evidence %q", id)
				}
				return err
			}
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
			if request.Phase != "" {
				if !isValidPhase(request.Phase) || !isPhaseCompatibleWithState(request.Phase, Active) {
					return fmt.Errorf("phase %q is invalid or incompatible with active case", request.Phase)
				}
				current.Phase = request.Phase
			} else if isImplementationKind(current.NextWork.Kind) {
				current.Phase = PhaseImplementationPending
			} else {
				current.Phase = PhasePlanningPending
			}
			encoded, err := json.Marshal(current.NextWork)
			if err != nil {
				return err
			}
			workJSON = string(encoded)
		case workflow.OutcomeBlocked:
			current.State = Blocked
			current.CurrentWorkID = ""
			current.NextWork = workflow.WorkProposal{}
			blockedReason := decision.Reason
			if blockedReason == "" {
				blockedReason = request.Assessment.Reason
			}
			if request.Phase != "" {
				if !isValidPhase(request.Phase) || !isPhaseCompatibleWithState(request.Phase, Blocked) {
					return fmt.Errorf("phase %q is invalid or incompatible with blocked case", request.Phase)
				}
				current.Phase = request.Phase
			} else if isDirectiveReason(blockedReason) {
				current.Phase = PhaseBlockedWaitingDirective
			} else if isContextReason(blockedReason) {
				current.Phase = PhaseBlockedWaitingContext
			} else {
				current.Phase = PhaseBlocked
			}
		case workflow.OutcomeReady:
			current.State = ReadyForVerification
			current.Phase = PhaseReadyForVerification
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
		now := domain.FormatTime(s.clock.Now())
		_, err = tx.ExecContext(ctx, `UPDATE workflow_cases SET state=?, phase=?, current_work_id=?, next_work_json=?, completed_steps=?, remaining_budget=?, progress_signature=?, updated_at=? WHERE case_id=?`,
			current.State, current.Phase, current.CurrentWorkID, workJSON, current.CompletedSteps, current.RemainingBudget, current.ProgressSignature, now, current.ID)
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
