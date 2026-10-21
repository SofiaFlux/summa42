package ghtriage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"bytes"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/workflow"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

// maxTransientFailures caps timeout and model-exit failures per work before the case is held.
const maxTransientFailures = 3

// KindStageFailure is the evidence recorded when a stage holds its case after failures.
const KindStageFailure = "github.issue.stage.failure"

type parseError struct{ err error }

func (e *parseError) Error() string { return e.err.Error() }
func (e *parseError) Unwrap() error { return e.err }

// failureClass is one of timeout, model-exit, parse, validation, internal.
func failureClass(err error) string {
	var parse *parseError
	var f *planFailure
	var exit *exec.ExitError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &parse):
		return "parse"
	case errors.As(err, &f):
		return f.class
	case errors.As(err, &exit):
		return "model-exit"
	}
	return "internal"
}

func transientClass(class string) bool { return class == "timeout" || class == "model-exit" }

// failureSignature is stage:class:work. Transient classes get a "#n" suffix from failStage so
// execution.FailAttempt's same-signature block only trips for parse, validation and internal.
func failureSignature(stage string, workID domain.ID, err error) string {
	return stage + ":" + failureClass(err) + ":" + string(workID)
}

// failStage fails the attempt with a classified signature and holds the case once the task is
// blocked (stable classes repeating) or the transient cap is reached.
func failStage(ctx context.Context, cases *workflowcase.Service, ex *execution.Service, store *evidence.Store, c workflowcase.Case, taskID, attemptID domain.ID, stage string, cause error) error {
	class := failureClass(cause)
	signature := failureSignature(stage, c.CurrentWorkID, cause)
	prior := 0
	if transientClass(class) {
		var err error
		if prior, err = ex.CountFailures(ctx, taskID, signature+"#"); err != nil {
			return err
		}
		signature += fmt.Sprintf("#%d", prior+1)
	}
	if err := ex.FailAttempt(ctx, attemptID, domain.FailureExecution, signature, nil); err != nil {
		return err
	}
	hold := transientClass(class) && prior+1 >= maxTransientFailures
	if !hold {
		task, err := ex.Task(ctx, taskID)
		if err != nil {
			return err
		}
		hold = task.State == domain.TaskBlocked
	}
	if !hold {
		return nil
	}
	msg := cause.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	raw, err := json.Marshal(planFailureRecord{CaseID: string(c.ID), WorkID: string(c.CurrentWorkID), Class: class, Error: msg})
	if err != nil {
		return err
	}
	object, err := store.Put(ctx, bytes.NewReader(raw), evidence.Metadata{MediaType: "application/json", Kind: KindStageFailure, Subject: string(attemptID)})
	if err != nil {
		return err
	}
	_, err = cases.Assess(ctx, workflowcase.AssessmentRequest{
		CaseID: c.ID, WorkID: c.CurrentWorkID, RemainingBudget: c.RemainingBudget, RequireLatestRevision: true,
		Assessment: workflow.Assessment{Verdict: workflow.Unknown, Reason: stage + "-failed:" + strings.TrimSpace(class), EvidenceIDs: []string{string(object.ID)}},
	})
	return err
}

// moveCase assesses the case's current work with the given verdict and reason, citing one
// evidence object. Unknown holds the case (BLOCKED); Ready moves it to READY_FOR_VERIFICATION
// with no next work. Neither is gated by MaxSteps or the budget, and the budget is passed
// through unchanged.
func moveCase(ctx context.Context, cases *workflowcase.Service, c workflowcase.Case, verdict workflow.Verdict, reason string, evidenceID domain.ID) error {
	_, err := cases.Assess(ctx, workflowcase.AssessmentRequest{
		CaseID: c.ID, WorkID: c.CurrentWorkID, RemainingBudget: c.RemainingBudget, RequireLatestRevision: verdict == workflow.Ready,
		Assessment: workflow.Assessment{Verdict: verdict, Reason: reason, EvidenceIDs: []string{string(evidenceID)}},
	})
	return err
}
