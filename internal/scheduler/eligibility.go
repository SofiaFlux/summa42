package scheduler

import (
	"sort"
	"strings"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/executors"
)

type ExecutorCapacity struct {
	TaskClasses  []string
	Capabilities map[string]CapabilityCapacity
}

// CapacityForExecutors is the production composition's assessment. Registry
// names are aliases only; semantic capabilities come from executor contracts.
func CapacityForExecutors(registry map[string]executors.Executor) CapacitySnapshot {
	result := CapacitySnapshot{Capabilities: map[string]CapabilityCapacity{}, Executors: map[string]ExecutorCapacity{}}
	for kind, executor := range registry {
		if executor == nil || strings.TrimSpace(kind) == "" {
			continue
		}
		c := executors.Describe(kind, executor)
		level := c.Enforcement
		if level != domain.EnforcementEnforced && level != domain.EnforcementPartial {
			level = domain.EnforcementUnenforced
		}
		offer := ExecutorCapacity{TaskClasses: append([]string(nil), c.TaskClasses...), Capabilities: map[string]CapabilityCapacity{}}
		for _, cap := range append(c.Capabilities, kind) {
			if strings.TrimSpace(cap) == "" {
				continue
			}
			assessed := CapabilityCapacity{Accessible: true, Enforcement: level}
			offer.Capabilities[cap] = assessed
			old, found := result.Capabilities[cap]
			if !found || enforcementRank(level) > enforcementRank(old.Enforcement) {
				result.Capabilities[cap] = assessed
			}
		}
		result.Executors[kind] = offer
	}
	return result
}

func (c ExecutorCapacity) Supports(task domain.Task) bool {
	available := false
	for _, cap := range c.Capabilities {
		if cap.Accessible && enforcementRank(cap.Enforcement) >= enforcementRank(task.RequiredEnforcement) {
			available = true
			break
		}
	}
	if !available {
		return false
	}
	if len(c.TaskClasses) > 0 {
		matched := false
		for _, class := range c.TaskClasses {
			if class == task.TaskClass {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return capacityEligible(task, CapacitySnapshot{Capabilities: c.Capabilities})
}

func eligibleKinds(task domain.Task, capacity CapacitySnapshot) []string {
	var result []string
	for kind, offered := range capacity.Executors {
		if offered.Supports(task) {
			result = append(result, kind)
		}
	}
	sort.Strings(result)
	return result
}

// A legacy snapshot is an explicit, trusted assessment by the caller, but each
// executor still offers only its own kind. Described snapshots bind semantic
// capabilities to one executor; their union cannot authorize a mixed Attempt.
func (w *Worker) effectiveCapacity(input CapacitySnapshot) CapacitySnapshot {
	result := CapacitySnapshot{Capabilities: map[string]CapabilityCapacity{}, Executors: map[string]ExecutorCapacity{}}
	for kind, executor := range w.executors {
		var offer ExecutorCapacity
		if input.Executors != nil {
			declared, ok := input.Executors[kind]
			if !ok {
				continue
			}
			actual := CapacityForExecutors(map[string]executors.Executor{kind: executor}).Executors[kind]
			classes := append([]string(nil), actual.TaskClasses...)
			if len(declared.TaskClasses) > 0 {
				classes = append([]string(nil), declared.TaskClasses...)
				if len(actual.TaskClasses) > 0 {
					classes = nil
					for _, class := range declared.TaskClasses {
						for _, supported := range actual.TaskClasses {
							if class == supported {
								classes = append(classes, class)
								break
							}
						}
					}
					if len(classes) == 0 {
						continue
					}
				}
			}
			offer = ExecutorCapacity{TaskClasses: classes, Capabilities: map[string]CapabilityCapacity{}}
			for cap, assessed := range declared.Capabilities {
				available, ok := input.Capabilities[cap]
				if !ok || !assessed.Accessible || !available.Accessible {
					continue
				}
				supported, ok := actual.Capabilities[cap]
				if !ok || !supported.Accessible {
					continue
				}
				if enforcementRank(available.Enforcement) < enforcementRank(assessed.Enforcement) {
					assessed.Enforcement = available.Enforcement
				}
				if enforcementRank(supported.Enforcement) < enforcementRank(assessed.Enforcement) {
					assessed.Enforcement = supported.Enforcement
				}
				offer.Capabilities[cap] = assessed
			}
		} else if described, ok := executor.(executors.ContractProvider); ok {
			contract := described.ExecutionContract()
			offer = ExecutorCapacity{TaskClasses: append([]string(nil), contract.TaskClasses...), Capabilities: map[string]CapabilityCapacity{}}
			for _, cap := range append(append([]string(nil), contract.Capabilities...), kind) {
				if available, ok := input.Capabilities[cap]; ok && available.Accessible {
					if enforcementRank(contract.Enforcement) < enforcementRank(available.Enforcement) {
						available.Enforcement = contract.Enforcement
					}
					offer.Capabilities[cap] = available
				}
			}
		} else {
			offer = ExecutorCapacity{Capabilities: map[string]CapabilityCapacity{}}
			if available, ok := input.Capabilities[kind]; ok {
				offer.Capabilities[kind] = available
			}
		}
		if strings.TrimSpace(kind) == "" {
			continue
		}
		result.Executors[kind] = offer
		for cap, assessed := range offer.Capabilities {
			old, present := result.Capabilities[cap]
			if !present || enforcementRank(assessed.Enforcement) > enforcementRank(old.Enforcement) {
				result.Capabilities[cap] = assessed
			}
		}
	}
	return result
}
