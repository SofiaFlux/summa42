package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/purpose"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

// cardColumns lists the persistent columns of board_cards in lexical order.
const cardColumns = `card_id, mission_id, source, source_id, revision_id,
	column_name, title, description, required_capabilities_json, grant_json, created_at, updated_at`

const moveColumns = `move_id, card_id, from_column, to_column, actor, reason, evidence_ids_json, created_at`

type Service struct {
	store    *state.Store
	clock    clock.Clock
	purposes *purpose.Service
	evidence *evidence.Store
}

func NewService(store *state.Store, clk clock.Clock, purposes *purpose.Service, evidenceStore *evidence.Store) *Service {
	return &Service{
		store:    store,
		clock:    clk,
		purposes: purposes,
		evidence: evidenceStore,
	}
}

func (s *Service) DB() *sql.DB {
	if s == nil || s.store == nil {
		return nil
	}
	return s.store.DB()
}

func (s *Service) SyncItem(ctx context.Context, item Item) (Card, bool, error) {
	if s == nil || s.store == nil || s.purposes == nil || s.clock == nil {
		return Card{}, false, errors.New("board service is not configured")
	}
	item.MissionID = domain.ID(strings.TrimSpace(string(item.MissionID)))
	item.Source = strings.TrimSpace(item.Source)
	item.SourceID = strings.TrimSpace(item.SourceID)
	item.RevisionID = strings.TrimSpace(item.RevisionID)
	if item.MissionID == "" || item.Source == "" || item.SourceID == "" || item.RevisionID == "" {
		return Card{}, false, errors.New("mission, source, source ID, and revision ID are required")
	}

	capsJSON, err := json.Marshal(item.RequiredCapabilities)
	if err != nil {
		return Card{}, false, fmt.Errorf("encode required capabilities: %w", err)
	}
	grantJSON, err := json.Marshal(item.Grant)
	if err != nil {
		return Card{}, false, fmt.Errorf("encode grant: %w", err)
	}

	var result Card
	var created bool
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.purposes.ValidatePurposeTx(ctx, tx, domain.PurposeRef{Kind: domain.PurposeMission, ID: item.MissionID}); err != nil {
			return err
		}

		existing, err := scanCard(tx.QueryRowContext(ctx,
			`SELECT `+cardColumns+` FROM board_cards WHERE mission_id = ? AND source = ? AND source_id = ?`,
			item.MissionID, item.Source, item.SourceID))
		if err == nil {
			// Card already exists
			if existing.RevisionID == item.RevisionID {
				result = existing
				created = false
				return nil
			}
			// New revision: update metadata but keep existing column
			now := s.clock.Now().UTC()
			nowS := domain.FormatTime(now)
			_, err = tx.ExecContext(ctx, `
				UPDATE board_cards
				SET revision_id = ?, title = ?, description = ?, required_capabilities_json = ?, grant_json = ?, updated_at = ?
				WHERE card_id = ?`,
				item.RevisionID, item.Title, item.Description, string(capsJSON), string(grantJSON), nowS, existing.ID)
			if err != nil {
				return fmt.Errorf("update card revision: %w", err)
			}
			existing.RevisionID = item.RevisionID
			existing.Title = item.Title
			existing.Description = item.Description
			existing.RequiredCapabilities = append([]string(nil), item.RequiredCapabilities...)
			existing.Grant = item.Grant
			existing.UpdatedAt = now
			result = existing
			created = false
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		// Insert new card in INTAKE column
		cardID := domain.NewID("card")
		moveID := domain.NewID("move")
		now := s.clock.Now().UTC()
		nowS := domain.FormatTime(now)

		_, err = tx.ExecContext(ctx, `
			INSERT INTO board_cards (
				card_id, mission_id, source, source_id, revision_id, column_name,
				title, description, required_capabilities_json, grant_json, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			cardID, item.MissionID, item.Source, item.SourceID, item.RevisionID, ColumnIntake,
			item.Title, item.Description, string(capsJSON), string(grantJSON), nowS, nowS)
		if err != nil {
			return fmt.Errorf("insert card: %w", err)
		}

		// Record initial creation move
		_, err = tx.ExecContext(ctx, `
			INSERT INTO board_card_moves (
				move_id, card_id, from_column, to_column, actor, reason, evidence_ids_json, created_at
			) VALUES (?, ?, '', ?, 'system:intake', 'Initial intake', '[]', ?)`,
			moveID, cardID, ColumnIntake, nowS)
		if err != nil {
			return fmt.Errorf("insert initial card move: %w", err)
		}

		result = Card{
			ID:                   cardID,
			MissionID:            item.MissionID,
			Source:               item.Source,
			SourceID:             item.SourceID,
			RevisionID:           item.RevisionID,
			Column:               ColumnIntake,
			Title:                item.Title,
			Description:          item.Description,
			RequiredCapabilities: append([]string(nil), item.RequiredCapabilities...),
			Grant:                item.Grant,
			CreatedAt:            now,
			UpdatedAt:            now,
		}
		created = true
		return nil
	})
	if err != nil {
		return Card{}, false, err
	}
	return result, created, nil
}

func (s *Service) MoveCard(ctx context.Context, req MoveRequest) (Card, Move, error) {
	if s == nil || s.store == nil || s.purposes == nil || s.clock == nil {
		return Card{}, Move{}, errors.New("board service is not configured")
	}
	req.CardID = domain.ID(strings.TrimSpace(string(req.CardID)))
	req.Actor = strings.TrimSpace(req.Actor)
	req.Reason = strings.TrimSpace(req.Reason)
	if req.CardID == "" || req.Actor == "" || req.Reason == "" {
		return Card{}, Move{}, errors.New("card ID, actor, and reason are required")
	}
	if !IsValidColumn(req.ToColumn) {
		return Card{}, Move{}, fmt.Errorf("invalid destination column %q", req.ToColumn)
	}

	var updatedCard Card
	var recordedMove Move

	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		card, err := scanCard(tx.QueryRowContext(ctx, `SELECT `+cardColumns+` FROM board_cards WHERE card_id = ?`, req.CardID))
		if err != nil {
			return fmt.Errorf("get card %s: %w", req.CardID, err)
		}

		if card.Column == req.ToColumn {
			return fmt.Errorf("card %s is already in column %s", card.ID, req.ToColumn)
		}
		if !isAllowedColumnTransition(card.Column, req.ToColumn) {
			return fmt.Errorf("invalid card transition from %s to %s", card.Column, req.ToColumn)
		}

		if err := s.purposes.ValidatePurposeTx(ctx, tx, domain.PurposeRef{Kind: domain.PurposeMission, ID: card.MissionID}); err != nil {
			return err
		}

		// Gate: moving to IN_PROGRESS requires mission and grant covering required capabilities
		if req.ToColumn == ColumnInProgress {
			for _, cap := range card.RequiredCapabilities {
				if !containsCapability(card.Grant.Capabilities, cap) {
					return fmt.Errorf("gate denied: card %s requires capability %q which is not in grant", card.ID, cap)
				}
			}
		}

		// Verify evidence IDs exist if cited
		for _, evID := range req.EvidenceIDs {
			var found int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM evidence_objects WHERE evidence_id = ?`, evID).Scan(&found); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("card move cites unknown evidence %q", evID)
				}
				return err
			}
		}

		evJSON, err := json.Marshal(req.EvidenceIDs)
		if err != nil {
			return fmt.Errorf("encode evidence IDs: %w", err)
		}

		now := s.clock.Now().UTC()
		nowS := domain.FormatTime(now)
		moveID := domain.NewID("move")

		// Record the move in the audit log
		_, err = tx.ExecContext(ctx, `
			INSERT INTO board_card_moves (
				move_id, card_id, from_column, to_column, actor, reason, evidence_ids_json, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			moveID, card.ID, card.Column, req.ToColumn, req.Actor, req.Reason, string(evJSON), nowS)
		if err != nil {
			return fmt.Errorf("record card move: %w", err)
		}

		// Update card's column and timestamp
		_, err = tx.ExecContext(ctx, `
			UPDATE board_cards
			SET column_name = ?, updated_at = ?
			WHERE card_id = ?`,
			req.ToColumn, nowS, card.ID)
		if err != nil {
			return fmt.Errorf("update card column: %w", err)
		}

		recordedMove = Move{
			ID:          moveID,
			CardID:      card.ID,
			FromColumn:  card.Column,
			ToColumn:    req.ToColumn,
			Actor:       req.Actor,
			Reason:      req.Reason,
			EvidenceIDs: append([]domain.ID(nil), req.EvidenceIDs...),
			CreatedAt:   now,
		}

		card.Column = req.ToColumn
		card.UpdatedAt = now
		updatedCard = card
		return nil
	})
	if err != nil {
		return Card{}, Move{}, err
	}
	return updatedCard, recordedMove, nil
}

func isAllowedColumnTransition(from, to Column) bool {
	if from == to {
		return false
	}
	switch from {
	case ColumnIntake:
		return to == ColumnTriage || to == ColumnHeld
	case ColumnTriage:
		return to == ColumnReady || to == ColumnHeld
	case ColumnReady:
		return to == ColumnInProgress || to == ColumnTriage || to == ColumnHeld
	case ColumnInProgress:
		return to == ColumnReview || to == ColumnReady || to == ColumnHeld
	case ColumnReview:
		return to == ColumnDone || to == ColumnInProgress || to == ColumnTriage || to == ColumnHeld
	case ColumnDone:
		return to == ColumnHeld
	case ColumnHeld:
		return to == ColumnIntake || to == ColumnTriage || to == ColumnReady || to == ColumnInProgress || to == ColumnReview
	default:
		return false
	}
}

func (s *Service) GetCard(ctx context.Context, cardID domain.ID) (Card, error) {
	if s == nil || s.store == nil {
		return Card{}, errors.New("board service is not configured")
	}
	cardID = domain.ID(strings.TrimSpace(string(cardID)))
	if cardID == "" {
		return Card{}, errors.New("card ID is required")
	}
	return scanCard(s.store.DB().QueryRowContext(ctx, `SELECT `+cardColumns+` FROM board_cards WHERE card_id = ?`, cardID))
}

func (s *Service) GetCardBySource(ctx context.Context, missionID domain.ID, source, sourceID string) (Card, error) {
	if s == nil || s.store == nil {
		return Card{}, errors.New("board service is not configured")
	}
	missionID = domain.ID(strings.TrimSpace(string(missionID)))
	source = strings.TrimSpace(source)
	sourceID = strings.TrimSpace(sourceID)
	if missionID == "" || source == "" || sourceID == "" {
		return Card{}, errors.New("mission ID, source, and source ID are required")
	}
	return scanCard(s.store.DB().QueryRowContext(ctx,
		`SELECT `+cardColumns+` FROM board_cards WHERE mission_id = ? AND source = ? AND source_id = ?`,
		missionID, source, sourceID))
}

func (s *Service) ListCards(ctx context.Context, missionID domain.ID, column Column) ([]Card, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("board service is not configured")
	}
	missionID = domain.ID(strings.TrimSpace(string(missionID)))
	if missionID == "" {
		return nil, errors.New("mission ID is required")
	}

	var rows *sql.Rows
	var err error
	if column != "" {
		rows, err = s.store.DB().QueryContext(ctx,
			`SELECT `+cardColumns+` FROM board_cards WHERE mission_id = ? AND column_name = ? ORDER BY created_at, rowid`,
			missionID, column)
	} else {
		rows, err = s.store.DB().QueryContext(ctx,
			`SELECT `+cardColumns+` FROM board_cards WHERE mission_id = ? ORDER BY created_at, rowid`,
			missionID)
	}
	if err != nil {
		return nil, fmt.Errorf("list cards: %w", err)
	}
	defer rows.Close()

	cards := make([]Card, 0)
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			return nil, fmt.Errorf("scan card: %w", err)
		}
		cards = append(cards, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list cards: %w", err)
	}
	return cards, nil
}

func (s *Service) ListMoves(ctx context.Context, cardID domain.ID) ([]Move, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("board service is not configured")
	}
	cardID = domain.ID(strings.TrimSpace(string(cardID)))
	if cardID == "" {
		return nil, errors.New("card ID is required")
	}
	rows, err := s.store.DB().QueryContext(ctx,
		`SELECT `+moveColumns+` FROM board_card_moves WHERE card_id = ? ORDER BY created_at, rowid`, cardID)
	if err != nil {
		return nil, fmt.Errorf("list card moves: %w", err)
	}
	defer rows.Close()

	moves := make([]Move, 0)
	for rows.Next() {
		m, err := scanMove(rows)
		if err != nil {
			return nil, fmt.Errorf("scan card move: %w", err)
		}
		moves = append(moves, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list card moves: %w", err)
	}
	return moves, nil
}

func (s *Service) OwnerSummary(ctx context.Context, missionID domain.ID) (OwnerSummary, error) {
	if s == nil || s.store == nil {
		return OwnerSummary{}, errors.New("board service is not configured")
	}
	missionID = domain.ID(strings.TrimSpace(string(missionID)))
	if missionID == "" {
		return OwnerSummary{}, errors.New("mission ID is required")
	}

	summary := OwnerSummary{
		MissionID:    missionID,
		ColumnCounts: make(map[Column]int),
		HeldCards:    make([]HeldCardInfo, 0),
	}

	rows, err := s.store.DB().QueryContext(ctx,
		`SELECT column_name, count(*) FROM board_cards WHERE mission_id = ? GROUP BY column_name`, missionID)
	if err != nil {
		return OwnerSummary{}, fmt.Errorf("count cards by column: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var col Column
		var count int
		if err := rows.Scan(&col, &count); err != nil {
			return OwnerSummary{}, fmt.Errorf("scan column count: %w", err)
		}
		summary.ColumnCounts[col] = count
		summary.TotalCards += count
	}
	if err := rows.Err(); err != nil {
		return OwnerSummary{}, fmt.Errorf("count cards by column: %w", err)
	}

	// Fetch details for HELD cards with latest move reason
	heldRows, err := s.store.DB().QueryContext(ctx, `
		SELECT c.card_id, c.source, c.source_id, c.title, m.reason, m.created_at
		FROM board_cards c
		JOIN board_card_moves m ON m.card_id = c.card_id
		WHERE c.mission_id = ? AND c.column_name = ? AND m.to_column = ?
		ORDER BY m.created_at DESC, m.rowid DESC`, missionID, ColumnHeld, ColumnHeld)
	if err != nil {
		return OwnerSummary{}, fmt.Errorf("query held cards: %w", err)
	}
	defer heldRows.Close()

	seenHeld := make(map[domain.ID]bool)
	for heldRows.Next() {
		var info HeldCardInfo
		var heldAt string
		if err := heldRows.Scan(&info.CardID, &info.Source, &info.SourceID, &info.Title, &info.Reason, &heldAt); err != nil {
			return OwnerSummary{}, fmt.Errorf("scan held card info: %w", err)
		}
		if seenHeld[info.CardID] {
			continue
		}
		seenHeld[info.CardID] = true
		info.HeldAt, _ = parseTime(heldAt)
		summary.HeldCards = append(summary.HeldCards, info)
	}
	if err := heldRows.Err(); err != nil {
		return OwnerSummary{}, fmt.Errorf("query held cards: %w", err)
	}

	return summary, nil
}

type rowScanner interface{ Scan(...any) error }

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(domain.TimeLayout, s)
	if err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

func scanCard(row rowScanner) (Card, error) {
	var c Card
	var capsJSON, grantJSON, createdS, updatedS string
	err := row.Scan(&c.ID, &c.MissionID, &c.Source, &c.SourceID, &c.RevisionID,
		&c.Column, &c.Title, &c.Description, &capsJSON, &grantJSON, &createdS, &updatedS)
	if err != nil {
		return Card{}, err
	}
	if err := json.Unmarshal([]byte(capsJSON), &c.RequiredCapabilities); err != nil {
		return Card{}, fmt.Errorf("decode required capabilities: %w", err)
	}
	if err := json.Unmarshal([]byte(grantJSON), &c.Grant); err != nil {
		return Card{}, fmt.Errorf("decode grant: %w", err)
	}
	c.CreatedAt, _ = parseTime(createdS)
	c.UpdatedAt, _ = parseTime(updatedS)
	return c, nil
}

func scanMove(row rowScanner) (Move, error) {
	var m Move
	var evJSON, createdS string
	err := row.Scan(&m.ID, &m.CardID, &m.FromColumn, &m.ToColumn, &m.Actor, &m.Reason, &evJSON, &createdS)
	if err != nil {
		return Move{}, err
	}
	if err := json.Unmarshal([]byte(evJSON), &m.EvidenceIDs); err != nil {
		return Move{}, fmt.Errorf("decode evidence IDs: %w", err)
	}
	m.CreatedAt, _ = parseTime(createdS)
	return m, nil
}

func containsCapability(caps []string, target string) bool {
	for _, c := range caps {
		if c == target {
			return true
		}
	}
	return false
}
