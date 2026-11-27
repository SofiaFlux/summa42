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
	ancestors, err := ancestorsTx(ctx, tx, envelopeID)
	if err != nil {
		return domain.Reservation{}, err
	}
	available, err := availableIn(ctx, tx, envelopeID)
	if err != nil {
		return domain.Reservation{}, err
	}
	if amount > available {
		return domain.Reservation{}, fmt.Errorf("%w: requested=%d available=%d", domain.ErrBudgetExceeded, amount, available)
	}
	for _, anc := range ancestors {
		ancLimit, _, _, _, err := budgetIn(ctx, tx, anc)
		if err != nil {
			return domain.Reservation{}, err
		}
		sAmt, hAmt, uAmt, err := subtreeTotalsTx(ctx, tx, anc)
		if err != nil {
			return domain.Reservation{}, err
		}
		ancAvail := ancLimit - sAmt - hAmt - uAmt
		if amount > ancAvail {
			return domain.Reservation{}, fmt.Errorf("%w: ancestor %s requested=%d available=%d", domain.ErrBudgetExceeded, anc, amount, ancAvail)
		}
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

// AvailableInTx returns remaining capacity on envelopeID inside the caller's
// transaction so lease-renewal and commit boundaries share the same snapshot.
func (s *Service) AvailableInTx(ctx context.Context, tx *sql.Tx, envelopeID domain.ID) (int64, error) {
	if err := s.configured(); err != nil {
		return 0, err
	}
	if tx == nil {
		return 0, errors.New("available budget check requires transaction")
	}
	if envelopeID == "" {
		return 0, errors.New("envelope id is required")
	}
	return availableIn(ctx, tx, envelopeID)
}

// AllowRenewalInTx reports whether envelopeID and every ancestor still permit
// a lease renewal inside tx: settled + unresolved exposure must remain strictly
// below hard_limit, and total settled + unresolved + held must not exceed
// hard_limit. Active HELD reservations inside hard_limit still allow renewal.
func (s *Service) AllowRenewalInTx(ctx context.Context, tx *sql.Tx, envelopeID domain.ID) (bool, error) {
	if err := s.configured(); err != nil {
		return false, err
	}
	if tx == nil {
		return false, errors.New("renewal budget check requires transaction")
	}
	if envelopeID == "" {
		return false, errors.New("envelope id is required")
	}
	ancestors, err := ancestorsTx(ctx, tx, envelopeID)
	if err != nil {
		return false, err
	}
	hardLimit, settled, held, unresolved, err := budgetIn(ctx, tx, envelopeID)
	if err != nil {
		return false, err
	}
	if settled+unresolved >= hardLimit || settled+unresolved+held > hardLimit {
		return false, nil
	}
	for _, anc := range ancestors {
		ancLimit, _, _, _, err := budgetIn(ctx, tx, anc)
		if err != nil {
			return false, err
		}
		sAmt, hAmt, uAmt, err := subtreeTotalsTx(ctx, tx, anc)
		if err != nil {
			return false, err
		}
		if sAmt+uAmt >= ancLimit || sAmt+uAmt+hAmt > ancLimit {
			return false, nil
		}
	}
	return true, nil
}

const maxEnvelopeDepth = 8

func ancestorsTx(ctx context.Context, q queryRower, envelopeID domain.ID) ([]domain.ID, error) {
	curr := envelopeID
	seen := map[domain.ID]bool{envelopeID: true}
	var ancestors []domain.ID
	for {
		var parent sql.NullString
		err := q.QueryRowContext(ctx, `SELECT parent_envelope_id FROM resource_envelopes WHERE envelope_id = ?`, curr).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("resource envelope %q not found", curr)
		}
		if err != nil {
			return nil, err
		}
		if !parent.Valid || strings.TrimSpace(parent.String) == "" {
			return ancestors, nil
		}
		parentID := domain.ID(strings.TrimSpace(parent.String))
		if seen[parentID] {
			return nil, fmt.Errorf("resource envelope hierarchy cycle detected at %s", parentID)
		}
		if len(ancestors) >= maxEnvelopeDepth {
			return nil, fmt.Errorf("resource envelope hierarchy exceeds maximum depth %d", maxEnvelopeDepth)
		}
		seen[parentID] = true
		ancestors = append(ancestors, parentID)
		curr = parentID
	}
}

func subtreeTotalsTx(ctx context.Context, tx *sql.Tx, rootID domain.ID) (settled, held, unresolved int64, err error) {
	type node struct {
		id    domain.ID
		depth int
	}
	queue := []node{{id: rootID, depth: 0}}
	seen := map[domain.ID]bool{rootID: true}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.depth > maxEnvelopeDepth {
			return 0, 0, 0, fmt.Errorf("resource envelope subtree exceeds maximum depth %d", maxEnvelopeDepth)
		}
		_, sAmt, hAmt, uAmt, err := budgetIn(ctx, tx, cur.id)
		if err != nil {
			return 0, 0, 0, err
		}
		settled += sAmt
		held += hAmt
		unresolved += uAmt

		rows, err := tx.QueryContext(ctx, `SELECT envelope_id FROM resource_envelopes WHERE parent_envelope_id = ?`, cur.id)
		if err != nil {
			return 0, 0, 0, err
		}
		for rows.Next() {
			var childID domain.ID
			if scanErr := rows.Scan(&childID); scanErr != nil {
				rows.Close()
				return 0, 0, 0, scanErr
			}
			if seen[childID] {
				rows.Close()
				return 0, 0, 0, fmt.Errorf("resource envelope hierarchy cycle detected at %s", childID)
			}
			seen[childID] = true
			queue = append(queue, node{id: childID, depth: cur.depth + 1})
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			rows.Close()
			return 0, 0, 0, rowsErr
		}
		rows.Close()
	}
	return settled, held, unresolved, nil
}

func childrenLimitSumTx(ctx context.Context, tx *sql.Tx, parentID, excludeChildID domain.ID) (int64, error) {
	var total int64
	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(hard_limit), 0) FROM resource_envelopes WHERE parent_envelope_id = ? AND envelope_id <> ?`,
		parentID, excludeChildID,
	).Scan(&total)
	return total, err
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func budgetIn(ctx context.Context, q queryRower, envelopeID domain.ID) (hardLimit, settled, held, unresolved int64, err error) {
	err = q.QueryRowContext(ctx, `
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
		return 0, 0, 0, 0, fmt.Errorf("resource envelope %q not found", envelopeID)
	}
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return hardLimit, settled, held, unresolved, nil
}

func availableIn(ctx context.Context, q queryRower, envelopeID domain.ID) (int64, error) {
	hardLimit, settled, held, unresolved, err := budgetIn(ctx, q, envelopeID)
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
