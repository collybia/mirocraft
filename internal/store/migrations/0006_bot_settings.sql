-- The chat bots an operator has configured.
--
-- One row per platform, created on first save. The token is stored as it was
-- given: it is presented to Discord or Telegram, and a hash cannot be
-- presented. What protects it is what protects the database file — its
-- permissions — which is why the panel never returns it through the API.

CREATE TABLE bot_settings (
    -- "discord" or "telegram".
    provider   TEXT PRIMARY KEY,
    token      TEXT NOT NULL,
    enabled    INTEGER NOT NULL DEFAULT 0,
    -- What the last connection attempt did, so the panel can show why a bot
    -- that is switched on is not answering. An operator who pasted the wrong
    -- token should read that here rather than in the logs.
    last_status TEXT NOT NULL DEFAULT '',
    last_error  TEXT NOT NULL DEFAULT '',
    -- The bot's own name on the platform, filled in after it connects. Shown
    -- so an operator can tell which of their bots this token belongs to.
    account     TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);
