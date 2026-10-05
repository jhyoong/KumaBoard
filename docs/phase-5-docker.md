# Phase 5: Docker (v5)

Source: PLAN.md sections "Roadmap: v5", "On the plugin system", "Agent accounts and
install layout", "Open decisions" item 6.

## Goal

List, start, stop, and restart containers on the Linux box and the macOS desktop. This is the
third widget, so it is also where the extension pattern is extracted, if a real one exists.

## Tasks

### 5a. Access decisions

- [ ] Debian: add `homelab-agent` to the `docker` group. This is equivalent to root. Accept
      it consciously and record the decision
- [ ] macOS desktop: macOS has no `docker` group and the socket belongs to the operator's user.
      Choose one:
      - Exact-match `sudo -u <you>` sudoers rules for the specific `docker` invocations
      - A socket proxy that exposes only list, start, stop, and restart
- [ ] Record the chosen approach in the README

### 5b. Agent Docker module

- [ ] New `docker` capability declared in config
- [ ] List containers (name, image, state, uptime)
- [ ] Start, stop, restart by container name. The name is passed as an argv element to
      `exec`, never concatenated into a shell string, and validated against an allowlist
      pattern declared in config
- [ ] Results returned through the existing command result path with the same statuses

### 5c. Server and frontend

- [ ] Route Docker requests to devices declaring the `docker` capability
- [ ] Container list widget with per-container start, stop, restart buttons
- [ ] Audit log entries for every container action

### 5d. Extract the extension pattern

With three widgets in place (commands, wake and sleep, Docker):

- [ ] Compare the three. Extract only the commonality that is actually there
- [ ] Backend: compile-time registry where a module registers itself in `init()`
- [ ] Frontend: components register in a map keyed by capability
- [ ] Do not use Go's `plugin` package. It needs matching Go and dependency versions, works
      only on Linux and macOS, and breaks static single-binary builds
- [ ] State in the README that extension means recompiling the server. Do not promise "add
      a widget without touching core code"

## Acceptance criteria

PLAN.md does not list criteria for this phase. These are derived from its rules. Confirm
them before starting.

- [ ] Containers on Debian and the macOS desktop list correctly and can be started, stopped, and
      restarted from the dashboard
- [ ] A container name that fails the allowlist pattern is rejected before anything executes
- [ ] The agent account on the macOS desktop has no Docker access beyond the chosen list, start,
      stop, restart path
- [ ] Every container action appears in the audit log
- [ ] Adding a fourth capability requires a new module and component plus a recompile, and
      nothing else in core changes

## Dependencies

- Phase 2, so the Docker-capable agent can be pushed
- macOS desktop Docker access decision made
