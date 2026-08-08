-- Scheduled action chains.
--
-- Distinct from backup_schedules, which answers one fixed question ("back this
-- server up on this cron"). This table holds an ordered chain an operator
-- composes: warn the players, wait a minute, warn again, stop. Several chains
-- per server are expected — a daily restart and a weekly maintenance window are
-- different things — so the key is an id rather than the server.

CREATE TABLE schedules (
    id          TEXT PRIMARY KEY,
    server_id   TEXT NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    cron        TEXT NOT NULL,
    -- The action chain, as a JSON array. Stored opaquely rather than in a
    -- child table: the chain is always read and written whole, and a table
    -- per action would buy joins nobody needs and an ordering column to keep
    -- correct.
    actions     TEXT NOT NULL,
    enabled     INTEGER NOT NULL DEFAULT 1,
    last_run_at TEXT,
    -- The outcome of the last run, so an operator can see a chain that has
    -- been failing every night without reading the logs.
    last_status TEXT NOT NULL DEFAULT '',
    last_error  TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE INDEX idx_schedules_server ON schedules (server_id);
CREATE INDEX idx_schedules_enabled ON schedules (enabled);
