package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/verification"
)

type Outcome string

const (
	StepIdle      Outcome = "IDLE"
	StepCompleted Outcome = "COMPLETED"
	StepFailed    Outcome = "FAILED"
)

type StepResult struct {
	Outcome     Outcome
	TaskID      domain.ID
	AttemptID   domain.ID
	EvidenceIDs []domain.ID
}

type Worker struct {
	scheduler     *Service
	execution     *execution.Service
	evidence      *evidence.Store
	verification  *verification.Service
	executors     map[string]executors.Executor
	clock         clock.Clock
	workspaceRoot string
}

func NewWorker(schedulerSvc *Service, executionSvc *execution.Service, evidenceStore *evidence.Store, verificationSvc *verification.Service, registry map[string]executors.Executor, clk clock.Clock, workspaceRoot string) (*Worker, error) {
	if schedulerSvc == nil || executionSvc == nil || evidenceStore == nil || verificationSvc == nil || clk == nil {
		return nil, errors.New("worker requires scheduler, execution, evidence, verification and clock")
	}
	if strings.TrimSpace(workspaceRoot) == "" {
		return nil, errors.New("worker requires a workspace root")
	}
	cleaned := make(map[string]executors.Executor, len(registry))
	for kind, executor := range registry {
		kind = strings.TrimSpace(kind)
		if kind != "" && executor != nil {
			cleaned[kind] = executor
		}
	}
	if len(cleaned) == 0 {
		return nil, errors.New("worker requires at least one executor")
	}
	return &Worker{scheduler: schedulerSvc, execution: executionSvc, evidence: evidenceStore, verification: verificationSvc, executors: cleaned, clock: clk, workspaceRoot: workspaceRoot}, nil
}

func (w *Worker) StepOnce(ctx context.Context, capacity CapacitySnapshot) (StepResult, error) {
	capacity = w.effectiveCapacity(capacity)
	candidate, err := w.scheduler.Next(ctx, capacity)
	if err != nil {
		return StepResult{}, err
	}
	if candidate == nil {
		return StepResult{Outcome: StepIdle}, nil
	}
	kind, err := w.scheduler.ChooseExecutor(ctx, candidate.Task, eligibleKinds(candidate.Task, capacity))
	if err != nil {
		return StepResult{}, err
	}
	executor, ok := w.executors[kind]
	if !ok {
		return StepResult{}, fmt.Errorf("executor %q is not registered", kind)
	}
	attempt, err := w.scheduler.Lease(ctx, candidate.Task.ID, kind)
	if err != nil {
		return StepResult{}, err
	}
	result := StepResult{Outcome: StepFailed, TaskID: candidate.Task.ID, AttemptID: attempt.ID}
	if err := w.executeAttempt(ctx, executor, kind, candidate.Task, attempt, &result); err != nil {
		return StepResult{}, err
	}
	return result, nil
}

func (w *Worker) executeAttempt(ctx context.Context, executor executors.Executor, kind string, task domain.Task, attempt domain.Attempt, result *StepResult) (err error) {
	workspace := filepath.Join(w.workspaceRoot, string(attempt.ID))
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return err
	}
	envelope := executors.AttemptEnvelope{
		TaskID: task.ID, AttemptID: attempt.ID,
		Objective: task.Objective, PayloadJSON: append([]byte(nil), task.PayloadJSON...),
		AcceptanceCriteria: append([]string(nil), task.AcceptanceCriteria...),
		Workspace:          workspace, VisibleCapabilities: append([]string(nil), task.RequiredCapabilities...),
		ResourceEnvelopeID: task.ResourceEnvelopeID,
	}
	execCtx, cancel := context.WithTimeout(ctx, w.scheduler.leaseDuration)
	defer cancel()
	outcome, execErr := w.runExecutor(execCtx, executor, envelope)
	if execErr != nil || outcome.ExitCode != 0 {
		return w.failExecution(ctx, executor, kind, task, attempt, result, outcome, execErr)
	}
	return w.completeExecution(ctx, kind, task, attempt, result, outcome)
}

func (w *Worker) runExecutor(ctx context.Context, executor executors.Executor, envelope executors.AttemptEnvelope) (result executors.ExecutionResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = executors.ExecutionResult{}
			err = fmt.Errorf("executor panic: %v", recovered)
		}
	}()
	return executor.Start(ctx, envelope)
}

func (w *Worker) persistEvidence(ctx context.Context, outcome executors.ExecutionResult) ([]domain.ID, error) {
	type blob struct {
		content string
		media   string
		kind    string
	}
	blobs := make([]blob, 0, len(outcome.Evidence)+2)
	if outcome.Stdout != "" {
		blobs = append(blobs, blob{outcome.Stdout, "text/plain", string(executors.EvidenceStdout)})
	}
	if outcome.Stderr != "" {
		blobs = append(blobs, blob{outcome.Stderr, "text/plain", string(executors.EvidenceStderr)})
	}
	for _, item := range outcome.Evidence {
		if item.Content == "" {
			continue
		}
		blobs = append(blobs, blob{item.Content, "text/plain", string(item.Kind)})
	}
	ids := make([]domain.ID, 0, len(blobs))
	for _, b := range blobs {
		object, err := w.evidence.Put(ctx, strings.NewReader(b.content), evidence.Metadata{MediaType: b.media, Kind: b.kind})
		if err != nil {
			return nil, err
		}
		ids = append(ids, object.ID)
	}
	return ids, nil
}

func (w *Worker) completeExecution(ctx context.Context, kind string, task domain.Task, attempt domain.Attempt, result *StepResult, outcome executors.ExecutionResult) error {
	ids, err := w.persistEvidence(ctx, outcome)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return w.failExecution(ctx, nil, kind, task, attempt, result, outcome, errors.New("executor returned no evidence"))
	}
	usageID, err := w.persistUsage(ctx, kind, task, attempt, outcome.Usage)
	if err != nil {
		return err
	}
	if usageID != "" {
		ids = append(ids, usageID)
	}
	if _, err := w.verification.CompleteAttempt(ctx, attempt.ID, verification.CompletionManifest{EvidenceIDs: ids}); err != nil {
		if errors.Is(err, domain.ErrStaleAttempt) || errors.Is(err, domain.ErrLeaseInactive) {
			result.Outcome = StepFailed
			result.EvidenceIDs = ids
			return nil
		}
		return err
	}
	result.Outcome = StepCompleted
	result.EvidenceIDs = ids
	return nil
}

func (w *Worker) failExecution(ctx context.Context, _ executors.Executor, kind string, task domain.Task, attempt domain.Attempt, result *StepResult, outcome executors.ExecutionResult, execErr error) error {
	ids, err := w.persistEvidence(ctx, outcome)
	if err != nil {
		return err
	}
	usageID, err := w.persistUsage(ctx, kind, task, attempt, outcome.Usage)
	if err != nil {
		return err
	}
	if usageID != "" {
		ids = append(ids, usageID)
	}
	signature := "worker:" + kind + ":" + string(task.ID)
	if execErr != nil && strings.HasPrefix(execErr.Error(), "executor panic:") {
		signature = "worker:panic:" + kind + ":" + string(task.ID)
	}
	if err := w.execution.FailAttempt(ctx, attempt.ID, domain.FailureExecution, signature, ids); err != nil {
		if errors.Is(err, domain.ErrStaleAttempt) || errors.Is(err, domain.ErrLeaseInactive) {
			result.Outcome = StepFailed
			result.EvidenceIDs = ids
			return nil
		}
		return err
	}
	result.Outcome = StepFailed
	result.EvidenceIDs = ids
	return nil
}

func (w *Worker) Run(ctx context.Context, capacity CapacitySnapshot, interval time.Duration) error {
	if interval <= 0 {
		return errors.New("worker requires a positive poll interval")
	}
	if err := w.stepGuarded(ctx, capacity); err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := w.stepGuarded(ctx, capacity); err != nil {
				return err
			}
		}
	}
}

func (w *Worker) stepGuarded(ctx context.Context, capacity CapacitySnapshot) error {
	if err := ctx.Err(); err != nil {
		return nil
	}
	_, err := w.StepOnce(ctx, capacity)
	if err != nil && ctx.Err() != nil {
		return nil
	}
	return err
}
