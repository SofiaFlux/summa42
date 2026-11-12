-- +goose Up
-- Owner notifications. One row per event, keyed by what happened, so an event
-- is queued once. A row is marked SENDING before the message leaves; a send
-- whose outcome is not known is kept as UNKNOWN and never sent again.
CREATE TABLE notification_outbox (
    event_key TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    message TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('PENDING', 'SENDING', 'DELIVERED', 'UNKNOWN')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX notification_outbox_state ON notification_outbox(state, created_at);

-- +goose Down
DROP INDEX notification_outbox_state;
DROP TABLE notification_outbox;
