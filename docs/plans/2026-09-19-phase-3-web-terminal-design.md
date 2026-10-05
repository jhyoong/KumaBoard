# Phase 3: Web Terminal Design

Date: 2026-09-19
Source: docs/phase-3-web-terminal.md

## Summary

An xterm.js terminal in the dashboard, relayed by the server between a browser
WebSocket and an agent WebSocket. The browser never talks to an agent directly.
Unix (Linux + macOS) only; Windows ConPTY deferred to phase 3.1.

## Decisions made

- **Recording:** metadata only (user, device, start/end, bytes in/out). No full
  transcripts.
- **Session limit:** max 2 concurrent terminal sessions per device. No per-user
  limit.
- **Windows:** compile-time stub that returns "unsupported". Skipped for this
  phase.
- **Agent terminal socket:** reuses the same TLS config and base URL as the
  control socket, with path `/ws/terminal`.
- **`terminal_enabled` toggle:** exposed via the existing
  `PATCH /api/devices/{name}` endpoint.

## Architecture: Hub-integrated broker

A new `server/terminal/` package that the hub and API layer both reference. The
hub sends `terminal_open` and receives `terminal_open_result` via the existing
control socket. The broker manages tickets, pairing, relay, and session tracking.

## Server terminal broker (`server/terminal/`)

### Ticket system

- `POST /api/devices/{name}/terminal` (session-cookie auth) creates a `Ticket`
  with a random ID, bound to the requesting user and device. Single-use, expires
  after 30 seconds. Returns `{ "ticket": "...", "session_id": "..." }`.
- Tickets are stored in-memory in the broker. A background sweep cleans expired
  tickets.

### WebSocket endpoint

- `GET /ws/terminal` accepts both browser and agent connections. The first text
  frame determines which:
  - Browser sends `{ "ticket": "..." }` -- the broker validates the ticket,
    consumes it, marks the browser side as connected.
  - Agent sends `{ "session_id": "...", "agent_ticket": "..." }` -- the broker
    validates the agent ticket, marks the agent side as connected.
- Once both sides are paired, the broker relays: binary frames pass through
  untouched (raw PTY bytes), text frames are JSON control messages (`resize`
  from browser, `exit` from agent).
- Either side closing ends the session and closes the other socket.

### Session lifecycle

1. Browser requests a ticket via REST.
2. Server sends `terminal_open` to the agent via the hub's control socket (with
   `session_id`, `agent_ticket`, `cols`, `rows`).
3. Agent replies `terminal_open_result: ok` on the control socket.
4. Browser opens `/ws/terminal` and sends its ticket.
5. Agent opens `/ws/terminal` and sends its agent ticket.
6. Broker pairs them and begins relaying.
7. If pairing doesn't complete within 30s, void tickets and close the browser
   socket with an error.

### Limits and timeouts

- Max 2 concurrent sessions per device (checked at ticket creation).
- 30-minute idle timeout (no bytes relayed in either direction). Reset on any
  frame.
- Reused or expired tickets rejected immediately on both sockets.

### Tracking

- On session open: insert a row into `terminal_sessions` and write to audit log.
- During relay: increment `bytes_in` / `bytes_out` counters in memory.
- On session close: update `ended_at` and final byte counts in
  `terminal_sessions`, write to audit log.

### Gate checks (at ticket creation)

- Device has `terminal` in its declared capabilities (from last handshake).
- Device has `terminal_enabled` set server-side.
- Device is connected (hub has a live session).

## Agent terminal (`agent/terminal/`)

### Handling `terminal_open`

- New case in `app.OnMessage` for `TypeTerminalOpen`.
- If `terminal` is not in the agent's local config capabilities, reply
  `terminal_open_result: refused`. This is defense-in-depth: even a compromised
  server cannot open a terminal on a device that disallows it.
- If allowed, reply `terminal_open_result: ok` on the control socket.

### Terminal socket

- Dial `wss://<server>/ws/terminal` using the same TLS config and base URL as
  the control socket.
- First text frame: `{ "session_id": "...", "agent_ticket": "..." }`. No device
  token on this socket.

### PTY spawning (`pty_unix.go`)

- Use `creack/pty` to spawn the user's default shell (`$SHELL`, fallback
  `/bin/sh`) as the agent's own user account.
- Set initial size from `cols`/`rows` in the `terminal_open` message.
- Two goroutines: PTY stdout to socket (binary frames), socket to PTY stdin
  (binary frames).
- Handle `resize` text frames via `pty.Setsize`.
- On shell exit, send `{ "type": "exit", "code": <exit_code> }` and close the
  socket.

### PTY cleanup

- On socket close, kill the PTY process group
  (`syscall.Kill(-pid, SIGKILL)`) and reap with `os.Process.Wait`.

### Windows stub (`pty_windows.go`)

- Compile-time stub that always returns "unsupported".

### Selftest extension

- When `terminal` is in config capabilities, selftest spawns a PTY, writes a
  short string, reads it back, and closes.

## Frontend

### Terminal view

- New route: `/devices/:name/terminal`.
- "Terminal" button on the device detail page. Visible when device is connected,
  has `terminal` capability, and has `terminal_enabled` set.

### Connection flow

1. On mount, `POST /api/devices/{name}/terminal` to get ticket and session ID.
2. Open WebSocket to `/ws/terminal`.
3. Send `{ "ticket": "..." }` as the first text frame.
4. Binary frames wired to `xterm.Terminal.write()` for output.
5. `xterm.Terminal.onData` sends binary frames for input.
6. `xterm.Terminal.onResize` sends `{ "type": "resize", "cols": ..., "rows": ... }` text frames.
7. On `exit` text frame, display exit code and close.

### Cleanup

- On unmount, close the WebSocket. Server closes the agent side, killing the PTY
  within 5 seconds.
- `beforeunload` fallback for tab close.

### Dependencies

- `xterm` and `@xterm/addon-fit` added to web package.

### UI

- Dark terminal background, full content width.
- Small header bar: device name, session status, "Disconnect" button.
- Error states: ticket failed, pairing timeout, unexpected close.

## API and store changes

### `PATCH /api/devices/{name}` extension

- Accept `terminal_enabled` (boolean) in the request body.
- Update `UpdateDeviceSettings` to write `terminal_enabled`.

### New REST endpoint

- `POST /api/devices/{name}/terminal` -- returns
  `{ "ticket": "...", "session_id": "..." }` or error with reason
  (`not_connected`, `terminal_not_enabled`, `terminal_not_capable`,
  `session_limit`).

### New WebSocket endpoint

- `GET /ws/terminal` -- no auth middleware; authentication via ticket in first
  frame. Mounted on root mux.

### Store additions

- `InsertTerminalSession(ctx, id, deviceID, user)`.
- `CloseTerminalSession(ctx, id, bytesIn, bytesOut)`.
- `CountActiveTerminalSessions(ctx, deviceID)`.

### Migration

No new migration needed. `terminal_sessions` table and `terminal_enabled` column
already exist in `0001_init.sql`.

## Wire format

- Binary frames: raw PTY bytes, relayed untouched.
- Text frames: JSON control only. Browser sends `resize`, agent sends `exit`.
  First text frame from each side is the ticket/hello.

## Security

- Browser never talks to the agent directly.
- Browser ticket: single-use, 30s expiry, bound to device and user. In a frame,
  never in the URL.
- Agent ticket: single-use, 30s expiry, sent on the control socket. Device token
  never on the terminal socket.
- TLS end-to-end: browser-server (HTTPS), agent-server (pinned CA).
- Agent-side capability check as defense-in-depth.
- Shell runs as `homelab-agent`, not root.

## Out of scope

- Windows ConPTY (phase 3.1).
- Full transcript recording.
- Session sharing between users.
- Web-based file transfer through the terminal.
