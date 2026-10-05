CREATE TABLE devices (
    id                    INTEGER PRIMARY KEY,
    name                  TEXT NOT NULL UNIQUE,
    mac                   TEXT NOT NULL DEFAULT '',
    os                    TEXT NOT NULL DEFAULT '',
    arch                  TEXT NOT NULL DEFAULT '',
    token_hash            BLOB,
    capabilities_json     TEXT NOT NULL DEFAULT '[]',
    schedule_json         TEXT NOT NULL DEFAULT '{}',
    normally_off          INTEGER NOT NULL DEFAULT 0,
    terminal_enabled      INTEGER NOT NULL DEFAULT 0,
    last_seen             TEXT,
    last_disconnect_at    TEXT,
    last_reject_reason    TEXT NOT NULL DEFAULT '',
    agent_version         TEXT NOT NULL DEFAULT '',
    desired_agent_version TEXT NOT NULL DEFAULT '',
    protocol_version      INTEGER NOT NULL DEFAULT 0,
    created_at            TEXT NOT NULL
);

CREATE TABLE commands (
    device_id         INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    name              TEXT NOT NULL,
    description       TEXT NOT NULL DEFAULT '',
    timeout_s         INTEGER NOT NULL,
    expect_disconnect INTEGER NOT NULL DEFAULT 0,
    updated_at        TEXT NOT NULL,
    PRIMARY KEY (device_id, name)
);

CREATE TABLE command_runs (
    id           TEXT PRIMARY KEY,
    device_id    INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    command      TEXT NOT NULL,
    requested_by TEXT NOT NULL,
    requested_at TEXT NOT NULL,
    started_at   TEXT,
    finished_at  TEXT,
    exit_code    INTEGER,
    status       TEXT NOT NULL,
    stdout_tail  TEXT NOT NULL DEFAULT '',
    stderr_tail  TEXT NOT NULL DEFAULT '',
    truncated    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX command_runs_device_idx ON command_runs(device_id, requested_at DESC);
CREATE INDEX command_runs_status_idx ON command_runs(status);

CREATE TABLE terminal_sessions (
    id         TEXT PRIMARY KEY,
    device_id  INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    user       TEXT NOT NULL,
    started_at TEXT NOT NULL,
    ended_at   TEXT,
    bytes_in   INTEGER NOT NULL DEFAULT 0,
    bytes_out  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE audit_log (
    id     INTEGER PRIMARY KEY,
    ts     TEXT NOT NULL,
    actor  TEXT NOT NULL,
    action TEXT NOT NULL,
    target TEXT NOT NULL DEFAULT '',
    result TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT ''
);

CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    created_at    TEXT NOT NULL,
    password_hash TEXT NOT NULL
);

CREATE TABLE sessions (
    id         TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL
);

CREATE TABLE wake_jobs (
    id             TEXT PRIMARY KEY,
    device_id      INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    state          TEXT NOT NULL,
    attempts       INTEGER NOT NULL DEFAULT 0,
    started_at     TEXT NOT NULL,
    finished_at    TEXT,
    failure_reason TEXT NOT NULL DEFAULT ''
);

CREATE TABLE agent_upgrades (
    id             TEXT PRIMARY KEY,
    device_id      INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    from_version   TEXT NOT NULL,
    to_version     TEXT NOT NULL,
    requested_by   TEXT NOT NULL,
    started_at     TEXT NOT NULL,
    finished_at    TEXT,
    state          TEXT NOT NULL,
    failure_reason TEXT NOT NULL DEFAULT ''
);

CREATE TABLE releases (
    version    TEXT NOT NULL,
    os         TEXT NOT NULL,
    arch       TEXT NOT NULL,
    sha256     TEXT NOT NULL,
    signature  TEXT NOT NULL,
    size_bytes INTEGER NOT NULL,
    added_at   TEXT NOT NULL,
    PRIMARY KEY (version, os, arch)
);
