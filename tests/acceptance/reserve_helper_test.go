package acceptance_test

import (
	"context"
	"database/sql"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/resources"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
)

func reserveInOwnTx(ctx context.Context, box *summa42runtime.Box, envelopeID domain.ID, amount int64, e resources.Enforceability) (domain.Reservation, error) {
	var r domain.Reservation
	err := box.Store.WithTx(ctx, func(tx *sql.Tx) (err error) {
		r, err = box.Resources.ReserveInTx(ctx, tx, envelopeID, amount, e)
		return err
	})
	return r, err
}

func settleInOwnTx(ctx context.Context, box *summa42runtime.Box, id domain.ID, amount int64) error {
	return box.Store.WithTx(ctx, func(tx *sql.Tx) error {
		return box.Resources.SettleInTx(ctx, tx, id, amount)
	})
}
