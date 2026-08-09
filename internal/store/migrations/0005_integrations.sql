-- Links between a panel account and an account on a chat platform.
--
-- The link is what lets a bot act for a person: the bot authenticates as
-- itself and names the chat account it is acting for, and the panel resolves
-- that to an account here. Authorization stays in the panel, which is the
-- project's rule about management logic applied to permissions.

CREATE TABLE integration_links (
    id          TEXT PRIMARY KEY,
    -- "discord" or "telegram". A column rather than a table per platform:
    -- everything about them is the same except the name.
    provider    TEXT NOT NULL,
    -- The account id on that platform, as the platform spells it.
    external_id TEXT NOT NULL,
    user_id     TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at  TEXT NOT NULL
);

-- One chat account maps to one panel account, and one panel account has at
-- most one chat account per platform. Both directions matter: without the
-- first, two people could link the same Discord account and each see the
-- other's servers; without the second, an abandoned link would linger and
-- keep answering.
CREATE UNIQUE INDEX idx_integration_links_external ON integration_links (provider, external_id);
CREATE UNIQUE INDEX idx_integration_links_user ON integration_links (provider, user_id);

-- One-time codes a person carries from the panel to the chat.
--
-- Only the hash is stored, like every other credential here. The code is
-- short enough to retype from a phone screen, which is exactly why it is also
-- short-lived and single-use: those two properties are what make a code
-- someone could guess not worth guessing.
CREATE TABLE integration_codes (
    hash       TEXT PRIMARY KEY,
    provider   TEXT NOT NULL,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX idx_integration_codes_user ON integration_codes (provider, user_id);
CREATE INDEX idx_integration_codes_expiry ON integration_codes (expires_at);
