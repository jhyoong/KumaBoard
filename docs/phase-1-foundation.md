# Phase 1: Foundation (v1)

Source: PLAN.md sections "Protocol specification", "Security model", "Device state
model", "Agent design", "Control plane", "Backup and restore", "Roadmap: v1",
"Project structure", "Build and deployment".

## Goal

A trustworthy core: protocol, agent, server, device map, named commands, wake and sleep
buttons, dashboard login, audit log, service packaging, and nightly backup.

Phase 1 agents contain no upgrade code. Moving the fleet to the first upgrade-capable build
in phase 2 is one last manual `make deploy-<device>` per device.

## Sub-phases

Build in this order. Each sub-phase has its own tests before the next starts.

### 1a. Protocol package (`proto/`)

The single most important piece. Build and test before any UI work.

- [ ] `envelope.go`: `v`, `id` (ULID), `type`, `ts`, `payload`
- [ ] `messages.go`: every message type from the table in PLAN.md, including `terminal_open`,
      `terminal_open_result`, `wol_request`, `upgrade_request`, and `upgrade_result`, even
      though nothing sends them yet
- [ ] `upgrade.go`: `upgrade_request` and `upgrade_result` payloads. These shapes are frozen
      at protocol v1. Fields may be added; none change meaning or are removed
- [ ] `terminal.go`: `terminal_open` payload and terminal socket control frames
- [ ] `version.go`: protocol version constant and supported range (N and N-1)
- [ ] Enforce the 1 MiB maximum encoded message size; anything larger is a protocol error
      that closes the connection
- [ ] Error codes: `auth_failed`, `unknown_device`, `protocol_version_unsupported`,
      `duplicate_session`
- [ ] Run statuses: `running`, `ok`, `failed`, `timeout`, `unknown_command`, `busy`,
      `dispatched`, `disconnected_as_expected`, `no_disconnect`, `lost`
- [ ] Unit tests for encode/decode round trips and size limit

### 1b. Server core

- [ ] `server/pki/`: generate CA (10 years) and server certificate (2 years) on first run.
      SANs from `listen_addrs` plus configured hostnames. Keys mode `0600` under
      `/var/lib/homelab-cp/pki/`. Write public CA to `ca.pem`
- [ ] `homelab-cp cert reissue` CLI: new server cert from the same CA
- [ ] Startup warning when the server certificate has under 60 days left
- [ ] `server/store/`: SQLite in WAL mode via `modernc.org/sqlite` (no CGO). Schema for
      `devices`, `commands`, `command_runs`, `audit_log`, `users`, `sessions`. Create the
      `terminal_sessions`, `wake_jobs`, `agent_upgrades`, and `releases` tables now so later
      phases are additive
- [ ] No metrics table. Latest metrics and last 60 samples per device in an in-memory ring
      buffer
- [ ] `server/hub/`: WebSocket listener at `/ws` on port 8443, TLS only. Handshake: verify
      token against SHA-256 hash in constant time, check `protocol_version` in supported
      range, reply `hello_ack` with `session_id`, `server_time`, `metrics_interval_s`,
      `heartbeat_interval_s`
- [ ] Redact `hello` payloads before any log or audit write
- [ ] Treat everything in `hello` (capabilities, commands) as untrusted: store and display,
      never grant on it
- [ ] Refresh the `commands` table from each handshake
- [ ] Liveness: `ping` every 15s, close if no `pong` within 10s
- [ ] Duplicate sessions: close the old one, accept the new. At most one session per device
- [ ] Version-rejected agents are recorded and shown as "incompatible agent, needs
      redeploy", not silently dropped
- [ ] `server/registry/`: device identity, capabilities, schedules, and the four-state
      model (`online`, `offline_expected`, `offline_unexpected`, `stale`). Per-device
      `expected_offline` windows and `normally_off` flag, with a 5-minute grace period on
      either side of a window. `stale` after 3 missed metrics intervals
- [ ] Command router: dispatch by name only, correlate by `id`, time out at declared
      timeout plus 5s, persist results with 64 KiB stdout/stderr tails and a `truncated` flag
- [ ] `expect_disconnect` handling: record `dispatched`, then `disconnected_as_expected` if
      the session drops before timeout, `no_disconnect` if it does not
- [ ] On any session close, mark that device's in-flight runs `lost`. On server startup,
      mark all rows still `running` or `dispatched` as `lost`
- [ ] `server/wake/`: send the WoL magic packet directly from the LAN interface to the
      subnet's directed broadcast address on UDP port 9, never `255.255.255.255`
- [ ] Token revocation per device, which closes the session immediately
- [ ] Append-only `audit_log` writes for: logins and failures, command executions, WoL and
      sleep triggers, token issuance and revocation, agent version mismatches

### 1c. Agent core

- [ ] `agent/config/`: YAML loader for `device.name`, `server.url`, `server.ca_file`,
      `token_file`, `capabilities`, `commands`. `run` is an argv array, never a string.
      Server addressed by static IP, not hostname
- [ ] `agent/transport/`: dial with a TLS config whose only root is `ca.pem`. Standard
      library verification. No `InsecureSkipVerify`, no fingerprint comparison, no custom
      verify callback
- [ ] Send `hello` with protocol version, agent version, device name, token, OS, arch,
      capabilities, and declared commands
- [ ] Reconnect forever with backoff: 1s doubling to 60s, plus or minus 20 percent jitter.
      Full `hello` on every reconnect; no resumable state
- [ ] Treat 45s of server silence as dead and redial
- [ ] Sleep detection: if wall-clock time between heartbeat ticks jumps by more than 30s,
      drop the socket and redial immediately with backoff reset
- [ ] Never exit on a network error
- [ ] `agent/collectors/`: CPU, memory, disk, uptime, load. Push on the server-supplied
      interval
- [ ] `agent/commands/`: look the name up locally. Unknown name returns `unknown_command`
      and executes nothing. One run per command at a time; a second request returns `busy`.
      Fixed minimal environment with explicit `PATH`, working directory set to the binary
      directory. Absolute paths in `run`
- [ ] `expect_disconnect` path: send `command_result` with `status: "dispatched"`, wait 2s
      to flush, then run
- [ ] On session drop, let the running process finish and discard its output. No buffering,
      no delivery after reconnect, no retry
- [ ] Stamp version and protocol version into the binary via `-ldflags`
- [ ] Build targets with `CGO_ENABLED=0`: `linux/amd64`, `linux/arm` (`GOARM=7`, or
      `linux/arm64` per the phase 0 check), `darwin/arm64`, `windows/amd64`

### 1d. Auth and dashboard

- [ ] `server/auth/`: single operator account, argon2id password hash
- [ ] Session cookie: `HttpOnly`, `Secure`, `SameSite=Strict`, 12-hour expiry
- [ ] Bind only to `listen_addrs` (LAN static IP and optionally Tailscale IP). Never
      `0.0.0.0`
- [ ] Reject requests whose `Host` header is not a configured address or hostname.
      Check `Origin` on WebSocket upgrades
- [ ] Login rate limit per source IP: 5 failures then 60s lockout, written to the audit log
- [ ] `homelab-cp passwd` CLI for password reset over SSH
- [ ] Every route returns 401 without a valid session
- [ ] `web/`: React + Vite, embedded in the server binary via `embed`
- [ ] Login page
- [ ] Device map with the four-state indicator, current metrics, agent version, and
      protocol version
- [ ] Per-device command buttons rendered from declared capabilities, with run history
- [ ] Wake and Sleep controls for `wol-target` devices
- [ ] Device registration flow that shows the plaintext token exactly once
- [ ] No plugin manifest, no dynamic component loading, no layout engine. Widgets are plain
      components chosen by a `switch` on capability

### 1e. Service packaging and deployment

- [ ] Dedicated unprivileged account on every OS, starting at boot with no user logged in:
      - Linux: system user `homelab-agent`, no password, no sudo group, systemd unit with
        `User=homelab-agent`, `Restart=always`, `RestartSec=5`
      - macOS: standard user `homelab-agent` hidden from the login window, LaunchDaemon in
        `/Library/LaunchDaemons` with `UserName`, `RunAtLoad`, `KeepAlive`. Never a per-user
        launchd agent
      - Windows: local standard user `homelab-agent` as the service logon identity. Never
        `SYSTEM`
- [ ] Install layout from PLAN.md: agent-owned binary directory; root- or admin-owned
      config and `ca.pem` that the agent can read but not write; token file readable by the
      agent only (`0600` or equivalent ACL)
- [ ] One exact-match sudoers rule per privileged command in `/etc/sudoers.d/homelab-agent`.
      Windows equivalent: a pre-registered scheduled task the agent may run but not change
- [ ] Example per-device configs in `configs/` with no secrets
- [ ] `make deploy-<device>` targets that copy the binary and restart the service
- [ ] Windows Defender: if the binary is flagged, add an exclusion for the binary directory
      and note it in the README
- [ ] Pi: the agent never tails Blocky logs from the SD card

### 1f. Backup

- [ ] `deploy/backup/backup.sh`: `sqlite3 .backup` for a consistent copy, `tar` the state
      directory (database copy, `pki/`, `releases/`, server config), encrypt with `age` to a
      public key, `scp` to `backups/` on the macOS desktop with a dedicated SSH key, keep the last
      7 archives on both sides, log success or failure to the journal
- [ ] Root-owned systemd timer on the control plane host at 05:30 daily
- [ ] Restore procedure documented: same static IP, decrypt and untar into
      `/var/lib/homelab-cp`, fix ownership, start the server. Agents reconnect on their own
- [ ] Password manager holds: `age` private key, CA private key

## Deployment order

1. Run the server on a temporary host with one or two agents while the control plane host is pending.
2. When the control plane host is ready with its static IP, move the server using the restore procedure.
3. Deploy the full fleet only after the control plane host holds the static IP.

## Acceptance criteria

Phase 1 is done when all of these hold.

- [ ] Server restarts; all awake agents reconnect within 120s with no manual intervention
- [ ] Every agent machine reboots; the service starts with no user logged in and reconnects
- [ ] macOS desktop Wi-Fi drops for 5 minutes; it reconnects and exactly one session exists for it
- [ ] Linux box sleeps 01:00 to 05:00 for a week; status shows `offline_expected`
      throughout, never `offline_unexpected`
- [ ] Headless Windows box wakes from the dashboard and appears online within 120s
- [ ] The `sleep` command on the headless Windows box ends as `disconnected_as_expected`, not `timeout`
- [ ] Killing an agent mid-command records the run as `lost`
- [ ] Packet capture on the LAN shows no token in cleartext
- [ ] Dashboard returns 401 for every route without a valid session
- [ ] A test agent built with an unsupported `protocol_version` shows "incompatible", not a
      silent reconnect loop
- [ ] Each agent runs as its dedicated account; it cannot write its own config file
- [ ] Restore drill: the latest backup archive, restored to a scratch directory, starts a
      server that lists all devices

## Dependencies

- Phase 0 hardware checks complete
- Static IP or temporary host available
- llm-server moved to a LaunchDaemon before the macOS workstation agent is deployed
