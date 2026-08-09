-- Which proxy a server sits behind.
--
-- A column on the server rather than a join table: a server is reachable
-- through at most one proxy, and modelling "at most one" as a table invites
-- rows that say otherwise.
--
-- ON DELETE SET NULL rather than CASCADE: deleting a proxy must not delete the
-- servers behind it. They stop being proxied and go on running, which is what
-- an operator removing a proxy means.

ALTER TABLE servers ADD COLUMN proxy_id TEXT REFERENCES servers (id) ON DELETE SET NULL;

CREATE INDEX idx_servers_proxy ON servers (proxy_id);
