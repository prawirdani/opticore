-- +goose Up
-- tree_read_at is set when Opticore has read the device's whole parameter tree
-- into the cache. NULL means the cache is only the Inform parameters, so the
-- session queues one automatic root get.
ALTER TABLE devices ADD COLUMN tree_read_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE devices DROP COLUMN tree_read_at;
