package resources

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

func newLedger(t *testing.T, hardLimit int64) (*Service, context.Context, domain.ID) {
	t.Helper()
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 15, 15, 0, 0, 0, time.UTC))
	envelopeID := domain.NewID("envelope")
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, ?, ?)`,
		envelopeID, hardLimit, domain.FormatTime(clk.Now()),
	); err != nil {
		t.Fatal(err)
	}
	return New(store, clk), ctx, envelopeID
}

func hardCapped() Enforceability {
	return Enforceability{CostControl: CostTechnicallyCapped, RequireHardCap: true, Source: "provider-max-cost"}
}

func TestUnresolvedExposureStillConsumesBudget(t *testing.T) {
	svc, ctx, envelopeID := newLedger(t, 100)
	r, err := svc.Reserve(ctx, envelopeID, 70, hardCapped())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkUnresolved(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reserve(ctx, envelopeID, 40, hardCapped()); !errors.Is(err, domain.ErrBudgetExceeded) {
		t.Fatalf("expected budget exceeded, got %v", err)
	}
	available, err := svc.Available(ctx, envelopeID)
	if err != nil {
		t.Fatal(err)
	}
	if available != 30 {
		t.Fatalf("available = %d, want 30", available)
	}
}

func TestEstimatedOnlyCannotSatisfyRequiredHardCap(t *testing.T) {
	svc, ctx, envelopeID := newLedger(t, 100)
	_, err := svc.Reserve(ctx, envelopeID, 10, Enforceability{
		CostControl:    CostEstimatedOnly,
		RequireHardCap: true,
		Source:         "model-estimate",
	})
	if !errors.Is(err, ErrHardCapUnavailable) {
		t.Fatalf("got %v, want ErrHardCapUnavailable", err)
	}
}

func TestPotentiallyUnboundedCannotSatisfyRequiredHardCap(t *testing.T) {
	svc, ctx, envelopeID := newLedger(t, 100)
	_, err := svc.Reserve(ctx, envelopeID, 10, Enforceability{
		CostControl:    CostPotentiallyOpen,
		RequireHardCap: true,
		Source:         "remote-executor",
	})
	if !errors.Is(err, ErrHardCapUnavailable) {
		t.Fatalf("got %v, want ErrHardCapUnavailable", err)
	}
}

func TestTechnicalCeilingProfilesSatisfyHardCapRequirement(t *testing.T) {
	for _, control := range []CostControl{CostTechnicallyCapped, CostPrepaidQuota, CostVerifiedStop} {
		t.Run(string(control), func(t *testing.T) {
			svc, ctx, envelopeID := newLedger(t, 100)
			if _, err := svc.Reserve(ctx, envelopeID, 10, Enforceability{CostControl: control, RequireHardCap: true, Source: "test"}); err != nil {
				t.Fatalf("%s should satisfy hard-cap requirement: %v", control, err)
			}
		})
	}
}

func TestAvailableSubtractsSettledHeldAndUnresolvedSeparately(t *testing.T) {
	svc, ctx, envelopeID := newLedger(t, 100)
	settled, err := svc.Reserve(ctx, envelopeID, 20, hardCapped())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Settle(ctx, settled.ID, 15); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reserve(ctx, envelopeID, 25, hardCapped()); err != nil {
		t.Fatal(err)
	}
	unresolved, err := svc.Reserve(ctx, envelopeID, 30, hardCapped())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkUnresolved(ctx, unresolved.ID); err != nil {
		t.Fatal(err)
	}

	available, err := svc.Available(ctx, envelopeID)
	if err != nil {
		t.Fatal(err)
	}
	if available != 30 { // 100 - 15 settled - 25 held - 30 unresolved
		t.Fatalf("available = %d, want 30", available)
	}
}

func TestReleaseRejectsUnresolved(t *testing.T) {
	svc, ctx, envelopeID := newLedger(t, 100)
	held, err := svc.Reserve(ctx, envelopeID, 20, hardCapped())
	if err != nil {
		t.Fatal(err)
	}
	unresolved, err := svc.Reserve(ctx, envelopeID, 30, hardCapped())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkUnresolved(ctx, unresolved.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Release(ctx, held.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Release(ctx, unresolved.ID); err == nil {
		t.Fatal("release of an UNRESOLVED reservation must be rejected")
	}
	available, err := svc.Available(ctx, envelopeID)
	if err != nil {
		t.Fatal(err)
	}
	if available != 70 { // unresolved exposure still consumes 30
		t.Fatalf("available = %d, want 70", available)
	}
}

func TestSettleFlagsOverSettlement(t *testing.T) {
	svc, ctx, envelopeID := newLedger(t, 100)
	over, err := svc.Reserve(ctx, envelopeID, 20, Enforceability{CostControl: CostEstimatedOnly, Source: "estimate"})
	if err != nil {
		t.Fatal(err)
	}
	within, err := svc.Reserve(ctx, envelopeID, 20, Enforceability{CostControl: CostEstimatedOnly, Source: "estimate"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Settle(ctx, over.ID, 25); err != nil {
		t.Fatal(err)
	}
	if err := svc.Settle(ctx, within.ID, 20); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[domain.ID]int{over.ID: 1, within.ID: 0} {
		var got int
		if err := svc.store.DB().QueryRowContext(ctx, `SELECT over_settled FROM resource_reservations WHERE reservation_id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("over_settled(%s) = %d, want %d", id, got, want)
		}
	}
}

func TestSettlementRecordsActualCostEvenWhenItExceedsReservation(t *testing.T) {
	svc, ctx, envelopeID := newLedger(t, 100)
	r, err := svc.Reserve(ctx, envelopeID, 20, Enforceability{CostControl: CostEstimatedOnly, Source: "estimate"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Settle(ctx, r.ID, 110); err != nil {
		t.Fatalf("ledger must record actual cost even after overrun: %v", err)
	}
	available, err := svc.Available(ctx, envelopeID)
	if err != nil {
		t.Fatal(err)
	}
	if available != -10 {
		t.Fatalf("available = %d, want -10 honest overrun", available)
	}
	if _, err := svc.Reserve(ctx, envelopeID, 1, hardCapped()); !errors.Is(err, domain.ErrBudgetExceeded) {
		t.Fatalf("new reservation after overrun = %v, want ErrBudgetExceeded", err)
	}
}

func TestExecutorTimeoutDoesNotImplicitlyReleaseReservation(t *testing.T) {
	svc, ctx, envelopeID := newLedger(t, 100)
	if _, err := svc.Reserve(ctx, envelopeID, 60, hardCapped()); err != nil {
		t.Fatal(err)
	}
	// No explicit Release/Settle/MarkUnresolved call: a caller timing out does not mutate the ledger.
	available, err := svc.Available(ctx, envelopeID)
	if err != nil {
		t.Fatal(err)
	}
	if available != 40 {
		t.Fatalf("available = %d, want 40", available)
	}
}
