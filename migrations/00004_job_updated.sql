-- +goose Up
-- jobs.updated_at feeds the SSE job tail: cancel, finish and every terminal
-- task of the job touch it.
ALTER TABLE jobs ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
CREATE INDEX jobs_updated_idx ON jobs (updated_at, id);

-- A terminal task moves the job's counts, so it must move the job cursor too.
-- +goose StatementBegin
CREATE FUNCTION touch_job_updated() RETURNS trigger AS $$
BEGIN
  IF NEW.job_id IS NOT NULL THEN
    UPDATE jobs SET updated_at = now() WHERE id = NEW.job_id;
  END IF;
  RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER tasks_touch_job
AFTER UPDATE OF status ON tasks
FOR EACH ROW
WHEN (OLD.status IS DISTINCT FROM NEW.status
      AND NEW.status IN ('done','failed','unknown','cancelled','expired'))
EXECUTE FUNCTION touch_job_updated();

-- +goose Down
DROP TRIGGER IF EXISTS tasks_touch_job ON tasks;
DROP FUNCTION IF EXISTS touch_job_updated();
DROP INDEX IF EXISTS jobs_updated_idx;
ALTER TABLE jobs DROP COLUMN updated_at;
