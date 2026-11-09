-- +goose Up
CREATE TABLE IF NOT EXISTS foreign_evidence_lineage (
    evidence_id TEXT PRIMARY KEY REFERENCES evidence_objects(evidence_id) ON DELETE CASCADE,
    content_hash TEXT NOT NULL,
    origin_collective_id TEXT NOT NULL,
    producer_principal_id TEXT NOT NULL,
    delegation_id TEXT NOT NULL DEFAULT '',
    signature_hex TEXT NOT NULL,
    trust_tier TEXT NOT NULL CHECK (trust_tier IN ('UNTRUSTED', 'PROVISIONAL', 'VERIFIED_PARTNER', 'CONSTITUTIONAL_COLLECTIVE')),
    corroboration_level TEXT NOT NULL CHECK (corroboration_level IN ('NONE', 'SELF_ATTESTED', 'INDEPENDENT_WITNESS', 'MULTI_PARTY_ATTESTED')),
    witnesses_json TEXT NOT NULL DEFAULT '[]',
    upstream_lineage_json TEXT NOT NULL DEFAULT '[]',
    created_at TEXT NOT NULL,
    verified_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_foreign_evidence_origin_producer
    ON foreign_evidence_lineage(origin_collective_id, producer_principal_id);

CREATE TABLE IF NOT EXISTS external_task_reconciliations (
    reconciliation_id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(task_id),
    attempt_id TEXT NOT NULL REFERENCES attempts(attempt_id),
    origin_collective_id TEXT NOT NULL DEFAULT '',
    producer_principal_id TEXT NOT NULL DEFAULT '',
    external_signal_state TEXT NOT NULL CHECK (external_signal_state IN ('CLAIMED_SUCCESS', 'CLAIMED_FAILURE', 'CLAIMED_PROGRESS')),
    reconciliation_state TEXT NOT NULL CHECK (reconciliation_state IN ('VERIFIED_ACCEPTANCE', 'REJECTED', 'OUTCOME_UNKNOWN')),
    reconciliation_reason TEXT NOT NULL,
    evidence_ids_json TEXT NOT NULL DEFAULT '[]',
    blind_retry_prevented INTEGER NOT NULL DEFAULT 1,
    evaluated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_external_reconciliations_task_attempt
    ON external_task_reconciliations(task_id, attempt_id, evaluated_at DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_external_reconciliations_task_attempt;
DROP TABLE IF EXISTS external_task_reconciliations;
DROP INDEX IF EXISTS idx_foreign_evidence_origin_producer;
DROP TABLE IF EXISTS foreign_evidence_lineage;
