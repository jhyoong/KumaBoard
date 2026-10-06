ALTER TABLE agent_upgrades ADD COLUMN failure_detail TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_upgrades ADD COLUMN closed_by      TEXT NOT NULL DEFAULT '';  -- agent | server | admin
ALTER TABLE agent_upgrades ADD COLUMN updated_at     TEXT NOT NULL DEFAULT '';
UPDATE agent_upgrades SET updated_at = COALESCE(finished_at, started_at);
UPDATE agent_upgrades SET closed_by = CASE
        WHEN failure_reason = 'abandoned' THEN 'admin'
        WHEN failure_reason IN ('handshake_at_from_version', 'send_failed') THEN 'server'
        ELSE 'agent' END
    WHERE state IN ('verified', 'rolled_back', 'failed');
CREATE INDEX agent_upgrades_device_idx ON agent_upgrades(device_id, started_at DESC);

CREATE TABLE agent_upgrade_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    upgrade_id TEXT NOT NULL REFERENCES agent_upgrades(id) ON DELETE CASCADE,
    ts         TEXT NOT NULL,
    source     TEXT NOT NULL,              -- agent | server | admin
    state      TEXT NOT NULL,
    reason     TEXT NOT NULL DEFAULT '',
    detail     TEXT NOT NULL DEFAULT '',
    applied    INTEGER NOT NULL DEFAULT 1  -- 0: recorded, did not change the row
);
CREATE INDEX agent_upgrade_events_upgrade_idx ON agent_upgrade_events(upgrade_id, id);

ALTER TABLE devices ADD COLUMN upgrade_dispatch_json TEXT NOT NULL DEFAULT '';
