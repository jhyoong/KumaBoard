# Phase 7: Alerts and DNS panel (v7)

Source: PLAN.md sections "Roadmap: v7", "Device state model", "Control plane: Components"
(Alert engine), "Platform notes" (Pi), "Backup and restore".

## Goal

Threshold and state alert rules with downtime-window suppression, browser notifications,
and a DNS panel reading Blocky query stats from a database rather than the Pi's SD card.

## Tasks

### 7a. Alert engine (`server/alerts/`)

- [ ] Rule types:
      - Disk usage above a threshold per device
      - Device `offline_unexpected` (the state exists from phase 1; this is where it starts
        alerting)
      - Command run failed, timed out, or lost
      - Nightly backup missed or failed
- [ ] Suppression: never alert inside a declared `expected_offline` window plus its grace
      period, and never for `normally_off` devices
- [ ] Debounce so a Wi-Fi flap on the macOS desktop does not alert on every reconnect
- [ ] Storage: `alert_rules` and `alerts` tables with fired, acknowledged, and resolved
      timestamps
- [ ] Audit log entries for each fired and acknowledged alert

### 7b. Backup reporting

- [ ] The backup script from phase 1f writes a status the server can read (for example a
      row in the database or a file with a timestamp), so a missed backup becomes a rule

### 7c. Notifications

- [ ] Browser notifications from the dashboard, permission requested on first use
- [ ] Alert list view with acknowledge and resolve

### 7d. DNS panel

- [ ] Configure Blocky on the Pi to write query logs to a database on the Linux box or mini
      PC (phase 0 table). The Pi's SD card is never read by the agent
- [ ] Server reads query stats from that database: total queries, blocked count, top
      domains, top clients
- [ ] DNS panel widget in the dashboard

## Acceptance criteria

PLAN.md does not list criteria for this phase. These are derived from its rules. Confirm
them before starting.

- [ ] The Linux box's nightly sleep for a week produces zero alerts
- [ ] Pulling the Pi's power outside any window produces one `offline_unexpected` alert
      within the grace period, not one per minute
- [ ] Disabling the backup timer for a night produces a missed-backup alert
- [ ] Filling a disk past the threshold on a test device produces one alert that resolves
      when space is freed
- [ ] The DNS panel shows data with no agent process reading files on the Pi's SD card

## Dependencies

- Phase 1 four-state model and backup timer
- Blocky query logging redirected to a database
