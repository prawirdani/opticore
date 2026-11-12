-- +goose Up
-- Tables for stages after the spike (README §9).

CREATE TABLE jobs (
  id           BIGSERIAL PRIMARY KEY,
  name         TEXT NOT NULL,
  spec         JSONB NOT NULL,               -- task template + device selector
  max_parallel INT NOT NULL DEFAULT 10,
  status       TEXT NOT NULL DEFAULT 'running'
               CHECK (status IN ('running','done','cancelled')),
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE tasks
  ADD CONSTRAINT tasks_job_fk FOREIGN KEY (job_id) REFERENCES jobs(id) ON DELETE SET NULL;

CREATE TABLE firmware (
  id             BIGSERIAL PRIMARY KEY,
  file_name      TEXT NOT NULL UNIQUE,
  sha256         TEXT NOT NULL,
  size           BIGINT NOT NULL,
  product_classes TEXT[] NOT NULL,           -- allowlist
  version        TEXT NOT NULL,              -- expected SoftwareVersion after upgrade
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE api_tokens (
  id         BIGSERIAL PRIMARY KEY,
  name       TEXT NOT NULL UNIQUE,
  hash       TEXT NOT NULL,                  -- SHA-256 of the token
  scopes     TEXT[] NOT NULL,                -- 'read', 'write', 'reveal'
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at TIMESTAMPTZ
);

-- +goose Down
DROP TABLE api_tokens;
DROP TABLE firmware;
ALTER TABLE tasks DROP CONSTRAINT tasks_job_fk;
DROP TABLE jobs;
