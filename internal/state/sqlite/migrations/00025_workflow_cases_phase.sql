-- +goose Up
-- Add phase column to workflow_cases for two-level state and lifecycle tracking (#113).
ALTER TABLE workflow_cases
    ADD COLUMN phase TEXT NOT NULL DEFAULT 'PLANNING_PENDING'
    CHECK (phase IN (
        'PLANNING_PENDING',
        'PLANNING_IN_PROGRESS',
        'IMPLEMENTATION_PENDING',
        'IMPLEMENTATION_IN_PROGRESS',
        'READY_FOR_VERIFICATION',
        'CLOSED',
        'BLOCKED_WAITING_DIRECTIVE',
        'BLOCKED_WAITING_CONTEXT',
        'BLOCKED'
    ));

-- Backfill existing rows based on state and work kind
UPDATE workflow_cases
SET phase = 'READY_FOR_VERIFICATION'
WHERE state = 'READY_FOR_VERIFICATION';

UPDATE workflow_cases
SET phase = 'CLOSED'
WHERE state = 'CLOSED';

UPDATE workflow_cases
SET phase = 'BLOCKED'
WHERE state = 'BLOCKED';

UPDATE workflow_cases
SET phase = 'IMPLEMENTATION_PENDING'
WHERE state = 'ACTIVE' AND next_work_json LIKE '%implement%';

CREATE INDEX workflow_cases_mission_phase ON workflow_cases(mission_id, phase);

-- +goose Down
DROP INDEX IF EXISTS workflow_cases_mission_phase;
ALTER TABLE workflow_cases DROP COLUMN phase;
