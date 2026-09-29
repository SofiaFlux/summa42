package ghtriage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/SofiaFlux/summa42/internal/domain"
)

const (
	PlanSchema   = "github.issue.plan.v1"
	KindPlan     = "github.issue.plan"
	PlanQuestion = "Write a small, actionable implementation plan for this issue. " +
		"Return one JSON object with a summary and 1 to 8 steps. Each step needs " +
		"an objective and a verifiable done_when condition. Do not perform the work."
	PlanningAssessmentReason = "planning-started"
	PlanReviewWorkKind       = "github.issue.plan.review"
)

type PlanStep struct {
	Objective string `json:"objective"`
	DoneWhen  string `json:"done_when"`
}

type PlanOutput struct {
	Summary string     `json:"summary"`
	Steps   []PlanStep `json:"steps"`
}

func (p PlanOutput) Validate() error {
	if textLength(p.Summary, 1, 1000) != nil {
		return errors.New("plan summary must contain 1 to 1000 characters")
	}
	if len(p.Steps) < 1 || len(p.Steps) > 8 {
		return errors.New("plan must contain 1 to 8 steps")
	}
	for i, step := range p.Steps {
		if textLength(step.Objective, 1, 300) != nil || textLength(step.DoneWhen, 1, 1000) != nil {
			return fmt.Errorf("plan step %d needs a bounded objective and done_when", i+1)
		}
	}
	return nil
}

func textLength(value string, min, max int) error {
	n := utf8.RuneCountInString(strings.TrimSpace(value))
	if n < min || n > max {
		return errors.New("text length is outside the allowed range")
	}
	return nil
}

func ParsePlanOutput(raw []byte) (PlanOutput, error) {
	var output PlanOutput
	if err := decodeExactlyOne(raw, &output); err != nil {
		return PlanOutput{}, err
	}
	if err := output.Validate(); err != nil {
		return PlanOutput{}, err
	}
	return output, nil
}

type PlanInput struct {
	Schema             string    `json:"schema"`
	Question           string    `json:"question"`
	Snapshot           Snapshot  `json:"snapshot"`
	Decision           Decision  `json:"decision"`
	DecisionEvidenceID domain.ID `json:"decision_evidence_id"`
}

type PlanModel interface {
	Plan(context.Context, PlanInput) (PlanOutput, error)
}

// Plan is the durable evidence written before a ready-to-plan case is
// assessed. Its citations make a later reader independent of the model call.
type Plan struct {
	Schema             string    `json:"schema"`
	CaseID             domain.ID `json:"case_id"`
	DecisionEvidenceID domain.ID `json:"decision_evidence_id"`
	SnapshotEvidenceID domain.ID `json:"snapshot_evidence_id"`
	Repository         string    `json:"repository"`
	Issue              int64     `json:"issue"`
	Revision           string    `json:"revision"`
	PlanOutput
}

func (p Plan) Canonical() ([]byte, error) {
	if p.Schema != PlanSchema || p.CaseID == "" || p.DecisionEvidenceID == "" ||
		p.SnapshotEvidenceID == "" || strings.TrimSpace(p.Repository) == "" ||
		p.Issue <= 0 || strings.TrimSpace(p.Revision) == "" {
		return nil, errors.New("plan identity and citations are required")
	}
	if err := p.PlanOutput.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(p)
}
