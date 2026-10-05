# Phase 3: Web terminal (v3)

Source: PLAN.md sections "Terminal sessions (v3)", "Terminal policy (v3)",
"Platform notes", "Roadmap: v3".

## Goal

An xterm.js terminal in the dashboard, relayed by the server between a browser socket and
an agent socket. The browser never talks to an agent directly.

Linux and macOS first. Windows ConPTY may slip to phase 3.1 and must not block this phase.
The terminal stays off on the Windows desktop.

## Tasks

### 3a. Server terminal broker (`server/terminal/`)

- [ ] REST endpoint that issues a browser ticket: single-use, 30s, bound to one device,
      session-cookie auth
- [ ] WebSocket endpoint at `/ws/terminal`. The first text frame carries the ticket, never
      the URL
- [ ] Send `terminal_open` on the agent's control socket with `session_id`, a single-use
      30s `agent_ticket`, `cols`, and `rows`
- [ ] Only open a terminal when both hold: `terminal` is in the agent's declared
      capabilities and `terminal_enabled` is set for the device server-side
- [ ] Pair the browser socket and the agent socket by `session_id` and relay frames.
      Binary frames are raw PTY bytes relayed untouched. Text frames are JSON control
      (`resize` browser to agent, `exit` agent to browser)
- [ ] Either side closing ends the session; close the other socket. No separate close
      message
- [ ] If pairing does not complete within 30s, void both tickets and close the browser
      socket with an error
- [ ] Maximum two concurrent sessions per device
- [ ] Idle timeout at 30 minutes
- [ ] Record every session in `terminal_sessions`: user, device, start, end, bytes in and
      out. Write open and close to the audit log
- [ ] Reject a reused or expired ticket on both sockets

### 3b. Agent terminal (`agent/terminal/`)

- [ ] On `terminal_open`, check `terminal` is in the local capabilities. If not, reply
      `terminal_open_result: refused` and do nothing else. This protects the Windows desktop
      even from a compromised server
- [ ] Otherwise reply `ok`, dial `/ws/terminal` with the pinned CA, and send
      `{ "session_id", "agent_ticket" }` as the first text frame. No device token travels on
      this socket
- [ ] `pty_unix.go`: spawn a shell via `creack/pty` as the agent account. Handle `resize`
      control frames. Send `exit` with the code when the shell ends
- [ ] Kill the PTY process group when the socket closes
- [ ] `pty_windows.go`: ConPTY via `aymanbagabas/go-pty` or `UserExistsError/conpty`,
      running from a service in session 0, with its own resize handling. If this stalls,
      ship without it and move it to phase 3.1
- [ ] Extend `--selftest` to initialise and close a PTY when `terminal` is enabled

### 3c. Frontend

- [ ] Terminal view with xterm.js: request ticket, open socket, send ticket as first frame,
      wire binary frames to the terminal and resize events to text frames
- [ ] Close the socket when the tab or view closes

### 3d. Rollout

- [ ] Push the terminal-capable agent build via phase 2 upgrades, canary order
- [ ] Add `terminal` to the capabilities list in each device's config, except the main
      Windows desktop, and restart the agent
- [ ] Set `terminal_enabled` per device in the dashboard

## Decision to make at this phase

- [ ] Terminal session recording: metadata only (assumed) or full transcripts. Transcripts
      help audit but grow storage

## Operator notes

- The shell runs as `homelab-agent`. Escalate inside the session with your own credentials:
  `su - <you>` on Linux and macOS, `runas /user:<you>` on Windows. The agent account never
  gets general sudo

## Acceptance criteria

- [ ] A terminal opens on Debian, Pi, control plane host, and both Macs as the `homelab-agent` account;
      `su - <you>` works inside it
- [ ] Resize works; closing the browser tab kills the shell on the device within 5s
- [ ] A session survives 10 minutes idle, then closes cleanly at 30
- [ ] A reused or expired ticket is rejected on both the browser and the agent socket
- [ ] With `terminal` absent from the Windows desktop's config, a forced `terminal_open` is
      refused by the agent
- [ ] Packet capture shows no terminal content in cleartext

## Dependencies

- Phase 2 complete, so the terminal-capable agent can be pushed rather than redeployed
- `terminal.go` message shapes from phase 1a
