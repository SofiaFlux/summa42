-- +goose Up
CREATE TABLE github_issue_triage_reviews (
    decision_evidence_id TEXT NOT NULL REFERENCES evidence_objects(evidence_id),
    reviewer_version     TEXT NOT NULL,
    state_fingerprint    TEXT NOT NULL,
    verdict_evidence_id  TEXT NOT NULL REFERENCES evidence_objects(evidence_id),
    created_at           TEXT NOT NULL,
    PRIMARY KEY (decision_evidence_id, reviewer_version, state_fingerprint)
);

CREATE INDEX github_issue_triage_reviews_verdict
    ON github_issue_triage_reviews(verdict_evidence_id);

-- +goose Down
DROP INDEX github_issue_triage_reviews_verdict;
DROP TABLE github_issue_triage_reviews;
