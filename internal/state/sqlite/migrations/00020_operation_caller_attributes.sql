-- +goose Up
-- Caller policy attributes are persisted with the operation so DISPATCH
-- re-evaluates policy over the same input.attributes.caller as PREPARE.
ALTER TABLE external_operations
    ADD COLUMN caller_attributes_json TEXT NOT NULL DEFAULT '{}';

-- An Owner-signed policy set records its signature; sets created before
-- `summa42 policy upgrade` carry none.
ALTER TABLE policy_sets ADD COLUMN signer_principal_id TEXT REFERENCES principals(principal_id);
ALTER TABLE policy_sets ADD COLUMN signature BLOB;

-- +goose Down
ALTER TABLE policy_sets DROP COLUMN signature;
ALTER TABLE policy_sets DROP COLUMN signer_principal_id;
ALTER TABLE external_operations DROP COLUMN caller_attributes_json;
