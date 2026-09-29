-- +goose Up
CREATE TABLE evidence_subjects (
    kind TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    evidence_id TEXT NOT NULL REFERENCES evidence_objects(evidence_id) ON DELETE CASCADE,
    PRIMARY KEY (kind, subject_id)
);

-- +goose Down
DROP TABLE evidence_subjects;
