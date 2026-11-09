package obligation

import (
	"context"

	"github.com/SofiaFlux/summa42/internal/scheduler"
)

// PlannerAdapter integrates obligation evaluation with the scheduler, guaranteeing that
// compliance and contractual obligations are evaluated, synthesized, and prioritized
// ahead of discretionary goals.
type PlannerAdapter struct {
	obligationSvc *Service
	schedulerSvc  *scheduler.Service
}

func NewPlannerAdapter(obSvc *Service, schedSvc *scheduler.Service) *PlannerAdapter {
	return &PlannerAdapter{
		obligationSvc: obSvc,
		schedulerSvc:  schedSvc,
	}
}

// PlanNext evaluates active obligations to ensure lead-time threshold tasks are synthesized,
// then queries the scheduler for the next candidate, ensuring obligations are not starved.
func (p *PlannerAdapter) PlanNext(ctx context.Context, capacity scheduler.CapacitySnapshot) (*scheduler.TaskCandidate, error) {
	if _, _, err := p.obligationSvc.EvaluateAndSchedule(ctx); err != nil {
		return nil, err
	}
	return p.schedulerSvc.Next(ctx, capacity)
}
