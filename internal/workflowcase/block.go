package workflowcase

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/workflow"
)

// Block holds an ACTIVE case (BLOCKED) by assessing its current work as
// Unknown, so the hold is recorded like any stage decision. It is the Owner's
// manual stop, for example after an incident; evidenceID must already exist.
func (s *Service) Block(ctx context.Context, caseID domain.ID, reason string, evidenceID domain.ID) (Case, error) {
	if strings.TrimSpace(reason) == "" {
		return Case{}, errors.New("block reason is required")
	}
	c, err := s.Get(ctx, caseID)
	if err != nil {
		return Case{}, err
	}
	if c.State != Active {
		return Case{}, fmt.Errorf("case %s is %s, not ACTIVE", c.ID, c.State)
	}
	result, err := s.Assess(ctx, AssessmentRequest{
		CaseID: c.ID, WorkID: c.CurrentWorkID, RemainingBudget: c.RemainingBudget, ProgressSignature: c.ProgressSignature,
		Assessment: workflow.Assessment{Verdict: workflow.Unknown, Reason: reason, EvidenceIDs: []string{string(evidenceID)}},
	})
	if err != nil {
		return Case{}, err
	}
	return result.Case, nil
}
