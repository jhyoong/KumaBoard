# Phase 2: Agent upgrades (v2)

Source: PLAN.md sections "Agent upgrades", "Upgrade messages (frozen at protocol v1)",
"Roadmap: v2".

## Goal

Push signed agent binaries from the control plane, one device at a time, with self-rollback.
The design goal is never to end up with a device that is running and unreachable.

Do this second, before the fleet is fully settled and while the agent is still changing
daily. The first upgrade-capable build goes out by manual `make deploy-<device>`; everything
after that is pushed.

## Tasks

### 2a. Keys and release tooling

- [ ] Generate both ed25519 keypairs:
      - Current private key at `/var/lib/homelab-cp/pki/release_ed25519`, `root:root`,
        mode `0600`, plus a copy in the password manager
      - Next private key in the password manager only. It never touches the control plane host until
        needed
- [ ] Compile both public keys into the agent at build time via `-ldflags` (two key slots)
- [ ] `make release VERSION=x.y.z`: builds all four targets, computes SHA-256 per artifact,
      emits an unsigned `manifest.json`
- [ ] `homelab-cp sign --version x.y.z` CLI, run as root, signs each artifact. The signed
      message is the exact byte string `homelab-agent\n<version>\n<os>\n<arch>\n<sha256>\n`
- [ ] Release layout on the control plane host: `/var/lib/homelab-cp/releases/<version>/` with
      `manifest.json` and one binary per `os_arch` subdirectory
- [ ] Confirm the server process runs as an unprivileged user that cannot read the current
      signing key

### 2b. Release store and artifact serving

- [ ] `server/releases/`: load manifests into the `releases` table (version, os, arch,
      sha256, signature, size_bytes, added_at)
- [ ] Serve artifacts only via `/api/agent/releases/<version>/<os>_<arch>`, authenticated by
      device token (`Authorization: Bearer` plus `X-Device-Name`). Dashboard cookies are not
      valid here and device tokens are not valid on dashboard routes. Never serve the
      directory as static files

### 2c. Agent selftest

- [ ] `--selftest` flag, local checks only, no network: parse config and fail on anything
      invalid, confirm token file and `ca.pem` exist and are readable, initialise each
      metrics collector once, initialise and close a PTY if `terminal` is enabled, print
      version and protocol version, exit 0

### 2d. Agent upgrade module (`agent/upgrade/`)

- [ ] Handle `upgrade_request`; report each state via `upgrade_result`: `downloading`,
      `verifying`, `selftest`, `swapped`, `restarting`
- [ ] Download to a temp file in the binary directory over HTTPS with the pinned CA
- [ ] Verify size and SHA-256, rebuild the signed string from the agent's own compiled-in
      os/arch and its own computed hash, verify the ed25519 signature against either key
      slot. Any failure reports `failed / verify_failed` and leaves the agent untouched
- [ ] Run the new binary with `--selftest` as a subprocess; require exit 0 within 15s
- [ ] Swap:
      - Unix: atomic `rename(new, current)`
      - Windows: rename `agent.exe` to `agent.exe.old`, move the new binary into place
      - Keep the previous binary as `.old` on every platform
- [ ] Write `upgrade_pending.json` (`from_version`, `to_version`, `started_at`, empty
      `rollback_reason`)
- [ ] Report `restarting`, then exit and let the service manager restart the process. Never
      `exec()` into the new binary
- [ ] The agent never retries an upgrade on its own

### 2e. Rollback on startup

- [ ] If `upgrade_pending.json` exists, compare own version to the marker:
      - Own version equals `to_version`: probation. Handshake within 120s deletes the
        marker and `.old` and sends `upgrade_result: verified`. No handshake within 120s
        writes `rollback_reason: no_handshake`, restores `.old`, and exits. On Windows,
        rename the running `agent.exe` to `agent.exe.failed` first; delete `.failed` at the
        next start
      - Own version equals `from_version`: send `upgrade_result: rolled_back` with the
        marker's reason after handshake, then delete the marker
      - Neither: stale marker. Log and delete
- [ ] Accepted limit: a binary that will not start at all is not covered. Mitigations are
      the selftest, one-device-at-a-time rollout, and the retained `.old` binary. The
      launcher-binary fix is deferred until a start failure actually occurs

### 2f. Service restart configuration

- [ ] systemd: `Restart=always`, `RestartSec=5` (restarts on clean exit)
- [ ] launchd: `KeepAlive=true`; note the 10s respawn throttle
- [ ] Windows SCM: exit nonzero, failure actions restart after 5s, and
      `FailureActionsOnNonCrashFailures=true`

### 2g. Server trigger model and dashboard

- [ ] `desired_agent_version` per device. At every handshake, compare to reported
      `agent_version`
- [ ] Send `upgrade_request` only when all hold: versions differ, no other upgrade is in
      flight anywhere in the fleet, and no `agent_upgrades` row for this device and target
      version is already `rolled_back` or `failed`
- [ ] If a device handshakes at `from_version` while its upgrade row is in flight, close the
      row as `rolled_back`, using the agent's reported reason when it arrives
- [ ] Persist every state transition in `agent_upgrades` with timestamps and write each to
      the audit log
- [ ] Dashboard: per-device target version control, upgrade state, Retry button (records a
      fresh attempt), Abandon button (marks `failed(abandoned)`)
- [ ] No roll-out-to-all button. Set per device only
- [ ] Downgrade uses the identical path; the target is a version, not necessarily newer
- [ ] The server is never upgraded this way

## Rollout

Canary order for every release: control plane host's own agent, Linux box, Pi, macOS workstation, macOS desktop,
Windows desktop, headless Windows box last.

## Acceptance criteria

- [ ] A clean upgrade succeeds on all seven devices, one at a time
- [ ] An artifact with a corrupted byte is rejected at the hash check, agent unaffected
- [ ] An artifact signed with the wrong key is rejected, agent unaffected
- [ ] A validly signed artifact offered under a different version or arch is rejected
- [ ] A deliberately broken binary (wrong arch) fails `--selftest` and is never swapped in
- [ ] A binary pointed at the wrong server URL rolls back within 120s, the old version
      reports the failure, and the server does not resend the request until Retry
- [ ] The headless Windows box, left asleep with a new `desired_agent_version` set, upgrades on its
      next wake with no manual action
- [ ] A downgrade to the previous version works through the same path
- [ ] The server process, as its own unprivileged user, cannot read the signing key

## Dependencies

- Phase 1 complete and accepted
- Both signing keypairs generated (phase 0 table)
- The binary directory is writable by the agent account (phase 1e install layout)
