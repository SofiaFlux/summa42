package scheduler

import (
	"context"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/executors"
	"testing"
)

func TestSplitCapabilitiesDoNotQualifyOneExecutor(t *testing.T) {
	cap := CapabilityCapacity{Accessible: true, Enforcement: domain.EnforcementEnforced}
	capacity := CapacitySnapshot{Capabilities: map[string]CapabilityCapacity{"read": cap, "write": cap}, Executors: map[string]ExecutorCapacity{
		"reader": {Capabilities: map[string]CapabilityCapacity{"read": cap}},
		"writer": {Capabilities: map[string]CapabilityCapacity{"write": cap}},
	}}
	task := domain.Task{RequiredCapabilities: []string{"read", "write"}, RequiredEnforcement: domain.EnforcementEnforced}
	if got := eligibleKinds(task, capacity); len(got) != 0 {
		t.Fatalf("split capabilities selected %v", got)
	}
	capacity.Executors["both"] = ExecutorCapacity{Capabilities: capacity.Capabilities}
	if got := eligibleKinds(task, capacity); len(got) != 1 || got[0] != "both" {
		t.Fatalf("complete executor selected %v", got)
	}
}

type limitedExecutor struct{}

func (limitedExecutor) Start(context.Context, executors.AttemptEnvelope) (executors.ExecutionResult, error) {
	return executors.ExecutionResult{}, nil
}
func (limitedExecutor) ExecutionContract() executors.Contract {
	return executors.Contract{TaskClasses: []string{"review"}, Capabilities: []string{"read"}, Enforcement: domain.EnforcementPartial}
}

func TestCallerSnapshotCannotExpandActualContract(t *testing.T) {
	w := &Worker{executors: map[string]executors.Executor{"limited": limitedExecutor{}}}
	cap := CapabilityCapacity{Accessible: true, Enforcement: domain.EnforcementEnforced}
	input := CapacitySnapshot{Capabilities: map[string]CapabilityCapacity{"read": cap, "write": cap}, Executors: map[string]ExecutorCapacity{"limited": {Capabilities: map[string]CapabilityCapacity{"read": cap, "write": cap}}}}
	got := w.effectiveCapacity(input)
	for _, task := range []domain.Task{
		{TaskClass: "implement", RequiredCapabilities: []string{"read"}},
		{TaskClass: "review", RequiredCapabilities: []string{"write"}},
		{TaskClass: "review", RequiredCapabilities: []string{"read"}, RequiredEnforcement: domain.EnforcementEnforced},
	} {
		if len(eligibleKinds(task, got)) != 0 {
			t.Fatalf("expanded actual contract for %+v", task)
		}
	}
	if len(eligibleKinds(domain.Task{TaskClass: "review", RequiredCapabilities: []string{"read"}, RequiredEnforcement: domain.EnforcementPartial}, got)) != 1 {
		t.Fatal("valid intersection was removed")
	}
}

func TestContractClassAndEnforcementAreBothRequired(t *testing.T) {
	offer := ExecutorCapacity{TaskClasses: []string{"review"}, Capabilities: map[string]CapabilityCapacity{"read": {Accessible: true, Enforcement: domain.EnforcementPartial}}}
	if offer.Supports(domain.Task{TaskClass: "implement", RequiredCapabilities: []string{"read"}}) {
		t.Fatal("wrong task class supported")
	}
	if offer.Supports(domain.Task{TaskClass: "review", RequiredCapabilities: []string{"read"}, RequiredEnforcement: domain.EnforcementEnforced}) {
		t.Fatal("partial capability promoted")
	}
}

func TestEmptyCapabilityTaskStillRequiresExecutorAvailabilityAndEnforcement(t *testing.T) {
	task := domain.Task{RequiredEnforcement: domain.EnforcementEnforced}
	partial := ExecutorCapacity{Capabilities: map[string]CapabilityCapacity{"executor": {Accessible: true, Enforcement: domain.EnforcementPartial}}}
	if partial.Supports(task) {
		t.Fatal("empty capabilities bypassed enforcement")
	}
	if (ExecutorCapacity{}).Supports(domain.Task{}) {
		t.Fatal("unavailable executor qualified")
	}
}
