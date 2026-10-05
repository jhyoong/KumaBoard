# Phase 6: Task queue (v6)

Source: PLAN.md sections "Roadmap: v6", "Control plane: Components" (Scheduler).

## Goal

Reusable task definitions, cron scheduling, and run history. Each task declares what to do
when its target device is offline.

## Tasks

### 6a. Task model

- [ ] Task definition: name, target device, named command (or Docker action from phase 5),
      cron expression, offline policy, enabled flag
- [ ] Offline policies:
      - `skip`: do nothing if the device is not online at fire time
      - `queue_until_online`: run at the next handshake
      - `wake_run_sleep`: hand off to the phase 4 state machine
- [ ] Storage: `tasks` and `task_runs` tables. Task runs reference `command_runs` and, where
      relevant, `wake_jobs`

### 6b. Scheduler (`server/scheduler/`)

- [ ] Cron-style evaluation in the server's local time, consistent with the schedule
      configuration in the device state model
- [ ] Respect declared downtime windows: a `queue_until_online` task fired during a window
      waits for the handshake, it does not alert
- [ ] One in-flight run per task. A task that fires while its previous run is still going
      records `skipped_busy`
- [ ] On server startup, any `task_runs` row left in flight is closed as `lost`, matching
      the command run rule
- [ ] Every fire, skip, and outcome is written to the audit log

### 6c. Dashboard

- [ ] Task list with create, edit, enable, disable, delete
- [ ] Run history per task with status and link to the underlying command run or wake job
- [ ] Manual "run now" button

## Acceptance criteria

PLAN.md does not list criteria for this phase. These are derived from its rules. Confirm
them before starting.

- [ ] A `skip` task fired while the Linux box sleeps records a skip and does not alert
- [ ] A `queue_until_online` task fired while the Linux box sleeps runs on its next
      handshake, once
- [ ] A `wake_run_sleep` task on the headless Windows box runs end to end through the phase 4 state
      machine
- [ ] Restarting the server mid-run closes the run as `lost` and does not re-fire it
- [ ] A task's cron schedule survives a server restart

## Dependencies

- Phase 4 for the `wake_run_sleep` policy
- Phase 5 if Docker actions are to be schedulable
