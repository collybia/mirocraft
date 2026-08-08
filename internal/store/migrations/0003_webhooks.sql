-- Webhooks.
--
-- The secret is stored in plain text rather than hashed, unlike a password or
-- an API token. It has to be: signing a delivery requires the secret itself,
-- and a hash cannot produce an HMAC. It is the same trade every webhook
-- implementation makes, and the reason the value is never returned by the API
-- after it is set.

CREATE TABLE webhooks (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    url        TEXT NOT NULL,
    secret     TEXT NOT NULL DEFAULT '',
    events     TEXT NOT NULL DEFAULT '',
    enabled    INTEGER NOT NULL DEFAULT 1,

    -- Delivery health, so an operator can see a hook that has been failing
    -- rather than wondering why nothing arrives.
    last_status      INTEGER,
    last_error       TEXT NOT NULL DEFAULT '',
    last_attempt_at  TEXT,
    last_success_at  TEXT,
    failure_count    INTEGER NOT NULL DEFAULT 0,

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX idx_webhooks_user ON webhooks (user_id);
