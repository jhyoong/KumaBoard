# KumaBoard

A self-hosted control plane for a small fleet of Linux, macOS, and Windows machines. A Go agent on each device
connects outbound to a central server over WebSocket (TLS), reporting metrics, accepting
named commands, and supporting push upgrades with automatic rollback. A single-page web
dashboard shows the fleet at a glance.

## Architecture

```
          Browser (Dashboard)
                |
          HTTPS / SSE
                |
    +-----------v----------------+
    |   KumaBoard Server         |
    |   (Go, SQLite, embedded UI)|
    +-----------+----------------+
                |  WSS control socket
                |  (agents dial outbound,
                |   pinned CA, per-device token)
    +-------+---+---+-------+--------+
    v       v       v       v        v
  Agent   Agent   Agent   Agent    Agent
  Linux   macOS  Windows   Pi    (server host)
```

Agents only ever dial outbound. No inbound ports are opened on managed devices.

## Components

| Directory | Description |
|---|---|
| `cmd/kumaboard` | Server binary (API, WebSocket hub, embedded dashboard) |
| `cmd/kuma-agent` | Agent binary (metrics, commands, upgrade, selftest) |
| `server/` | Server packages: API routes, auth, hub, store, SSE, wake |
| `agent/` | Agent packages: collectors, commands, transport, upgrade |
| `proto/` | Wire protocol shared by server and agent (JSON over WSS) |
| `internal/` | Shared internals: build info, ed25519 signing, integration tests |
| `web/` | React + TypeScript dashboard (Vite, embedded into the server) |
| `configs/` | Example server config and per-OS agent configs |
| `deploy/` | Service files (systemd, launchd, Windows SCM) and backup scripts |
| `releases/` | Build output for `make release` (not checked in) |
| `docs/` | Design plan, phase breakdowns, implementation notes |

## Supported platforms

| Platform | Arch | Service manager | Example config |
|---|---|---|---|
| Linux | amd64, arm64 | systemd | `configs/kuma-agent.linux.example.yaml` |
| macOS | arm64 | launchd (LaunchDaemon) | `configs/kuma-agent.macos.example.yaml` |
| Windows | amd64 | SCM service | `configs/kuma-agent.windows.example.yaml` |

The server runs on Linux/amd64 and also runs an agent for its own host. Device names are
free-form (`^[a-z0-9][a-z0-9-]{0,62}$`); the names used in the docs and examples
(`linux-box`, `macos-workstation`, `windows-desktop`, ...) are placeholders.

Devices that are asleep or powered off part of the day are a first-class case: mark a
device "normally off" or give it a schedule and it shows as expected-offline instead of
alerting, and Wake-on-LAN targets can be woken from the dashboard.

### Lightweight devices (Raspberry Pi)

The agent is a single static binary with no runtime dependencies, so it runs comfortably
on small ARM boards such as a Raspberry Pi with 1 GB of RAM. Use the `linux/arm64` build
on a 64-bit OS and start from `configs/kuma-agent.pi.example.yaml`, which is deliberately
minimal:

- metrics only, no custom commands;
- nothing that tails logs or writes frequently, to avoid SD card wear;
- CPU temperature is read from the SoC thermal zone; there is no GPU collector for the
  Pi's VideoCore.

Setup is otherwise identical to any other Linux host (see "Agent setup" below).

## Current status

**Phase 1 (Foundation)** -- complete. Protocol, agent, server, dashboard, auth,
packaging, backup.

**Phase 2 (Agent upgrades)** -- complete. Signed push upgrades with self-rollback.
Ed25519 release signing, artifact serving, agent selftest, download/verify/swap/restart
cycle, 120-second probation with rollback, per-device upgrade control in the dashboard.

Upcoming: Phase 3 (web terminal), Phase 4 (wake-run-sleep), and beyond. See
`docs/PHASES.md` for the full roadmap.

## Building

Requires Go 1.26+ and Node.js (for the dashboard).

```sh
# Run tests
make test

# Build everything (dashboard + server + all agent targets)
make all

# Build a versioned release (all four agent targets + manifest)
make release VERSION=0.2.0

# Sign a release (run as root on the control plane host)
kumaboard sign --version 0.2.0

# Ingest a signed release into the database for serving
kumaboard release ingest --version 0.2.0 -config /etc/kumaboard/config.yaml
```

Cross-compiled agent targets: `linux/amd64`, `linux/arm64`, `darwin/arm64`,
`windows/amd64`.

## Security model

### TLS and certificate authority

The server generates its own ECDSA P-256 CA on first start. All communication happens
over `wss://` (WebSocket over TLS) on port 8443. The CA issues a server certificate
with SANs matching the `listen_addrs` and `hostnames` in the server config.

- CA validity: 10 years. Server certificate validity: 2 years.
- Private keys are written with mode `0600`.
- The CA public cert (`ca.pem`) is copied to each agent machine and used as the sole
  trust root. Agents reject any server certificate not signed by this CA.
- To reissue the server certificate (e.g. after an IP change):
  `kumaboard cert reissue -config /etc/kumaboard/config.yaml`.

### Agent authentication

Each device gets a unique bearer token generated by a CSPRNG (32 bytes, base64). The
token is shown exactly once at registration time and stored on the agent machine as a
file readable only by the agent account.

Agents authenticate every WebSocket connection and every HTTP request with two headers:

- `Authorization: Bearer <token>`
- `X-Device-Name: <name>`

The server stores only a SHA-256 hash of each token, never the plaintext. Token
comparison uses constant-time compare.

### Dashboard authentication

The dashboard uses session cookies (`kb_session`), not bearer tokens. Sessions are
server-side (stored in SQLite), expire after 12 hours, and use a 32-byte CSPRNG
session ID.

- Passwords are hashed with Argon2id (64 MB memory, 3 iterations, 4 threads).
- A rate limiter locks an IP after 5 failed login attempts for 60 seconds.
- Cookies are set with `HttpOnly`, `Secure`, `SameSite=Strict`.

Dashboard session cookies cannot download agent artifacts. Agent bearer tokens cannot
access dashboard API routes. The two auth domains are completely separate.

### Host and origin checks

The server validates the `Host` header against a configured allowlist (derived from
`listen_addrs` and `hostnames`). Requests with an unrecognized host get HTTP 421. The
`Origin` header, when present, is checked against the same allowlist.

### Release signing

Agent binaries are signed with ed25519. The agent compiles in two public key slots
(current + next) for zero-downtime key rotation. The signed message is:

```
homelab-agent\n<version>\n<os>\n<arch>\n<sha256>\n
```

The signing private key lives at `/var/lib/kumaboard/pki/release_ed25519` with mode
`0600`, owned by root. The server process runs as an unprivileged user (`kumaboard`)
that cannot read this key. Signing is done via the `kumaboard sign` CLI, run as root.

A second "next" key is kept offline in a password manager for rotation.

### Agent account isolation

Every managed device runs the agent under a dedicated low-privilege account:

| Platform | Account | Shell | Notes |
|---|---|---|---|
| Linux | `kuma-agent` (system user) | `/bin/bash` | Created by `useradd --system` |
| macOS | `kuma-agent` (hidden standard user) | `/bin/bash` | Created by `sysadminctl`, `IsHidden=1` |
| Windows | `kuma-agent` (local standard user) | N/A | Denied interactive and RDP logon via `secpol.msc` |

The agent account can only read its own config and CA cert, and read/write its binary
directory (for upgrades) and token file. It cannot read the server config, the signing
key, or other users' files.

### Audit log

Every security-relevant action is written to the audit log in SQLite: logins (success
and failure), device registrations, upgrades, command executions, and password changes.
The audit log is viewable in the dashboard.

### Backup

Nightly backup runs on the control plane host. The SQLite database, PKI material, release
artifacts, and server config are archived, encrypted with `age` (public key only on disk;
private key in the password manager), and sent to a second always-on host over SSH. Both sides keep
the last 7 backups.

## Server setup

These steps are for the control plane host (Linux/amd64).

### 1. Build the server

```sh
make server
```

This builds the dashboard and compiles it into the server binary at `bin/kumaboard`.

### 2. Create the system user and directories

```sh
sudo deploy/systemd/setup-server.sh
```

This creates:
- System user `kumaboard` (no login shell, home at `/var/lib/kumaboard`)
- `/opt/kumaboard/` -- binary location
- `/etc/kumaboard/` -- config location
- `/var/lib/kumaboard/` -- data directory (SQLite, PKI, releases), owned by `kumaboard`
- Installs and enables the systemd service

### 3. Install the binary and config

```sh
sudo cp bin/kumaboard /opt/kumaboard/kumaboard
sudo cp configs/kumaboard.example.yaml /etc/kumaboard/config.yaml
```

Edit `/etc/kumaboard/config.yaml` to set the server's listen address and WoL settings:

```yaml
listen_addrs: ["192.168.1.150:8443"]
hostnames: []
data_dir: /var/lib/kumaboard
wol:
  interface: eth0
  broadcast: 192.168.1.255
metrics_interval_s: 30
heartbeat_interval_s: 15
```

### 4. Set the operator password

```sh
sudo -u kumaboard /opt/kumaboard/kumaboard passwd -config /etc/kumaboard/config.yaml
```

This prompts for a password (minimum 12 characters) and writes the Argon2id hash to the
database. The username defaults to `admin`.

### 5. Start the server

```sh
sudo systemctl start kumaboard
```

On first start, the server generates the CA and server certificate in
`/var/lib/kumaboard/pki/`. Copy the CA certificate to each agent machine:

```sh
sudo cat /var/lib/kumaboard/pki/ca.pem
```

### 6. Register devices

Run this once per device. The token is shown once and never stored in plaintext on the
server.

```sh
sudo -u kumaboard /opt/kumaboard/kumaboard device add \
  -config /etc/kumaboard/config.yaml \
  [-mac AA:BB:CC:DD:EE:FF] \
  [-normally-off] \
  <device-name>
```

Save the printed token. You will write it to a file on the agent machine.

### 7. Generate release signing keys

```sh
# Generate the current signing keypair
sudo mkdir -p /var/lib/kumaboard/pki
openssl genpkey -algorithm ed25519 -out /var/lib/kumaboard/pki/release_ed25519
sudo chmod 0600 /var/lib/kumaboard/pki/release_ed25519
sudo chown root:root /var/lib/kumaboard/pki/release_ed25519
```

Extract the public key hex for compiling into agents (see "Building agents with release
keys" below). Generate a second "next" keypair and store it in your password manager
only.

## Agent setup

### Linux (systemd)

Applies to any systemd-based distribution (Debian, Ubuntu, Raspberry Pi OS, ...),
including the control plane host's own agent.

#### 1. Run the setup script

```sh
sudo deploy/systemd/setup-agent.sh
```

This creates:
- System user `kuma-agent`
- `/opt/kuma-agent/` -- binary location, writable by `kuma-agent` (for upgrades)
- `/etc/kuma-agent/` -- config, CA cert, and token
- Installs and enables the systemd service

#### 2. Install files

```sh
# Binary
sudo cp bin/kuma-agent_linux_amd64 /opt/kuma-agent/kuma-agent
sudo chown kuma-agent:kuma-agent /opt/kuma-agent/kuma-agent
sudo chmod 0755 /opt/kuma-agent/kuma-agent

# Config (edit to match device name and server address)
sudo cp configs/kuma-agent.linux.example.yaml /etc/kuma-agent/config.yaml

# CA certificate (copied from the server)
sudo cp ca.pem /etc/kuma-agent/ca.pem

# Token (from the 'device add' output)
echo '<token>' | sudo install -m 0600 -o kuma-agent -g kuma-agent /dev/stdin /etc/kuma-agent/token
```

#### 3. Start

```sh
sudo systemctl start kuma-agent
sudo journalctl -u kuma-agent -f
```

The agent systemd unit uses `Restart=always` and `RestartSec=5`.

### macOS (launchd)

Applies to Apple Silicon Macs (`darwin/arm64`).

#### 1. Run the setup script

```sh
sudo deploy/launchd/setup-agent.sh
```

This creates:
- A hidden standard user `kuma-agent` (non-admin)
- `/opt/kuma-agent/` -- binary location
- `/etc/kuma-agent/` -- config location
- `/var/log/kuma-agent.log` -- log file
- Installs the LaunchDaemon plist

#### 2. Install files

```sh
# Binary
sudo cp bin/kuma-agent_darwin_arm64 /opt/kuma-agent/kuma-agent
sudo chown kuma-agent:staff /opt/kuma-agent/kuma-agent
sudo chmod 0755 /opt/kuma-agent/kuma-agent

# Config
sudo cp configs/kuma-agent.macos.example.yaml /etc/kuma-agent/config.yaml

# CA certificate
sudo cp ca.pem /etc/kuma-agent/ca.pem

# Token
echo '<token>' | sudo install -m 0600 -o kuma-agent -g staff /dev/stdin /etc/kuma-agent/token
```

#### 3. Start

```sh
sudo launchctl bootstrap system /Library/LaunchDaemons/com.kumaboard.agent.plist
tail -f /var/log/kuma-agent.log
```

The LaunchDaemon uses `KeepAlive=true` with a 10-second respawn throttle.

Note: if FileVault is enabled (`fdesetup status`), a reboot requires a hands-on unlock
before the agent can start.

### Windows (SCM)

Applies to Windows 10/11 and Windows Server (`windows/amd64`).

#### 1. Create the service account

Create a local standard user `kuma-agent` with a strong password. Then in `secpol.msc`,
add `kuma-agent` to "Deny log on locally" and "Deny log on through Remote Desktop
Services". Do not add it to "Deny log on as a service". Never run the agent as SYSTEM.

#### 2. Run the setup script (as Administrator)

```powershell
.\deploy\windows\setup.ps1
```

This creates `C:\ProgramData\kuma-agent\` with restricted ACLs and `C:\scripts\`.

#### 3. Install files

```powershell
# Binary
Copy-Item bin\kuma-agent_windows_amd64.exe C:\ProgramData\kuma-agent\bin\kuma-agent.exe

# Config
Copy-Item configs\kuma-agent.windows.example.yaml C:\ProgramData\kuma-agent\config.yaml

# CA certificate
Copy-Item ca.pem C:\ProgramData\kuma-agent\ca.pem

# Token -- write the token string to a file, then lock it down
Set-Content C:\ProgramData\kuma-agent\token '<token>'
icacls C:\ProgramData\kuma-agent\token /inheritance:r /grant:r 'Administrators:F' 'kuma-agent:R'

# Sleep script (only for machines that should sleep on command)
Copy-Item deploy\windows\sleep.ps1 C:\scripts\sleep.ps1
```

#### 4. Install and start the service

```powershell
C:\ProgramData\kuma-agent\bin\kuma-agent.exe install
sc start kuma-agent
```

The install command prompts for the `kuma-agent` account password. It creates the
service with automatic start, restart-after-5s recovery, and
`FailureActionsOnNonCrashFailures`. The service manager grants "Log on as a service"
automatically.

Check `C:\ProgramData\kuma-agent\agent.log` and the dashboard to confirm the agent is
connected.

If Windows Defender flags the binary, add an exclusion for
`C:\ProgramData\kuma-agent\bin\`.

## Agent config reference

```yaml
device:
  name: <device-name>      # Must match the name registered on the server

server:
  url: wss://<server-ip>:8443/ws
  ca_file: /etc/kuma-agent/ca.pem   # Path to the server's CA certificate

token_file: /etc/kuma-agent/token   # Path to the bearer token file

capabilities:               # What this agent supports
  - metrics                 # Always present
  - custom-commands         # Enable named commands
  - wol-target              # This device can be woken by WoL

commands:                   # Named commands (only if custom-commands is enabled)
  uptime:
    description: "Show system uptime"
    run: ["/usr/bin/uptime"]
    timeout_s: 5
```

Commands are defined locally on each agent. The server can only trigger commands the
agent declares; it cannot send arbitrary shell strings.

## Building agents with release keys

To compile the ed25519 public keys into agent binaries (required for upgrade
verification):

```sh
make agent-all \
  RELEASE_KEY_CURRENT=<hex-encoded-current-public-key> \
  RELEASE_KEY_NEXT=<hex-encoded-next-public-key>
```

Or for a versioned release:

```sh
make release VERSION=0.3.0 \
  RELEASE_KEY_CURRENT=<hex-encoded-current-public-key> \
  RELEASE_KEY_NEXT=<hex-encoded-next-public-key>
```

The agent verifies downloaded binaries against either key slot, allowing the current key
to be rotated to the next key without leaving a window where agents cannot verify
signatures.

## Releasing an agent upgrade

1. Build a release: `make release VERSION=x.y.z RELEASE_KEY_CURRENT=... RELEASE_KEY_NEXT=...`
2. Sign it (as root on the control plane host): `kumaboard sign --version x.y.z -config /etc/kumaboard/config.yaml`
3. Ingest it: `kumaboard release ingest --version x.y.z -config /etc/kumaboard/config.yaml`
4. In the dashboard, set the target version per device. Upgrades are pushed one device at
   a time at the next handshake.

The agent downloads the binary over HTTPS (pinned CA), verifies the SHA-256 hash and
ed25519 signature, runs `--selftest` on the new binary, swaps it into place, and exits
to let the service manager restart it. If the new binary fails to handshake within 120
seconds, it rolls back to the previous binary automatically.

## CLI reference

```
kumaboard serve -config PATH             Start the server
kumaboard passwd -config PATH            Set the operator password (min 12 chars)
kumaboard device add -config PATH NAME   Register a device, print its token once
kumaboard cert reissue -config PATH      Reissue the server TLS certificate
kumaboard sign --version x.y.z           Sign release artifacts (run as root)
kumaboard release ingest --version x.y.z Ingest a signed release into the database
kumaboard version                        Print version and protocol version

kuma-agent run -config PATH              Run the agent
kuma-agent selftest -config PATH         Local-only self-check (config, certs, collectors)
kuma-agent version                       Print version and protocol version
```

## File layout

### Server (control plane host)

```
/opt/kumaboard/kumaboard              Server binary
/etc/kumaboard/config.yaml            Server config
/var/lib/kumaboard/
  kumaboard.db                        SQLite database (devices, sessions, audit, upgrades)
  pki/
    ca.pem                            CA certificate (distribute to agents)
    ca.key                            CA private key (mode 0600)
    server.pem                        Server TLS certificate
    server.key                        Server TLS key (mode 0600)
    release_ed25519                   Signing key (mode 0600, root-owned)
  releases/<version>/                 Signed release artifacts
  backup/                             Encrypted nightly backups
```

### Agent (Linux / macOS)

```
/opt/kuma-agent/
  kuma-agent                          Agent binary (writable by kuma-agent for upgrades)
  kuma-agent.old                      Previous binary (kept after upgrade)
/etc/kuma-agent/
  config.yaml                         Agent config (root-owned, 0644)
  ca.pem                              Server CA certificate (root-owned, 0644)
  token                               Bearer token (kuma-agent-owned, 0600)
```

### Agent (Windows)

```
C:\ProgramData\kuma-agent\
  bin\kuma-agent.exe                  Agent binary
  bin\kuma-agent.exe.old              Previous binary (kept after upgrade)
  config.yaml                         Agent config
  ca.pem                              Server CA certificate
  token                               Bearer token (ACL: Administrators:F, kuma-agent:R)
```

## License

No license has been chosen yet. Until one is added, all rights are reserved.
