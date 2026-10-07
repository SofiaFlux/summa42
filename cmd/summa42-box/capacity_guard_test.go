package main

import (
	"context"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
)

type partialCapacityExecutor struct{}
type unassessedCapacityExecutor struct{}

func (unassessedCapacityExecutor) Start(context.Context, executors.AttemptEnvelope) (executors.ExecutionResult, error) {
	return executors.ExecutionResult{}, nil
}

func (partialCapacityExecutor) Start(context.Context, executors.AttemptEnvelope) (executors.ExecutionResult, error) {
	return executors.ExecutionResult{}, nil
}

func (partialCapacityExecutor) EnforcementLevel() domain.EnforcementLevel {
	return domain.EnforcementPartial
}

func TestPartialExecutorCapacityIsNotPromotedToEnforced(t *testing.T) {
	capacity, err := workerCapacity(&summa42runtime.Box{Executors: map[string]executors.Executor{"partial": partialCapacityExecutor{}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := capacity.Capabilities["partial"].Enforcement; got != domain.EnforcementPartial {
		t.Fatalf("partial executor advertised as %s", got)
	}
}

func TestUnknownExecutorCapacityDoesNotClaimEnforcement(t *testing.T) {
	capacity, err := workerCapacity(&summa42runtime.Box{Executors: map[string]executors.Executor{"unknown": unassessedCapacityExecutor{}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := capacity.Capabilities["unknown"].Enforcement; got != domain.EnforcementUnenforced {
		t.Fatalf("unassessed executor advertised as %s", got)
	}
}

func TestRegistryNameCannotInventSemanticCapability(t *testing.T) {
	capacity, err := workerCapacity(&summa42runtime.Box{Executors: map[string]executors.Executor{ghtriage.ExecutorKind: unassessedCapacityExecutor{}}})
	if err != nil {
		t.Fatal(err)
	}
	if capacity.Capabilities[ghtriage.RequiredCapability].Accessible {
		t.Fatal("a registry name invented github.issue.read without an executor contract")
	}
}
