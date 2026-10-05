# Development Phases

This is the index for the phase files split out of [PLAN.md](PLAN.md). Each phase file
holds the scope, ordered tasks, acceptance criteria, and dependencies for that phase.
PLAN.md stays the source of truth for the design; the phase files say what to build and
in which order.

## Phase list

| Phase | File | Ships |
|---|---|---|
| 0 | [phase-0-prerequisites.md](phase-0-prerequisites.md) | Hardware checks and decisions that must be settled before coding |
| 1 | [phase-1-foundation.md](phase-1-foundation.md) | Protocol, agent, server, dashboard, auth, packaging, backup (v1) |
| 2 | [phase-2-agent-upgrades.md](phase-2-agent-upgrades.md) | Signed push upgrades with self-rollback (v2) |
| 3 | [phase-3-web-terminal.md](phase-3-web-terminal.md) | Browser terminal to agents (v3) |
| 4 | [phase-4-wake-run-sleep.md](phase-4-wake-run-sleep.md) | Wake-run-sleep state machine, Windows terminal if slipped (v4) |
| 5 | [phase-5-docker.md](phase-5-docker.md) | Docker container control, extension pattern (v5) |
| 6 | [phase-6-task-queue.md](phase-6-task-queue.md) | Task definitions, cron scheduling, offline policy (v6) |
| 7 | [phase-7-alerts-dns.md](phase-7-alerts-dns.md) | Alert rules, notifications, DNS panel (v7) |

## Ordering rules

- Phases run in number order. Phase 2 deliberately comes before 3 so that every later
  agent change can be pushed instead of redeployed by hand.
- Phase 1 has internal sub-phases (1a to 1f). Protocol (1a) is built and tested before
  any UI work.
- A phase is done when its acceptance criteria all pass, not when it demos. Every phase
  must survive reboots, Wi-Fi drops, and the Linux box's 01:00 to 05:00 sleep window.
- Nothing from a later phase is pulled into phase 1.

## Deferred indefinitely

The four security widgets ("new MAC on LAN", "unexpected open ports", "port scan
detection", and related) are not scheduled. Each is a separate project. See PLAN.md,
"Deferred indefinitely".

## Out of scope for all phases

AI agent integration, Kubernetes management, control plane host provisioning, multi-user access
control, metrics history and graphing, intrusion detection.
