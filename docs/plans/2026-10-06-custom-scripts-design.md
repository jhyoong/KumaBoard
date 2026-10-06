# Custom scripts: design

- Date: 2026-10-06
- Status: proposed
- Scope: agent config → `proto` → hub/store → dashboard. Extends the existing
  named-commands feature so that operator-written scripts are practical to run
  from the dashboard. Not a numbered phase; it sits beside the phase list the
  way the temperature design did, and touches nothing phase 4 depends on
  except `command_runs`, which it only adds to.

## Problem statement

Phase 1 already ships the core of this: `commands:` in the agent's local YAML
(`agent/config/config.go`), declared in `hello`, stored per device
(`store.RecordHandshake`), rendered as one button each on the device page
(`web/src/views/DeviceDetail.tsx`), with run history and audit. A script can be
run today with `run: ["/bin/bash", "/opt/scripts/foo.sh"]`.

What makes that awkward for real scripts:

1. **Detection needs a restart.** The config is read once at agent start, so a
   new entry has no button until the service is restarted.
2. **No output until the end.** `command_output` is defined in the protocol and
   accepted by the hub (`router.appendOutput`), but the agent never sends it
   and the hub never forwards it to the browser.
3. **No way to stop a run.** It ends on exit or `timeout_s`.
4. **No guard against misclicks** on destructive scripts.
5. **Nothing checks the script file.** The config is root-owned, but a script
   it points at can sit somewhere the agent account can write. A compromised
   agent, or anyone in a web-terminal session (which runs as the same
   account), could then rewrite it and have the dashboard run the new content.

Constraints from the codebase:

- `proto` must not import `server/` or `agent/`. `Version = 1`,
  `MinSupported = 1`; the window is N/N-1.
- `CGO_ENABLED=0`; platform code is split `_unix.go` / `_windows.go` and both
  halves must compile. `golang.org/x/sys` is already a dependency.
- Both sides log and drop unknown message types (`hub.handleMessage` and
  `App.OnMessage` default cases), so new message types are additive.
- Everything in `hello` is untrusted input on the server.

## Hard rules

These are not up for negotiation in implementation or review.

- **R1. Definitions come only from the agent's local config file.** No API,
  message, or dashboard action can create, edit, or enable a command.
- **R2. The dashboard never supplies arguments.** `command_request` carries a
  name, `command_cancel` carries a run ID, and nothing else is read from
  either. A compromised dashboard or server can choose *which* declared
  command to start or stop, never *what* it executes. This supersedes the two
  "if a command ever accepts a parameter" bullets under *Command execution
  rules* in `PLAN.md`; those are replaced with "commands take no parameters".
- **R3. The server cannot trigger a config re-read.** Reload is driven by the
  agent watching its own file. There is no "rescan" message.

## Decisions

### D1. One concept: scripts are commands

No separate `scripts:` section, table, or UI group. `config.Command` gains two
optional fields:

```yaml
commands:
  backup-photos:
    description: "Rsync photos to the NAS"
    script: /opt/kuma-scripts/backup-photos.sh   # exec'd directly (shebang + x bit)
    timeout_s: 3600
    confirm: true

  clear-temp:
    description: "Clear the download temp folder"
    run: ["powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File"]
    script: C:\kuma-scripts\clear-temp.ps1         # appended as the last argv element
    timeout_s: 120
```

- `script`: absolute path to a script file. argv is `run + [script]`; with no
  `run`, the script is executed directly. At least one of `run` / `script` is
  required. Existing `run`-only entries keep working unchanged.
- `confirm`: dashboard asks before starting (D6).

`script` exists so the agent knows unambiguously which file to protect (D5).
Guessing from argv would misfire on legitimate data-file arguments.

- Rejected: a separate `scripts:` block. It would duplicate the runner, the
  store table, run history and the phase 4/6 references to "named command".

### D2. Hot reload by polling, agent-initiated

`agent/config` gets `LoadCommands(path) (map[string]Command, []Problem, error)`
which parses and validates only the `commands:` block. A watcher goroutine in
`agent/app` stats the config file every 5s; when size or mtime changes it
re-reads, and if the SHA-256 of the content differs it calls `LoadCommands`.

- Success: swap the runner's command map under its mutex and send
  `commands_update`.
- Parse failure: keep the previous set, log, and send `commands_update` with
  the old list plus a `config_error` string so the device page can say why
  nothing changed.
- Only `commands:` is live. `device`, `server`, `token_file` and
  `capabilities` still need a restart; a change to them is logged as ignored.
- A run in flight keeps the definition it started with. A removed command's
  run is not killed.
- On every (re)connect `hello` carries the current set, as today.

The D5 file check is also re-evaluated on each poll tick, so a script whose
permissions go bad loses its button without a config edit.

- Rejected: `fsnotify`. A new dependency, per-OS quirks with editors that
  write-then-rename, for a file that changes a few times a year.
- Rejected: dashboard "Rescan" button. Violates R3 for no gain over polling.
- Trust: unchanged. The file was already the root of trust and is not
  writable by the agent account.

### D3. Protocol: version 2, two new messages

`Version = 2`, `MinSupported = 1`. Server must be deployed before agents
(a v2 agent is rejected by a v1 server).

| Message | Direction | Payload |
|---|---|---|
| `commands_update` | agent → server | `{commands: []CommandDef, problems: []CommandProblem, config_error: string}` |
| `command_cancel` | server → agent | none; envelope `id` is the run ID |

- `CommandDef` gains `confirm bool`.
- `CommandProblem` is `{name, reason}`: a command present in the config but
  not declared because it failed the D5 check. `Hello` gains the same
  `problems` field.
- New run statuses: `cancelled`, `refused` (D5 check failed at run time).
- `command_output` gains an optional `skipped bool`; the agent now sends it (D4).

Server-side sanitising for everything above: cap the command list at 64
entries, names must match the existing `nameRe`, descriptions / reasons /
`config_error` through `truncateText` at 256 bytes.

The hub handles `commands_update` with the same store call as the handshake
(factor the command half of `RecordHandshake` into `ReplaceCommands`), audits
it as `commands_update` with added/removed names, and publishes a `device`
event so open dashboards redraw their buttons.

The server only sends `command_cancel` to sessions whose `hello` declared
protocol ≥ 2 (`Session` gains `ProtocolVersion`).

### D4. Live output

The 64 KiB per-stream cap changes meaning: it is now the **last** 64 KiB, at
every layer, so the dashboard behaves like `tail -f` and the stored result
matches the `stdout_tail` / `stderr_tail` column names. Streaming itself never
stops; only what is retained is bounded.

Agent: `capWriter` becomes a ring buffer that keeps the newest `MaxOutput`
bytes and counts the total written. A per-run flusher ticks every 250ms and
sends, per stream, the bytes written since the last flush as `command_output`
(`proto.Reply` to the request, so the envelope ID is the run ID). It holds
back an incomplete trailing UTF-8 sequence. If more than 64 KiB arrived within
one tick, only the newest 64 KiB is sent and the chunk carries
`skipped: true` (new optional field on `CommandOutput`). That bounds the wire
at roughly 256 KiB/s per stream however fast the script prints.
`expect_disconnect` commands do not stream. The final `command_result` carries
the ring contents (the tail), with `truncated: true` when anything was dropped
from the front, and stays authoritative.

Server: the agent is untrusted, so the server enforces the same bound itself.
`router.appendOutput` becomes tail-keeping (drop from the front, at a rune
boundary) instead of ignoring bytes past the cap, a chunk larger than 64 KiB
is cut to its tail, and `FinishRun` input is trimmed to the last 64 KiB.
`handleCommandOutput` also publishes an SSE `run_output` event `{run_id,
device, seq, stream, data, skipped}`. `GET /api/runs/{id}` overlays the
in-flight buffers onto `stdout_tail` / `stderr_tail` while the run is live, so
a page opened mid-run starts from the current tail and then follows the
stream.

Web: `live.ts` / `state.ts` learn `run_output`, append to the matching run and
trim to the last 64 KiB; a `skipped` chunk inserts an "output skipped" marker
line. `RunList` auto-expands a running row, keeps the view pinned to the
bottom unless the user has scrolled up, and its truncation note changes from
"output truncated at 64 KiB" to "showing the last 64 KiB".

Earlier output beyond the tail is gone for good; nothing writes a full log to
disk. A script that needs a complete log should write its own file.

### D5. Script file permission check

For each command with `script`, and for `run[0]` when it is an absolute path,
the agent verifies that its own account cannot modify what will be executed:

- Resolve the path component by component, following symlinks manually.
- The final file, and every directory traversed on the way (including the
  directories holding any symlink), must pass `notWritable`.

`notWritable` per platform:

| File | Rule |
|---|---|
| `agent/commands/filecheck_unix.go` | owner uid ≠ agent euid, and `access(W_OK)` fails. If the agent runs as root: not group- or world-writable. |
| `agent/commands/filecheck_windows.go` | Attempt `CreateFile` (no writes, `FILE_FLAG_BACKUP_SEMANTICS` for directories) requesting each of `FILE_WRITE_DATA`, `FILE_APPEND_DATA`, `DELETE`, `WRITE_DAC`, `WRITE_OWNER` (plus `FILE_ADD_FILE`, `FILE_DELETE_CHILD` for directories) in turn. Any success means writable. This asks the kernel for effective access, so group membership and ownership are covered without parsing the DACL. |

The check runs at three points:

1. Load and reload: a failing command is **not declared** (no button) and is
   reported as a `CommandProblem`, shown on the device page as
   "`backup-photos` not available: /opt/kuma-scripts is writable by the agent
   account".
2. Each poll tick (D2).
3. Immediately before exec: failure ends the run as `refused` with the reason
   in stderr. The resolved path is what gets executed.

The residual check-to-exec race needs a writable component in the chain,
which is exactly what the check just ruled out.

Deploy: `setup-agent.sh` (systemd, launchd) and `setup.ps1` create a
root/Administrators-owned scripts directory (`/opt/kuma-scripts`,
`C:\kuma-scripts`) that the agent account can read and execute only.
`deploy/windows/sleep.ps1` moves there and the example configs switch it to
`script:`.

- Rejected: pinned SHA-256 in the config. Every script edit would need a
  config edit; the permission check gives the property that matters (the
  agent account cannot change what runs) without that cost.

### D6. Confirm before run

`confirm: true` → `CommandDef.Confirm` → `commands.confirm` column → the
button opens a confirm dialog naming the device, command and description
before POSTing. This guards against misclicks only. It is enforced in the
browser and is not a security control; the design doc and example configs say
so.

### D7. Cancel

- API: `POST /api/runs/{id}/cancel` (session cookie auth, like every
  `/api/*` route). 404 unknown run, 409 if the run is not in flight, is
  `dispatched`, or the agent speaks protocol 1. Audited as `command_cancel`.
- Hub: looks up `inflight`, sends `command_cancel` with the run ID. The run
  stays `running` until the agent's `command_result` arrives; the existing
  timeout timer remains the backstop.
- Agent: `Runner.Execute` takes the run ID and registers a cancel func per
  run. Cancel sends SIGTERM to the process group, SIGKILL after 3s, and the
  result is `cancelled`. A cancel for an unknown run ID is ignored.
- Windows: `setProcAttr` is currently empty, so timeouts today leave child
  processes behind. This work assigns each run to a Job Object with
  `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` (`x/sys/windows`); cancel and timeout
  both terminate the job. That fixes the existing leak as a side effect.
- Web: a Stop button on running rows in `RunList` and beside the command
  button while it runs, hidden when `device.protocol_version < 2`.

### D8. Storage

`0006_commands_confirm.sql`:

```sql
ALTER TABLE commands ADD COLUMN confirm INTEGER NOT NULL DEFAULT 0;
CREATE TABLE command_problems (
    device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    name      TEXT NOT NULL,
    reason    TEXT NOT NULL,
    PRIMARY KEY (device_id, name)
);
ALTER TABLE devices ADD COLUMN commands_config_error TEXT NOT NULL DEFAULT '';
```

`command_runs` is unchanged; `cancelled` and `refused` are new values in the
existing free-text `status` column. `registry.Summary` and the web `Device`
type gain `command_problems` and `commands_config_error`.

## Build order

Each step leaves `make test` and `GOOS=windows GOARCH=amd64 go build ./...`
green.

1. **proto**: version 2, `commands_update`, `command_cancel`, `CommandDef.Confirm`,
   `CommandProblem`, new statuses, sanitisers. Tests in `messages_test.go`,
   including one that pins `CommandRequest`'s JSON shape to `{"name"}` (R2).
2. **agent config**: `script`, `confirm`, `LoadCommands`; validation tests.
3. **file check**: `filecheck_unix.go` / `filecheck_windows.go` + unit tests
   (temp dirs with owner/mode permutations; symlink in a writable directory).
4. **runner**: run-ID keyed, swappable command map, pre-exec check, streaming
   sink, cancel, Windows Job Object. Extends `runner_test.go`.
5. **agent app**: config watcher, `commands_update`, `command_cancel` handling.
6. **server**: migration, `ReplaceCommands`, hub handlers, `run_output` SSE,
   in-flight overlay on `GET /api/runs/{id}`, cancel endpoint.
7. **web**: types, `run_output` reducer, live output in `RunList`, Stop,
   confirm dialog, problems / config-error notice. Vitest for the reducer and
   the confirm gate.
8. **deploy + docs**: scripts directory in the setup scripts, example configs,
   `PLAN.md` (message table, command rules per R2, security section),
   `DEVICE-SETUP.md`.

## Integration tests (`internal/integration/`)

- Rewrite the agent's config file while connected → the new command appears
  in `ListCommands` and a `device` event fires; removing it removes it.
- Invalid YAML on reload → commands unchanged, `config_error` set, then
  cleared by a good write.
- `script` in an agent-writable temp dir → not declared, problem recorded,
  direct `RequestCommand` returns `ErrUnknownCommand`.
- Script made writable after declaration → next run ends `refused`.
- Slow script printing lines → `command_output` chunks arrive before the
  result; final stored output equals the concatenation.
- Script printing 200 KiB of numbered lines → stored `stdout_tail` is at most
  64 KiB, ends with the last line, `truncated` is true; `GET /api/runs/{id}`
  mid-run returns a tail no larger than 64 KiB.
- Burst of more than 64 KiB inside one flush tick → a chunk with `skipped`.
- A session sending oversized or endless `command_output` directly (no real
  agent) never grows the server's in-flight buffer past 64 KiB per stream.
- Cancel a `sleep 30` → `cancelled` within the kill grace; a child process
  started by the script is gone too.
- Cancel against a finished run → 409. Cancel against a protocol-1 session → 409.
- A `command_request` whose payload carries extra fields runs the declared
  argv unmodified (R2).

Unix-only tests skip on Windows as `commands_test.go` already does; the
Windows file check and Job Object get unit tests under a `windows` build tag
and a manual pass on the headless Windows box.

## Gotchas to carry into implementation

- The systemd unit has `ProtectSystem=strict` with only `/opt/kuma-agent`
  writable, and `PrivateTmp=true`. Scripts inherit that sandbox: one that
  writes elsewhere must go through an exact-match sudoers rule, as privileged
  commands do today. Document this next to the example config.
- `ProtectSystem=strict` makes `access(W_OK)` fail with `EROFS` on paths the
  agent could otherwise write. The owner-uid half of the Unix check is what
  still catches an agent-owned script there; keep both halves.
- The `custom-commands` capability is declared in configs but gates nothing
  in code. Left alone here.
- Protocol 2 agents must not be rolled out before the server is on 2.

## Settled (2026-10-06)

- **Output cap:** keep the last 64 KiB per stream, not the first (D4).
- **Concurrency:** unchanged. The busy lock stays per command name, so
  different scripts may run at once on one device. No exclusive flag.
- **Placement:** command buttons stay on the device detail page only. No
  quick buttons on the device cards for now.
