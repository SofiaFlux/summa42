// Package casereport builds the per-case report of the development plan (card
// M1-T3) from what the Box recorded: the case, its assessments, the evidence
// they cite and the model call ledger. It only reads.
//
// It reports what those records prove and says what it leaves out (Notes).
package casereport

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

// OwnerEvidencePrefix marks evidence an Owner action wrote, for example a
// manual block (summa42.owner.case_block).
const OwnerEvidencePrefix = "summa42.owner."

type Report struct {
	CaseID         string    `json:"case_id"`
	Source         string    `json:"source"`
	Object         string    `json:"object"`
	Revision       string    `json:"revision"`
	State          string    `json:"state"`
	Phase          string    `json:"phase,omitempty"`
	CompletedSteps int       `json:"completed_steps"`
	MaxSteps       int       `json:"max_steps"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	// WallSeconds is from the creation of the case to its last update.
	WallSeconds   float64       `json:"wall_seconds"`
	Stages        []Stage       `json:"stages"`
	ModelCalls    []ModelUsage  `json:"model_calls"`
	ModelTotal    ModelUsage    `json:"model_total"`
	Interventions Interventions `json:"owner_interventions"`
	Notes         []string      `json:"notes"`
}

// Stage is one assessed piece of work of the case, in the order assessed.
type Stage struct {
	WorkID     string    `json:"work_id"`
	Verdict    string    `json:"verdict"`
	Outcome    string    `json:"outcome"`
	Reason     string    `json:"reason"`
	NextKind   string    `json:"next_work_kind,omitempty"`
	AssessedAt time.Time `json:"assessed_at"`
	// Seconds since the previous assessment, or since the case was created.
	Seconds  float64    `json:"seconds"`
	Evidence []Evidence `json:"evidence"`
}

type Evidence struct {
	ID          string `json:"evidence_id"`
	Kind        string `json:"kind"`
	ContentHash string `json:"content_hash"`
}

// ModelUsage sums the model call ledger for one stage name, or for the case.
type ModelUsage struct {
	Stage  string `json:"stage,omitempty"`
	Calls  int    `json:"calls"`
	OK     int    `json:"ok"`
	Failed int    `json:"failed"`
	// Retries are calls beyond the first of a stage.
	Retries         int     `json:"retries"`
	DurationSeconds float64 `json:"duration_seconds"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	// CallsWithoutTokens is how many calls reported no token count; the
	// token sums cover the other calls only.
	CallsWithoutTokens int      `json:"calls_without_tokens"`
	Models             []string `json:"models"`
}

type Interventions struct {
	// OwnerEvidence counts cited evidence whose kind starts with
	// summa42.owner., by kind.
	OwnerEvidence map[string]int `json:"owner_evidence"`
	// Approvals and Rejections count the Owner's decisions on external
	// operations of this case's Tasks (for example a publication).
	Approvals  int `json:"approvals"`
	Rejections int `json:"rejections"`
	Total      int `json:"total"`
}

// Build reads the report of one case.
func Build(ctx context.Context, store *state.Store, cases *workflowcase.Service, caseID domain.ID) (Report, error) {
	if store == nil || cases == nil {
		return Report{}, errors.New("case report needs the store and the case service")
	}
	c, err := cases.Get(ctx, caseID)
	if err != nil {
		return Report{}, err
	}
	report := Report{
		CaseID: string(c.ID), Source: c.Source, Object: c.ObjectID, Revision: c.RevisionID,
		State: string(c.State), Phase: string(c.Phase), CompletedSteps: c.CompletedSteps, MaxSteps: c.MaxSteps,
		Stages: []Stage{}, ModelCalls: []ModelUsage{},
		Interventions: Interventions{OwnerEvidence: map[string]int{}},
	}
	var createdAt, updatedAt string
	if err := store.DB().QueryRowContext(ctx, `SELECT created_at, updated_at FROM workflow_cases WHERE case_id = ?`, c.ID).Scan(&createdAt, &updatedAt); err != nil {
		return Report{}, fmt.Errorf("read case times: %w", err)
	}
	if report.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Report{}, fmt.Errorf("parse case created_at: %w", err)
	}
	if report.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return Report{}, fmt.Errorf("parse case updated_at: %w", err)
	}
	report.WallSeconds = report.UpdatedAt.Sub(report.CreatedAt).Seconds()

	assessments, err := cases.ListAssessments(ctx, c.ID)
	if err != nil {
		return Report{}, err
	}
	previous := report.CreatedAt
	for _, record := range assessments {
		var request workflowcase.AssessmentRequest
		if err := json.Unmarshal([]byte(record.RequestJSON), &request); err != nil {
			return Report{}, fmt.Errorf("decode assessment %s request: %w", record.ID, err)
		}
		var result workflowcase.AssessmentResult
		if err := json.Unmarshal([]byte(record.ResultJSON), &result); err != nil {
			return Report{}, fmt.Errorf("decode assessment %s result: %w", record.ID, err)
		}
		stage := Stage{
			WorkID: record.WorkID, Verdict: string(request.Assessment.Verdict), Outcome: string(result.Decision.Outcome),
			Reason: result.Decision.Reason, AssessedAt: record.CreatedAt, Seconds: record.CreatedAt.Sub(previous).Seconds(),
			Evidence: []Evidence{},
		}
		if stage.Reason == "" {
			stage.Reason = request.Assessment.Reason
		}
		if result.Decision.Next != nil {
			stage.NextKind = result.Decision.Next.Kind
		}
		for _, id := range request.Assessment.EvidenceIDs {
			item := Evidence{ID: id}
			err := store.DB().QueryRowContext(ctx, `SELECT kind, content_hash FROM evidence_objects WHERE evidence_id = ?`, id).Scan(&item.Kind, &item.ContentHash)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return Report{}, fmt.Errorf("read evidence %s: %w", id, err)
			}
			if strings.HasPrefix(item.Kind, OwnerEvidencePrefix) {
				report.Interventions.OwnerEvidence[item.Kind]++
				report.Interventions.Total++
			}
			stage.Evidence = append(stage.Evidence, item)
		}
		report.Stages = append(report.Stages, stage)
		previous = record.CreatedAt
	}

	// A Task of this case is keyed by the work it was made for: the work ID
	// itself, or "<stage>:<work ID>". Work IDs are random, so the suffix
	// match cannot pick up another case's Task.
	workIDs := map[string]bool{}
	if c.CurrentWorkID != "" {
		workIDs[string(c.CurrentWorkID)] = true
	}
	for _, stage := range report.Stages {
		workIDs[stage.WorkID] = true
	}
	for workID := range workIDs {
		rows, err := store.DB().QueryContext(ctx, `
			SELECT d.decision_action, COUNT(*)
			FROM tasks t
			JOIN external_operations o ON o.task_id = t.task_id
			JOIN approval_decisions d ON d.approval_id = o.approval_id
			WHERE t.idempotency_key = ? OR t.idempotency_key LIKE '%:' || ?
			GROUP BY d.decision_action`, workID, workID)
		if err != nil {
			return Report{}, fmt.Errorf("read approval decisions: %w", err)
		}
		for rows.Next() {
			var action string
			var count int
			if err := rows.Scan(&action, &count); err != nil {
				rows.Close()
				return Report{}, err
			}
			if action == "APPROVE" {
				report.Interventions.Approvals += count
			} else {
				report.Interventions.Rejections += count
			}
			report.Interventions.Total += count
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return Report{}, err
		}
	}

	if err := readModelCalls(ctx, store, &report); err != nil {
		return Report{}, err
	}
	report.Notes = []string{
		"Owner interventions are the Owner's approvals and rejections of external operations of this case's Tasks, and evidence of kind " + OwnerEvidencePrefix + "* that an assessment of this case cites (for example a manual block). Cancels, restarts and anything the Owner did outside the Box are not counted.",
		"Model calls come from the model call ledger for this case ID. Token sums cover only the calls that reported tokens.",
		"Not in this report yet: plan hash, candidate commit and pull request reference as named fields (the evidence of each stage is listed with its kind and content hash), and wall time of Attempts.",
	}
	return report, nil
}

func readModelCalls(ctx context.Context, store *state.Store, report *Report) error {
	rows, err := store.DB().QueryContext(ctx,
		`SELECT stage, model, outcome, duration_ms, input_tokens, output_tokens FROM model_calls WHERE case_id = ? ORDER BY call_id`, report.CaseID)
	if err != nil {
		return fmt.Errorf("read model calls: %w", err)
	}
	defer rows.Close()
	byStage := map[string]*ModelUsage{}
	models := map[string]map[string]bool{"": {}}
	var order []string
	for rows.Next() {
		var stage, model, outcome string
		var durationMS int64
		var input, output sql.NullInt64
		if err := rows.Scan(&stage, &model, &outcome, &durationMS, &input, &output); err != nil {
			return fmt.Errorf("scan model call: %w", err)
		}
		usage := byStage[stage]
		if usage == nil {
			usage = &ModelUsage{Stage: stage}
			byStage[stage] = usage
			models[stage] = map[string]bool{}
			order = append(order, stage)
		}
		for _, sum := range []*ModelUsage{usage, &report.ModelTotal} {
			sum.Calls++
			if outcome == "ok" {
				sum.OK++
			} else {
				sum.Failed++
			}
			sum.DurationSeconds += float64(durationMS) / 1000
			if input.Valid || output.Valid {
				sum.InputTokens += input.Int64
				sum.OutputTokens += output.Int64
			} else {
				sum.CallsWithoutTokens++
			}
		}
		models[stage][model] = true
		models[""][model] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read model calls: %w", err)
	}
	names := func(set map[string]bool) []string {
		list := make([]string, 0, len(set))
		for name := range set {
			list = append(list, name)
		}
		sort.Strings(list)
		return list
	}
	for _, stage := range order {
		usage := byStage[stage]
		usage.Retries = usage.Calls - 1
		usage.Models = names(models[stage])
		report.ModelTotal.Retries += usage.Retries
		report.ModelCalls = append(report.ModelCalls, *usage)
	}
	report.ModelTotal.Models = names(models[""])
	return nil
}
