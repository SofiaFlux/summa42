package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/executors"
)

const UsageEvidenceKind = "executor.usage.v1"

type UsageRecord struct {
	TaskID       domain.ID       `json:"task_id"`
	AttemptID    domain.ID       `json:"attempt_id"`
	ExecutorKind string          `json:"executor_kind"`
	Usage        executors.Usage `json:"usage"`
}

// Usage is observed data, not a monetary settlement. Without a provider quote
// or a complete usage report, a missing count must never mean zero actual cost.
func (w *Worker) persistUsage(ctx context.Context, kind string, task domain.Task, attempt domain.Attempt, usage executors.Usage) (domain.ID, error) {
	if usage.WallTime < 0 || usage.InputTokens < 0 || usage.CachedInputTokens < 0 || usage.CacheWriteInputTokens < 0 || usage.OutputTokens < 0 || usage.ReasoningOutputTokens < 0 {
		return "", errors.New("executor returned negative usage")
	}
	if usage == (executors.Usage{}) {
		return "", nil
	}
	raw, err := json.Marshal(UsageRecord{TaskID: task.ID, AttemptID: attempt.ID, ExecutorKind: kind, Usage: usage})
	if err != nil {
		return "", err
	}
	object, err := w.evidence.Put(ctx, bytes.NewReader(raw), evidence.Metadata{MediaType: "application/json", Kind: UsageEvidenceKind})
	if err != nil {
		return "", err
	}
	if err := w.evidence.LinkSubject(ctx, UsageEvidenceKind, string(attempt.ID), object.ID); err != nil {
		return "", err
	}
	return object.ID, nil
}
