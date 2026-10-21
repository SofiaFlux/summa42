package resources

import (
	"context"
	"database/sql"

	"github.com/SofiaFlux/summa42/internal/domain"
)

// Test-only convenience wrappers that run the InTx operations in their own transaction.

func (s *Service) Reserve(ctx context.Context, envelopeID domain.ID, amount int64, enforceability Enforceability) (domain.Reservation, error) {
	if err := s.configured(); err != nil {
		return domain.Reservation{}, err
	}
	var reservation domain.Reservation
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		reservation, err = s.ReserveInTx(ctx, tx, envelopeID, amount, enforceability)
		return err
	})
	return reservation, err
}

func (s *Service) Settle(ctx context.Context, reservationID domain.ID, actualAmount int64) error {
	if err := s.configured(); err != nil {
		return err
	}
	return s.store.WithTx(ctx, func(tx *sql.Tx) error {
		return s.SettleInTx(ctx, tx, reservationID, actualAmount)
	})
}

func (s *Service) MarkUnresolved(ctx context.Context, reservationID domain.ID) error {
	if err := s.configured(); err != nil {
		return err
	}
	return s.store.WithTx(ctx, func(tx *sql.Tx) error {
		return s.MarkUnresolvedInTx(ctx, tx, reservationID)
	})
}

func (s *Service) Release(ctx context.Context, reservationID domain.ID) error {
	if err := s.configured(); err != nil {
		return err
	}
	return s.store.WithTx(ctx, func(tx *sql.Tx) error {
		return s.ReleaseInTx(ctx, tx, reservationID)
	})
}
