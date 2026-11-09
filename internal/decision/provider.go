package decision

import "context"

// DecisionProvider abstracts a cognitive mechanism (rule engine, classical ML model,
// specialist classifier, or constrained LLM) capable of resolving micro-decisions.
type DecisionProvider interface {
	// Info returns the provenance metadata for this provider.
	Info() ProviderInfo

	// Resolve resolves a bounded micro-decision request within its allowed outcome envelope.
	Resolve(ctx context.Context, req DecisionRequest) (DecisionResult, error)
}
