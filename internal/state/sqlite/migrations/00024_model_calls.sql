-- +goose Up
-- One row per model call. Pool state is derived from these rows, so there is
-- no second table that could drift from them.
CREATE TABLE model_calls (
    call_id       INTEGER PRIMARY KEY AUTOINCREMENT,
    pool          TEXT NOT NULL,
    harness       TEXT NOT NULL,
    model         TEXT NOT NULL,
    stage         TEXT NOT NULL,
    case_id       TEXT NOT NULL DEFAULT '',
    started_at    TEXT NOT NULL,
    duration_ms   INTEGER NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    outcome       TEXT NOT NULL CHECK (outcome IN ('ok', 'refused', 'empty', 'timeout', 'error', 'gated')),
    input_tokens  INTEGER CHECK (input_tokens IS NULL OR input_tokens >= 0),
    output_tokens INTEGER CHECK (output_tokens IS NULL OR output_tokens >= 0),
    reset_at      TEXT,
    reset_source  TEXT CHECK (reset_source IS NULL OR reset_source IN ('parsed', 'default')),
    message       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX model_calls_pool_call ON model_calls(pool, call_id);

-- +goose Down
DROP TABLE model_calls;
