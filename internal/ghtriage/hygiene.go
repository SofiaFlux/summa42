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

func appendBoundedFindings(dst []texthygiene.DirectiveFinding, more ...texthygiene.DirectiveFinding) []texthygiene.DirectiveFinding {
	for _, f := range more {
		if len(dst) >= texthygiene.MaxDirectiveFindings {
			return dst
		}
		dst = append(dst, f)
	}
	return dst
}

func scanPlanHygiene(plan Plan) []texthygiene.DirectiveFinding {
	var findings []texthygiene.DirectiveFinding
	findings = appendBoundedFindings(findings, texthygiene.ScanRoleDirectives("plan.summary", plan.Summary)...)
	for i, step := range plan.Steps {
		findings = appendBoundedFindings(findings, texthygiene.ScanRoleDirectives(fmt.Sprintf("plan.steps[%d].objective", i), step.Objective)...)
		findings = appendBoundedFindings(findings, texthygiene.ScanRoleDirectives(fmt.Sprintf("plan.steps[%d].done_when", i), step.DoneWhen)...)
	}
	return findings
}

// scanCodeAndPlanHygiene separates implementation-owned findings (which attribute
// rogue containment signals to the implementation attempt) from inherited plan
// findings, returning both capped at texthygiene.MaxDirectiveFindings for review.
func scanCodeAndPlanHygiene(plan Plan, impl ImplementationRecord, changes []repoworkspace.FileChange) (codeFindings, allFindings []texthygiene.DirectiveFinding) {
	codeFindings = appendBoundedFindings(codeFindings, texthygiene.ScanRoleDirectives("implementation.summary", impl.Summary)...)
	for i, cmd := range impl.Commands {
		codeFindings = appendBoundedFindings(codeFindings, texthygiene.ScanRoleDirectives(fmt.Sprintf("implementation.commands[%d].output", i), cmd.Output)...)
	}
	for _, change := range changes {
		if change.After != nil {
			codeFindings = appendBoundedFindings(codeFindings, texthygiene.ScanRoleDirectives("changes["+change.Path+"]", change.After.Content)...)
		}
	}
	allFindings = appendBoundedFindings(nil, codeFindings...)
	allFindings = appendBoundedFindings(allFindings, scanPlanHygiene(plan)...)
	return codeFindings, allFindings
}

func emitHygieneSignal(ctx context.Context, emitter ContainmentSignal, attemptID, taskID domain.ID, stage string, findings []texthygiene.DirectiveFinding, extra map[string]any) error {
	if emitter == nil || len(findings) == 0 || attemptID == "" || taskID == "" {
		return nil
	}
	details := map[string]any{
		"stage":    stage,
		"findings": findings,
	}
	for k, v := range extra {
		details[k] = v
	}
	return emitter.ApplySignal(ctx, attemptID, taskID, SignalReviewerDirectedInstruction, "output_hygiene", containment.StepWarn, details)
}
