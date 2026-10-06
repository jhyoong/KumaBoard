ALTER TABLE commands ADD COLUMN confirm INTEGER NOT NULL DEFAULT 0;
CREATE TABLE command_problems (
    device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    name      TEXT NOT NULL,
    reason    TEXT NOT NULL,
    PRIMARY KEY (device_id, name)
);
ALTER TABLE devices ADD COLUMN commands_config_error TEXT NOT NULL DEFAULT '';
