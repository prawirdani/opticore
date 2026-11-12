-- +goose Up
-- tasks.updated_at feeds the SSE tail: every status transition touches it.
ALTER TABLE tasks ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
CREATE INDEX tasks_updated_idx ON tasks (updated_at, id);

-- api_tokens lookups are by hash; 00002 did not declare it unique.
CREATE UNIQUE INDEX IF NOT EXISTS api_tokens_hash_idx ON api_tokens (hash);

-- +goose Down
DROP INDEX IF EXISTS api_tokens_hash_idx;
DROP INDEX IF EXISTS tasks_updated_idx;
ALTER TABLE tasks DROP COLUMN updated_at;
