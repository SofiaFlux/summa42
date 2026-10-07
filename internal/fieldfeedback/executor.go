package fieldfeedback

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/operations"
)

type EmitTaskLookup interface {
	FeedbackForEmitTask(
		context.Context,
		domain.ID,
	) (feedback domain.SanitizedFeedback, destination string, requiredApprovers []domain.ID, err error)
	MarkReported(context.Context, domain.ID, domain.ID, string) error
	ValidateEmitAttempt(context.Context, domain.ID, domain.ID) error
}

type EmitExecutor struct {
	feedback     EmitTaskLookup
	operations   *operations.Service
	providerName string
}

func (e *EmitExecutor) ExecutionContract() executors.Contract {
	return executors.Contract{TaskClasses: []string{"collective.feedback.emit"}, Capabilities: []string{feedbackCapability(e.providerName)}, Enforcement: domain.EnforcementEnforced}
}

func NewEmitExecutor(
	feedback EmitTaskLookup,
	operationsSvc *operations.Service,
	providerName string,
) (*EmitExecutor, error) {
	providerName = strings.TrimSpace(providerName)
	if feedback == nil || operationsSvc == nil || providerName == "" {
		return nil, errors.New("feedback emit executor requires lookup, operations service, and provider name")
	}
	return &EmitExecutor{feedback: feedback, operations: operationsSvc, providerName: providerName}, nil
}

func (e *EmitExecutor) Start(ctx context.Context, envelope executors.AttemptEnvelope) (executors.ExecutionResult, error) {
	if e == nil || e.feedback == nil || e.operations == nil || e.providerName == "" {
		return executors.ExecutionResult{ExitCode: 1}, errors.New("feedback emit executor is not configured")
	}
	if envelope.TaskID == "" || envelope.AttemptID == "" {
		return executors.ExecutionResult{ExitCode: 1}, errors.New("feedback emit executor requires task and attempt ids")
	}
	if err := e.feedback.ValidateEmitAttempt(ctx, envelope.TaskID, envelope.AttemptID); err != nil {
		return failureResult(err), err
	}

	artifact, destination, requiredApprovers, err := e.feedback.FeedbackForEmitTask(ctx, envelope.TaskID)
	if err != nil {
		return failureResult(err), err
	}
	if artifact.ID == "" || strings.TrimSpace(destination) == "" {
		err := errors.New("feedback emission linkage is incomplete")
		return failureResult(err), err
	}

	intent := EmitIntent{SanitizedFeedbackID: artifact.ID, Destination: destination}
	op, err := e.operations.Prepare(ctx, operations.PrepareRequest{
		AttemptID:         envelope.AttemptID,
		Provider:          e.providerName,
		TrustedSlotKey:    "feedback:" + e.providerName + ":" + destination + ":" + artifact.Fingerprint,
		Intent:            intent,
		Risk:              "LOW",
		RequiredApprovals: append([]domain.ID(nil), requiredApprovers...),
	})
	if err != nil {
		return failureResult(err), err
	}

	switch op.State {
	case domain.OperationConfirmedEffect:
		if err := e.feedback.MarkReported(ctx, artifact.ID, op.ID, op.ProviderReference); err != nil {
			return failureResult(err), err
		}
		return successResult(artifact.ID, op), nil
	case domain.OperationConfirmedNoEffect:
		err := errors.New("feedback effect is confirmed absent; governed retry is required")
		return failureResult(err), err
	case domain.OperationPrepared, domain.OperationDispatched, domain.OperationOutcomeUnknown:
		// Dispatch also owns read-only reconciliation for already-dispatched/unknown operations.
	default:
		err := fmt.Errorf("feedback operation reached unsupported state %s", op.State)
		return failureResult(err), err
	}

	settled, err := e.operations.Dispatch(ctx, op.ID, envelope.AttemptID)
	if err != nil {
		return failureResult(err), err
	}
	switch settled.State {
	case domain.OperationConfirmedEffect:
		if err := e.feedback.MarkReported(ctx, artifact.ID, settled.ID, settled.ProviderReference); err != nil {
			return failureResult(err), err
		}
		return successResult(artifact.ID, settled), nil
	case domain.OperationConfirmedNoEffect:
		err := errors.New("feedback effect is confirmed absent; governed retry is required")
		return failureResult(err), err
	default:
		err := fmt.Errorf("feedback operation did not settle to a confirmed outcome: %s", settled.State)
		return failureResult(err), err
	}
}

func successResult(feedbackID domain.ID, op domain.ExternalOperation) executors.ExecutionResult {
	content := fmt.Sprintf(
		"sanitized_feedback_id=%s\noperation_id=%s\nprovider_reference=%s",
		feedbackID, op.ID, strings.TrimSpace(op.ProviderReference),
	)
	return executors.ExecutionResult{
		ExitCode: 0,
		Stdout:   "feedback external effect confirmed",
		Evidence: []executors.Evidence{{
			Kind:    executors.EvidenceAgentMessage,
			Content: content,
		}},
	}
}

func failureResult(err error) executors.ExecutionResult {
	message := "feedback emission failed"
	if err != nil {
		message = err.Error()
	}
	return executors.ExecutionResult{ExitCode: 1, Stderr: message}
}
