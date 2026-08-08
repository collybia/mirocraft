-- Initial schema: users, tokens, servers, backups, custom themes and the
-- audit log.
--
-- Timestamps are stored as RFC 3339 strings in UTC, matching the API's wire
-- format, so no conversion is needed on the way out.

CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    email         TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'user',
    theme         TEXT NOT NULL DEFAULT 'system',
    totp_secret   TEXT NOT NULL DEFAULT '',
    blocked       INTEGER NOT NULL DEFAULT 0,
    max_servers   INTEGER NOT NULL DEFAULT 0,
    max_ram_mb    INTEGER NOT NULL DEFAULT 0,
    max_disk_mb   INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

-- Emails are compared case-insensitively, so uniqueness must be too.
CREATE UNIQUE INDEX idx_users_email ON users (lower(email));

CREATE TABLE tokens (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         TEXT NOT NULL DEFAULT '',
    hash         TEXT NOT NULL,
    scopes       TEXT NOT NULL DEFAULT '',
    kind         TEXT NOT NULL DEFAULT 'api',
    expires_at   TEXT,
    last_used_at TEXT,
    created_at   TEXT NOT NULL
);

-- The hash is the lookup key on every authenticated request.
CREATE UNIQUE INDEX idx_tokens_hash ON tokens (hash);
CREATE INDEX idx_tokens_user ON tokens (user_id);

CREATE TABLE servers (
    id            TEXT PRIMARY KEY,
    owner_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    core          TEXT NOT NULL,
    version       TEXT NOT NULL,
    kind          TEXT NOT NULL DEFAULT 'server',
    status        TEXT NOT NULL DEFAULT 'stopped',
    ram_mb        INTEGER NOT NULL,
    port          INTEGER NOT NULL,
    java_args     TEXT NOT NULL DEFAULT '',
    dir           TEXT NOT NULL,
    jar_name      TEXT NOT NULL DEFAULT '',
    auto_start    INTEGER NOT NULL DEFAULT 0,
    auto_restart  INTEGER NOT NULL DEFAULT 0,
    eula_accepted INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

-- Two servers cannot share a port on one host.
CREATE UNIQUE INDEX idx_servers_port ON servers (port);
CREATE INDEX idx_servers_owner ON servers (owner_id);

CREATE TABLE backups (
    id         TEXT PRIMARY KEY,
    server_id  TEXT NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    note       TEXT NOT NULL DEFAULT '',
    state      TEXT NOT NULL DEFAULT 'pending',
    size_bytes INTEGER NOT NULL DEFAULT 0,
    path       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

CREATE INDEX idx_backups_server ON backups (server_id);

CREATE TABLE custom_themes (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    base       TEXT NOT NULL DEFAULT 'dark',
    vars       TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX idx_custom_themes_user ON custom_themes (user_id);

CREATE TABLE audit_log (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL DEFAULT '',
    action     TEXT NOT NULL,
    target     TEXT NOT NULL DEFAULT '',
    ip         TEXT NOT NULL DEFAULT '',
    details    TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

CREATE INDEX idx_audit_user ON audit_log (user_id);
CREATE INDEX idx_audit_created ON audit_log (created_at);
