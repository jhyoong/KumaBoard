# Phase 0: Prerequisites and hardware checks

Source: PLAN.md sections "Prerequisites", "Wake and sleep on the headless Windows box",
"Agent accounts and install layout", "Open decisions".

## Goal

Confirm the assumptions the design rests on before writing code. Everything here is
hands-on work on the devices, not development.

## Must be done before phase 1 coding

### Headless Windows box WoL check

- [ ] `powercfg /a` lists S3. If only Modern Standby (S0) is offered, WoL needs its own test
- [ ] The box is on Ethernet, not Wi-Fi
- [ ] NIC properties: "Allow this device to wake the computer" and "Only allow a magic
      packet" are on
- [ ] WoL is enabled in the BIOS/UEFI
- [ ] A magic packet sent from another machine wakes the box from the state `sleep.ps1`
      leaves it in
- [ ] Do not change the third argument of `SetSuspendState` in `sleep.ps1`; it must stay
      `$false` so wake events remain armed

### Windows standard-user service check

- [ ] Create the `homelab-agent` local standard user on the headless Windows box
- [ ] Grant "Log on as a service"; deny interactive and RDP logon
- [ ] Confirm a service can start under that account with no user logged in
- [ ] Confirm `sleep.ps1` works when run as `homelab-agent`, not only from your own login.
      If `SetSuspendState` lacks the shutdown privilege, plan a scheduled task fallback
- [ ] Confirm `C:\scripts\` is not writable by `homelab-agent`

### Network

- [ ] LAN is one flat subnet (already confirmed per PLAN.md; re-verify if anything changed)
- [ ] Pick a temporary development host for the server until the control plane host is ready
- [ ] Reserve the control plane host's static IP or DHCP reservation. Every agent config will point
      at this IP

### Pi OS bitness

- [ ] Run `uname -m` on the Pi. `armv7l` means build `linux/arm` with `GOARM=7`;
      `aarch64` means add a `linux/arm64` build target instead

## Must be done before specific later steps

| Item | Needed before |
|---|---|
| llm-server moved from tmux to a `local.llm-server` LaunchDaemon running as the operator's user | Deploying the macOS workstation agent (phase 1e) |
| FileVault status recorded for both Macs. If on, a Mac reboot needs a hands-on unlock | Phase 1 acceptance |
| `age` keypair generated; private key in the password manager only, never on the control plane host | Phase 1f backup |
| Both ed25519 signing keypairs generated (current and next) | Phase 2 |
| Blocky query logging redirected to a database on the Linux box or control plane host | Phase 7 |
| `ca.pem` installed as a trusted root on the Windows desktop and iPhone | Optional; otherwise accept the browser warning |
| Tailscale on all seven devices | Optional convenience; not a security control |

## Decisions to make now

These do not block phase 1, but deciding early avoids rework.

- [ ] Metrics interval: 10s assumed, 30s is gentler on the Pi. Server-supplied, so it can
      change later
- [ ] Control plane host distribution: Debian or Ubuntu Server
- [ ] How releases reach the control plane host: `scp` of the release directory is assumed

## Permanent rule

Existing SSH access from the Windows desktop stays. It is the fallback path and is
never removed.
