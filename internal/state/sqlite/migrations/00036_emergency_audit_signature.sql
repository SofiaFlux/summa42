-- +goose Up
-- The Owner signature of the command that produced an emergency audit event.
-- NULL for events the runtime records itself (for example SAFE_MODE_TRIGGERED).
ALTER TABLE emergency_audit_events ADD COLUMN signature BLOB;

-- +goose Down
ALTER TABLE emergency_audit_events DROP COLUMN signature;
