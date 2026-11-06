-- +goose Up
CREATE TABLE attempt_containments (
    attempt_id TEXT PRIMARY KEY REFERENCES attempts(attempt_id),
    task_id TEXT NOT NULL REFERENCES tasks(task_id),
    ladder_state TEXT NOT NULL CHECK (ladder_state IN ('NOMINAL', 'WARNING', 'RESTRICTED', 'SUSPECTED_ROGUE')),
    violation_signals_json TEXT NOT NULL,
    investigation_task_id TEXT REFERENCES tasks(task_id),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX attempt_containments_task ON attempt_containments(task_id, ladder_state);

CREATE TABLE attempt_containment_events (
    event_id TEXT PRIMARY KEY,
    attempt_id TEXT NOT NULL REFERENCES attempts(attempt_id),
    task_id TEXT NOT NULL REFERENCES tasks(task_id),
    ladder_step TEXT NOT NULL CHECK (ladder_step IN ('OBSERVE', 'WARN', 'RESTRICT', 'TERMINATE')),
    signal_name TEXT NOT NULL,
    details_json TEXT,
    enforced_by TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX attempt_containment_events_attempt ON attempt_containment_events(attempt_id, created_at ASC);

-- +goose StatementBegin
CREATE TRIGGER attempt_containment_events_no_update
BEFORE UPDATE ON attempt_containment_events
BEGIN
    SELECT RAISE(ABORT, 'containment audit trail is immutable');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER attempt_containment_events_no_delete
BEFORE DELETE ON attempt_containment_events
BEGIN
    SELECT RAISE(ABORT, 'containment audit trail is immutable');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER attempt_containments_no_delete
BEFORE DELETE ON attempt_containments
BEGIN
    SELECT RAISE(ABORT, 'containment records cannot be deleted');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS attempt_containments_no_delete;
DROP TRIGGER IF EXISTS attempt_containment_events_no_delete;
DROP TRIGGER IF EXISTS attempt_containment_events_no_update;
DROP TABLE attempt_containment_events;
DROP TABLE attempt_containments;
