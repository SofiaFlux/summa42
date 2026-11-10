-- +goose Up
-- The heartbeat timeout used to be derived from the lease duration of whichever
-- process opened the state last. It is now the Owner's horizon: replace every
-- derived value with the seven-day default, clear a safe mode that the derived
-- value triggered, and treat this upgrade (an Owner action) as a heartbeat.
UPDATE dead_man_switch
SET heartbeat_timeout_seconds = 604800,
    state = 'NOMINAL',
    safe_mode_triggered_at = NULL,
    last_heartbeat_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE singleton = 1;

-- +goose Down
SELECT 1;
