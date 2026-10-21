-- +goose Up
-- Set when a reservation settled for more than it reserved (an overrun the
-- ledger records honestly but that must be visible to review).
ALTER TABLE resource_reservations ADD COLUMN over_settled INTEGER NOT NULL DEFAULT 0 CHECK (over_settled IN (0, 1));

-- +goose Down
ALTER TABLE resource_reservations DROP COLUMN over_settled;
