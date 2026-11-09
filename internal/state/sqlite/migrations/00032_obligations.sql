-- +goose Up
ALTER TABLE obligations ADD COLUMN due_at TEXT;
ALTER TABLE obligations ADD COLUMN grace_period_seconds INTEGER NOT NULL DEFAULT 0;
ALTER TABLE obligations ADD COLUMN consequences_json TEXT NOT NULL DEFAULT '{}';
ALTER TABLE obligations ADD COLUMN cost_of_delay_rate INTEGER NOT NULL DEFAULT 0;
ALTER TABLE obligations ADD COLUMN lead_time_seconds INTEGER NOT NULL DEFAULT 86400;
ALTER TABLE obligations ADD COLUMN lifecycle_state TEXT NOT NULL DEFAULT 'PENDING';
ALTER TABLE obligations ADD COLUMN synthesized_task_id TEXT;
ALTER TABLE obligations ADD COLUMN entity_id TEXT;
ALTER TABLE obligations ADD COLUMN updated_at TEXT;

CREATE INDEX obligations_due_lifecycle ON obligations(lifecycle_state, due_at);

-- Seed default resource envelope for synthesized obligations if not present
INSERT OR IGNORE INTO resource_envelopes (envelope_id, hard_limit, created_at)
VALUES ('envelope_default', 100000000, '2026-01-01T00:00:00Z');

-- +goose Down
DROP INDEX obligations_due_lifecycle;
ALTER TABLE obligations DROP COLUMN updated_at;
ALTER TABLE obligations DROP COLUMN entity_id;
ALTER TABLE obligations DROP COLUMN synthesized_task_id;
ALTER TABLE obligations DROP COLUMN lifecycle_state;
ALTER TABLE obligations DROP COLUMN lead_time_seconds;
ALTER TABLE obligations DROP COLUMN cost_of_delay_rate;
ALTER TABLE obligations DROP COLUMN consequences_json;
ALTER TABLE obligations DROP COLUMN grace_period_seconds;
ALTER TABLE obligations DROP COLUMN due_at;
