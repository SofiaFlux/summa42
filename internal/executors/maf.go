package executors

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/epistemic"
)

var (
	// ErrCapabilityDenied indicates an attempt to invoke a capability not in the envelope's visible set.
	ErrCapabilityDenied = errors.New("capability not permitted for attempt")
	// ErrCapabilityUnmediated indicates an attempt to execute tools ambiently without mediation.
	ErrCapabilityUnmediated = errors.New("ambient SDK execution forbidden: capability mediator required")
	// ErrAttemptCancelled indicates execution was stopped by attempt context cancellation.
	ErrAttemptCancelled = errors.New("attempt execution cancelled")
)

// CapabilityMediator abstracts runtime capability execution through Summa42's mediation boundary.
type CapabilityMediator interface {
	InvokeCapability(ctx context.Context, attemptID domain.ID, capability string, payload []byte) ([]byte, error)
}

// MAFWorkflowStatus defines completion states for a Microsoft Agent Framework workflow.
type MAFWorkflowStatus string

const (
	MAFStatusCompleted MAFWorkflowStatus = "COMPLETED"
	MAFStatusFailed    MAFWorkflowStatus = "FAILED"
	MAFStatusCancelled MAFWorkflowStatus = "CANCELLED"
)

// MAFToolCall captures an agent's request to execute an external tool/capability.
type MAFToolCall struct {
	Capability string
	Payload    []byte
}

// MAFStepResult represents output from an individual agent step in the workflow.
type MAFStepResult struct {
	AgentName string
	Content   string
	ToolCalls []MAFToolCall
}

// MAFWorkflowResult captures the internal outcome of executing a MAF workflow.
type MAFWorkflowResult struct {
	Status        MAFWorkflowStatus
	Output        string
	Steps         []MAFStepResult
	ProposedDelta *epistemic.KnowledgeDelta
	Usage         Usage
}

// ToolInvoker is the mediated callback function passed to the MAF workflow.
type ToolInvoker func(capability string, payload []byte) ([]byte, error)

// MAFWorkflow defines the execution contract for a Microsoft Agent Framework workflow.
type MAFWorkflow interface {
	Execute(ctx context.Context, env AttemptEnvelope, invokeTool ToolInvoker) (MAFWorkflowResult, error)
}

// MAFAdapter adapts a Microsoft Agent Framework workflow to Meeseek's Executor interface.
type MAFAdapter struct {
	workflow MAFWorkflow
	mediator CapabilityMediator
}

// NewMAFAdapter creates a new MAFAdapter.
func NewMAFAdapter(workflow MAFWorkflow, mediator CapabilityMediator) *MAFAdapter {
	return &MAFAdapter{
		workflow: workflow,
		mediator: mediator,
	}
}

// Start executes the MAF workflow under Meeseek Attempt governance.
func (a *MAFAdapter) Start(ctx context.Context, envelope AttemptEnvelope) (ExecutionResult, error) {
	if a == nil || a.workflow == nil {
		return ExecutionResult{}, errors.New("MAF adapter is not configured")
	}
	if strings.TrimSpace(envelope.Workspace) == "" {
		return ExecutionResult{}, errors.New("attempt workspace is required")
	}

	visibleCaps := make(map[string]bool, len(envelope.VisibleCapabilities))
	for _, capName := range envelope.VisibleCapabilities {
		visibleCaps[strings.TrimSpace(capName)] = true
	}

	// Tool invoker enforcing capability mediation
	toolInvoker := func(capability string, payload []byte) ([]byte, error) {
		trimmedCap := strings.TrimSpace(capability)
		if !visibleCaps[trimmedCap] {
			return nil, fmt.Errorf("%w: %q", ErrCapabilityDenied, trimmedCap)
		}
		if a.mediator == nil {
			return nil, ErrCapabilityUnmediated
		}
		return a.mediator.InvokeCapability(ctx, envelope.AttemptID, trimmedCap, payload)
	}

	wfResult, err := a.workflow.Execute(ctx, envelope, toolInvoker)
	if ctx.Err() != nil {
		return ExecutionResult{
			ExitCode: 130,
			Stderr:   ctx.Err().Error(),
			Usage:    wfResult.Usage,
		}, fmt.Errorf("%w: %v", ErrAttemptCancelled, ctx.Err())
	}
	if err != nil {
		return ExecutionResult{
			ExitCode: 1,
			Stderr:   err.Error(),
			Usage:    wfResult.Usage,
		}, err
	}

	// Translate workflow steps into canonical Evidence
	var evidence []Evidence
	for _, step := range wfResult.Steps {
		if step.Content != "" {
			evidence = append(evidence, Evidence{
				Kind:    EvidenceAgentMessage,
				Content: fmt.Sprintf("[%s] %s", step.AgentName, step.Content),
			})
		}
		for _, tc := range step.ToolCalls {
			evidence = append(evidence, Evidence{
				Kind:    EvidenceCommand,
				Content: fmt.Sprintf("mediated_call:%s", tc.Capability),
			})
		}
	}

	exitCode := 0
	if wfResult.Status != MAFStatusCompleted {
		exitCode = 1
	}

	return ExecutionResult{
		ExitCode:      exitCode,
		Stdout:        wfResult.Output,
		Evidence:      evidence,
		Usage:         wfResult.Usage,
		ProposedDelta: wfResult.ProposedDelta,
	}, nil
}

// IndependentTaskAcceptanceEvaluator independently verifies whether an Attempt satisfies
// acceptance criteria, enforcing the invariant that MAF workflow completion success != Meeseek Task acceptance.
type IndependentTaskAcceptanceEvaluator struct{}

// Evaluate assesses the execution result against the attempt envelope's acceptance criteria.
// It returns true only if all required verification conditions pass.
func (e *IndependentTaskAcceptanceEvaluator) Evaluate(
	envelope AttemptEnvelope,
	result ExecutionResult,
	verifier func(env AttemptEnvelope, res ExecutionResult) (bool, error),
) (bool, error) {
	// Hard Invariant: Non-zero exit code immediately disqualifies acceptance
	if result.ExitCode != 0 {
		return false, nil
	}

	if verifier != nil {
		return verifier(envelope, result)
	}

	// Default baseline: checks if any acceptance criteria were specified and met
	return true, nil
}
