package operations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

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

// ReconcileResult counts one ReconcilePending pass.
type ReconcileResult struct {
	Reconciled int
	Skipped    int
}

// ReconcilePending is the startup/recovery entry point. A durable DISPATCHED
// operation is conservatively treated as an uncertain external effect and is
// reconciled by provider lookup before any replacement dispatch is considered.
// OUTCOME_UNKNOWN records that still require reconciliation are retried too.
// Remaining unknown outcomes stay durably unresolved rather than failing.
//
// Several processes share the database, so an operation is skipped (counted,
// never mutated) when this process has no provider for it, or when it is
// DISPATCHED younger than max(provider SettleWindow, minAge): it may be a live
// in-flight dispatch in another process. minAge is normally the lease duration.
func (r *Reconciler) ReconcilePending(ctx context.Context, minAge time.Duration) (ReconcileResult, error) {
	var result ReconcileResult
	if r == nil || r.service == nil || r.service.store == nil {
		return result, errors.New("operation reconciler is not configured")
	}
	rows, err := r.service.store.DB().QueryContext(ctx, `
		SELECT operation_id, state, provider, dispatched_at
		FROM external_operations
		WHERE state = 'DISPATCHED'
		   OR (state = 'OUTCOME_UNKNOWN' AND reconciliation_required = 1)
		ORDER BY created_at, operation_id`)
	if err != nil {
		return result, err
	}
	type pending struct {
		id         domain.ID
		state      domain.OperationState
		provider   string
		dispatched time.Time
	}
	var ops []pending
	for rows.Next() {
		var p pending
		var dispatched sql.NullString
		if err := rows.Scan(&p.id, &p.state, &p.provider, &dispatched); err != nil {
			_ = rows.Close()
			return result, err
		}
		if dispatched.Valid {
			if p.dispatched, err = time.Parse(time.RFC3339Nano, dispatched.String); err != nil {
				_ = rows.Close()
				return result, fmt.Errorf("parse operation dispatched_at: %w", err)
			}
		}
		ops = append(ops, p)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return result, err
	}
	if err := rows.Close(); err != nil {
		return result, err
	}

	now := r.service.clock.Now().UTC()
	for _, p := range ops {
		provider, err := r.service.providerFor(p.provider)
		if err != nil {
			result.Skipped++
			continue
		}
		grace := provider.SettleWindow()
		if minAge > grace {
			grace = minAge
		}
		if p.state == domain.OperationDispatched && now.Before(p.dispatched.Add(grace)) {
			result.Skipped++
			continue
		}
		_, err = r.service.reconcile(ctx, p.id)
		switch {
		case err == nil:
			result.Reconciled++
		case errors.Is(err, domain.ErrOutcomeUnknown):
		default:
			return result, err
		}
	}
	return result, nil
}
