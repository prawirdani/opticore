-- +goose Up
-- Values sealed with OPTICORE_SECRET_KEY cannot be decrypted now that the key
-- is gone. A sealed string sent to a device would be garbage, so drop it.
-- Empty CR credentials make Opticore set them again on the next Inform.
--
-- devices.cr_password was sealed without the enc: prefix, and every password
-- written before this migration was sealed, so clear all of them.

UPDATE devices SET cr_password = '', cr_set_at = NULL WHERE cr_password <> '';

UPDATE tasks SET prev = NULL WHERE prev::text LIKE '%enc:%';

UPDATE tasks
SET status = 'failed',
    fault = 'value was sealed with OPTICORE_SECRET_KEY, which has been removed; re-create the task with the raw secret'
WHERE status IN ('pending', 'running') AND args::text LIKE '%enc:%';

-- +goose Down
-- Sealed values cannot be recovered, so there is nothing to restore.
SELECT 1;
