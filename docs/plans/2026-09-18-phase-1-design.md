# Phase 1 Design: Foundation

Date: 2026-09-18
Source: [PLAN.md](../PLAN.md), [phase-1-foundation.md](../phase-1-foundation.md),
[phase0checklist.txt](../phase0checklist.txt)

## Decisions made during brainstorming

| Decision | Choice |
|---|---|
| Naming | Everything renamed to kuma: binaries `kumaboard` and `kuma-agent`, account `kuma-agent`, paths `/etc/kuma-agent`, `/var/lib/kumaboard`, `/etc/kumaboard`. PLAN.md and phase files still use the old names and need a rename pass |
| Go module | `github.com/jhyoong/KumaBoard` |
| Frontend | React + Vite + TypeScript + Tailwind, embedded in the server binary |
| Dashboard live updates | Server-Sent Events on `/api/events` |
| Build order | Thin vertical slice first, then widen |
| Development fleet | Server on the macOS desktop. Agents on the macOS desktop, headless Windows box, and Linux box |
| Windows service install | `kuma-agent install` using `golang.org/x/sys/windows/svc` |
| Testing | Unit tests per package plus in-process integration tests over real TLS |
| Metrics interval | 30s default, server-supplied |
| Pi build | `linux/arm64` (Pi reports `aarch64`) |

Phase 0 findings carried in: headless Windows box supports S3, WoL confirmed working, headless Windows box
static IP 192.168.1.5, control plane host will be 192.168.1.150 on Debian, LAN is one flat subnet,
`kuma-agent` local account exists on the headless Windows box with no rights changed yet, releases
reach the control plane host by `scp`.

## Libraries

All pure Go so every target builds with `CGO_ENABLED=0`.

- `github.com/coder/websocket`
- `modernc.org/sqlite`
- `golang.org/x/crypto/argon2`
- `github.com/oklog/ulid/v2`
- `github.com/shirou/gopsutil/v4`
- `golang.org/x/sys/windows/svc`
- `gopkg.in/yaml.v3`

## 1. Repository layout, binaries, configuration

One Go module, two binaries.

- `kumaboard` (server). Subcommands: `serve`, `passwd`, `cert reissue`, `version`.
- `kuma-agent`. Subcommands: `run`, `install`, `uninstall` (Windows only), `version`.
  `--selftest` is reserved for phase 2 and not implemented.

```
KumaBoard/
  cmd/kumaboard/       cmd/kuma-agent/
  proto/               envelope, messages, upgrade, terminal, version
  agent/               config, transport, collectors, commands, service (windows svc)
  server/              api, auth, hub, registry, wake, pki, store, sse
  web/                 React + Vite + TypeScript + Tailwind, embedded via embed.FS
  internal/integration/
  configs/             example agent and server YAML, no secrets
  deploy/              systemd/, launchd/, windows/, backup/, hosts.mk
  Makefile             agent-linux-amd64, agent-linux-arm64, agent-darwin-arm64,
                       agent-windows-amd64, server, deploy-<device>
```

Server state lives under one `data_dir`, default `/var/lib/kumaboard`, holding
`kumaboard.db`, `pki/`, `releases/`, and `backup/`. Server config at
`/etc/kumaboard/config.yaml`:

```yaml
listen_addrs: ["192.168.1.150:8443"]
hostnames: []                  # extra Host header values and cert SANs
data_dir: /var/lib/kumaboard
wol:
  interface: eth0
  broadcast: 192.168.1.255
metrics_interval_s: 30
heartbeat_interval_s: 15
```

Agent config at `/etc/kuma-agent/config.yaml` (Windows:
`C:\ProgramData\kuma-agent\config.yaml`) follows the PLAN.md examples with `kuma-agent`
substituted. Token in a sibling `token` file. Binary dir `/opt/kuma-agent/` on Unix and
`C:\ProgramData\kuma-agent\bin\` on Windows. The agent account is `kuma-agent` on all
three OSes.

During development the server runs on the macOS desktop from a local `data_dir` with
`listen_addrs` set to the macOS desktop's LAN IP. Agents on the macOS desktop, headless Windows box, and
Linux box point at that IP. The move to the control plane host is the restore procedure plus editing
three agent configs and running `cert reissue` for the new SAN.

## 2. Protocol and connection lifecycle

`proto/` is a dependency-free package shared by both binaries. `Envelope` has `V int`,
`ID string` (ULID), `Type string`, `TS time.Time`, and `Payload json.RawMessage`. Typed
payload structs exist for every message in the PLAN.md table, including terminal and
upgrade ones nothing sends yet. `proto.Version = 1`, `proto.MinSupported = 1`. A
`Decode(r io.Reader)` helper enforces the 1 MiB cap with `io.LimitReader` and returns a
typed error the caller maps to a close.

### Hub (server)

One goroutine pair per session: a reader that dispatches by type and a writer draining a
buffered channel. No handler writes to the socket directly.

Handshake as in PLAN.md. Token check: SHA-256 of the presented token compared with
`subtle.ConstantTimeCompare` against the stored hash. Any failure sends `error` with the
code, writes an audit row, and closes. `protocol_version_unsupported` also sets
`devices.last_reject_reason` so the dashboard shows "incompatible agent, needs redeploy".

Duplicate session: close the old socket first, then register the new one, under one
mutex, so at most one session per device is ever registered.

Ping every `heartbeat_interval_s`, pong deadline 10s.

### Transport (agent)

`tls.Config{RootCAs: poolFromCAFile}` and nothing else. No `InsecureSkipVerify`, no
custom verify callback.

The reconnect loop is the process's main loop and never returns: dial, hello, wait for
`hello_ack` or `error`, run until the socket dies, back off, repeat. Backoff 1s doubling
to 60s with 20 percent jitter. A ticker goroutine compares `time.Now()` between ticks; a
jump over 30s closes the socket and resets backoff. 45s without any frame from the server
closes the socket. If the server returns `protocol_version_unsupported` the agent logs it
and keeps retrying at the 60s ceiling rather than exiting.

`hello_ack` carries `session_id`, `server_time`, `metrics_interval_s`, and
`heartbeat_interval_s`. The agent adopts both intervals on every handshake.

## 3. Storage, device state, commands, WoL

### Store

`modernc.org/sqlite`, WAL, `busy_timeout=5000`, migrations as numbered embedded SQL
files applied at startup. All tables from PLAN.md are created now, including
`terminal_sessions`, `wake_jobs`, `agent_upgrades`, and `releases`. `devices` gains
`mac`, `last_reject_reason`, and `last_disconnect_at`. Startup runs one statement setting
`command_runs.status = 'lost'` where status is `running` or `dispatched`.

### Registry and state

In-memory `DeviceState` map protected by a mutex, rebuilt from the database at startup.
State is derived, not stored:

- `online`: session exists and metrics arrived within 3 intervals
- `stale`: session exists but metrics are older than 3 intervals
- `offline_expected`: no session and either `normally_off` is set or now falls inside a
  window widened by `grace_period_s` on both sides
- `offline_unexpected`: otherwise

Windows are evaluated in the server's local time. A ticker re-evaluates every 10s and
publishes changes to the SSE broker. Metrics ring buffer: 60 samples per device,
fixed-size slice, no persistence.

### Commands

`POST /api/devices/{name}/commands/{cmd}` inserts a `command_runs` row as `running`,
sends `command_request`, and starts a timer at `timeout_s + 5`. The result handler matches
on envelope `ID`, updates the row, and cancels the timer. `dispatched` results set status
`dispatched` and re-arm the same timer; session close before it fires records
`disconnected_as_expected`, timer firing records `no_disconnect`. Session close marks any
`running` row for that device `lost`. `command_output` chunks are appended to the tails in
order by sequence number, capped at 64 KiB each, with `truncated` set on overflow.

Agent side: `exec.CommandContext` with `Env` set to a fixed `PATH` only, `Dir` set to the
binary dir, and stdout and stderr captured through 64 KiB capped writers. A per-command
mutex gives `busy`. Unknown name returns `unknown_command` and executes nothing. For
`expect_disconnect` the result is sent, the writer channel is drained, 2s elapse, then
the command starts. On session drop the running process finishes and its output is
discarded.

### WoL

`POST /api/devices/{name}/wake` builds the 102-byte magic packet from `devices.mac` and
sends it from a UDP socket bound to the configured interface address to `broadcast:9`.
Sends three packets 100ms apart. Audit row either way. Sleep is the `sleep` command on the
headless Windows box with `expect_disconnect`; nothing special server-side.

## 4. Auth, REST API, SSE, frontend

### Auth

`kumaboard passwd` creates the operator account if none exists, otherwise resets the
password. argon2id with 64 MiB memory, 3 iterations, 4 threads. `POST /api/login` checks
the per-IP limiter first (5 failures then 60s lockout, in memory), verifies, inserts a
`sessions` row with a 12-hour expiry, and sets a cookie that is `HttpOnly`, `Secure`,
`SameSite=Strict`.

One middleware chain on every route: Host allowlist from `listen_addrs` and `hostnames`,
then session lookup. Unauthenticated requests get 401 with no body. `/ws` is exempt from
the session check but applies the Host check. `Origin` is checked on `/ws` and
`/api/events`. Server binds only to `listen_addrs`, never `0.0.0.0`.

### REST API

JSON, all under `/api`:

- `POST /api/login`, `POST /api/logout`
- `GET /api/devices`
- `POST /api/devices` (register: name, MAC, `normally_off`, schedule; response includes
  the plaintext token once)
- `PATCH /api/devices/{name}` (schedule, `normally_off`, MAC)
- `POST /api/devices/{name}/revoke` (new token hash cleared, session closed)
- `GET /api/devices/{name}/metrics` (ring buffer)
- `GET /api/devices/{name}/commands`
- `POST /api/devices/{name}/commands/{cmd}`
- `GET /api/devices/{name}/runs`, `GET /api/runs/{id}`
- `POST /api/devices/{name}/wake`
- `GET /api/audit?limit=&before=`
- `GET /api/events` (SSE)

### SSE

One broker fanning out to connected browsers. Event types:

- `device`: full device summary on any state, session, or version change
- `metrics`: latest sample
- `run`: any `command_runs` change

Each event is a JSON object carrying the device name. On connect the client fetches
`GET /api/devices` once, then applies events. Browsers reconnect with the built-in
`EventSource` retry.

### Frontend

Vite + React + TypeScript + Tailwind, built into `web/dist`, embedded via `embed.FS`,
served at `/` with an SPA fallback. Views:

- Login
- Devices: grid of cards with state badge in four colours, CPU, memory, disk, uptime,
  agent version, protocol version, "incompatible" flag, command buttons, and a Wake
  button when `wol-target` is declared
- Device detail: metrics sparklines from the ring buffer, run history with expandable
  stdout and stderr tails, schedule editor, revoke token
- Register device: form, then a one-time token display with a copy button
- Audit log: paginated table

Widgets are a `switch` on capability. No plugin system, no dynamic loading.

## 5. Packaging, deployment, backup

### Service files

- systemd unit: `User=kuma-agent`, `Restart=always`, `RestartSec=5`,
  `ProtectSystem=strict`, `ReadWritePaths=/opt/kuma-agent`
- LaunchDaemon plist in `/Library/LaunchDaemons`: `UserName`, `RunAtLoad`, `KeepAlive`,
  stdout and stderr to `/var/log/kuma-agent.log`
- Windows: `kuma-agent install --account .\kuma-agent` creates the service via
  `x/sys/windows/svc/mgr`, sets the logon account, start type automatic, and failure
  actions restart after 5s with `FailureActionsOnNonCrashFailures`. The "Log on as a
  service" right is granted by a documented one-liner in `deploy/windows/README.md`

### Account and layout setup

Scripted per OS in `deploy/<os>/setup.sh` or `setup.ps1`: create the account, create
directories with the ownership from PLAN.md (config and `ca.pem` root-owned and readable,
`token` agent-only, binary dir agent-owned), install the service. The sudoers example for
`restart-llm-server` ships in `deploy/launchd/`.

### Deploy target

`make deploy-<device>` reads `deploy/hosts.mk` (device to SSH target and OS), builds the
right target, copies the binary over `scp`, and restarts the service over SSH. Config,
`ca.pem`, and token are copied once by hand during setup and never by this target.

### Backup

`deploy/backup/backup.sh` plus a systemd timer at 05:30: `sqlite3 .backup`, `tar` the
state directory, `age` encryption to a public key at `/etc/kumaboard/backup.age.pub`,
`scp` to `backups/` on the macOS desktop with a dedicated SSH key, retain 7 on both sides,
log success or failure to the journal. The timer is installed only after the move to the
control plane host. `deploy/backup/restore.md` documents the restore drill.

## 6. Testing

Unit tests in every package.

Integration tests in `internal/integration/` start a real server on a loopback port with a
temp `data_dir` and a real agent in the same process, against a CA generated for the test.
They cover:

- handshake success and each `error` code
- duplicate session replacement
- ping timeout
- reconnect after the server restarts
- sleep-jump redial, by injecting the clock
- command `ok`, `unknown_command`, `busy`, `timeout`
- `dispatched` to `disconnected_as_expected` and to `no_disconnect`
- `lost` on session close and on server startup

Frontend: Vitest for the state reducer that applies SSE events, nothing more.

## 7. Build order

Each step ends in something running.

1. `proto` with tests. Module skeleton, Makefile, `go vet` and `go test` clean.
2. `pki` and `store` with migrations. `kumaboard serve` starts, generates the CA, writes
   `ca.pem`.
3. Hub handshake and agent transport with backoff, sleep detection, and duplicate
   handling. Agent on the macOS desktop connects to the server on the macOS desktop. Integration
   tests for the connection lifecycle.
4. Metrics collectors, ring buffer, registry state model. Minimal devices page over SSE
   with login. Register the macOS desktop through the UI.
5. Commands end to end, including `expect_disconnect` and `lost`. Deploy to the Debian
   box and the headless Windows box by hand. Run history in the UI.
6. WoL and the `sleep` command on the headless Windows box. Wake button.
7. Auth hardening: rate limit, Host and Origin checks, `passwd`, audit log view.
8. Service packaging, setup scripts, `make deploy-<device>`, Windows `install`.
9. Backup script, timer, restore drill.
10. Acceptance criteria from `phase-1-foundation.md`, including the week of Linux box
    sleep.

## Acceptance criteria

Unchanged from [phase-1-foundation.md](../phase-1-foundation.md).
