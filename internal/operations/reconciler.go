package operations

import (
	"context"
	"errors"

	"github.com/SofiaFlux/summa42/internal/domain"
)

type Reconciler struct {
	service *Service
}

func NewReconciler(service *Service) *Reconciler {
	return &Reconciler{service: service}
}

func (r *Reconciler) Reconcile(ctx context.Context, operationID domain.ID) (domain.ExternalOperation, error) {
	if r == nil || r.service == nil {
		return domain.ExternalOperation{}, errors.New("operation reconciler is not configured")
	}
	if operationID == "" {
		return domain.ExternalOperation{}, errors.New("operation id is required")
	}
	return r.service.reconcile(ctx, operationID)
}

// ReconcilePending is the startup/recovery entry point. A durable DISPATCHED
// operation is conservatively treated as an uncertain external effect and is
// reconciled by provider lookup before any replacement dispatch is considered.
// OUTCOME_UNKNOWN records that still require reconciliation are retried too.
// Remaining unknown outcomes are expected and stay durably unresolved rather
// than failing Box startup.
func (r *Reconciler) ReconcilePending(ctx context.Context) (int, error) {
	if r == nil || r.service == nil || r.service.store == nil {
		return 0, errors.New("operation reconciler is not configured")
	}

	rows, err := r.service.store.DB().QueryContext(ctx, `
		SELECT operation_id
		FROM external_operations
		WHERE state = 'DISPATCHED'
		   OR (state = 'OUTCOME_UNKNOWN' AND reconciliation_required = 1)
		ORDER BY created_at, operation_id`)
	if err != nil {
		return 0, err
	}
	var operationIDs []domain.ID
	for rows.Next() {
		var operationID domain.ID
		if err := rows.Scan(&operationID); err != nil {
			_ = rows.Close()
			return 0, err
		}
		operationIDs = append(operationIDs, operationID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	settled := 0
	for _, operationID := range operationIDs {
		_, err := r.service.reconcile(ctx, operationID)
		if err != nil && !errors.Is(err, domain.ErrOutcomeUnknown) {
			return settled, err
		}
		if err == nil {
			settled++
		}
	}
	return settled, nil
}
