package purpose

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

// Mission is a stored Collective Mission row. At most one may be active at once.
type Mission struct {
	ID            domain.ID  `json:"id"`
	Statement     string     `json:"statement"`
	Active        bool       `json:"active"`
	CreatedAt     time.Time  `json:"created_at"`
	DeactivatedAt *time.Time `json:"deactivated_at,omitempty"`
}

type Service struct {
	store *state.Store
	clock clock.Clock
}

func New(store *state.Store, clk clock.Clock) *Service {
	return &Service{store: store, clock: clk}
}

func (s *Service) CreateMission(ctx context.Context, statement string) (domain.ID, error) {
	return s.createMission(ctx, statement, true)
}

// CreateStandbyMission stores a non-active Mission that may later be activated
// by an Owner-signed switch command.
func (s *Service) CreateStandbyMission(ctx context.Context, statement string) (domain.ID, error) {
	return s.createMission(ctx, statement, false)
}

func (s *Service) createMission(ctx context.Context, statement string, active bool) (domain.ID, error) {
	if err := s.configured(); err != nil {
		return "", err
	}
	statement = strings.TrimSpace(statement)
	if statement == "" {
		return "", fmt.Errorf("%w: Mission statement is required", domain.ErrInvalidPurpose)
	}
	id := domain.NewID("mission")
	now := domain.FormatTime(s.clock.Now())
	activeFlag := 0
	if active {
		activeFlag = 1
	}
	if _, err := s.store.DB().ExecContext(ctx,
		`INSERT INTO missions(mission_id, statement, active, created_at) VALUES (?, ?, ?, ?)`,
		id, statement, activeFlag, now,
	); err != nil {
		return "", fmt.Errorf("create Mission: %w", err)
	}
	return id, nil
}

// ListMissions returns all stored Collective Missions, active first.
func (s *Service) ListMissions(ctx context.Context) ([]Mission, error) {
	if err := s.configured(); err != nil {
		return nil, err
	}
	rows, err := s.store.DB().QueryContext(ctx,
		`SELECT mission_id, statement, active, created_at, deactivated_at FROM missions ORDER BY active DESC, created_at DESC, mission_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list Missions: %w", err)
	}
	defer rows.Close()
	var out []Mission
	for rows.Next() {
		var m Mission
		var activeInt int
		var createdS string
		var deactS sql.NullString
		if err := rows.Scan(&m.ID, &m.Statement, &activeInt, &createdS, &deactS); err != nil {
			return nil, err
		}
		m.Active = activeInt == 1
		m.CreatedAt, err = time.Parse(time.RFC3339Nano, createdS)
		if err != nil {
			return nil, fmt.Errorf("parse Mission created_at: %w", err)
		}
		if deactS.Valid && deactS.String != "" {
			t, err := time.Parse(time.RFC3339Nano, deactS.String)
			if err != nil {
				return nil, fmt.Errorf("parse Mission deactivated_at: %w", err)
			}
			m.DeactivatedAt = &t
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SwitchMission atomically deactivates any currently active Mission and
// activates targetID inside one SQLite transaction. Callers must enforce Owner
// authorization before invoking it.
func (s *Service) SwitchMission(ctx context.Context, targetID domain.ID) error {
	if err := s.configured(); err != nil {
		return err
	}
	targetID = domain.ID(strings.TrimSpace(string(targetID)))
	if targetID == "" {
		return fmt.Errorf("%w: Mission id is required", domain.ErrInvalidPurpose)
	}
	now := domain.FormatTime(s.clock.Now())
	return s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var activeInt int
		err := tx.QueryRowContext(ctx, `SELECT active FROM missions WHERE mission_id = ?`, targetID).Scan(&activeInt)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: Mission %q not found", domain.ErrInvalidPurpose, targetID)
		}
		if err != nil {
			return err
		}
		if activeInt == 1 {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE missions SET active = 0, deactivated_at = ? WHERE active = 1`, now,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE missions SET active = 1, deactivated_at = NULL WHERE mission_id = ?`, targetID,
		); err != nil {
			return err
		}
		return nil
	})
}

func (s *Service) ActiveMission(ctx context.Context) (domain.ID, string, error) {
	if err := s.configured(); err != nil {
		return "", "", err
	}
	var id domain.ID
	var statement string
	err := s.store.DB().QueryRowContext(ctx,
		`SELECT mission_id, statement FROM missions WHERE active = 1`,
	).Scan(&id, &statement)
	if err != nil {
		return "", "", fmt.Errorf("read active Mission: %w", err)
	}
	return id, statement, nil
}

func (s *Service) DeactivateMission(ctx context.Context, missionID domain.ID) error {
	if err := s.configured(); err != nil {
		return err
	}
	if missionID == "" {
		return fmt.Errorf("%w: Mission id is required", domain.ErrInvalidPurpose)
	}
	now := domain.FormatTime(s.clock.Now())
	result, err := s.store.DB().ExecContext(ctx,
		`UPDATE missions SET active = 0, deactivated_at = ? WHERE mission_id = ? AND active = 1`,
		now, missionID,
	)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: active Mission %q not found", domain.ErrInvalidPurpose, missionID)
	}
	return nil
}

func (s *Service) CreateGoal(ctx context.Context, purpose domain.PurposeRef, statement string) (domain.ID, error) {
	if err := s.configured(); err != nil {
		return "", err
	}
	statement = strings.TrimSpace(statement)
	if statement == "" {
		return "", fmt.Errorf("%w: Goal statement is required", domain.ErrInvalidPurpose)
	}
	id := domain.NewID("goal")
	now := domain.FormatTime(s.clock.Now())
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if err := validatePurpose(ctx, tx, purpose); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO goals(goal_id, purpose_kind, purpose_id, statement, active, created_at) VALUES (?, ?, ?, ?, 1, ?)`,
			id, purpose.Kind, purpose.ID, statement, now,
		)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("create Goal: %w", err)
	}
	return id, nil
}

func (s *Service) CreateObligation(ctx context.Context, source, statement string) (domain.ID, error) {
	if err := s.configured(); err != nil {
		return "", err
	}
	source = strings.TrimSpace(source)
	statement = strings.TrimSpace(statement)
	if source == "" || statement == "" {
		return "", fmt.Errorf("%w: Obligation source and statement are required", domain.ErrInvalidPurpose)
	}
	id := domain.NewID("obligation")
	now := domain.FormatTime(s.clock.Now())
	if _, err := s.store.DB().ExecContext(ctx,
		`INSERT INTO obligations(obligation_id, source, statement, active, created_at) VALUES (?, ?, ?, 1, ?)`,
		id, source, statement, now,
	); err != nil {
		return "", fmt.Errorf("create Obligation: %w", err)
	}
	return id, nil
}

func (s *Service) ValidatePurpose(ctx context.Context, purpose domain.PurposeRef) error {
	if err := s.configured(); err != nil {
		return err
	}
	return validatePurpose(ctx, s.store.DB(), purpose)
}

func (s *Service) ValidatePurposeTx(ctx context.Context, tx *sql.Tx, purpose domain.PurposeRef) error {
	if err := s.configured(); err != nil {
		return err
	}
	if tx == nil {
		return errors.New("purpose validation requires transaction")
	}
	return validatePurpose(ctx, tx, purpose)
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func validatePurpose(ctx context.Context, q queryRower, purpose domain.PurposeRef) error {
	if !purpose.Kind.Valid() || purpose.ID == "" {
		return fmt.Errorf("%w: kind=%q id=%q", domain.ErrInvalidPurpose, purpose.Kind, purpose.ID)
	}

	var one int
	switch purpose.Kind {
	case domain.PurposeMission:
		err := q.QueryRowContext(ctx,
			`SELECT 1 FROM missions WHERE mission_id = ? AND active = 1`, purpose.ID,
		).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: active Mission %q not found", domain.ErrInvalidPurpose, purpose.ID)
		}
		if err != nil {
			return err
		}
	case domain.PurposeObligation:
		err := q.QueryRowContext(ctx,
			`SELECT 1 FROM obligations WHERE obligation_id = ? AND active = 1`, purpose.ID,
		).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: active Obligation %q not found", domain.ErrInvalidPurpose, purpose.ID)
		}
		if err != nil {
			return err
		}
	case domain.PurposeCollectiveMaintenance,
		domain.PurposeGovernance,
		domain.PurposeStrategicPulse,
		domain.PurposeRecovery,
		domain.PurposeOwnerDirective:
		// These are explicit non-Mission lineage classes. Their authority is decided
		// by policy/control-plane checks; PurposeRef itself never grants authority.
		return nil
	default:
		return fmt.Errorf("%w: unsupported purpose kind %q", domain.ErrInvalidPurpose, purpose.Kind)
	}
	return nil
}

func (s *Service) configured() error {
	if s == nil || s.store == nil || s.clock == nil {
		return errors.New("purpose service is not configured")
	}
	return nil
}
