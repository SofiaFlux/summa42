package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/capacity"
)

// maxMessage keeps refusal excerpts small; provider messages can be long.
const maxMessage = 300

// RecordCall appends one call to the usage ledger.
func (s *Store) RecordCall(ctx context.Context, c capacity.Call) error {
	var reset, source sql.NullString
	if !c.ResetAt.IsZero() {
		reset = sql.NullString{String: c.ResetAt.UTC().Format(time.RFC3339), Valid: true}
	}
	if c.ResetSource != "" {
		source = sql.NullString{String: c.ResetSource, Valid: true}
	}
	msg := c.Message
	if len(msg) > maxMessage {
		msg = msg[:maxMessage]
	}
	_, err := s.DB().ExecContext(ctx, `
		INSERT INTO model_calls(pool,harness,model,stage,case_id,started_at,duration_ms,outcome,
			input_tokens,output_tokens,reset_at,reset_source,message)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.Pool, c.Harness, c.Model, c.Stage, c.CaseID, c.StartedAt.UTC().Format(time.RFC3339),
		c.Duration.Milliseconds(), string(c.Outcome), c.InputTokens, c.OutputTokens,
		reset, source, strings.ToValidUTF8(msg, ""))
	return err
}

// PoolState reports a pool as exhausted when its latest refusal has a reset
// time after now and no successful call came after that refusal.
func (s *Store) PoolState(ctx context.Context, pool string, now time.Time) (capacity.PoolState, error) {
	state := capacity.PoolState{Pool: pool}
	var refusalID int64
	var reset sql.NullString
	err := s.DB().QueryRowContext(ctx, `
		SELECT call_id, reset_at FROM model_calls
		WHERE pool=? AND outcome='refused' ORDER BY call_id DESC LIMIT 1`, pool).Scan(&refusalID, &reset)
	if errors.Is(err, sql.ErrNoRows) {
		return s.degradation(ctx, pool, now, state)
	}
	if err != nil {
		return state, err
	}
	var okAfter int
	if err := s.DB().QueryRowContext(ctx, `
		SELECT count(*) FROM model_calls WHERE pool=? AND outcome='ok' AND call_id>?`,
		pool, refusalID).Scan(&okAfter); err != nil {
		return state, err
	}
	if okAfter > 0 || !reset.Valid {
		return state, nil
	}
	until, err := time.Parse(time.RFC3339, reset.String)
	if err != nil {
		return state, err
	}
	if until.After(now) {
		state.Exhausted = true
		state.ResetAt = until
		return state, nil
	}
	return s.degradation(ctx, pool, now, state)
}

// Timeouts this many in degradedWindow, with no success after the first of them,
// mark a pool as not answering.
const (
	degradedTimeouts = 3
	degradedWindow   = time.Hour
)

func (s *Store) degradation(ctx context.Context, pool string, now time.Time, state capacity.PoolState) (capacity.PoolState, error) {
	var lastOK int64
	if err := s.DB().QueryRowContext(ctx,
		`SELECT coalesce(max(call_id), 0) FROM model_calls WHERE pool=? AND outcome='ok'`, pool).Scan(&lastOK); err != nil {
		return state, err
	}
	since := now.Add(-degradedWindow).UTC().Format(time.RFC3339)
	rows, err := s.DB().QueryContext(ctx, `
		SELECT started_at FROM model_calls
		WHERE pool=? AND outcome='timeout' AND call_id>? AND started_at>=?
		ORDER BY call_id`, pool, lastOK, since)
	if err != nil {
		return state, err
	}
	defer rows.Close()
	var latest string
	n := 0
	for rows.Next() {
		if err := rows.Scan(&latest); err != nil {
			return state, err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return state, err
	}
	if n >= degradedTimeouts {
		last, err := time.Parse(time.RFC3339, latest)
		if err != nil {
			return state, err
		}
		state.Degraded = true
		state.ResetAt = last.Add(degradedWindow)
	}
	return state, nil
}
