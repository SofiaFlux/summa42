-- +goose Up
-- The Collective manages its own work queue via a Kanban board (#124).
CREATE TABLE board_cards (
    card_id                    TEXT PRIMARY KEY,
    mission_id                 TEXT NOT NULL REFERENCES missions(mission_id),
    source                     TEXT NOT NULL,
    source_id                  TEXT NOT NULL,
    revision_id                TEXT NOT NULL,
    column_name                TEXT NOT NULL CHECK (column_name IN ('INTAKE', 'TRIAGE', 'READY', 'IN_PROGRESS', 'REVIEW', 'DONE', 'HELD')),
    title                      TEXT NOT NULL DEFAULT '',
    description                TEXT NOT NULL DEFAULT '',
    required_capabilities_json TEXT NOT NULL DEFAULT '[]',
    grant_json                 TEXT NOT NULL DEFAULT '{}',
    created_at                 TEXT NOT NULL,
    updated_at                 TEXT NOT NULL,
    UNIQUE (mission_id, source, source_id)
);

CREATE INDEX board_cards_mission_column ON board_cards(mission_id, column_name);

CREATE TABLE board_card_moves (
    move_id           TEXT PRIMARY KEY,
    card_id           TEXT NOT NULL REFERENCES board_cards(card_id),
    from_column       TEXT NOT NULL,
    to_column         TEXT NOT NULL,
    actor             TEXT NOT NULL,
    reason            TEXT NOT NULL,
    evidence_ids_json TEXT NOT NULL DEFAULT '[]',
    created_at        TEXT NOT NULL
);

CREATE INDEX board_card_moves_card_created ON board_card_moves(card_id, created_at);

-- +goose Down
DROP TABLE IF EXISTS board_card_moves;
DROP TABLE IF EXISTS board_cards;
