package executors

import "github.com/SofiaFlux/summa42/internal/domain"

// Contract describes supported work; it never grants Task authority. An empty
// TaskClasses list imposes no class constraint, while capabilities remain exact.
type Contract struct {
	TaskClasses  []string
	Capabilities []string
	Enforcement  domain.EnforcementLevel
}

type ContractProvider interface{ ExecutionContract() Contract }

// Describe is conservative for third-party executors without an assessment.
func Describe(kind string, executor Executor) Contract {
	if described, ok := executor.(ContractProvider); ok {
		c := described.ExecutionContract()
		c.TaskClasses = append([]string(nil), c.TaskClasses...)
		c.Capabilities = append([]string(nil), c.Capabilities...)
		return c
	}
	level := domain.EnforcementUnenforced
	if assessed, ok := executor.(interface {
		EnforcementLevel() domain.EnforcementLevel
	}); ok {
		level = assessed.EnforcementLevel()
	}
	return Contract{Capabilities: []string{kind}, Enforcement: level}
}
