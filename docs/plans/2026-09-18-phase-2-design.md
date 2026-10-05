# Phase 2 Design: Agent Upgrades

Date: 2026-09-18
Source: [PLAN.md](../PLAN.md), [phase-2-agent-upgrades.md](../phase-2-agent-upgrades.md)

## Decisions made during brainstorming

| Decision | Choice |
|---|---|
| Fleet state | Nothing deployed yet; everything local and integration tests only |
| Trigger timing | Handshake plus immediate: evaluate at handshake and again when target is set or Retry is pressed on a connected device |
| Dashboard scope | Per-device upgrade panel only; no separate releases page |
| Signing CLI | `kumaboard sign` subcommand in the existing binary; consistent with passwd/cert-reissue pattern |
| Release ingestion | `kumaboard release ingest` subcommand; reads signed manifest, inserts into releases table |
| Selftest form | `kuma-agent selftest` subcommand (matches existing CLI pattern); upgrade module calls it as `kuma-agent selftest` |

## 1. Release tooling

Three independent steps: build, sign, ingest.

### make release

`make release VERSION=x.y.z` builds all four agent targets into a local
`releases/<version>/` directory and writes an unsigned `manifest.json`.

Layout:

```
releases/0.4.0/
  manifest.json
  linux_amd64/kuma-agent
  linux_arm64/kuma-agent
  darwin_arm64/kuma-agent
  windows_amd64/kuma-agent.exe
```

`manifest.json` schema:

```json
{
  "version": "0.4.0",
  "artifacts": [
    {
      "os": "linux",
      "arch": "amd64",
      "sha256": "<hex>",
      "size_bytes": 12345678,
      "signature": ""
    }
  ]
}
```

The Makefile computes SHA-256 per artifact and populates the manifest. Signatures
are empty until the sign step.

### kumaboard sign

`sudo kumaboard sign --version x.y.z -config /etc/kumaboard/config.yaml`

Runs as root on the control plane host. Reads the private key from
`<data_dir>/pki/release_ed25519`. For each artifact in the manifest:

1. Read the binary, compute SHA-256, confirm it matches the manifest.
2. Construct the signed message: `homelab-agent\n<version>\n<os>\n<arch>\n<sha256>\n`.
3. Sign with ed25519.
4. Write the base64-encoded signature into the manifest.

Writes the updated `manifest.json` back with all signatures filled.

### kumaboard release ingest

`kumaboard release ingest --version x.y.z -config /etc/kumaboard/config.yaml`

Reads the signed manifest from `<data_dir>/releases/<version>/manifest.json`.
Validates all signatures are present. Inserts one row per artifact into the
`releases` table. Writes an audit log entry per artifact.

### Typical workflow

```
make release VERSION=0.4.0
scp -r releases/0.4.0 linux-server:/var/lib/kumaboard/releases/0.4.0
ssh linux-server 'sudo kumaboard sign --version 0.4.0 && kumaboard release ingest --version 0.4.0'
```

## 2. Public keys and buildinfo

Two ed25519 public keys compiled into every agent binary via `-ldflags -X`:

- `buildinfo.ReleaseKeyCurrentHex`
- `buildinfo.ReleaseKeyNextHex`

The Makefile accepts `RELEASE_KEY_CURRENT` and `RELEASE_KEY_NEXT` as hex-encoded
public key variables. A test keypair in `configs/dev-release-keys.txt` lets local
builds work. Real keys are passed on the command line for production builds and
never checked in.

Verification tries both keys: if either validates the signature, the check passes.
Key rotation: generate a new next key, compile it into the next build, start signing
with it, retire the old one.

## 3. Device-token auth and artifact serving

### Auth middleware

New middleware `RequireDeviceToken(store)` checks:

- `Authorization: Bearer <token>` header
- `X-Device-Name: <name>` header
- Looks up the device by name, verifies token hash with constant-time compare

Dashboard cookies are not valid for this path. Device tokens are not valid on
dashboard routes. The middleware is mounted separately from the session-auth wrapper
in `buildHandler`.

### Artifact endpoint

`GET /api/agent/releases/{version}/{os_arch}` serves the binary file. Authenticated
by device token only. Streams the file with `Content-Length` from the releases table.
Never serves a directory listing.

## 4. Agent selftest

`kuma-agent selftest` subcommand. Local checks only, no network:

1. Load and validate config (parse YAML, check all required fields).
2. Confirm token file exists and is readable.
3. Confirm `ca.pem` exists and is readable.
4. Initialize each metrics collector once with a short timeout.
5. Print version and protocol version.
6. Exit 0.

No PTY initialization (deferred to phase 3). The selftest should complete in under
2 seconds; the upgrade module enforces a 15-second outer timeout.

## 5. Agent upgrade module

### Package `agent/upgrade/`

`Upgrader` struct holds the binary path (`os.Executable()`), OS/arch, both parsed
public keys, the CA pool, server base URL, device name, token, and a logger.

### HandleUpgradeRequest

Runs in a single goroutine, guarded by a mutex to prevent concurrent upgrades:

1. Send `upgrade_result{state: downloading}`.
2. HTTP GET to `req.URL` with `Authorization: Bearer` and `X-Device-Name` headers,
   TLS config using the pinned CA pool. Download to a temp file in the binary
   directory (same filesystem as the current binary).
3. Send `upgrade_result{state: verifying}`.
4. Verify file size matches `req.SizeBytes`. Compute SHA-256 and compare to
   `req.SHA256`.
5. Reconstruct the signed message using `runtime.GOOS`/`runtime.GOARCH` and the
   computed hash. Verify the ed25519 signature against both key slots. Any failure:
   send `failed / verify_failed`, delete temp file, return.
6. Send `upgrade_result{state: selftest}`.
7. Run `<temp_file> selftest` as subprocess with 15-second timeout. Failure: send
   `failed / selftest_failed`, delete temp file, return.
8. Swap (see below).
9. Send `upgrade_result{state: swapped}`.
10. Write `upgrade_pending.json` to the binary directory:
    `{from_version, to_version, started_at, rollback_reason: ""}`.
11. Send `upgrade_result{state: restarting}`.
12. Exit. Unix/macOS: `os.Exit(0)` (systemd `Restart=always` and launchd
    `KeepAlive=true` restart on clean exit). Windows: `os.Exit(1)` (SCM restarts
    via failure actions with `FailureActionsOnNonCrashFailures`).

The agent never retries an upgrade on its own.

### Swap

**Unix:**

1. Remove `<binary>.old` if it exists.
2. Hard-link `<binary>` to `<binary>.old` (preserves the running inode as a backup).
3. `os.Rename(temp, <binary>)` -- atomic replace of the path.

**Windows:**

1. Remove `<binary>.old` if it exists.
2. Remove `<binary>.failed` if it exists (leftover from a previous rollback).
3. `os.Rename(<binary>, <binary>.old)`.
4. `os.Rename(temp, <binary>)`.

## 6. Rollback on startup

Checked before the transport connects, called from `app.New` or the run
subcommand.

If `upgrade_pending.json` exists in the binary directory:

- **Own version = `to_version` (probation):** Start a 120-second timer. When
  `OnConnected` fires (handshake succeeded), delete the marker and `.old`, send
  `upgrade_result{state: verified}`. If the timer fires first, write
  `rollback_reason: no_handshake` into the marker, restore `.old`, exit. On
  Windows: rename the running binary to `.failed` first, then `.old` to the
  binary path. Delete `.failed` at the next start.

- **Own version = `from_version` (rolled back):** After the next successful
  handshake, send `upgrade_result{state: rolled_back, reason: <from marker>}`,
  delete the marker.

- **Neither (stale marker):** Log a warning and delete the marker.

The probation state is communicated via an `UpgradeState` struct returned by the
startup check, which the `App` holds and acts on in `OnConnected`.

## 7. Server trigger model

### Eligibility check

```
eligible(device) =
    device.desired_agent_version != "" AND
    device.desired_agent_version != device.agent_version AND
    no agent_upgrades row anywhere in fleet is in-flight AND
    no agent_upgrades row for THIS device AND THIS target version
        is rolled_back or failed
```

In-flight states: `requested`, `downloading`, `verifying`, `selftest`, `swapped`,
`restarting`.

### Handshake trigger

In `hub.serve`, after `hello_ack` is sent:

1. If device handshakes at `from_version` while its upgrade row is in-flight,
   close the row as `rolled_back`.
2. Evaluate eligibility. If eligible, look up the release for
   `(desired_version, device.os, device.arch)`, create an `agent_upgrades` row as
   `requested`, send `upgrade_request`.

### Immediate trigger

When `desired_agent_version` is set via the API or Retry is pressed:

- If the device is currently connected, evaluate eligibility immediately and send
  the upgrade request through the existing session.
- If the device is not connected, the declarative model picks it up at next
  handshake.

### upgrade_result handler

New case in `hub.handleMessage` for `TypeUpgradeResult`:

- Update the `agent_upgrades` row state. Timestamp each transition. Audit log each
  transition.
- Terminal states (`verified`, `rolled_back`, `failed`) set `finished_at`.

### Store additions (server/store/upgrades.go)

- `CreateUpgrade(ctx, deviceID, fromVersion, toVersion, requestedBy) -> id`
- `UpdateUpgradeState(ctx, id, state, reason)`
- `GetActiveUpgrade(ctx) -> *Upgrade` (any in-flight row in the fleet)
- `GetDeviceUpgrades(ctx, deviceID) -> []Upgrade`
- `GetLatestUpgrade(ctx, deviceID) -> *Upgrade`
- `HasFailedUpgrade(ctx, deviceID, toVersion) -> bool`
- `AbandonUpgrade(ctx, id)`

## 8. Dashboard upgrade panel

On the device detail page, a new section below the existing info:

- **Target version dropdown:** populated by `GET /api/releases?os=<os>&arch=<arch>`.
  Setting it sends `PATCH /api/devices/{name}` with `desired_agent_version`. Empty
  option clears the target.
- **Upgrade status:** latest `agent_upgrades` row: state, from/to versions,
  timestamps, reason if failed.
- **Retry button:** visible when latest upgrade is `rolled_back` or `failed`.
  Sends `POST /api/devices/{name}/upgrades/retry`.
- **Abandon button:** visible when latest upgrade is in-flight. Sends
  `POST /api/devices/{name}/upgrades/{id}/abandon`. Marks `failed(abandoned)`.

### New API endpoints (session-authed)

- `GET /api/releases` (query: `os`, `arch`) -- `[{version, os, arch, added_at}]`
- `GET /api/devices/{name}/upgrades` -- upgrade history for one device
- `POST /api/devices/{name}/upgrades/retry` -- records a fresh attempt
- `POST /api/devices/{name}/upgrades/{id}/abandon` -- marks `failed(abandoned)`

The existing `PATCH /api/devices/{name}` body is extended to accept
`desired_agent_version`.

## 9. Service restart configuration

Already in place from phase 1:

- systemd: `Restart=always`, `RestartSec=5`
- launchd: `KeepAlive=true`, `ThrottleInterval=10`
- Windows: recovery actions restart after 5s, `FailureActionsOnNonCrashFailures=true`

No changes needed.

## 10. Testing

### Unit tests

- `agent/upgrade/`: mock HTTP server for downloads, mock binary for selftest,
  signature verification, swap logic in a temp directory, rollback marker handling.
- `server/store/upgrades.go`: CRUD on the upgrades and releases tables.
- `internal/signing/`: sign and verify round-trip.

### Integration tests

Extend existing harness:

- Server sends `upgrade_request` when desired version differs and conditions hold.
- Agent reports state transitions through to `verified`.
- Rollback: agent connects at `from_version` after upgrade row is in-flight.
- One-at-a-time: second device upgrade blocked while first is in flight.
- Retry and Abandon flows.

### Manual acceptance

The nine acceptance criteria from the spec are run on real hardware after
deployment.

## New files

```
agent/upgrade/upgrade.go            Upgrader, HandleUpgradeRequest
agent/upgrade/upgrade_unix.go       swap and rollback (Unix)
agent/upgrade/upgrade_windows.go    swap and rollback (Windows)
agent/upgrade/upgrade_test.go
agent/upgrade/pending.go            upgrade_pending.json read/write
cmd/kuma-agent/main.go              add selftest subcommand
server/store/upgrades.go            upgrade and release store operations
server/store/upgrades_test.go
server/api/releases.go              artifact serving and release list
server/api/upgrades.go              upgrade API endpoints (retry, abandon, list)
server/auth/devicetoken.go          device-token auth middleware
internal/buildinfo/buildinfo.go     add ReleaseKeyCurrentHex, ReleaseKeyNextHex
internal/signing/signing.go         ed25519 sign/verify shared by CLI and agent
internal/signing/signing_test.go
cmd/kumaboard/sign.go               kumaboard sign subcommand
cmd/kumaboard/release.go            kumaboard release ingest subcommand
web/src/components/UpgradePanel.tsx
```
