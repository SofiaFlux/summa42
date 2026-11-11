-- +goose Up
-- A resource envelope is created or re-limited only by an Owner-signed
-- grant. Each grant is kept with its signature: the record of who gave the
-- Collective which budget.
CREATE TABLE resource_envelope_grants (
    grant_id TEXT PRIMARY KEY,
    envelope_id TEXT NOT NULL REFERENCES resource_envelopes(envelope_id),
    action TEXT NOT NULL CHECK (action IN ('CREATE', 'SET_LIMIT')),
    hard_limit INTEGER NOT NULL CHECK (hard_limit >= 0),
    owner_principal_id TEXT NOT NULL,
    reason TEXT NOT NULL,
    issued_at TEXT NOT NULL,
    signature BLOB NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX resource_envelope_grants_envelope ON resource_envelope_grants(envelope_id, created_at);

-- +goose Down
DROP INDEX resource_envelope_grants_envelope;
DROP TABLE resource_envelope_grants;
