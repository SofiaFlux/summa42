package workflowcase

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Summary is a case with what a list needs beyond the case itself.
type Summary struct {
	Case                 Case
	UpdatedAt            time.Time
	LastAssessmentReason string // empty when the case was never assessed
}

// ListSummaries returns every case, or every case in one state, most recently
// updated first, each with the reason of its latest assessment. It only reads.
func (s *Service) ListSummaries(ctx context.Context, state State) ([]Summary, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("workflow case service is not configured")
	}
	query := `SELECT ` + caseColumns + `, updated_at,
		(SELECT result_json FROM workflow_assessments a WHERE a.case_id = workflow_cases.case_id
		 ORDER BY a.created_at DESC, a.rowid DESC LIMIT 1)
		FROM workflow_cases`
	var args []any
	if state != "" {
		query += ` WHERE state = ?`
		args = append(args, string(state))
	}
	query += ` ORDER BY updated_at DESC, case_id`
	rows, err := s.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list workflow case summaries: %w", err)
	}
	defer rows.Close()
	summaries := make([]Summary, 0)
	for rows.Next() {
		var updatedAt string
		var lastResult sql.NullString
		c, err := scanCaseWith(rows, &updatedAt, &lastResult)
		if err != nil {
			return nil, fmt.Errorf("scan workflow case summary: %w", err)
		}
		summary := Summary{Case: c}
		if summary.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
			return nil, fmt.Errorf("parse workflow case %s updated_at: %w", c.ID, err)
		}
		if lastResult.Valid {
			var result AssessmentResult
			if err := json.Unmarshal([]byte(lastResult.String), &result); err != nil {
				return nil, fmt.Errorf("decode last assessment of case %s: %w", c.ID, err)
			}
			summary.LastAssessmentReason = result.Decision.Reason
		}
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list workflow case summaries: %w", err)
	}
	return summaries, nil
}
