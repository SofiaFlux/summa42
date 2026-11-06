-- +goose Up
CREATE TABLE cubes (
    cube_id TEXT PRIMARY KEY,
    principal_id TEXT NOT NULL REFERENCES principals(principal_id),
    state TEXT NOT NULL CHECK (state IN ('ACTIVE', 'QUARANTINED', 'DRAINING', 'TERMINATED')),
    quarantine_reason TEXT,
    quarantined_at TEXT,
    quarantined_by TEXT REFERENCES principals(principal_id),
    quarantine_mode TEXT CHECK (quarantine_mode IN ('PASSIVE_WAIT', 'ACTIVE_RECON') OR quarantine_mode IS NULL),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX cubes_state ON cubes(state);

CREATE TABLE cube_quarantine_events (
    event_id TEXT PRIMARY KEY,
    cube_id TEXT NOT NULL REFERENCES cubes(cube_id),
    action TEXT NOT NULL CHECK (action IN ('QUARANTINE', 'UNQUARANTINE')),
    reason TEXT NOT NULL,
    enforced_by_principal_id TEXT NOT NULL REFERENCES principals(principal_id),
    mode TEXT CHECK (mode IN ('PASSIVE_WAIT', 'ACTIVE_RECON') OR mode IS NULL),
    created_at TEXT NOT NULL
);

CREATE INDEX cube_quarantine_events_cube ON cube_quarantine_events(cube_id, created_at ASC);

-- +goose StatementBegin
CREATE TRIGGER cube_quarantine_events_no_update
BEFORE UPDATE ON cube_quarantine_events
BEGIN
    SELECT RAISE(ABORT, 'quarantine audit trail is immutable');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER cube_quarantine_events_no_delete
BEFORE DELETE ON cube_quarantine_events
BEGIN
    SELECT RAISE(ABORT, 'quarantine audit trail is immutable');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS cube_quarantine_events_no_delete;
DROP TRIGGER IF EXISTS cube_quarantine_events_no_update;
DROP TABLE cube_quarantine_events;
DROP TABLE cubes;
