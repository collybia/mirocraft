-- Crossplay: letting Bedrock clients join a Java server.
--
-- Two columns rather than one, because they answer different questions and
-- the second outlives the first: turning crossplay off should not lose the
-- port it was using, or turning it back on would move the address every
-- player had saved.

ALTER TABLE servers ADD COLUMN crossplay INTEGER NOT NULL DEFAULT 0;

-- The UDP port Bedrock clients connect to. Separate from the Java port: they
-- are different protocols and can share a number only by accident.
ALTER TABLE servers ADD COLUMN bedrock_port INTEGER NOT NULL DEFAULT 0;

CREATE INDEX idx_servers_bedrock_port ON servers (bedrock_port);
