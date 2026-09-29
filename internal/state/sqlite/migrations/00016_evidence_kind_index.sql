-- +goose Up
-- The triage driver looks an accepted index up by walking the evidence objects
-- of one kind, newest first, because the store has no case or revision column.
-- Without an index on kind that is a full scan and sort of the whole table for
-- every case, on every revision, on every tick, growing with the store's age.
-- The column order is the query's own: kind is an equality, created_at and
-- evidence_id are the order and its tiebreak.
CREATE INDEX evidence_objects_kind_created_at
    ON evidence_objects(kind, created_at, evidence_id);

-- +goose Down
DROP INDEX evidence_objects_kind_created_at;
