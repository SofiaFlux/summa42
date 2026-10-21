package resources

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type CostControl string

const (
	CostTechnicallyCapped CostControl = "TECHNICALLY_CAPPED"
	CostPrepaidQuota      CostControl = "PREPAID_QUOTA"
	CostVerifiedStop      CostControl = "VERIFIED_STOP"
	CostEstimatedOnly     CostControl = "ESTIMATED_ONLY"
	CostPotentiallyOpen   CostControl = "POTENTIALLY_UNBOUNDED"
)

var ErrHardCapUnavailable = errors.New("required hard cost ceiling is not technically enforceable")

type Enforceability struct {
	CostControl    CostControl
	RequireHardCap bool
	Source         string
}

type Service struct {
	store *state.Store
	clock clock.Clock
}

func New(store *state.Store, clk clock.Clock) *Service {
	return &Service{store: store, clock: clk}
}

// ReserveInTx applies the same ledger invariants as Reserve while participating
// in a caller-owned trusted transaction. It exists so commit-boundary services
// can bind budget exposure atomically to their own durable state.
func (s *Service) ReserveInTx(ctx context.Context, tx *sql.Tx, envelopeID domain.ID, amount int64, enforceability Enforceability) (domain.Reservation, error) {
	if err := s.configured(); err != nil {
		return domain.Reservation{}, err
	}
	if tx == nil {
		return domain.Reservation{}, errors.New("resource reservation requires transaction")
	}
	if err := validateReservationRequest(envelopeID, amount, enforceability); err != nil {
		return domain.Reservation{}, err
	}

	now := s.clock.Now().UTC()
	reservation := domain.Reservation{
		ID: domain.NewID("reservation"), EnvelopeID: envelopeID, State: domain.ReservationHeld,
		Amount: amount, CreatedAt: now, UpdatedAt: now,
	}
	available, err := availableIn(ctx, tx, envelopeID)
	if err != nil {
		return domain.Reservation{}, err
	}
	if amount > available {
		return domain.Reservation{}, fmt.Errorf("%w: requested=%d available=%d", domain.ErrBudgetExceeded, amount, available)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO resource_reservations(
			reservation_id, envelope_id, state, reserved_amount, settled_amount,
			cost_control, cost_source, require_hard_cap, created_at, updated_at
		) VALUES (?, ?, ?, ?, NULL, ?, ?, ?, ?, ?)`,
		reservation.ID, reservation.EnvelopeID, reservation.State, reservation.Amount,
		enforceability.CostControl, strings.TrimSpace(enforceability.Source), boolInt(enforceability.RequireHardCap),
		formatTime(now), formatTime(now),
	); err != nil {
		return domain.Reservation{}, err
	}
	return reservation, nil
}

func (s *Service) SettleInTx(ctx context.Context, tx *sql.Tx, reservationID domain.ID, actualAmount int64) error {
	if err := s.configured(); err != nil {
		return err
	}
	if tx == nil {
		return errors.New("resource settlement requires transaction")
	}
	if reservationID == "" || actualAmount < 0 {
		return errors.New("reservation id and non-negative actual amount are required")
	}
	now := s.clock.Now().UTC()
	stateValue, settledAmount, reservedAmount, err := loadReservationState(ctx, tx, reservationID)
	if err != nil {
		return err
	}
	switch stateValue {
	case domain.ReservationSettled:
		if settledAmount.Valid && settledAmount.Int64 == actualAmount {
			return nil
		}
		return errors.New("reservation already settled with a different actual amount")
	case domain.ReservationHeld, domain.ReservationUnresolved:
		_, err := tx.ExecContext(ctx,
			`UPDATE resource_reservations SET state = ?, settled_amount = ?, over_settled = ?, updated_at = ? WHERE reservation_id = ?`,
			domain.ReservationSettled, actualAmount, boolInt(actualAmount > reservedAmount), formatTime(now), reservationID,
		)
		return err
	default:
		return fmt.Errorf("cannot settle reservation in state %s", stateValue)
	}
}

func (s *Service) MarkUnresolvedInTx(ctx context.Context, tx *sql.Tx, reservationID domain.ID) error {
	if err := s.configured(); err != nil {
		return err
	}
	if tx == nil {
		return errors.New("mark unresolved requires transaction")
	}
	if reservationID == "" {
		return errors.New("reservation id is required")
	}
	now := s.clock.Now().UTC()
	stateValue, _, _, err := loadReservationState(ctx, tx, reservationID)
	if err != nil {
		return err
	}
	switch stateValue {
	case domain.ReservationUnresolved:
		return nil
	case domain.ReservationHeld:
		_, err := tx.ExecContext(ctx,
			`UPDATE resource_reservations SET state = ?, updated_at = ? WHERE reservation_id = ?`,
			domain.ReservationUnresolved, formatTime(now), reservationID,
		)
		return err
	default:
		return fmt.Errorf("cannot mark reservation unresolved from state %s", stateValue)
	}
}

func (s *Service) ReleaseInTx(ctx context.Context, tx *sql.Tx, reservationID domain.ID) error {
	if err := s.configured(); err != nil {
		return err
	}
	if tx == nil {
		return errors.New("resource release requires transaction")
	}
	if reservationID == "" {
		return errors.New("reservation id is required")
	}
	now := s.clock.Now().UTC()
	stateValue, _, _, err := loadReservationState(ctx, tx, reservationID)
	if err != nil {
		return err
	}
	switch stateValue {
	case domain.ReservationReleased:
		return nil
	case domain.ReservationHeld:
		_, err := tx.ExecContext(ctx,
			`UPDATE resource_reservations SET state = ?, updated_at = ? WHERE reservation_id = ?`,
			domain.ReservationReleased, formatTime(now), reservationID,
		)
		return err
	default:
		return fmt.Errorf("cannot release reservation in state %s", stateValue)
	}
}

func (s *Service) Available(ctx context.Context, envelopeID domain.ID) (int64, error) {
	if err := s.configured(); err != nil {
		return 0, err
	}
	if envelopeID == "" {
		return 0, errors.New("envelope id is required")
	}
	return availableIn(ctx, s.store.DB(), envelopeID)
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func availableIn(ctx context.Context, q queryRower, envelopeID domain.ID) (int64, error) {
	var hardLimit, settled, held, unresolved int64
	err := q.QueryRowContext(ctx, `
		SELECT e.hard_limit,
		       COALESCE(SUM(CASE WHEN r.state = 'SETTLED' THEN r.settled_amount ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN r.state = 'HELD' THEN r.reserved_amount ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN r.state = 'UNRESOLVED' THEN r.reserved_amount ELSE 0 END), 0)
		FROM resource_envelopes e
		LEFT JOIN resource_reservations r ON r.envelope_id = e.envelope_id
		WHERE e.envelope_id = ?
		GROUP BY e.envelope_id, e.hard_limit`, envelopeID,
	).Scan(&hardLimit, &settled, &held, &unresolved)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("resource envelope %q not found", envelopeID)
	}
	if err != nil {
		return 0, err
	}
	return hardLimit - settled - held - unresolved, nil
}

func loadReservationState(ctx context.Context, tx *sql.Tx, reservationID domain.ID) (domain.ReservationState, sql.NullInt64, int64, error) {
	var stateValue domain.ReservationState
	var settled sql.NullInt64
	var reserved int64
	if err := tx.QueryRowContext(ctx,
		`SELECT state, settled_amount, reserved_amount FROM resource_reservations WHERE reservation_id = ?`, reservationID,
	).Scan(&stateValue, &settled, &reserved); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", sql.NullInt64{}, 0, fmt.Errorf("reservation %q not found", reservationID)
		}
		return "", sql.NullInt64{}, 0, err
	}
	return stateValue, settled, reserved, nil
}

func validateReservationRequest(envelopeID domain.ID, amount int64, e Enforceability) error {
	if envelopeID == "" || amount <= 0 {
		return errors.New("envelope id and positive reservation amount are required")
	}
	if !e.CostControl.valid() {
		return fmt.Errorf("invalid cost control %q", e.CostControl)
	}
	if strings.TrimSpace(e.Source) == "" {
		return errors.New("cost-control source is required")
	}
	if e.RequireHardCap && !e.CostControl.providesHardCeiling() {
		return fmt.Errorf("%w: cost_control=%s", ErrHardCapUnavailable, e.CostControl)
	}
	return nil
}

func (c CostControl) valid() bool {
	switch c {
	case CostTechnicallyCapped, CostPrepaidQuota, CostVerifiedStop, CostEstimatedOnly, CostPotentiallyOpen:
		return true
	default:
		return false
	}
}

func (c CostControl) providesHardCeiling() bool {
	switch c {
	case CostTechnicallyCapped, CostPrepaidQuota, CostVerifiedStop:
		return true
	default:
		return false
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func formatTime(value time.Time) string { return domain.FormatTime(value) }

func (s *Service) configured() error {
	if s == nil || s.store == nil || s.clock == nil {
		return errors.New("resource ledger is not configured")
	}
	return nil
}
