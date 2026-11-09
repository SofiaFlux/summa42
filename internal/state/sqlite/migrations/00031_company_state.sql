-- +goose Up
CREATE TABLE company_entities (
    entity_id TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN (
        'CUSTOMER',
        'CONTRACT',
        'COMMITMENT',
        'ASSET',
        'POLICY',
        'PRODUCT_SERVICE',
        'RISK'
    )),
    name TEXT NOT NULL,
    status TEXT NOT NULL,
    attributes_json TEXT NOT NULL DEFAULT '{}',
    exposure_amount INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX company_entities_kind_status ON company_entities(kind, status);

CREATE TABLE company_state_transitions (
    transition_id TEXT PRIMARY KEY,
    entity_id TEXT NOT NULL REFERENCES company_entities(entity_id),
    task_id TEXT NOT NULL,
    evidence_id TEXT NOT NULL,
    transition_kind TEXT NOT NULL,
    previous_state_json TEXT,
    new_state_json TEXT NOT NULL,
    recorded_at TEXT NOT NULL
);

CREATE INDEX company_state_transitions_entity ON company_state_transitions(entity_id, recorded_at);
CREATE INDEX company_state_transitions_task ON company_state_transitions(task_id);

CREATE TABLE company_projections (
    projection_id TEXT PRIMARY KEY,
    source_system TEXT NOT NULL,
    external_id TEXT NOT NULL,
    entity_id TEXT NOT NULL REFERENCES company_entities(entity_id),
    payload_json TEXT NOT NULL DEFAULT '{}',
    synced_at TEXT NOT NULL,
    UNIQUE(source_system, external_id)
);

CREATE INDEX company_projections_entity ON company_projections(entity_id);

-- +goose Down
DROP TABLE company_projections;
DROP TABLE company_state_transitions;
DROP TABLE company_entities;
