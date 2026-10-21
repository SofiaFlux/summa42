-- +goose Up
-- Experience may only reorder executors (README: no authority expansion).
-- experience_rules is append-only, so lift the update guard just long enough to
-- roll back any pre-existing non-preference rule.
DROP TRIGGER experience_rules_no_update;
UPDATE experience_rules
   SET state = 'ROLLED_BACK',
       rolled_back_at = COALESCE(rolled_back_at, strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
 WHERE adaptation_kind <> 'EXECUTOR_PREFERENCE' AND state <> 'ROLLED_BACK';
-- +goose StatementBegin
CREATE TRIGGER experience_rules_no_update
BEFORE UPDATE ON experience_rules
BEGIN
    SELECT RAISE(ABORT, 'experience rules are immutable');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER adaptation_grants_kind_insert
BEFORE INSERT ON adaptation_grants
WHEN NEW.adaptation_kind <> 'EXECUTOR_PREFERENCE'
BEGIN SELECT RAISE(ABORT, 'only EXECUTOR_PREFERENCE adaptation is permitted'); END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER adaptation_grants_kind_update
BEFORE UPDATE ON adaptation_grants
WHEN NEW.adaptation_kind <> 'EXECUTOR_PREFERENCE'
BEGIN SELECT RAISE(ABORT, 'only EXECUTOR_PREFERENCE adaptation is permitted'); END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER experience_rules_kind_insert
BEFORE INSERT ON experience_rules
WHEN NEW.adaptation_kind <> 'EXECUTOR_PREFERENCE'
BEGIN SELECT RAISE(ABORT, 'only EXECUTOR_PREFERENCE adaptation is permitted'); END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER experience_rules_kind_update
BEFORE UPDATE ON experience_rules
WHEN NEW.adaptation_kind <> 'EXECUTOR_PREFERENCE'
BEGIN SELECT RAISE(ABORT, 'only EXECUTOR_PREFERENCE adaptation is permitted'); END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER experience_rules_kind_update;
DROP TRIGGER experience_rules_kind_insert;
DROP TRIGGER adaptation_grants_kind_update;
DROP TRIGGER adaptation_grants_kind_insert;
