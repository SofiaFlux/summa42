package policy

import (
	"context"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

// PolicyInput is the rego input. Every top-level key and every key of
// Attributes other than "caller" is set by the operations service; caller
// assertions live only under Attributes["caller"].
type PolicyInput struct {
	Risk             string         `json:"risk"`
	AuthorityValid   bool           `json:"authority_valid"`
	OwnerPrincipalID domain.ID      `json:"owner_principal_id"`
	Now              time.Time      `json:"now"`
	Attributes       map[string]any `json:"attributes,omitempty"`
}

type PolicyEngine interface {
	Evaluate(ctx context.Context, in PolicyInput) (domain.PolicyDecision, error)
}
