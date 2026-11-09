-- +goose Up
CREATE TABLE IF NOT EXISTS business_invariants (
    invariant_id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    description TEXT NOT NULL,
    target_kind TEXT NOT NULL,
    rule_type TEXT NOT NULL,
    rule_config_json TEXT NOT NULL DEFAULT '{}',
    severity TEXT NOT NULL DEFAULT 'BREACH',
    action_type TEXT NOT NULL DEFAULT 'REMEDIATION_TASK',
    action_config_json TEXT NOT NULL DEFAULT '{}',
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS invariant_evaluations (
    evaluation_id TEXT PRIMARY KEY,
    invariant_id TEXT NOT NULL REFERENCES business_invariants(invariant_id),
    entity_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    observed_value_json TEXT NOT NULL DEFAULT '{}',
    evidence_id TEXT NOT NULL DEFAULT '',
    triggered_task_id TEXT NOT NULL DEFAULT '',
    triggered_packet_id TEXT NOT NULL DEFAULT '',
    containment_signal TEXT NOT NULL DEFAULT '',
    evaluated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_invariant_evaluations_inv_entity ON invariant_evaluations(invariant_id, entity_id);
CREATE INDEX IF NOT EXISTS idx_invariant_evaluations_status ON invariant_evaluations(status);

-- +goose Down
DROP INDEX IF EXISTS idx_invariant_evaluations_status;
DROP INDEX IF EXISTS idx_invariant_evaluations_inv_entity;
DROP TABLE IF EXISTS invariant_evaluations;
DROP TABLE IF EXISTS business_invariants;
