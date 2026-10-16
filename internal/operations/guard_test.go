package operations_test

import (
	"context"
	"database/sql"
	"errors"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/SofiaFlux/summa42/internal/testutil"
	"testing"
)

func TestOperationGuardRejectsPreparationAndDispatch(t *testing.T) {
	h := newHarness(t)
	denied := errors.New("publication revoked")
	guard := func(context.Context, *sql.Tx) error { return denied }
	req := operations.PrepareRequest{AttemptID: h.attempt.ID, Provider: h.provider.Name(), TrustedSlotKey: "guarded", Intent: testutil.PurchaseIntent{SKU: "guarded", Quantity: 1}, Risk: "LOW"}
	if _, err := h.svc.PrepareWithGuard(h.ctx, req, guard); !errors.Is(err, denied) {
		t.Fatal(err)
	}
	op, err := h.svc.Prepare(h.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.DispatchWithGuard(h.ctx, op.ID, h.attempt.ID, guard); !errors.Is(err, denied) {
		t.Fatal(err)
	}
	current, err := h.svc.Operation(h.ctx, op.ID)
	if err != nil || current.State != domain.OperationPrepared {
		t.Fatalf("%+v %v", current, err)
	}
	if h.provider.DispatchCount() != 0 {
		t.Fatal("effect crossed rejected guard")
	}
}
