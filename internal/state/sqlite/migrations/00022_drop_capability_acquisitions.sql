-- +goose Up
DROP INDEX idx_capability_acquisitions_semantic_state;
DROP TABLE capability_acquisition_proposals;

-- +goose Down
CREATE TABLE capability_acquisition_proposals (
    proposal_id TEXT PRIMARY KEY,
    semantic_name TEXT NOT NULL,
    candidate_provider TEXT NOT NULL,
    candidate_version TEXT NOT NULL,
    incumbent_provider TEXT,
    rationale TEXT NOT NULL,
    evaluation_evidence_json TEXT NOT NULL,
    authority_requirements_json TEXT NOT NULL,
    minimum_enforcement TEXT NOT NULL CHECK (minimum_enforcement IN ('UNENFORCED', 'PARTIAL', 'ENFORCED')),
    access_context TEXT NOT NULL,
    requires_approval INTEGER NOT NULL CHECK (requires_approval IN (0, 1)),
    approval_id TEXT,
    state TEXT NOT NULL CHECK (state IN ('PROPOSED', 'APPROVAL_PENDING', 'APPROVED', 'ACTIVATED', 'REJECTED')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX idx_capability_acquisitions_semantic_state
    ON capability_acquisition_proposals(semantic_name, state);

