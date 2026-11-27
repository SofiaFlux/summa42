-- +goose Up
-- Backfill the Collective root envelope and default Project cleanly without
-- altering existing Owner-set limits or active Collective Missions.

INSERT OR IGNORE INTO resource_envelopes(envelope_id, parent_envelope_id, hard_limit, created_at)
VALUES (
    'envelope_collective_root',
    NULL,
    COALESCE((SELECT SUM(hard_limit) FROM resource_envelopes WHERE envelope_id <> 'envelope_collective_root' AND parent_envelope_id IS NULL), 0),
    '2026-01-01T00:00:00Z'
);

UPDATE resource_envelopes
SET parent_envelope_id = 'envelope_collective_root'
WHERE envelope_id <> 'envelope_collective_root'
  AND parent_envelope_id IS NULL;

INSERT OR IGNORE INTO resource_envelopes(envelope_id, parent_envelope_id, hard_limit, created_at)
VALUES ('envelope_project_default', 'envelope_collective_root', 0, '2026-01-01T00:00:00Z');

CREATE TABLE projects (
    project_id       TEXT PRIMARY KEY,
    slug             TEXT NOT NULL UNIQUE,
    name             TEXT NOT NULL,
    state            TEXT NOT NULL CHECK (state IN ('ACTIVE', 'PAUSED', 'ARCHIVED')),
    envelope_id      TEXT NOT NULL UNIQUE REFERENCES resource_envelopes(envelope_id),
    manifest_sha256  TEXT NOT NULL DEFAULT '',
    autonomy_step    TEXT NOT NULL DEFAULT 'A1' CHECK (autonomy_step IN ('A0', 'A1', 'A2', 'A3')),
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    archived_at      TEXT
);

INSERT OR IGNORE INTO projects(project_id, slug, name, state, envelope_id, manifest_sha256, autonomy_step, created_at, updated_at)
VALUES ('project_default', 'default', 'Default Project', 'ACTIVE', 'envelope_project_default', '', 'A1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

ALTER TABLE resource_envelopes
    ADD COLUMN project_id TEXT REFERENCES projects(project_id);

CREATE INDEX resource_envelopes_project ON resource_envelopes(project_id);

UPDATE resource_envelopes
SET project_id = 'project_default'
WHERE envelope_id = 'envelope_project_default';

ALTER TABLE workflow_cases
    ADD COLUMN project_id TEXT NOT NULL DEFAULT 'project_default';

CREATE INDEX workflow_cases_project ON workflow_cases(project_id, state);

ALTER TABLE board_cards
    ADD COLUMN project_id TEXT NOT NULL DEFAULT 'project_default';

CREATE INDEX board_cards_project_column ON board_cards(project_id, column_name);

ALTER TABLE tasks
    ADD COLUMN project_id TEXT REFERENCES projects(project_id);

CREATE INDEX tasks_project_state ON tasks(project_id, state);

CREATE TABLE project_events (
    event_id            TEXT PRIMARY KEY,
    project_id          TEXT NOT NULL REFERENCES projects(project_id),
    action              TEXT NOT NULL CHECK (action IN ('CREATE', 'PAUSE', 'RESUME', 'ARCHIVE', 'APPLY_MANIFEST')),
    from_state          TEXT NOT NULL DEFAULT '',
    to_state            TEXT NOT NULL,
    manifest_sha256     TEXT NOT NULL DEFAULT '',
    owner_principal_id  TEXT NOT NULL REFERENCES principals(principal_id),
    reason              TEXT NOT NULL,
    issued_at           TEXT NOT NULL,
    signature           BLOB NOT NULL,
    created_at          TEXT NOT NULL
);

CREATE INDEX project_events_project_created ON project_events(project_id, created_at);

-- +goose StatementBegin
CREATE TRIGGER project_events_no_update
BEFORE UPDATE ON project_events
BEGIN
    SELECT RAISE(ABORT, 'project audit events are immutable');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER project_events_no_delete
BEFORE DELETE ON project_events
BEGIN
    SELECT RAISE(ABORT, 'project audit events are immutable');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS project_events_no_delete;
DROP TRIGGER IF EXISTS project_events_no_update;
DROP INDEX IF EXISTS project_events_project_created;
DROP TABLE IF EXISTS project_events;

DROP INDEX IF EXISTS tasks_project_state;
ALTER TABLE tasks DROP COLUMN project_id;

DROP INDEX IF EXISTS board_cards_project_column;
ALTER TABLE board_cards DROP COLUMN project_id;

DROP INDEX IF EXISTS workflow_cases_project;
ALTER TABLE workflow_cases DROP COLUMN project_id;

DROP INDEX IF EXISTS resource_envelopes_project;
ALTER TABLE resource_envelopes DROP COLUMN project_id;

DROP TABLE IF EXISTS projects;

UPDATE resource_envelopes
SET parent_envelope_id = NULL
WHERE parent_envelope_id = 'envelope_collective_root';

DELETE FROM resource_envelopes
WHERE envelope_id IN ('envelope_project_default', 'envelope_collective_root')
  AND NOT EXISTS (SELECT 1 FROM resource_reservations r WHERE r.envelope_id = resource_envelopes.envelope_id);
