# Phase 4: Wake-run-sleep (v4)

Source: PLAN.md sections "Wake-run-sleep (v4)", "Wake and sleep on the headless Windows box",
"Roadmap: v4".

## Goal

Chain wake, command, and sleep on the headless Windows box as an explicit state machine with every
transition persisted. Phase 1 shipped the two halves (Wake button and `sleep` command);
this phase joins them.

Also ships the Windows terminal (phase 3.1) if it slipped from phase 3.

## Tasks

### 4a. State machine (`server/wake/`)

- [ ] Implement the states and transitions exactly as in PLAN.md:

  ```
  idle
    -> waking             server sends WoL directly; retry every 10s, up to 6 attempts
    -> waiting_for_agent  wait for the agent handshake, timeout 180s
    -> running            execute the requested command(s)
    -> sleeping           invoke the device's sleep command (expect_disconnect)
    -> verifying          run ends as disconnected_as_expected within 120s
    -> done | failed(reason)
  ```

- [ ] Persist every transition in `wake_jobs` (`state`, `attempts`, `started_at`,
      `finished_at`, `failure_reason`) and write it to the audit log
- [ ] Failure at any step leaves the device awake and surfaces in the dashboard. No blind
      retry
- [ ] Never chain a sleep onto a command whose result is unknown. A run that ended `lost` or
      `timeout` stops the job at `failed` with the device left awake
- [ ] On server startup, any `wake_jobs` row still in a non-terminal state is closed as
      `failed(server_restart)` and the device is left as is

### 4b. Dashboard

- [ ] Wake-run-sleep control on `wol-target` devices: pick one or more named commands, start
      the job
- [ ] Job status view showing the current state, attempts, and failure reason
- [ ] Job history per device

### 4c. Windows terminal (phase 3.1, if slipped)

- [ ] Finish ConPTY support in `agent/terminal/pty_windows.go`
- [ ] Push via upgrade to the headless Windows box only; the Windows desktop stays without `terminal`
- [ ] Re-run the phase 3 acceptance criteria against the headless Windows box

## Acceptance criteria

PLAN.md does not list criteria for this phase. These are derived from its rules. Confirm
them before starting.

- [ ] A job from a sleeping headless Windows box wakes it, runs a command, sleeps it, and ends `done`
- [ ] A job whose command times out ends `failed` with the device left awake and no sleep
      sent
- [ ] Killing the agent mid-command ends the job `failed` with reason from the `lost` run
      and no sleep sent
- [ ] If the box does not handshake within 180s of the first WoL, the job ends `failed` after
      6 WoL attempts
- [ ] Every transition of a job is visible in `wake_jobs` and the audit log
- [ ] Restarting the server mid-job closes the job as failed and never sends a sleep
      afterwards

## Dependencies

- Phase 1 WoL sender and `sleep` command with `expect_disconnect`
- Phase 3 for the Windows terminal carry-over
