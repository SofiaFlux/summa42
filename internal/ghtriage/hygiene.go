package ghtriage

import (
	"context"
	"fmt"

	"github.com/SofiaFlux/summa42/internal/containment"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/repoworkspace"
	"github.com/SofiaFlux/summa42/internal/texthygiene"
)

// SignalReviewerDirectedInstruction names the containment signal emitted when
// producer output trips texthygiene.RoleDirectiveRules.
// This is a tripwire, not a proof (card M9-T8).
const SignalReviewerDirectedInstruction = "reviewer_directed_instruction"

// ContainmentSignal records deterministic output-hygiene tripwire findings as
// rogue signals without depending directly on concrete containment storage.
type ContainmentSignal interface {
	ApplySignal(ctx context.Context, attemptID, taskID domain.ID, signalName, enforcedBy string, step containment.LadderStep, details map[string]any) error
}

func scanPlanHygiene(plan Plan) []texthygiene.DirectiveFinding {
	var findings []texthygiene.DirectiveFinding
	findings = append(findings, texthygiene.ScanRoleDirectives("plan.summary", plan.Summary)...)
	for i, step := range plan.Steps {
		findings = append(findings, texthygiene.ScanRoleDirectives(fmt.Sprintf("plan.steps[%d].objective", i), step.Objective)...)
		findings = append(findings, texthygiene.ScanRoleDirectives(fmt.Sprintf("plan.steps[%d].done_when", i), step.DoneWhen)...)
	}
	return findings
}

func scanImplementationHygiene(plan Plan, impl ImplementationRecord, changes []repoworkspace.FileChange) []texthygiene.DirectiveFinding {
	findings := scanPlanHygiene(plan)
	findings = append(findings, texthygiene.ScanRoleDirectives("implementation.summary", impl.Summary)...)
	for i, cmd := range impl.Commands {
		findings = append(findings, texthygiene.ScanRoleDirectives(fmt.Sprintf("implementation.commands[%d].output", i), cmd.Output)...)
	}
	for _, change := range changes {
		if change.After != nil {
			findings = append(findings, texthygiene.ScanRoleDirectives("changes["+change.Path+"]", change.After.Content)...)
		}
	}
	return findings
}

func emitHygieneSignal(ctx context.Context, emitter ContainmentSignal, attemptID, taskID domain.ID, stage string, findings []texthygiene.DirectiveFinding) error {
	if emitter == nil || len(findings) == 0 || attemptID == "" || taskID == "" {
		return nil
	}
	return emitter.ApplySignal(ctx, attemptID, taskID, SignalReviewerDirectedInstruction, "output_hygiene", containment.StepWarn, map[string]any{
		"stage":    stage,
		"findings": findings,
	})
}
