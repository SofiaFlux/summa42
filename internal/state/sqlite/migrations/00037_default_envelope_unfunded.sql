-- +goose Up
-- Migration 00032 seeded a shared envelope with a 100,000,000 unit limit for
-- Tasks synthesized from obligations and invariants. No Owner granted that
-- budget. Leave the envelope in place but unfunded; the Owner funds it when
-- obligation scheduling is put to use. An envelope that already has
-- reservations, or a limit someone changed, is left alone.
UPDATE resource_envelopes
SET hard_limit = 0
WHERE envelope_id = 'envelope_default'
  AND hard_limit = 100000000
  AND NOT EXISTS (SELECT 1 FROM resource_reservations WHERE envelope_id = 'envelope_default');

-- +goose Down
SELECT 1;
