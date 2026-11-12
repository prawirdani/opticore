-- +goose Up
-- Columns marked "enc" hold AES-GCM ciphertext (README §8), or JSON whose
-- secret leaves are ciphertext.

CREATE TABLE devices (
  id               TEXT PRIMARY KEY,           -- OUI-ProductClass-Serial
  serial           TEXT NOT NULL UNIQUE,
  oui              TEXT NOT NULL,
  product_class    TEXT NOT NULL,
  software_version TEXT NOT NULL DEFAULT '',
  first_inform     TIMESTAMPTZ NOT NULL,
  last_inform      TIMESTAMPTZ NOT NULL,
  inform_interval  INT  NOT NULL DEFAULT 300,  -- seconds, as last read
  last_addr        TEXT NOT NULL DEFAULT '',
  cr_url           TEXT NOT NULL DEFAULT '',
  cr_username      TEXT NOT NULL DEFAULT '',
  cr_password      TEXT NOT NULL DEFAULT '',   -- enc
  cr_set_at        TIMESTAMPTZ,                -- NULL until credentials are set
  pending_reboot   BOOLEAN NOT NULL DEFAULT false, -- SPV returned Status 1
  last_fault       JSONB,                      -- {code, string, task_id, at}
  params           JSONB NOT NULL DEFAULT '{}' -- lazy parameter cache
);
CREATE INDEX devices_product_idx ON devices (product_class, software_version);

CREATE TABLE tasks (
  id          BIGSERIAL PRIMARY KEY,
  device_id   TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  job_id      BIGINT,
  batch_id    UUID,                         -- ordered batch (§7)
  batch_seq   INT NOT NULL DEFAULT 0,
  name        TEXT NOT NULL CHECK (name IN ('get','set','add_object','delete_object',
                                            'reboot','reset','download','ping','revert')),
  args        JSONB NOT NULL DEFAULT '{}',  -- enc leaves for secret values
  prev        JSONB,                        -- set: {path: [value, type]} (enc leaves)
  result      JSONB,                        -- get/ping output, new instance number, …
  status      TEXT NOT NULL DEFAULT 'pending'
              CHECK (status IN ('pending','running','done','failed',
                                'unknown','cancelled','expired')),
  attempts    INT  NOT NULL DEFAULT 0,
  fault_code  INT,
  fault       TEXT NOT NULL DEFAULT '',
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  run_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at  TIMESTAMPTZ NOT NULL,
  -- Idempotency key for tasks created through the API or hook. A retried
  -- request must not enqueue the RPC twice.
  idem_key    TEXT
);
CREATE INDEX tasks_pending_idx ON tasks (device_id, run_at) WHERE status = 'pending';
CREATE INDEX tasks_job_idx ON tasks (job_id) WHERE job_id IS NOT NULL;
CREATE UNIQUE INDEX tasks_idem_idx ON tasks (device_id, idem_key) WHERE idem_key IS NOT NULL;

CREATE TABLE events (
  id        BIGSERIAL PRIMARY KEY,
  device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  at        TIMESTAMPTZ NOT NULL,
  type      TEXT NOT NULL,
  detail    JSONB NOT NULL DEFAULT '{}',
  -- Inform events are deduped by (device, type, inform time, event code).
  -- A retried Inform repeats the same CurrentTime, so the retry adds no rows.
  dedupe_key TEXT
);
CREATE INDEX events_device_at_idx ON events (device_id, at DESC);
CREATE UNIQUE INDEX events_dedupe_idx ON events (device_id, dedupe_key) WHERE dedupe_key IS NOT NULL;

-- +goose Down
DROP TABLE events;
DROP TABLE tasks;
DROP TABLE devices;
