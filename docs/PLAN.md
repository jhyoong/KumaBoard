# Homelab Control Plane — Architecture Plan (Revision 3)

A self-hosted control plane for a multi-OS homelab, built around a generic Go agent
that connects outbound to a central server.

This revision keeps the Revision 2 architecture and closes the gaps found in review: the
terminal protocol, upgrade retry behaviour, commands that drop the connection, the agent
account model, backup, and a set of internal contradictions and stale milestone numbers.

---

## Design principles

1. **The protocol is the product.** Get framing, versioning, and reconnection right before
   building features on top of them.
2. **Sleeping devices are normal, not exceptional.** Two of seven devices are off or asleep
   for part of every day. Offline is a first-class, expected state.
3. **Constrain what agents can do.** Named commands defined locally on each agent, not
   arbitrary strings from the server.
4. **Never the only way in.** Existing SSH access from the Windows desktop stays. This
   tool must never become the single path to the infrastructure.
5. **Abstract after three examples, not before.** No plugin interface until three widgets
   exist and their commonality is visible.
6. **Every milestone ends in something trustworthy.** A milestone is not done when it demos.
   It is done when it survives reboots, Wi-Fi drops, and the 01:00 sleep window.

### Non-goals for this project

- Replacing SSH
- Managing Kubernetes clusters
- Time-series metrics history or graphing
- Intrusion detection
- Multi-user access control (single operator)

---

## Threat model

The realistic threats on a home network, in rough order of likelihood:

1. **Malware or a malicious download on the headless Windows box.** This is the highest-risk
   device on the LAN and it will hold an agent config and a token.
2. **A compromised browser tab or extension on the Windows desktop**, reaching an unauthenticated
   dashboard at a predictable LAN address.
3. **Another device on the LAN** — IoT hardware, a guest phone — scanning for open ports.
4. **Own mistakes**: a config committed to a public repo, a token pasted into a chat, a
   binary built with a hardcoded secret.

"It's only on my home network" is not the boundary here. The LAN contains a machine whose
entire purpose is downloading files from strangers, and the control plane can execute
commands on the Windows desktop. Design accordingly.

Explicitly out of the threat model: a targeted attacker with physical access, and nation-state
adversaries.

---

## Device inventory

The reference fleet this design is sized for. Names are placeholders for one device of each
kind; any mix of Linux, macOS, and Windows machines works.

| Device | OS / Arch | Role | Availability | Agent account | Terminal (from v3) | Notes |
|---|---|---|---|---|---|---|
| Windows desktop | Windows / amd64 | Interactive desktop | Always on | `homelab-agent` local user | Off | Keep existing SSH as the fallback path |
| Headless Windows box | Windows / amd64 | Untrusted downloads, game server | **Asleep (S3) by default** | `homelab-agent` local user | On | WoL target; sleeps via `sleep.ps1` (`SetSuspendState('Suspend', $false, $false)`) |
| macOS workstation | macOS / arm64 | LLM inference | Always on | Dedicated non-admin user, LaunchDaemon | On | llm-server moves from tmux to its own launchd service |
| Linux box | Linux / amd64 | Docker host | **Asleep 01:00–05:00** | `homelab-agent` system user | On | Add to `docker` group at v5 |
| macOS desktop | macOS / arm64 | Docker, dev work, backup target | Always on, Wi-Fi | Dedicated non-admin user, LaunchDaemon | On | Wi-Fi: expect reconnect churn |
| Raspberry Pi | Linux / arm | DNS (Blocky) + reverse proxy | Always on | `homelab-agent` system user | On | 1GB RAM, SD card wear is the failure mode |
| Linux server | Linux / amd64 | **Control plane host** | Always on | runs server + own agent | On | Static IP required |

**Tailscale note.** The design assumes a flat home
LAN and does not depend on Tailscale. Installing it on all seven is still recommended — it
costs nothing, gives safe remote dashboard access, and survives any future VLAN
segmentation — but it is a convenience layer, not a security control in this plan.

---

## Architecture

```
                  ┌──────────────────────────────┐
                  │   Browser (Dashboard UI)      │
                  └───────┬──────────────┬────────┘
                   HTTPS  │              │  WSS (terminal, ticket-auth,
                          │              │       one socket per session)
                  ┌───────▼──────────────▼────────────────────────┐
                  │            Control Plane Server                │
                  │  ┌──────────┬─────────┬────────┬────────────┐ │
                  │  │ Device   │ Command │ Auth / │ Terminal   │ │
                  │  │ Registry │ Router  │ Session│ Broker     │ │
                  │  └──────────┴─────────┴────────┴────────────┘ │
                  │  ┌──────────────────┐  ┌────────────────────┐ │
                  │  │ SQLite (state,   │  │ In-memory metrics  │ │
                  │  │ audit, commands) │  │ ring buffer        │ │
                  │  └──────────────────┘  └────────────────────┘ │
                  └───────────────────┬───────────────────────────┘
                                      │ WSS control socket
                                      │ (agents dial outbound, pinned CA,
                                      │  per-device token)
         ┌────────────┬───────────────┼───────────────┬────────────┐
         ▼            ▼               ▼               ▼            ▼
    ┌────────┐  ┌──────────┐   ┌───────────┐   ┌──────────┐  ┌────────┐
    │ Agent  │  │  Agent   │   │  Agent    │   │  Agent   │  │ Agent  │
    │ Linux  │  │ macOS    │   │ macOS     │   │ Pi       │  │ Win x2 │
    └────────┘  └──────────┘   └───────────┘   └──────────┘  └────────┘
                                                                  ▲
                                          WoL magic packet ───────┘
                                          (sent directly by the
                                            server; LAN is flat)
```

Agents only ever dial outbound. No inbound ports are opened on any managed device.

---

## Protocol specification

The single most important section. Build and test this before any UI work.

### Transport

- WebSocket over TLS (`wss://`) on port 8443.
- JSON message bodies in v1. Revisit msgpack only if profiling shows it matters, which at
  seven devices it will not.
- One **control socket** per agent, long-lived, at `/ws`.
- Terminal sessions (v3) use **separate sockets** at `/ws/terminal`, so raw byte streams
  never multiplex against control traffic. This is a deliberate simplicity trade: more
  connections, no stream IDs.

### Message envelope

```json
{
  "v": 1,
  "id": "01J8X...",
  "type": "hello",
  "ts": "2026-09-18T14:03:22Z",
  "payload": { }
}
```

- `v` — protocol version. Present on every message including `hello`.
- `id` — ULID. Echoed by the response so requests can be correlated.
- `type` — see table below.
- Maximum encoded message size: **1 MiB**. Anything larger is a protocol error and closes
  the connection.

### Message types

| Type | Direction | Purpose |
|---|---|---|
| `hello` | agent → server | Handshake: version, identity, token, capabilities, OS/arch |
| `hello_ack` | server → agent | Accepted, plus server-tuned intervals and session ID |
| `ping` / `pong` | both | Liveness |
| `metrics` | agent → server | CPU, memory, disk, uptime, load; plus optional per-adapter `gpus` array with the `gpu` capability (additive, no version bump; see `docs/research/gpu-metrics.md`) |
| `command_request` | server → agent | Run a named command from the agent's local definitions |
| `command_result` | agent → server | Exit code, truncated stdout/stderr, duration — or `dispatched` for `expect_disconnect` commands |
| `command_output` | agent → server | Optional streaming chunk with a sequence number |
| `terminal_open` | server → agent | Ask the agent to dial a terminal socket (v3) |
| `terminal_open_result` | agent → server | `ok` or `refused` (v3) |
| `wol_request` | server → agent | Ask a `wol-sender` agent to emit a magic packet. **Defined but dormant** — the server sends WoL directly while the LAN stays flat |
| `upgrade_request` | server → agent | Move to a target agent version. **Frozen shape — see below** |
| `upgrade_result` | agent → server | Progress and outcome of an upgrade attempt, including rollbacks |
| `error` | both | Coded error; may precede a close |

All message types are defined in `proto/` from v1, even those nothing sends yet.

### Handshake and version negotiation

1. Agent dials `wss://<server-ip>:8443/ws`, validating the server certificate against the
   **pinned CA certificate** in its config. That CA is the only trust root; the system
   trust store is not consulted.
2. Agent sends `hello`:

```json
{
  "v": 1,
  "id": "...",
  "type": "hello",
  "payload": {
    "protocol_version": 1,
    "agent_version": "0.3.1",
    "device_name": "windows-headless",
    "token": "<per-device secret>",
    "os": "windows",
    "arch": "amd64",
    "capabilities": ["metrics", "custom-commands", "wol-target"],
    "commands": [
      {"name": "sleep", "description": "Put the machine to sleep", "timeout_s": 15, "expect_disconnect": true}
    ]
  }
}
```

3. Server verifies the token against the stored hash for that device name, checks
   `protocol_version` is within its supported range, and replies `hello_ack`:

```json
{
  "v": 1,
  "id": "<echoed>",
  "type": "hello_ack",
  "payload": {
    "session_id": "...",
    "server_time": "2026-09-18T14:03:22Z",
    "metrics_interval_s": 10,
    "heartbeat_interval_s": 15
  }
}
```

4. On any failure the server sends `error` with a machine-readable code
   (`auth_failed`, `unknown_device`, `protocol_version_unsupported`, `duplicate_session`)
   and closes.

Everything an agent declares in `hello` — capabilities, command names, descriptions — is
**untrusted input**. The server stores and displays it but never grants anything on the
strength of it. Sensitive switches (`terminal_enabled`) are server-side settings.

**Version mismatch must be visible.** A version-rejected agent shows in the dashboard as
"incompatible agent, needs redeploy" rather than disappearing into a silent reconnect loop.

**The server must always support protocol versions N and N−1.** This is a hard rule, not a
preference. Support for an old version is dropped one release *after* every agent has moved
off it, never in the same release.

**Accepted limitation:** an agent more than one protocol version behind is rejected at
`hello` and cannot be upgraded remotely. It is redeployed by hand over SSH. At seven devices
this is acceptable; the headless Windows box is the likely candidate, so wake and upgrade it before
retiring a protocol version.

### Upgrade messages (frozen at protocol v1)

These are the repair path for any agent the server still accepts, so they must never be the
thing that breaks. Fields may be added; existing fields never change meaning and are never
removed.

`upgrade_request` payload:

```json
{
  "version": "0.4.0",
  "os": "linux",
  "arch": "amd64",
  "sha256": "<hex>",
  "size_bytes": 9412608,
  "signature": "<base64 ed25519>",
  "url": "https://<server-ip>:8443/api/agent/releases/0.4.0/linux_amd64"
}
```

- The signature covers the exact byte string
  `homelab-agent\n<version>\n<os>\n<arch>\n<sha256>\n`. The agent rebuilds this string from
  its **own** compiled-in os/arch and the hash it computed itself, so an artifact signed for
  another platform or version cannot verify.
- The agent downloads `url` over HTTPS using the same pinned CA, with headers
  `Authorization: Bearer <device token>` and `X-Device-Name: <name>`. This endpoint accepts
  device tokens only; the dashboard cookie is not valid on it, and vice versa.

`upgrade_result` payload:

```json
{
  "from_version": "0.3.1",
  "to_version": "0.4.0",
  "state": "rolled_back",
  "reason": "no_handshake"
}
```

- `state` is a progress value (`downloading`, `verifying`, `selftest`, `swapped`,
  `restarting`) or a terminal value (`verified`, `rolled_back`, `failed`).
- `reason` is set on `rolled_back` and `failed`: `download_failed`, `verify_failed`,
  `selftest_failed`, `swap_failed`, `no_handshake`.

### Liveness and reconnection

- Server sends `ping` every 15s. Agent must `pong` within 10s or the server marks the
  session dead and closes it.
- Agent independently treats 45s of silence from the server as a dead connection and
  reconnects.
- Reconnect backoff: 1s, doubling to a 60s ceiling, with ±20% jitter. Retries forever.
  Worst case after a long server outage is therefore ~72s to the next attempt.
- On reconnect the agent performs a full `hello` again. Sessions carry no resumable state.
- **Resume from sleep:** the agent's heartbeat ticker also compares wall-clock time between
  ticks. A jump of more than 30s means the machine slept; the agent drops the socket and
  redials immediately with backoff reset. Without this, a woken machine can sit on a dead
  socket for up to 45s (timers may not count time spent asleep).
- **Duplicate sessions:** if a device reconnects while an old session is still registered,
  the server closes the old one and accepts the new. Expect this regularly from the
  Wi-Fi-connected macOS desktop. At any moment there is at most one session per device.

### Commands and correlation

- `command_request` carries the command **name only**, never a shell string.
- The agent looks the name up in its local config. Unknown name → `command_result` with
  `status: "unknown_command"`. Nothing is executed.
- One run of a given command per device at a time; a second request while one is running
  returns `status: "busy"`.
- The server times out a request at the command's declared timeout plus 5s and records
  `status: "timeout"`.
- stdout and stderr are captured to a cap (64 KiB each). Beyond that, the result is flagged
  truncated. Long-running output uses `command_output` chunks with sequence numbers.

**Commands that drop the connection** (sleep, reboot, shutdown) are marked
`expect_disconnect: true` in the agent config and declared in `hello`:

1. The agent sends `command_result` with `status: "dispatched"` *before* running anything.
2. It waits 2s for the message to flush, then runs the command.
3. The server records `dispatched`, then finalises the run:
   - session drops before the timeout → `disconnected_as_expected` (success)
   - session still up at the timeout → `no_disconnect` (failure)

**Dropped sessions.** When a session closes for any reason, the server marks that device's
in-flight runs as `lost`. The agent lets the process finish and discards the output: no
buffering, no delivery after reconnect, no automatic retry. On server startup, any rows still
`running` or `dispatched` are marked `lost`. `lost` means "outcome unknown" and is treated
that way everywhere, including wake-run-sleep.

Run statuses: `running`, `ok`, `failed` (nonzero exit), `timeout`, `unknown_command`, `busy`,
`dispatched`, `disconnected_as_expected`, `no_disconnect`, `lost`.

### Terminal sessions (v3)

The browser never talks to an agent. The server pairs two sockets.

1. Browser requests a **browser ticket** from the REST API (session cookie auth): single-use,
   30s, bound to one device.
2. Browser opens `wss://<server>:8443/ws/terminal` and sends the ticket as its **first text
   frame** — not in the URL, so it never lands in logs or history.
3. Server sends `terminal_open` on the agent's control socket:
   `{ "session_id": "...", "agent_ticket": "<single-use, 30s>", "cols": 120, "rows": 32 }`.
4. Agent checks `terminal` is in its **local** capabilities. If not, it replies
   `terminal_open_result` with `refused` and nothing else happens. This check is what
   protects the Windows desktop even from a compromised server.
5. Otherwise the agent replies `ok`, dials `/ws/terminal` with the pinned CA, and sends
   `{ "session_id": "...", "agent_ticket": "..." }` as its first text frame. The ticket
   arrived over the authenticated control socket, so no device token travels on this socket.
6. Server pairs the two sockets and relays frames.

Framing on both terminal sockets:

- **Binary frames** — raw PTY bytes, relayed untouched.
- **Text frames** — JSON control: `{"type":"resize","cols":..,"rows":..}` (browser → agent)
  and `{"type":"exit","code":..}` (agent → browser).

Either side closing ends the session: the server closes the other socket and the agent kills
the PTY process group. There is no separate close message. The server enforces a maximum of
two concurrent sessions per device and the 30-minute idle timeout. If the pairing does not
complete within 30s, both tickets are void and the browser socket is closed with an error.

---

## Security model

### Transport

**`wss://` with a self-signed CA that agents pin, plus per-device bearer tokens.**

Rationale: it removes cleartext tokens from the wire and blocks LAN-local interception and
impersonation, for maybe a day of work. Full mTLS is stronger but adds certificate issuance,
distribution, and rotation across seven devices on three operating systems — a real burden
for a marginal gain at this scale. The handshake is designed so mTLS can be added later
without a protocol change: verify the client certificate at the TLS layer and treat the
token as a second factor.

Implementation:

1. Server generates a CA and a server certificate on first run. The certificate's SANs are
   the entries of `listen_addrs` (LAN static IP, Tailscale IP if present) plus any configured
   hostnames. Stored in `/var/lib/homelab-cp/pki/`, keys mode `0600`.
2. The server writes the public CA certificate to `ca.pem`. Each agent gets a copy and its
   config points at it (`ca_file`). The agent builds a TLS config whose **only** root is that
   certificate and uses normal standard-library verification. No fingerprint comparison, no
   `InsecureSkipVerify`, no custom verify callback.
3. `homelab-cp cert reissue` issues a new server certificate from the same CA — used when an
   address changes or the certificate nears expiry. Agents are unaffected because the CA is
   unchanged.
4. Issue the CA for 10 years and the server certificate for 2. The server logs a warning at
   startup when the certificate has under 60 days left.

**Browsers** do not pin. On the two devices that use the dashboard (Windows desktop,
iPhone) either install `ca.pem` as a trusted root or accept the browser warning. Installing
it is a one-time step and is the recommended path.

### Device authentication

- One token per device, 32 bytes from a CSPRNG, base64-encoded.
- Stored server-side as a **SHA-256 hash**, compared in constant time. A slow password hash
  is unnecessary for a 256-bit random secret and would add cost to every reconnect.
- The plaintext is shown once, at device registration, and never again. It is never logged:
  `hello` payloads are redacted before any logging or audit write.
- Stored agent-side in a separate file, mode `0600` / agent-only ACL, owned by the agent
  account — not inline in the YAML, so configs can be committed to a repo safely.
- A token can be revoked per device from the dashboard, which immediately closes the session.

### Dashboard authentication — required in v1

- Single operator account. Password hashed with argon2id.
- Session cookie: `HttpOnly`, `Secure`, `SameSite=Strict`, 12-hour expiry.
- Server binds only to the addresses in `listen_addrs` — the LAN static IP and, if wanted,
  the Tailscale IP. Never `0.0.0.0`.
- Requests whose `Host` header is not one of the configured addresses or hostnames are
  rejected (DNS-rebinding guard). WebSocket upgrades also check `Origin`.
- Remote access is via Tailscale, never a port-forward. The dashboard is never exposed to
  the internet, with or without a password.
- Login attempts are rate-limited per source IP (5 failures → 60s lockout) and written to
  the audit log.
- Forgotten password: `homelab-cp passwd` on the control plane host, run over SSH.

Known limit: a malicious browser *extension* with access to the dashboard origin acts as the
logged-in operator. Nothing in this design stops that; keep the Windows desktop's extensions
minimal.

### Agent accounts and install layout

The agent runs as a dedicated unprivileged account on every OS and starts at boot with no
user logged in.

| | Linux (including Pi and the control plane host) | macOS | Windows |
|---|---|---|---|
| Account | System user `homelab-agent`, shell `/bin/bash`, no password, no sudo group | Standard (non-admin) user `homelab-agent`, hidden from the login window | Local standard user `homelab-agent`; "Log on as a service" granted; interactive and RDP logon denied |
| Service | systemd unit, `User=homelab-agent` | **LaunchDaemon** in `/Library/LaunchDaemons` with `UserName`, `RunAtLoad`, `KeepAlive` | Windows service with the account as its logon identity |
| Binary dir (agent-owned) | `/opt/homelab-agent/` | `/opt/homelab-agent/` | `C:\ProgramData\homelab-agent\bin\` |
| Config + `ca.pem` (root/admin-owned, agent can read) | `/etc/homelab-agent/` | `/etc/homelab-agent/` | `C:\ProgramData\homelab-agent\` |
| Token (agent-only) | `/etc/homelab-agent/token`, `0600` | `/etc/homelab-agent/token`, `0600` | `C:\ProgramData\homelab-agent\token`, ACL: agent + Administrators |

- The **binary directory is writable by the agent account**. Self-upgrade needs this: the
  download, the rename and the marker file all happen there. Consequence, accepted: a
  compromised agent can replace its own binary. It already runs as that account, so this
  grants nothing new.
- The **config is not writable by the agent**. A compromised agent cannot add commands to
  its own allowlist.
- A macOS launchd *agent* (per-user) is not used: it only runs while that user has a login
  session, which fails on a headless Mac after a reboot.
- **FileVault:** if enabled on either Mac, the machine does not reach the network after a
  reboot until someone unlocks the disk. Record whether it is on; if it is, a Mac reboot is
  a hands-on event regardless of this tool.

**Commands that need privilege** get one exact-match rule each, never blanket rights:

```
# /etc/sudoers.d/homelab-agent   (Linux and macOS)
homelab-agent ALL=(root) NOPASSWD: /bin/launchctl kickstart -k system/local.llm-server
```

The command's `run` array then calls `sudo -n` with exactly those arguments. On Windows the
equivalent is a pre-registered scheduled task that the agent account may run, not change.

**llm-server on the macOS workstation** moves out of tmux into its own LaunchDaemon (`local.llm-server`, running
as the operator's user). tmux sockets are per-user, so a separate agent account cannot reach
a tmux session anyway, and a launchd service also survives reboots.

| Operation | Requirement |
|---|---|
| CPU / memory / disk metrics | None, unprivileged |
| GPU metrics (`gpu` capability) | None, unprivileged: IOKit on macOS, PDH + registry on Windows, sysfs on Linux; `nvidia-smi` spawned only if installed |
| WoL magic packet | None — UDP broadcast to port 9 needs no privilege (sent by the server) |
| Named command execution | Whatever the command itself needs; one exact-match sudoers rule per privileged command |
| Docker control (v5) | Debian: membership of the `docker` group. **This is equivalent to root** — accept it consciously. macOS desktop: see open decisions |
| Windows sleep | `SetSuspendState` needs the shutdown privilege, which standard users normally hold on Windows client editions. Verify on the box; fall back to a scheduled task if not |
| Shell / terminal (v3) | Runs as the agent account, deliberately not as the operator |

Do not run the Windows agent as `SYSTEM`. A `SYSTEM` service accepting remote commands on
the headless Windows box is the single worst configuration available on this network.

### Command execution rules

- Commands are defined **only** in agent-side config. The server can never send a shell string.
- If a command ever accepts a parameter, it is passed as an argv element to `exec`, never
  concatenated into `sh -c`.
- Parameters, when they exist, are validated against an allowlist pattern declared alongside
  the command.
- Commands run with a fixed minimal environment (explicit `PATH`, working directory set to
  the binary dir). Use absolute paths in `run`.

### Terminal policy (v3)

- A terminal opens only if **both** hold: `terminal` is in the agent's local capabilities,
  and `terminal_enabled` is set for the device on the server. **Off on the main Windows
  Windows desktop** — omitted from its agent config, so even a compromised server cannot open
  one.
- Ticket flow and framing are specified under **Terminal sessions** in the protocol section.
  No session cookie and no device token travels on a terminal socket.
- The shell runs as the agent account. To do operator-level work, escalate inside the
  session with your own credentials: `su - <you>` on Linux and macOS, `runas /user:<you>` on
  Windows. The agent account itself never gets general sudo.
- Every session is recorded in `terminal_sessions`: who, which device, start, end, byte counts.
- Idle sessions close after 30 minutes. Maximum two concurrent sessions per device.

### Audit

One append-only `audit_log` table covering: logins and failures, command executions, terminal
session open/close, WoL and sleep triggers, token issuance and revocation, agent version
mismatches, upgrade transitions. This is the first thing consulted when something odd
happens, so write to it from day one.

---

## Device state model

Four states, not two:

| State | Meaning | Alerting |
|---|---|---|
| `online` | Session active, heartbeats current | — |
| `offline_expected` | Disconnected inside a declared downtime window, or device is `normally_off` | Silent |
| `offline_unexpected` | Disconnected outside any window | Alert (v7) |
| `stale` | Session alive but no metrics for 3 intervals | Warn in UI |

Per-device schedule configuration, server-side. Times are the server's local time.

```yaml
linux-box:
  expected_offline:
    - { days: "*", from: "01:00", to: "05:00" }
  grace_period_s: 300

windows-headless:
  normally_off: true       # offline is the default state, never alerts
```

The 5-minute grace period on either side of a window prevents a nightly 01:05 alert every
day until alerts get switched off entirely — the failure mode that kills homelab dashboards.

### Wake and sleep on the headless Windows box

The box is put to sleep with
`[System.Windows.Forms.Application]::SetSuspendState('Suspend', $false, $false)`. That is
S3 sleep, and the third argument (`disableWakeEvent = $false`) leaves wake events armed,
which WoL requires. Do not change it to `$true`.

**The server sends the magic packet directly.** The LAN is confirmed flat, so no relay logic
is built. The packet goes to the subnet's directed broadcast address (for example
`192.168.1.255:9`) from the LAN interface, not to `255.255.255.255`, so Docker or Tailscale
interfaces on the control plane host cannot swallow it. The `wol-sender` capability and `wol_request`
message stay defined so relaying can be added without a protocol change if the network is
ever segmented.

Verify on the hardware before building any of this (one evening):

- `powercfg /a` lists **S3**. If the box only offers Modern Standby (S0), WoL behaves
  differently and needs its own test.
- The box is on **Ethernet**. WoL over Wi-Fi is not dependable.
- NIC properties: "Allow this device to wake the computer" and "Only allow a magic packet"
  are on; WoL is enabled in the BIOS/UEFI.
- A magic packet from another machine wakes it from the state `sleep.ps1` leaves it in.
- `sleep.ps1` works when run as the `homelab-agent` standard user, not just over your own
  SSH login.

### Wake-run-sleep (v4)

The highest-value capability for this specific device set, and the fiddliest. Build it as an
explicit state machine with every transition persisted:

```
idle
  → waking            server sends WoL directly;
                      retry every 10s, up to 6 attempts
  → waiting_for_agent  wait for the agent handshake, timeout 180s
  → running            execute the requested command(s)
  → sleeping           invoke the device's sleep command (expect_disconnect)
  → verifying          run ends as disconnected_as_expected within 120s
  → done | failed(reason)
```

Failure at any step leaves the device awake rather than retrying blindly, and surfaces in the
dashboard. Never chain a sleep onto a command whose result is unknown: a run that ended
`lost` or `timeout` stops the machine at `failed` with the device left awake.

v1 ships the two simple halves — a Wake button and the `sleep` command — without the chained
state machine.

---

## Agent design

One generic static binary, behaviour driven by local config.

### Config

Minimal and safe to commit. Secrets live in a separate file; tunables come from the server.
The server is addressed by **static IP**, not hostname: DNS is served by the Pi, which is
itself a managed device, and the control plane must stay reachable when the Pi is down.
(`192.168.1.10` below is a placeholder.)

```yaml
# /etc/homelab-agent/config.yaml  (macOS workstation)
device:
  name: macos-workstation

server:
  url: wss://192.168.1.10:8443/ws
  ca_file: /etc/homelab-agent/ca.pem

token_file: /etc/homelab-agent/token

capabilities:
  - metrics
  - custom-commands
  # - terminal          # added at v3

commands:
  restart-llm-server:
    description: "Restart the llm-server inference server"
    run: ["/usr/bin/sudo", "-n", "/bin/launchctl", "kickstart", "-k", "system/local.llm-server"]
    timeout_s: 20
```

```yaml
# C:\ProgramData\homelab-agent\config.yaml  (headless Windows box)
device:
  name: windows-headless

server:
  url: wss://192.168.1.10:8443/ws
  ca_file: C:\ProgramData\homelab-agent\ca.pem

token_file: C:\ProgramData\homelab-agent\token

capabilities:
  - metrics
  - custom-commands
  - wol-target

commands:
  sleep:
    description: "Put the machine to sleep"
    run: ["powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "C:\\scripts\\sleep.ps1"]
    timeout_s: 15
    expect_disconnect: true
```

Note `run` is an **argv array**, not a string. This is the difference between a command
runner and a remote shell. `C:\scripts\` must not be writable by the agent account.

### Responsibilities

- Dial the control plane, verify it against the pinned CA, authenticate, declare
  capabilities and available commands.
- Push metrics on the server-supplied interval.
- Execute named commands and return structured results, including the `dispatched` path for
  `expect_disconnect` commands.
- Spawn PTY sessions when `terminal` is enabled (v3).
- Reconnect indefinitely with jittered backoff; redial immediately after a detected sleep.
  Never exit on a network error.
- (`wol-sender` is defined but not implemented while the LAN stays flat.)

### Platform notes

- **PTY on Linux/macOS:** `creack/pty`. Straightforward.
- **PTY on Windows:** ConPTY, via `aymanbagabas/go-pty` or `UserExistsError/conpty`, with its
  own resize handling, running from a service in session 0. This is a genuine platform split
  affecting two of seven devices. **Fallback plan:** if ConPTY stalls v3, ship the terminal
  for Linux and macOS and mark Windows terminal as v3.1. Do not let it block the milestone.
- **Windows Defender:** an unsigned executable that downloads and replaces itself may be
  flagged, most likely on the headless Windows box. If it is, add an exclusion for the binary
  directory; note it in the README.
- **Raspberry Pi:** the agent is fine on 1GB. Build `linux/arm` with `GOARM=7` for a 32-bit OS; if
  the Pi runs a 64-bit OS, add a `linux/arm64` target instead. Do not have the agent tail
  Blocky query logs from the SD card. When the DNS panel is built (v7), configure Blocky to
  write query logs to a database on the Linux box or control plane host and read from there. SD wear is
  the Pi's main failure mode.

---

## Control plane

### Components

- **Device registry** — identity, tokens, capabilities, schedules, state.
- **Command router** — dispatch, correlation, timeout, result persistence.
- **Terminal broker** (v3) — ticket issuance and browser-to-agent socket pairing.
- **Auth / session** — login, cookies, rate limiting.
- **Scheduler** (v6) — cron-style task execution with an offline policy per task.
- **Alert engine** (v7) — rule evaluation with downtime-window suppression.

### Storage

SQLite, WAL mode, with the frontend embedded in the server binary. Use a pure-Go driver
(`modernc.org/sqlite`) so the server builds with `CGO_ENABLED=0` like the agent.

```
devices             id, name, os, arch, token_hash, capabilities_json,
                    schedule_json, normally_off, terminal_enabled,
                    last_seen, agent_version, desired_agent_version,
                    protocol_version, created_at
commands            device_id, name, description, timeout_s, updated_at
                    (refreshed from the agent at each handshake)
command_runs        id, device_id, command, requested_by, requested_at,
                    started_at, finished_at, exit_code, status,
                    stdout_tail, stderr_tail, truncated
terminal_sessions   id, device_id, user, started_at, ended_at, bytes_in, bytes_out
audit_log           id, ts, actor, action, target, result, detail
users               id, username, password_hash, created_at
sessions            id, user_id, expires_at
wake_jobs           id, device_id, state, attempts, started_at, finished_at, failure_reason
agent_upgrades      id, device_id, from_version, to_version, requested_by,
                    started_at, finished_at, state, failure_reason
releases            version, os, arch, sha256, signature, size_bytes, added_at
```

`command_runs.status` takes the values listed under *Commands and correlation*. Rows left
`running` or `dispatched` at server startup are set to `lost`. `token_hash` is SHA-256.

**No metrics table.** Latest metrics and the last 60 samples per device live in an in-memory
ring buffer, lost on restart. Adding retention later is easy; removing a bad time-series
schema is not. Persist metrics only when a concrete need for history appears.

### Frontend

React + Vite, embedded in the server binary via `embed`.

- Device map with the four-state indicator and current metrics.
- Per-device command buttons, rendered from declared capabilities.
- Terminal view (xterm.js) — v3.
- Wake / sleep controls for `wol-target` devices.
- Per-device upgrade status with Retry / Abandon (v2).
- Login page.

**No plugin manifest, no dynamic component loading, no layout engine in v1.** Widgets are
plain components chosen by a `switch` on capability. After three widgets exist, extract the
pattern that's actually there.

### On the plugin system

Go's `plugin` package requires matching Go and dependency versions between host and plugin,
works only on Linux and macOS, and is incompatible with static single-binary builds. It
cannot deliver "add a widget without touching core code."

What is achievable, from v5 onward, is a **compile-time registry**: a new backend module
registers itself in `init()`, a new frontend component registers in a map, and the server is
recompiled. That is a perfectly good extension model for a single-operator homelab. State it
honestly in the README so the design isn't warped chasing something Go won't do.

---

## Agent upgrades

The design goal is not "push a new binary." It is **never end up with a device that is
running and unreachable**. An agent upgrading itself is upgrading the only component that
could report the upgrade failing, and a bricked agent on the headless Windows box means
waking it, attaching a monitor, and losing the evening.

**Accepted limitation:** v1 agents contain no upgrade code. Moving the fleet to the first
upgrade-capable build (v2) is one last manual `make deploy-<device>` per device. From then on
upgrades are pushed.

### Release artifacts

Binaries are hosted by the control plane, not fetched from GitHub. No device needs internet
access, there is one source of truth, and the existing TLS and auth are reused.

```
make release VERSION=0.4.0
  → builds linux/amd64, linux/arm, darwin/arm64, windows/amd64
  → computes SHA-256 per artifact
  → emits manifest.json (unsigned)
```

On the control plane host:

```
/var/lib/homelab-cp/releases/
└── 0.4.0/
    ├── manifest.json
    ├── linux_amd64/agent
    ├── linux_arm/agent
    ├── darwin_arm64/agent
    └── windows_amd64/agent.exe
```

`manifest.json` holds, per artifact: os, arch, size, SHA-256, and signature. The server
serves artifacts only through `/api/agent/releases/...`, authenticated by device token; it
never serves the directory as static files.

### Signing

ed25519. The signed message is the byte string
`homelab-agent\n<version>\n<os>\n<arch>\n<sha256>\n`, so a signature is bound to one version
on one platform. Public keys are compiled into the agent at build time via `-ldflags`, with
**two key slots** (current and next) so rotation doesn't require visiting every device.

Generate **both** keypairs before the first upgrade-capable build:

- **Current** private key: `/var/lib/homelab-cp/pki/release_ed25519`, owned by `root`, mode
  `0600`, plus a copy in the password manager.
- **Next** private key: password manager **only**. It never touches the control plane host until the day
  it is needed. If the control plane host is lost or rooted, this is the key that still works.
- The server process runs as an unprivileged user that **cannot read** the current key.
- Signing is performed by a separate CLI (`homelab-cp sign --version 0.4.0`) run as root when
  publishing a release. The long-running server never holds the key.

This means compromise of the web-facing process — the likeliest path by a wide margin —
does not yield the signing key. Only full root on the control plane host does. Accept that as the known
limit: the signature protects against a compromised server process, corrupted downloads, and
a tampered releases directory, but not against root on the control plane. A compromised
server process can still push any *previously signed* version (downgrade is a feature); it
cannot push anything unsigned.

### Trigger model

Each device carries a `desired_agent_version`. At every handshake the server compares it to
the reported `agent_version`.

The server sends `upgrade_request` only when **all** of these hold:

- the versions differ;
- no other upgrade is in flight anywhere in the fleet;
- there is no `agent_upgrades` row for this device **and this target version** already in
  `rolled_back` or `failed`.

The last condition is what prevents a loop. After a failed attempt the device shows
"upgrade to 0.4.0 failed: <reason>" with a **Retry** button, which records a fresh attempt.
Setting a different `desired_agent_version` also re-arms it. The agent itself never retries.

This is declarative rather than event-driven, which matters here: the headless Windows box is asleep
most of the time, so setting its target version means the upgrade applies whenever it next
wakes, with no queue to manage.

Rules:

- **Set per device only.** No roll-out-to-all button. The convenience is small and the
  failure mode is seven dead agents at once.
- **One upgrade in flight at a time.** If a device vanishes mid-upgrade (new binary will not
  start), its row stays in flight and blocks the rest. An **Abandon** button marks it
  `failed(abandoned)` once you have dealt with the device.
- **Canary order:** the control plane host's own agent → Linux box → Pi → macOS workstation → macOS desktop →
  Windows desktop → headless Windows box last.
- **Downgrade uses the identical path.** The target is a version, not necessarily a newer one.
- **The server is never upgraded this way.** If it breaks, SSH to the control plane host.

### Upgrade sequence

1. Server sends `upgrade_request` (payload specified in the protocol section).
2. Agent downloads to a temp file **in the binary directory** — same filesystem as the
   current binary, required for the atomic rename in step 5, and writable by the agent
   account by design. Reports `downloading`.
3. Verify size and SHA-256, rebuild the signed string from the agent's own os/arch and the
   computed hash, and verify the ed25519 signature against either compiled-in key. Any
   failure aborts, reports `failed / verify_failed`, and leaves the agent untouched.
4. **Selftest.** Run the new binary as a subprocess with `--selftest` and require exit 0
   within 15s. Cheapest, highest-value check in the flow.
5. Swap:
   - **Unix:** `rename(new, current)`. Atomic, and the running process keeps its open inode.
   - **Windows:** a running `.exe` cannot be deleted or overwritten, but it *can* be renamed.
     Move `agent.exe` → `agent.exe.old`, move the new binary into place.
   - The previous binary is retained as `.old` on every platform.
6. Write `upgrade_pending.json` in the binary directory: `from_version`, `to_version`,
   `started_at`, and an empty `rollback_reason`.
7. Report `restarting`, exit, and let the service manager restart the process. Do **not**
   `exec()` into the new binary — the service manager must own the lifecycle, because it is
   what restarts the agent if the new version dies.

### What `--selftest` does

Local checks only, no network:

- Parse the config file and fail on anything invalid
- Confirm the token file and `ca.pem` exist and are readable
- Initialise each metrics collector once and discard the result
- Initialise a PTY and close it immediately, if `terminal` is enabled (this is what catches a
  broken ConPTY build)
- Print version and protocol version, exit 0

Deliberately excluded: connecting to the server. A transient network problem must not block an
upgrade, and connectivity is verified in the next step anyway.

### Rollback

On startup, if `upgrade_pending.json` exists, the agent compares **its own version** to the
marker:

- **Own version = `to_version`** — this is the new binary on probation.
  - Handshake completes within **120 seconds** → delete the marker, delete `.old`, send
    `upgrade_result: verified`.
  - No handshake within 120 seconds → write `rollback_reason: no_handshake` into the marker,
    put `.old` back, exit. (Windows: rename the running `agent.exe` → `agent.exe.failed`,
    then `.old` → `agent.exe`; `.failed` is deleted at the next start.)
- **Own version = `from_version`** — a rollback happened. After the handshake, send
  `upgrade_result: rolled_back` with the marker's reason, then delete the marker.
- **Neither** — stale marker. Log it and delete it.

Server side: if a device handshakes at `from_version` while its upgrade row is in flight, the
row is closed as `rolled_back`, using the agent's reported reason when one arrives. That
closed row is what stops the trigger model from sending the request again.

Known and accepted: if the *server* is unreachable for those 120 seconds, a healthy new
binary rolls itself back. It fails safe, shows as a failed upgrade, and you press Retry.

This covers the common failure: a new agent that runs but cannot talk to the server, through
a protocol mismatch or a config migration bug. The failure surfaces in the dashboard instead
of the device silently vanishing.

**What this deliberately does not cover:** a new binary that will not start at all. Nothing is
then running to perform the rollback. The mitigations are the selftest in step 4, which
catches most such cases before the swap is committed, the one-device-at-a-time rollout, and
the retained `.old` binary for manual recovery over SSH.

The full fix is a small launcher binary that the service manager runs, which reverts to `.old`
if the agent dies repeatedly within 30 seconds. It is deferred: roughly 100 lines, but it is
the one component that must never itself break. Revisit it if a start-failure actually occurs,
or if a device ever becomes physically awkward to reach.

### Restart mechanics

| Platform | Configuration |
|---|---|
| systemd | `Restart=always`, `RestartSec=5`. Restarts on clean exit |
| launchd (LaunchDaemon) | `KeepAlive=true`. Note the 10s respawn throttle |
| Windows SCM | Exit with a nonzero code, set failure actions to restart after 5s, and set `FailureActionsOnNonCrashFailures=true` — recovery actions otherwise fire only on an actual crash |

### Upgrade states

Persisted in `agent_upgrades`, each transition timestamped and written to the audit log:

```
requested → downloading → verifying → selftest → swapped → restarting
          → verified
          → rolled_back(reason)
          → failed(reason)
```

Visible per device in the dashboard, alongside the running agent version and protocol version.

---

## Backup and restore

Everything that matters lives in `/var/lib/homelab-cp`: the SQLite database, the CA and
server keys, the current signing key, and the releases. Lose the CA key and every agent has
to be revisited by hand. Keep the backup as simple as the restore.

### Nightly backup

A root-owned systemd timer on the control plane host, 05:30 daily, running one script
(`deploy/backup/backup.sh`):

1. `sqlite3 homelab.db ".backup '/var/lib/homelab-cp/backup/homelab.db'"` — a consistent
   copy while the server keeps running.
2. `tar` the state directory (database copy, `pki/`, `releases/`, server config).
3. Encrypt with `age` to a **public key**. The control plane host never holds the matching private key.
4. `scp` the archive to a `backups/` directory on the **macOS desktop** (always on) using a
   dedicated SSH key.
5. Keep the last 7 archives on both sides; delete older ones.
6. Write success or failure to the journal. (From v7, a missed backup becomes an alert rule.)

### One-time, by hand

Into the password manager: the `age` private key, the CA private key, the current signing
key, and the **next** signing key. These are the items that cannot be regenerated without
touching every device.

### Restore

1. Install the server binary on the replacement host and give it the **same static IP**.
2. Decrypt the newest archive with the `age` private key and untar into
   `/var/lib/homelab-cp`. Fix ownership: signing key `root:root 0600`, everything else owned
   by the server user.
3. Start the server.

Agents reconnect on their own: same IP, same CA, same token hashes. Nothing is reinstalled on
any device. The same procedure moves the server from a temporary development host to the
control plane host when it is ready.

---

## Roadmap

Effort figures assume solo evening work and include the reliability tail, not just first demo.

### v1 — Foundation (~40–55 h)

Protocol, agent, device map, named commands, wake and sleep buttons, authentication, backup.

- Full protocol implementation: envelope, handshake, version negotiation, heartbeat,
  jittered reconnect with sleep detection, correlation IDs. All message types defined in
  `proto/`, including terminal and upgrade messages that nothing sends yet
- Agent building for linux/amd64, linux/arm, darwin/arm64, windows/amd64
- `wss` with pinned CA (`ca.pem` as sole trust root); per-device tokens hashed server-side
- Device map with four-state status and live metrics (in-memory)
- Named command execution with results and history, including `expect_disconnect` and `lost`
- WoL sent directly by the server, and the `sleep` command for the headless Windows box
- Dashboard login, session cookies, rate limiting, Host/Origin checks, `homelab-cp passwd`
- Audit log
- Service packaging: systemd, LaunchDaemon, Windows service, each under the dedicated
  account with the install layout from the security section
- Nightly backup script and timer

Until the control plane host is ready, run the server on a temporary host for development with one or
two agents. Deploy the full fleet only once the control plane host holds its static IP; the move is the
restore procedure.

**Acceptance criteria — v1 is done when all of these hold:**

- [ ] Server restarts; all awake agents reconnect within 120s with no manual intervention
- [ ] Every agent machine reboots; the service starts with **no user logged in** and reconnects
- [ ] macOS desktop Wi-Fi drops for 5 minutes; it reconnects and exactly one session exists for it
- [ ] Linux box sleeps 01:00–05:00 for a week; status shows `offline_expected` throughout,
      never `offline_unexpected`
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

### v2 — Agent upgrades (~15–20 h)

Do this second, before the fleet is fully settled and while you are still iterating on the
agent daily. The first upgrade-capable build goes out by manual redeploy; everything after
that is pushed.

- `make release`, manifest generation, signing CLI; both signing keypairs generated
- Release store and token-authenticated artifact serving
- `--selftest`
- Download, verify, swap, marker file, restart
- Self-rollback on handshake failure; marker handling by own-version comparison
- `desired_agent_version` per device with dashboard controls, Retry and Abandon
- Upgrade state and agent version surfaced per device

**Acceptance criteria:**

- [ ] A clean upgrade succeeds on all seven devices, one at a time
- [ ] An artifact with a corrupted byte is rejected at the hash check, agent unaffected
- [ ] An artifact signed with the wrong key is rejected, agent unaffected
- [ ] A validly signed artifact offered under a different version or arch is rejected
- [ ] A deliberately broken binary (wrong arch) fails `--selftest` and is never swapped in
- [ ] A binary pointed at the wrong server URL rolls back within 120s, the old version
      reports the failure, and **the server does not resend the request** until Retry
- [ ] The headless Windows box, left asleep with a new `desired_agent_version` set, upgrades on its
      next wake with no manual action
- [ ] A downgrade to the previous version works through the same path
- [ ] The server process, as its own unprivileged user, cannot read the signing key

### v3 — Web terminal (~15–20 h)

xterm.js front end, `terminal_open` flow, ticket auth on both sockets, binary/text framing,
resize, idle timeout, session records. Linux and macOS first; Windows ConPTY may slip to
v3.1. Off on the Windows desktop. Agents gain the capability through a pushed upgrade plus a
one-line config change per device.

**Acceptance criteria:**

- [ ] A terminal opens on Debian, Pi, control plane host and both Macs as the `homelab-agent` account;
      `su - <you>` works inside it
- [ ] Resize works; closing the browser tab kills the shell on the device within 5s
- [ ] A session survives 10 minutes idle, then closes cleanly at 30
- [ ] A reused or expired ticket is rejected on both the browser and the agent socket
- [ ] With `terminal` absent from the Windows desktop's config, a forced `terminal_open` is
      refused by the agent
- [ ] Packet capture shows no terminal content in cleartext

### v4 — Wake-run-sleep (~10–15 h)

The full wake state machine with persisted transitions. Windows terminal too, if it slipped
from v3.

### v5 — Docker (~20–25 h)

List, start, stop, restart containers on Debian and macOS desktop. Third widget — this is where
the extension pattern gets extracted, if it's real. Needs the macOS desktop Docker access
decision (see open decisions).

### v6 — Task queue (~25–35 h)

Reusable task definitions, cron scheduling, run history. Each task declares an offline
policy: `skip`, `queue_until_online`, or `wake_run_sleep`.

### v7 — Alerts + DNS panel (~25–30 h)

Threshold rules (disk, unexpected offline, command failures, missed backup) with
downtime-window suppression. Browser notifications. Blocky query stats read from a database,
not the Pi's SD card.

### Deferred indefinitely

The four security widgets. "New MAC on LAN" needs ARP scanning or router integration,
"unexpected open ports" needs a scanner plus a maintained baseline, and "port scan detection"
is an IDS. Each is a separate project. Note also the ordering: harden this tool before
building security monitoring with it, since it opens a more powerful path than most of what
those widgets would detect.

---

## Project structure

```
homelab-cp/
├── proto/                  # Shared protocol types — build this first
│   ├── envelope.go
│   ├── messages.go
│   ├── upgrade.go          # frozen at protocol v1
│   ├── terminal.go         # terminal_open + terminal socket frames
│   └── version.go
├── agent/
│   ├── collectors/         # CPU, RAM, disk, uptime
│   ├── commands/           # argv executor, expect_disconnect handling
│   ├── terminal/           # pty_unix.go, pty_windows.go (ConPTY)
│   ├── upgrade/            # download, verify, selftest, swap, rollback
│   ├── config/
│   └── transport/          # dial, pin CA, handshake, backoff, heartbeat
├── server/
│   ├── api/
│   ├── auth/               # argon2id, sessions, rate limiting
│   ├── hub/                # agent connections, session lifecycle
│   ├── registry/           # devices, state machine, schedules
│   ├── terminal/           # ticket issuance, socket pairing
│   ├── wake/               # WoL sender, wake-run-sleep state machine
│   ├── releases/           # release store, manifest, signing CLI
│   ├── pki/                # CA and server certificate generation
│   └── store/              # SQLite
├── web/
│   ├── components/
│   └── views/
├── configs/                # Example per-device YAML (no secrets)
├── deploy/
│   ├── systemd/
│   ├── launchd/            # LaunchDaemon plists
│   ├── windows/
│   └── backup/             # backup.sh + systemd timer
├── Makefile
└── README.md
```

---

## Build and deployment

### Targets

```
make agent-linux-amd64     # Linux box, control plane host
make agent-linux-arm       # Raspberry Pi (GOARM=7; see open decisions)
make agent-darwin-arm64    # macOS workstation, macOS desktop
make agent-windows-amd64   # both Windows machines
make server                # control plane, frontend embedded
```

All targets build with `CGO_ENABLED=0`. Stamp version and protocol version into the binary at build time via `-ldflags`, and surface
both in the dashboard. Diagnosing a fleet requires knowing what is deployed where.

### Deployment steps

1. Give the control plane host a static IP or a DHCP reservation. Do this before anything else.
2. Install and start the server. It generates its CA and certificate on first run and writes
   the public CA certificate to `ca.pem`.
3. Create the operator account.
4. Register each device in the dashboard; copy the one-time token.
5. Create the `homelab-agent` account on the device. Copy the agent binary, the config,
   `ca.pem`, and the token file into the install layout from the security section; install
   the service.
6. Confirm the device appears online, then run one command against it.

### Upgrades

Manual redeploy in v1 via a `make deploy-<device>` target that copies the binary and restarts
the service. Replaced by the push-upgrade system in v2 — see **Agent upgrades** above. The first
upgrade-capable build is itself rolled out with `make deploy-<device>`.

Because upgrades are never atomic across seven machines, protocol version negotiation is not
optional. It is what makes a staggered rollout survivable, and the N/N−1 support rule is what
keeps a stale agent reachable long enough to be upgraded. An agent that falls further behind
than that is redeployed by hand.

---

## Prerequisites

| Item | Status / when | Notes |
|---|---|---|
| Static IP / DHCP reservation for the control plane host | Confirmed feasible; before fleet deployment | Every agent's `server.url` is this IP. A change means editing seven configs and running `cert reissue` |
| LAN is one flat subnet | **Confirmed** | Server emits WoL packets directly; no relay logic built |
| Headless Windows box WoL hardware check | **Before v1 coding** | The checklist under *Wake and sleep on the headless Windows box*. One evening |
| Windows standard-user service check | **Before v1 coding** | Service starts under `homelab-agent`, and `sleep.ps1` works as that user |
| llm-server moved from tmux to a LaunchDaemon | Before deploying the macOS workstation agent | Otherwise `restart-llm-server` has nothing to restart |
| FileVault status on both Macs | Before v1 acceptance | If on, record "Mac reboot needs hands-on unlock" as a known limitation |
| `age` keypair; secrets in the password manager | Before v1 acceptance | Private key never on the control plane host |
| Both signing keypairs generated | Before v2 | Current key root-owned on the control plane host; next key in the password manager only |
| `ca.pem` installed on the main machine and iPhone | Optional | Otherwise accept the browser warning |
| Local DNS name for the control plane | Not blocking | Agent configs use the static IP permanently, so this no longer sits on the critical path |
| Blocky query logging to a database | Before v7 | Keeps the DNS panel off the Pi's SD card |
| Tailscale on remaining devices | Optional | For remote access; not a security control in this design |
| Keep existing SSH access | **Permanent** | The fallback path. Never remove it |

---

## Open decisions

None of these block v1.

1. **Metrics interval.** 10s is assumed. 30s is gentler on the Pi and entirely sufficient for
   a dashboard nobody watches continuously. It is server-supplied, so it can change any time.
2. **Terminal session recording.** Metadata only is assumed. Full transcripts are useful for
   audit but are another storage-growth problem. Decide at v3.
3. **Control plane host distribution.** Debian, or Ubuntu Server
   for newer packages. Either works.
4. **Publishing releases to the control plane host.** `scp` of the release directory is assumed. A
   dashboard upload form is nicer but means the server process writes into the releases
   directory, which slightly weakens the signing story.
5. **Game server management.** Not addressed. Fits naturally as named commands on the
   headless Windows box (`start-gameserver`, `stop-gameserver`, `backup-saves`), which is a good first test
   of whether the command model is expressive enough.
6. **Docker access on the macOS desktop (decide at v5).** macOS has no `docker` group, and the
   Docker socket belongs to the operator's user, which the separate agent account cannot
   reach. Options: exact-match `sudo -u <you>` rules for the specific `docker` invocations,
   or a socket proxy that exposes only list/start/stop/restart. Debian is unaffected.
7. **Pi OS bitness.** 32-bit → `linux/arm` with `GOARM=7`; 64-bit → add `linux/arm64`. Check
   with `uname -m` before the first Pi build.

---

## Out of scope

- AI agent integration — it may share the control plane host, but the control plane does not
  manage it
- Kubernetes cluster management
- Control plane host provisioning
- Multi-user access control
- Metrics history and graphing
- Intrusion detection
- Anything on this list creeping into v1
