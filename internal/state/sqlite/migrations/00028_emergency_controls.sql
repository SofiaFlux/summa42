-- +goose Up
CREATE TABLE emergency_controls (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    break_glass_state TEXT NOT NULL CHECK (break_glass_state IN ('ARMED', 'FROZEN', 'RESTORED')),
    frozen_at TEXT,
    frozen_by_principal_id TEXT REFERENCES principals(principal_id),
    freeze_reason TEXT,
    restored_at TEXT,
    restored_by_principal_id TEXT REFERENCES principals(principal_id),
    updated_at TEXT NOT NULL
);

CREATE TABLE dead_man_switch (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    state TEXT NOT NULL CHECK (state IN ('NOMINAL', 'SAFE_MODE')),
    last_heartbeat_at TEXT NOT NULL,
    heartbeat_timeout_seconds INTEGER NOT NULL CHECK (heartbeat_timeout_seconds > 0),
    safe_mode_triggered_at TEXT,
    updated_at TEXT NOT NULL
);

CREATE TABLE emergency_audit_events (
    event_id TEXT PRIMARY KEY,
    event_type TEXT NOT NULL CHECK (event_type IN (
        'BREAK_GLASS_TRIGGERED',
        'SYSTEM_RESUMED',
        'HEARTBEAT_RECORDED',
        'SAFE_MODE_TRIGGERED',
        'SAFE_MODE_CLEARED'
    )),
    principal_id TEXT REFERENCES principals(principal_id),
    reason TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX emergency_audit_events_created ON emergency_audit_events(created_at ASC);

-- +goose StatementBegin
CREATE TRIGGER emergency_audit_events_no_update
BEFORE UPDATE ON emergency_audit_events
BEGIN
    SELECT RAISE(ABORT, 'emergency audit trail is immutable');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER emergency_audit_events_no_delete
BEFORE DELETE ON emergency_audit_events
BEGIN
    SELECT RAISE(ABORT, 'emergency audit trail is immutable');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS emergency_audit_events_no_delete;
DROP TRIGGER IF EXISTS emergency_audit_events_no_update;
DROP TABLE emergency_audit_events;
DROP TABLE dead_man_switch;
DROP TABLE emergency_controls;
