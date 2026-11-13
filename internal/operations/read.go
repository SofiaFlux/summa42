package operations

import (
	"context"
	"errors"
	"strings"

	"github.com/SofiaFlux/summa42/internal/domain"
)

func (s *Service) Operation(ctx context.Context, operationID domain.ID) (domain.ExternalOperation, error) {
	if err := s.configured(); err != nil {
		return domain.ExternalOperation{}, err
	}
	operationID = domain.ID(strings.TrimSpace(string(operationID)))
	if operationID == "" {
		return domain.ExternalOperation{}, errors.New("operation id is required")
	}
	return s.loadOperation(ctx, operationID)
}

// ApprovalOperation is the newest operation that carries an approval. An
// approval adopted by later Attempts (adoptableApprovalInTx) is carried by
// several operations of one effect slot, and its subject is the first one,
// which may be CANCELLED by now.
func (s *Service) ApprovalOperation(ctx context.Context, approvalID domain.ID) (domain.ExternalOperation, error) {
	if err := s.configured(); err != nil {
		return domain.ExternalOperation{}, err
	}
	approvalID = domain.ID(strings.TrimSpace(string(approvalID)))
	if approvalID == "" {
		return domain.ExternalOperation{}, errors.New("approval id is required")
	}
	var id domain.ID
	if err := s.store.DB().QueryRowContext(ctx,
		`SELECT operation_id FROM external_operations WHERE approval_id = ? ORDER BY operation_sequence DESC LIMIT 1`,
		approvalID,
	).Scan(&id); err != nil {
		return domain.ExternalOperation{}, err
	}
	return s.loadOperation(ctx, id)
}
