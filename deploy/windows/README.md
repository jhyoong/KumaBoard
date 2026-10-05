# Windows agent setup

Normal path: run `deploy-agent.ps1` from the operator box (see its header; `-WhatIf` for a dry run). It pushes everything to `%USERPROFILE%\kuma-deploy` on the target with a verified token and prints ONE elevated command, labeled `[PowerShell (admin)]` / `[cmd.exe (admin)]`:

    powershell.exe -NoProfile -ExecutionPolicy Bypass -File <dir>\setup.ps1 -StageDir <dir>

`setup.ps1 -StageDir` checks the installed `config.yaml` first (timestamped backup + diff; refuses to change anything unless `-OverwriteConfig` or `-KeepConfig`), stops the service before replacing the exe, moves the token into place with a locked ACL, registers/starts the service and prints `agent.log`. Afterwards check the dashboard or `deploy-agent.ps1 -VerifyOnly -DeviceName <name>`; never rerun the deploy to verify (it refuses to re-key a registered device without `-RotateToken`).

Manual path (what `-StageDir` automates):

1. Run `setup.ps1` as Administrator. It is idempotent and aborts on the first failing command. It:
   - creates the local standard user `kuma-agent` if missing (random password nobody knows, password never expires, member of Users only — never an administrator);
   - via `secedit`, adds `kuma-agent` to "Deny log on locally", "Deny log on through Remote Desktop Services" and "Log on as a service", then re-exports the policy to verify. If a domain GPO overrides local user rights the script stops and tells you to set them in `secpol.msc` (Local Policies, User Rights Assignment). Never add it to "Deny log on as a service";
   - creates `C:\ProgramData\kuma-agent` (+ `bin`) and `C:\scripts` with the ACLs. The agent gets read-only access to the data directory, Modify on `bin` (self-upgrade) and Modify on a pre-created `agent.log` (without it the service exits with code 2 and restart-loops).
2. Copy `kuma-agent.exe` to `C:\ProgramData\kuma-agent\bin\`, `config.yaml` and `ca.pem` to `C:\ProgramData\kuma-agent\`, and `sleep.ps1` to `C:\scripts\`. Write the token to `C:\ProgramData\kuma-agent\token` and lock it down with `icacls C:\ProgramData\kuma-agent\token /inheritance:r /grant:r Administrators:F SYSTEM:F kuma-agent:R`.
3. Run `setup.ps1 -InstallService` as Administrator. It sets a fresh random password on `kuma-agent` and registers the service with it (automatic start, restart-after-5s recovery, `FailureActionsOnNonCrashFailures`). Nobody has to know the password.
4. `sc.exe start kuma-agent`, then check `C:\ProgramData\kuma-agent\agent.log` and the dashboard.
5. Verify `sleep.ps1` works as the agent account by clicking `sleep` in the dashboard. If `SetSuspendState` is refused for a standard user, register a scheduled task that runs `sleep.ps1` as an administrator and change the `sleep` command to `schtasks /Run /TN KumaSleep`.
6. If Windows Defender flags the binary, add an exclusion for `C:\ProgramData\kuma-agent\bin\`.

Never run the service as SYSTEM.
