-- +goose Up
-- The /v1 API no longer authenticates: the listener address is the control.

DROP TABLE IF EXISTS api_tokens;

-- +goose Down
CREATE TABLE api_tokens (
  id         BIGSERIAL PRIMARY KEY,
  name       TEXT NOT NULL UNIQUE,
  hash       TEXT NOT NULL,                  -- SHA-256 of the token
  scopes     TEXT[] NOT NULL,                -- 'read', 'write', 'reveal'
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at TIMESTAMPTZ
);
