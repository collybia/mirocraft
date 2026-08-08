-- Scheduled backups.
--
-- One schedule per server rather than a list: "back this server up on this
-- cron, keep this many" is what an operator actually wants, and several
-- overlapping schedules on one directory would race each other.

CREATE TABLE backup_schedules (
    server_id   TEXT PRIMARY KEY REFERENCES servers (id) ON DELETE CASCADE,
    cron        TEXT NOT NULL,
    keep_last   INTEGER NOT NULL DEFAULT 7,
    enabled     INTEGER NOT NULL DEFAULT 0,
    last_run_at TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);
