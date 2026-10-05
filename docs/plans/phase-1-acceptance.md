# Phase 1 Acceptance Record

| Criterion | Procedure | Date | Observed Result | Pass/Fail |
|---|---|---|---|---|
| Server restart reconnects all awake agents within 120s | `systemctl restart kumaboard` on the control plane host; watch the devices page; every awake device is `online` within two minutes | | | |
| Every agent machine reboots and reconnects with no user logged in | Reboot each of the seven; do not log in; device shows `online` | | | |
| macOS desktop Wi-Fi drop for 5 minutes, exactly one session | Turn Wi-Fi off for 5 minutes, back on; device returns to `online`; server log shows one `agent connected` and no `replacing duplicate session` loop; `SessionCount` is checked via a temporary log line or by clicking a command once | | | |
| Linux box 01:00 to 05:00 for a week shows `offline_expected` | Set the window with grace 300s; each morning check the audit log and device state history for any `offline_unexpected` transition | | | |
| Headless Windows box wakes from the dashboard within 120s | With the box asleep, click Wake; time until `online` | | | |
| `sleep` ends as `disconnected_as_expected` | Click `sleep` on the headless Windows box; the run status is checked on its page | | | |
| Killing an agent mid-command records `lost` | Run `uptime`-style long command (`sleep 20` in a test config), `kill -9` the agent process; run shows `lost` | | | |
| No token in cleartext on the LAN | Task 24 step 2 procedure: `sudo tcpdump -i en0 -A port 8443 \| grep -c -i "token"` while an agent reconnects; expected `0` | | | |
| 401 for every `/api` route without a session | `curl` each route listed in design section 4 without a cookie; all 401 | | | |
| Unsupported protocol version shows "incompatible" | Build a test agent with `proto.Version` temporarily set to 99; deploy to the Linux box; the card shows the incompatible banner; redeploy the real build | | | |
| Agent cannot write its own config | On each device, as the agent account: `touch /etc/kuma-agent/config.yaml` fails (Unix) or `echo x >> C:\ProgramData\kuma-agent\config.yaml` fails (Windows) | | | |
| Restore drill lists all devices | `deploy/backup/restore.md` drill section | | | |
