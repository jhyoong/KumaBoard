#Requires -Version 5.1
<#
.SYNOPSIS
  Zero-touch KumaBoard agent onboarding for macOS and Linux targets, driven
  entirely from a Windows operator box (PowerShell 5.1).
  Registers the device on the control plane, mints a fresh token on EVERY
  run (auto-rotation), stages signed artifacts, pushes them, runs the
  privileged install remotely via sudo -S, then polls the control plane
  until the agent checks in. After PASS there are ZERO manual steps left on
  the macOS/Linux target.
.USAGE
  # one command per device - macos-workstation, release 0.2.0:
  .\deploy-agent.ps1 -Version 0.2.0 -BinaryType darwin_arm64 -TargetSsh admin@192.168.1.128 -DeviceName macos-workstation
  # newest release on control plane (default -Version latest):
  .\deploy-agent.ps1 -BinaryType darwin_arm64 -TargetSsh admin@192.168.1.128 -DeviceName macos-workstation
  # linux target (add -IncludeGpu for the gpu capability):
  .\deploy-agent.ps1 -BinaryType linux_arm64 -TargetSsh admin@192.168.1.21 -DeviceName pi
  # stage only (no rotation, no target contact, no password prompt):
  .\deploy-agent.ps1 -BinaryType linux_arm64 -TargetSsh admin@192.168.1.21 -DeviceName pi -StageOnly
  # build from repo HEAD on the control plane instead of a signed release:
  .\deploy-agent.ps1 -FromBin -BinaryType linux_amd64 -TargetSsh admin@192.168.1.21 -DeviceName linux-box
  # new windows device (MAC enables Wake-on-LAN; read it with 'getmac /v' on the target):
  .\deploy-agent.ps1 -BinaryType windows_amd64 -TargetSsh admin@192.168.1.x -DeviceName new-win -Mac aa:bb:cc:dd:ee:ff
.VERIFY
  Status check only (touches NO token, NO config, NO target files):
    .\deploy-agent.ps1 -VerifyOnly -DeviceName new-win [-Version 0.2.0]
  Use this after the windows elevated step, or any time later. Never rerun
  the full deploy to "verify": that would try to re-key the device.
.DRYRUN
  Operator validation (args only: ZERO network calls, ZERO writes; prints the plan):
    pwsh -NoProfile -File deploy/windows/deploy-agent.ps1 -WhatIf -BinaryType windows_amd64 -TargetSsh admin@192.168.1.x -DeviceName new-win
  Windows PowerShell 5.1 equivalent:
    powershell -NoProfile -ExecutionPolicy Bypass -File deploy\windows\deploy-agent.ps1 -WhatIf -BinaryType windows_amd64 -TargetSsh admin@192.168.1.x -DeviceName new-win
  Not validated on the authoring box (no pwsh there); run the line above
  once before the first live deploy after any edit to this script.
.NOTES
  WHAT YOU TYPE: exactly ONE prompt on macOS/Linux deploys - the TARGET's
  sudo password (Read-Host -AsSecureString; never echoed; travels only via
  stdin through 'tr -d CR | sudo -S' on the target). Registration and
  tokens are fully automated: unknown devices are registered with
  'kumaboard device add' (-Mac, or the real MAC read from the target over
  ssh). An ALREADY-registered device is re-keyed only after a y/N prompt
  (or -RotateToken): re-keying disconnects the running agent until the new
  token is installed, so it is never implicit.
  - Checks + prompts: before any change the script checks the control plane
    (repo, python3, db, pki, bin/kumaboard, leftover scratch token) and the
    target (os/arch vs -BinaryType, required tools, earlier staged files,
    existing service/config). Anything that would be overridden (re-key,
    different installed config, leftover staged token, arch mismatch) is a
    y/N prompt; -RotateToken / -OverwriteConfig pre-answer theirs, -Yes all.
    No console (redirected stdin) = no. Binary sha256 is checked control
    plane -> staging -> target.
  - Token order (nothing live changes until delivery is proven):
      1 mint: python3 secrets.token_urlsafe(32) into a 0600 scratch file
        on the control plane (-ControlPlaneScratch); NO DB write
      2 deliver: scp -3 control plane -> target (streams through this
        process; never written to Windows disk, never in a PS variable)
      3 verify: sha256 of the target copy must equal the minted sha256
      4 commit: register if new, then sha256 -> devices.token_hash (the
        server verifies plain SHA-256 of the bearer token -
        server/store/devices.go HashToken + server/hub/hub.go)
    If 2 or 3 fails the script aborts before 4: the DB and any running
    agent are untouched.
  - Plaintext cleanup runs in a finally block on EVERY exit path: the
    control-plane scratch copy is always shredded; the target copy is
    removed unless it was handed off (windows: the elevated step moves it
    into place; macOS/Linux: the install consumed it).
  - Windows targets are NOT zero-touch: installing the kuma-agent service
    (SCM) requires local elevation and plain OpenSSH grants none. The
    script pushes artifacts and prints a paste-safe elevated checklist plus
    the explicit reason. No psexec/WinRM - out of scope, not provisioned.
  - Default artifact: signed release tree on the control plane,
      <repo>/data-local/releases/<version>/<os>_<arch>/<binary>
    -Version defaults to the newest dir there (ls | sort -V | tail -1);
    explicit -Version is validated numeric semver and its artifact must
    exist (no zero-key bin/ fallback; -FromBin is the explicit opt-in).
  - Verification: ssh + python3 against data-local/kumaboard.db (sqlite3
    CLI is NOT installed on the control plane), polling up to 90 s (agent
    reconnect backoff base is 1 s). PASS = agent_version equals -Version
    AND last_reject_reason empty AND last_seen within 90 s. Rotation nulls
    last_seen first, so PASS can only come from a NEW check-in with the
    NEW token. On FAIL: reject reason + last 20 lines of journalctl --user
    -u kumaboard.service filtered by device name (server runs as a systemd
    USER unit under the control-plane ssh user).
  - PS 5.1 compatible: no ternary ?:, no ??, no pipeline-chain operators, no pwsh-only
    syntax; verified by line-by-line review (no pwsh on any reachable box).
    Remote python/bash payloads were extracted and checked (bash -n,
    py_compile) and the control-plane ones run LIVE against the real
    data-local/kumaboard.db before being embedded.
#>
[CmdletBinding(SupportsShouldProcess = $true)]
param(
  # release version to deploy; 'latest' = newest dir under
  # <repo>/data-local/releases on the control plane
  [string]$Version = 'latest',
  # e.g. darwin_arm64, linux_amd64, linux_arm64, windows_amd64, windows_arm64
  # (required unless -VerifyOnly)
  [ValidatePattern('^(windows|linux|darwin)_(amd64|arm64)$')][string]$BinaryType,
  # ssh target of the device to deploy TO, e.g. admin@192.168.1.128
  # (required unless -VerifyOnly)
  [string]$TargetSsh,
  # device name in kumaboard; validated against the agent's own rule
  # ^[a-z0-9][a-z0-9-]{0,62}$ (agent/config/config.go), auto-registered if new
  [string]$DeviceName,
  # MAC for Wake-on-LAN, passed to 'kumaboard device add -mac' on first
  # registration. Required for WoL on windows targets (never read over cmd.exe);
  # on macOS/Linux it overrides the MAC read from the target.
  [ValidatePattern('^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$')][string]$Mac,
  # control plane ssh target, e.g. admin@192.168.1.150 (default: $env:KUMA_CONTROL_PLANE)
  [string]$ControlPlane = $env:KUMA_CONTROL_PLANE,
  # KumaBoard checkout on the control plane (holds bin/, data-local/)
  [string]$ControlPlaneRepo = '~/projects/KumaBoard',
  # control-plane dir for the transient 0600 plaintext token copy; must
  # exist and be private to the ssh user (default: $env:KUMA_CONTROL_PLANE_SCRATCH)
  [string]$ControlPlaneScratch = $env:KUMA_CONTROL_PLANE_SCRATCH,
  # server URL agents connect to, e.g. wss://192.168.1.150:8443/ws
  # (default: $env:KUMA_SERVER_URL)
  [string]$ServerUrl = $env:KUMA_SERVER_URL,
  # add the gpu capability on Linux targets (darwin/windows always get it)
  [switch]$IncludeGpu,
  # build from repo HEAD on the control plane into bin/ instead of using a
  # signed release artifact
  [switch]$FromBin,
  # no rotation, no target contact, no password prompt; stage into .\staging
  [switch]$StageOnly,
  # allow re-keying an ALREADY registered device (disconnects its running
  # agent until the new token is installed). Without it such runs abort.
  [switch]$RotateToken,
  # read-only: report the device's status from the control plane and exit.
  # No token, no config, no staging, no target contact.
  [switch]$VerifyOnly,
  # replace an existing, DIFFERENT agent config on the target (a timestamped
  # backup is kept and the diff printed). Without it such runs abort.
  [switch]$OverwriteConfig,
  # local agent config to deploy verbatim instead of the generated one (e.g.
  # a copy of configs/kuma-agent.windows.example.yaml with commands:)
  [string]$ConfigFile,
  # -VerifyOnly: keep polling up to this many seconds for a fresh check-in
  [ValidateRange(0, 600)][int]$VerifyWaitSec = 30,
  # answer yes to every y/N override prompt (re-key, replace config, delete
  # leftovers, arch mismatch). Without it each prompt asks; with no
  # interactive console an unanswered prompt means no and the run aborts.
  [switch]$Yes,
  [string]$StagingDir = (Join-Path (Get-Location) 'staging'),
  [string]$LocalKitDir = (Get-Location)
)

$ErrorActionPreference = 'Stop'
$cpRepo    = $ControlPlaneRepo
$cpData    = "${cpRepo}/data-local"
$cpScratch = $ControlPlaneScratch
foreach ($req in @(@('ControlPlane', $ControlPlane, 'KUMA_CONTROL_PLANE'), @('ControlPlaneScratch', $ControlPlaneScratch, 'KUMA_CONTROL_PLANE_SCRATCH'), @('ServerUrl', $ServerUrl, 'KUMA_SERVER_URL'))) {
  if (-not $req[1]) { throw "-$($req[0]) is required (or set `$env:$($req[2]))" }
}
$isWin     = $BinaryType -like 'windows_*'
$isMac     = $BinaryType -like 'darwin_*'
$binName   = 'kuma-agent'
if ($isWin) { $binName = 'kuma-agent.exe' }
$pass       = $null
$passPlain  = $null

function Step($m) { Write-Host "==> $m" -ForegroundColor Cyan }

# y/N prompt before overriding anything that already exists. -Yes, or the
# matching specific switch passed as $Preset, answers yes up front. With no
# interactive console the answer is no: the caller aborts, it never guesses.
function Confirm-Override {
  param([string]$Question, [bool]$Preset = $false)
  if ($Preset -or $Yes) {
    Write-Host "  $Question -> yes (preset by switch)" -ForegroundColor Yellow
    return $true
  }
  if (-not [Environment]::UserInteractive -or [Console]::IsInputRedirected) {
    Write-Host "  $Question -> no (no interactive console to ask)" -ForegroundColor Yellow
    return $false
  }
  while ($true) {
    $a = ([string](Read-Host "  $Question [y/N]")).Trim().ToLower()
    if ($a -eq 'y' -or $a -eq 'yes') { return $true }
    if ($a -eq '' -or $a -eq 'n' -or $a -eq 'no') { return $false }
    Write-Host '  please answer y or n' -ForegroundColor Yellow
  }
}

# last non-empty, trimmed line of remote output ('' if none)
function Get-LastLine([string]$Text) {
  return [string](@($Text -split "`n" | ForEach-Object { $_.Trim() } | Where-Object { $_ }) | Select-Object -Last 1)
}

# Run a native exe and capture stdout+stderr as text plus the exit code.
# PS 5.1: under $ErrorActionPreference='Stop', any stderr line from a native
# exe that is redirected (2>&1, 2>$null) becomes a terminating
# NativeCommandError (ssh prints warnings on stderr routinely). EAP is relaxed
# in THIS function's scope only, so the script-wide 'Stop' is untouched and
# restored automatically on return. Every ssh/scp call goes through here.
function Invoke-Native {
  param([string]$Exe, [string[]]$ArgList, [object]$StdIn = $null)
  $ErrorActionPreference = 'Continue'
  if ($null -ne $StdIn) {
    $raw = $StdIn | & $Exe @ArgList 2>&1
  } else {
    $raw = & $Exe @ArgList 2>&1
  }
  $rc = $LASTEXITCODE
  $text = (@($raw) | ForEach-Object { "$_" }) -join "`n"
  return [pscustomobject]@{ ExitCode = $rc; Output = $text }
}

# Run ssh; throw naming the exact command on non-zero exit. Returns combined
# stdout+stderr so failure diagnostics are never empty.
function Invoke-SshText {
  param([string]$Target, [string]$RemoteCmd, [switch]$AllowFail)
  $r = Invoke-Native 'ssh' @('-o', 'BatchMode=yes', '-o', 'ConnectTimeout=10', $Target, $RemoteCmd)
  if ($r.ExitCode -ne 0 -and -not $AllowFail) {
    throw "ssh failed (exit $($r.ExitCode)): ssh $Target `"$RemoteCmd`"`n$($r.Output)"
  }
  return $r.Output
}

# Run scp; throw naming the exact command on non-zero exit.
# -ThreeWay: remote-to-remote copy routed through this host (scp -3), so the
# data never lands on local disk.
function Invoke-Scp {
  param([string]$Src, [string]$Dst, [switch]$Recurse, [switch]$ThreeWay)
  $scpArgs = @('-o', 'BatchMode=yes', '-o', 'ConnectTimeout=10')
  if ($Recurse) { $scpArgs += '-r' }
  if ($ThreeWay) { $scpArgs += '-3' }
  $r = Invoke-Native 'scp' ($scpArgs + @($Src, $Dst))
  if ($r.ExitCode -ne 0) {
    throw "scp failed (exit $($r.ExitCode)): scp $Src $Dst`n$($r.Output)"
  }
}

# Fail-fast reachability probe: key-only (BatchMode), 5 s connect timeout.
function Assert-SshReachable {
  param([string]$Target, [string]$Role)
  $r = Invoke-Native 'ssh' @('-o', 'BatchMode=yes', '-o', 'ConnectTimeout=5', $Target, 'exit')
  if ($r.ExitCode -ne 0) {
    throw ("$Role $Target is not reachable with key-based SSH (ssh exit $($r.ExitCode)).`n" +
      "  ssh said: $($r.Output)`n" +
      "  check: host up, user name correct (-ControlPlane / -TargetSsh), key in authorized_keys.`n" +
      "  test by hand: ssh -o BatchMode=yes $Target exit")
  }
}

# Windows targets: OpenSSH hands the command line to the target's DefaultShell
# (HKLM:\SOFTWARE\OpenSSH), which is cmd.exe OR powershell.exe depending on
# the box. Under PowerShell, '%USERPROFILE%' is not expanded and 'if not exist'
# is a parse error. 'cmd /c <cmd>' runs the same cmd.exe line under either
# shell. Keep wrapped commands free of ( ) ; & | - PowerShell would parse
# those before cmd.exe sees them.
function Get-WinCmd([string]$Cmd) {
  return "cmd /c $Cmd"
}

# sha256 (lowercase hex) of a file on the target, '' if it cannot be read.
function Get-TargetSha256([string]$Path) {
  if ($isWin) {
    $out = Invoke-SshText $TargetSsh (Get-WinCmd "certutil -hashfile $Path SHA256") -AllowFail
  } else {
    $out = Invoke-SshText $TargetSsh "sha256sum $Path 2>/dev/null || shasum -a 256 $Path" -AllowFail
  }
  foreach ($l in ($out -split "`n")) {
    $h = ($l.Trim() -split '\s+')[0]
    # certutil prints the hex on its own line (older builds space-separated)
    if ($isWin) { $h = ($l -replace '\s', '') }
    if ($h -match '^[0-9a-fA-F]{64}$') { return $h.ToLower() }
  }
  return ''
}

# --- remote payloads -----------------------------------------------------------
# Payloads never travel as quoted command-line text: Windows PowerShell 5.1
# does not escape embedded double quotes when it builds a native command
# line, so '"file:"' inside an ssh argument silently loses its quotes.
# Instead each payload is base64-encoded ([A-Za-z0-9+/=], nothing to quote)
# and decoded on the remote side: python via 'python3 -' (script on stdin),
# the sudo install script via a file under /tmp/kbagent.
function ConvertTo-B64([string]$Text) {
  # CR stripped: this file may be checked out with CRLF line endings
  return [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes(($Text -replace "`r", '')))
}
# control-plane python: cd into the repo, decode, run with argv
function Get-CpPyCmd([string]$Py, [string]$PyArgs) {
  return "cd $cpRepo || exit 97; echo $(ConvertTo-B64 $Py) | base64 --decode | python3 - $PyArgs"
}

# Markers are KBREG:yes/no on purpose: 'NOT-REGISTERED' contains the
# substring 'REGISTERED', so a naive substring match would call unregistered
# devices registered.
$REG_PY = @'
import sqlite3, sys
c = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True)
row = c.execute("select 1 from devices where name=?", (sys.argv[2],)).fetchone()
print("KBREG:" + ("yes" if row else "no"))
c.close()
'@
# MINT: new random token into a 0600 scratch file ONLY. No DB write: the
# live token_hash is untouched until delivery to the target is verified.
# Prints the sha256 hex the target copy must match. The plaintext never
# leaves the control plane except via scp -3 straight to the target.
$MINT_PY = @'
import hashlib, os, secrets, sys
d, n = sys.argv[1], sys.argv[2]
os.makedirs(d, mode=0o700, exist_ok=True)
t = secrets.token_urlsafe(32).encode()
p = os.path.join(d, n + ".token")
fd = os.open(p, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
os.write(fd, t)
os.close(fd)
os.chmod(p, 0o600)
print("MINTED sha256=" + hashlib.sha256(t).hexdigest())
'@
# COMMIT: sha256(raw token) -> devices.token_hash (matches store.HashToken),
# only if the scratch file still hashes to what the target verified.
# last_seen=NULL: verification can never pass on a pre-deploy check-in.
$COMMIT_PY = @'
import hashlib, os, sqlite3, sys
db, n, d, want = sys.argv[1:5]
t = open(os.path.join(d, n + ".token"), "rb").read()
if hashlib.sha256(t).hexdigest() != want:
    print("COMMIT-MISMATCH")
    sys.exit(3)
c = sqlite3.connect(db, timeout=15)
cur = c.execute("UPDATE devices SET token_hash=?, last_seen=NULL WHERE name=?", (hashlib.sha256(t).digest(), n))
c.commit()
c.close()
print("COMMITTED rows=" + str(cur.rowcount))
'@
# VER prints:  agent_version|last_reject_reason|fresh|last_seen
# fresh=1 when last_seen is within 90 s of server UTC.
$VER_PY = @'
import sqlite3, sys
from datetime import datetime, timezone
now = datetime.now(timezone.utc)
c = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True)
r = c.execute("SELECT agent_version, last_reject_reason, IFNULL(last_seen, 0) FROM devices WHERE name=?", (sys.argv[2],)).fetchone()
c.close()
if not r:
    print("NOROW||0|never")
    sys.exit(0)
f = 0
if r[2] and str(r[2]) != "0":
    seen = datetime.strptime(str(r[2])[:19], "%Y-%m-%dT%H:%M:%S").replace(tzinfo=timezone.utc)
    if (now - seen).total_seconds() <= 90:
        f = 1
print((r[0] or "") + "|" + (r[1] or "") + "|" + str(f) + "|" + str(r[2]))
'@
# Real MAC of the target: en0 first (macOS + most SBCs report ether there),
# fallback ip link (Linux). PS double quotes with `$2 so awk survives.
$MAC_GET_BSH = "ifconfig en0 2>/dev/null | awk '/ether/{print `$2}' ; ip link 2>/dev/null | awk '/link\/ether/{print `$2; exit}'"

# Privileged install scripts, written to /tmp/kbagent/install.sh (base64)
# and run under sudo. Both run the setup kit first (idempotent: creates
# kuma-agent user + dirs) and end with the INSTALL-OK marker the PS side
# requires. The token is already verified in /tmp/kbagent/token.
$INSTALL_MAC_BSH = 'set -e; cd /tmp/kbagent; if [ -f setup-agent.sh ]; then tr -d "\r" < setup-agent.sh > setup-clean.sh; bash setup-clean.sh >/dev/null 2>&1 || true; fi; install -d -m 0755 -o kuma-agent -g staff /opt/kuma-agent; install -d -m 0755 -o root -g wheel /etc/kuma-agent; install -m 0755 -o kuma-agent -g staff /tmp/kbagent/kuma-agent /opt/kuma-agent/kuma-agent; xattr -d com.apple.quarantine /opt/kuma-agent/kuma-agent 2>/dev/null || true; if [ -f /etc/kuma-agent/config.yaml ]; then if ! cmp -s /tmp/kbagent/config.yaml /etc/kuma-agent/config.yaml; then cp -p /etc/kuma-agent/config.yaml /etc/kuma-agent/config.yaml.bak-$(date +%Y%m%d-%H%M%S); fi; fi; install -m 0644 -o root -g wheel /tmp/kbagent/config.yaml /etc/kuma-agent/config.yaml; install -m 0644 -o root -g wheel /tmp/kbagent/ca.pem /etc/kuma-agent/ca.pem; install -m 0600 -o kuma-agent -g staff /tmp/kbagent/token /etc/kuma-agent/token; install -m 0644 -o root -g wheel /tmp/kbagent/com.kumaboard.agent.plist /Library/LaunchDaemons/com.kumaboard.agent.plist; touch /var/log/kuma-agent.log; chown kuma-agent:staff /var/log/kuma-agent.log; launchctl bootout system /Library/LaunchDaemons/com.kumaboard.agent.plist 2>/dev/null || true; launchctl bootstrap system /Library/LaunchDaemons/com.kumaboard.agent.plist; pmset -a sleep 0; rm -f /tmp/kbagent/token /tmp/kbagent/config.yaml; echo INSTALL-OK'
$INSTALL_LINUX_BSH = 'set -e; cd /tmp/kbagent; if [ -f setup-agent.sh ]; then tr -d "\r" < setup-agent.sh > setup-clean.sh; bash setup-clean.sh >/dev/null 2>&1 || true; fi; id kuma-agent >/dev/null 2>&1 || useradd --system --create-home --home-dir /var/lib/kuma-agent --shell /bin/bash kuma-agent; install -d -m 0755 -o kuma-agent -g kuma-agent /opt/kuma-agent; install -d -m 0755 -o root -g root /etc/kuma-agent; if [ ! -f /etc/systemd/system/kuma-agent.service ]; then if [ -f /tmp/kbagent/kuma-agent.service ]; then install -m 0644 -o root -g root /tmp/kbagent/kuma-agent.service /etc/systemd/system/kuma-agent.service; fi; fi; install -m 0755 -o kuma-agent -g kuma-agent /tmp/kbagent/kuma-agent /opt/kuma-agent/kuma-agent; if [ -f /etc/kuma-agent/config.yaml ]; then if ! cmp -s /tmp/kbagent/config.yaml /etc/kuma-agent/config.yaml; then cp -p /etc/kuma-agent/config.yaml /etc/kuma-agent/config.yaml.bak-$(date +%Y%m%d-%H%M%S); fi; fi; install -m 0644 -o root -g root /tmp/kbagent/config.yaml /etc/kuma-agent/config.yaml; install -m 0644 -o root -g root /tmp/kbagent/ca.pem /etc/kuma-agent/ca.pem; install -m 0600 -o kuma-agent -g kuma-agent /tmp/kbagent/token /etc/kuma-agent/token; systemctl daemon-reload; systemctl restart kuma-agent; systemctl is-active kuma-agent; rm -f /tmp/kbagent/token /tmp/kbagent/config.yaml; echo INSTALL-OK'


# --- 1. argument validation (no network, no writes) ---------------------------
# Same rule the agent enforces on device.name (agent/config/config.go nameRe):
# die here, not in an unknown_device / config-load respawn loop on the target.
$nameRule = '^[a-z0-9][a-z0-9-]{0,62}$'
$dryRun = [bool]$WhatIfPreference
while ($true) {
  if (-not $DeviceName) {
    if ($dryRun) { throw '-WhatIf needs -DeviceName on the command line (dry-run validates args only, no prompts)' }
    $DeviceName = Read-Host 'Device name (lowercase, digits, hyphens; auto-registered if new)'
  }
  if ($DeviceName -cmatch $nameRule) { break }
  $msg = "'" + $DeviceName + "' rejected: must match " + $nameRule + ' (starts with a letter/digit, max 63 chars, no spaces, dots, uppercase)'
  if ($dryRun) { throw $msg }
  Write-Host $msg -ForegroundColor Yellow
  $DeviceName = $null
}
if ($VerifyOnly) {
  if ($StageOnly -or $RotateToken -or $FromBin) { throw '-VerifyOnly cannot be combined with -StageOnly, -RotateToken or -FromBin' }
} else {
  if (-not $BinaryType) { throw '-BinaryType is required (e.g. windows_amd64, linux_arm64, darwin_arm64)' }
  if (-not $TargetSsh) { throw '-TargetSsh is required (user@host of the device)' }
}
# ssh targets and control-plane paths are interpolated into remote shell
# command lines: allow only plain user@host / path characters.
foreach ($t in @($ControlPlane, $TargetSsh | Where-Object { $_ })) {
  if ($t -notmatch '^([A-Za-z0-9._-]+@)?[A-Za-z0-9._-]+$') { throw "ssh target '$t' rejected: expected user@host" }
}
foreach ($d in @($ControlPlaneRepo, $ControlPlaneScratch)) {
  if ($d -notmatch '^[A-Za-z0-9_./~-]+$') { throw "control-plane path '$d' rejected: letters, digits, _ . / ~ - only" }
}
if ($ServerUrl -notmatch '^wss://[A-Za-z0-9.:\[\]-]+(/\S*)?$') { throw "-ServerUrl '$ServerUrl' rejected: expected wss://host:port/ws" }
if ($FromBin -and ($Version -ne 'latest')) {
  throw '-FromBin builds from repo HEAD and cannot be combined with an explicit -Version'
}
if (($Version -ne 'latest') -and -not ($Version.Trim() -match '^[0-9]+(\.[0-9]+)*$')) {
  throw "-Version '$Version' is not a valid version (numeric dot-separated, e.g. 0.2.0)"
}
if ($ConfigFile) {
  if (-not (Test-Path -LiteralPath $ConfigFile -PathType Leaf)) { throw "-ConfigFile '$ConfigFile' not found" }
  $cfText = Get-Content -LiteralPath $ConfigFile -Raw
  if ($cfText -notmatch ('(?m)^\s+name:\s*' + [regex]::Escape($DeviceName) + '\s*$')) { throw "-ConfigFile '$ConfigFile': device.name is not '$DeviceName'" }
  if ($cfText -notmatch '(?m)^token_file:') { throw "-ConfigFile '$ConfigFile' has no token_file:" }
  # the install puts the token and ca.pem at fixed paths; a config pointing
  # elsewhere would start an agent that cannot read either
  if ($BinaryType) {
    $cfWant = @{ token_file = '/etc/kuma-agent/token'; ca_file = '/etc/kuma-agent/ca.pem' }
    if ($BinaryType -like 'windows_*') { $cfWant = @{ token_file = 'C:\ProgramData\kuma-agent\token'; ca_file = 'C:\ProgramData\kuma-agent\ca.pem' } }
    foreach ($k in $cfWant.Keys) {
      if ($cfText -notmatch ('(?m)^\s*' + $k + ':\s*["'']?' + [regex]::Escape($cfWant[$k]) + '["'']?\s*$')) {
        throw "-ConfigFile '$ConfigFile': $k must be $($cfWant[$k]) (where the install puts it for $BinaryType)"
      }
    }
  }
}
if ($Mac) { $Mac = $Mac.ToLower() }
if ($isWin -and -not $Mac -and -not $VerifyOnly) {
  Write-Host "WARN: no -Mac given: if '$DeviceName' is new it is registered WITHOUT a MAC (Wake-on-LAN unset;" -ForegroundColor Yellow
  Write-Host "      the MAC is not readable over cmd.exe). Get it on the target with 'getmac /v' and pass -Mac aa:bb:cc:dd:ee:ff." -ForegroundColor Yellow
}

# --- dry run: print the plan, touch nothing ------------------------------------
if ($dryRun -and $VerifyOnly) {
  Write-Host 'DRY RUN (-WhatIf): arguments valid. Plan (nothing below is executed):'
  Write-Host "  -VerifyOnly: probe ssh $ControlPlane, read device '$DeviceName' status (read-only DB query), poll up to $VerifyWaitSec s"
  return
}
if ($dryRun) {
  Write-Host 'DRY RUN (-WhatIf): arguments valid. Plan (nothing below is executed):'
  Write-Host "  control plane   $ControlPlane  (repo $cpRepo, scratch $cpScratch)"
  Write-Host "  target          $TargetSsh  ($BinaryType)"
  Write-Host "  device          $DeviceName  (mac: $(if ($Mac) { $Mac } else { 'unset / auto' }))"
  Write-Host "  server url      $ServerUrl"
  Write-Host "  artifact        $(if ($FromBin) { 'build repo HEAD on control plane (-FromBin)' } else { "release $Version from ${cpData}/releases" })"
  Write-Host "  staging dir     $StagingDir"
  Write-Host "  agent config    $(if ($ConfigFile) { "verbatim from $ConfigFile" } else { 'generated' }); existing different config on target: $(if ($OverwriteConfig -or $Yes) { 'backup + overwrite (preset)' } else { 'ask y/N' })"
  Write-Host "  token           registered device: $(if ($RotateToken -or $Yes) { 're-key (preset)' } else { 'ask y/N before re-keying' })"
  Write-Host '  steps: 1 probe ssh + prerequisites (control plane, target: os/arch, tools, leftovers)'
  Write-Host '         2 resolve artifact  3 register if new  4 token  5 stage + push (sha256 checked)'
  Write-Host '         6 install (windows: elevated checklist)  7 verify'
  if ($StageOnly) { Write-Host '  -StageOnly: stops after staging; no token, no target contact' }
  return
}

# --- 1b. preflight: fail fast if ssh endpoints are unreachable ------------------
# built only now: $DeviceName may have come from the prompt above
$regChk = Get-CpPyCmd $REG_PY "data-local/kumaboard.db $DeviceName"
$verCmd = Get-CpPyCmd $VER_PY "data-local/kumaboard.db $DeviceName"
foreach ($tool in 'ssh', 'scp') {
  if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) { throw "$tool not found on PATH (enable the Windows 'OpenSSH Client' optional feature)" }
}
Assert-SshReachable $ControlPlane 'control plane'

# --- 1c. control-plane prerequisites (one read-only round trip) ----------------
Step "checking control-plane prerequisites ($ControlPlane)"
$cpChk = "cd $cpRepo 2>/dev/null || { echo KBMISS:repo-dir; echo KBCHK:done; exit 0; }; " +
  'for c in python3 base64 sha256sum; do command -v $c >/dev/null 2>&1 || echo KBMISS:$c; done; ' +
  'for f in data-local/kumaboard.db data-local/config.yaml data-local/pki/ca.pem; do [ -f $f ] || echo KBMISS:$f; done; ' +
  '[ -x bin/kumaboard ] || echo KBMISS:bin/kumaboard; ' +
  "[ -e ${cpScratch}/${DeviceName}.token ] && echo KBLEFT:scratch-token; echo KBCHK:done"
$cpChkOut = Invoke-SshText $ControlPlane $cpChk -AllowFail
if ($cpChkOut -notmatch 'KBCHK:done') { throw "control-plane prerequisite check did not complete:`n$cpChkOut" }
$cpMissing = @([regex]::Matches($cpChkOut, 'KBMISS:(\S+)') | ForEach-Object { $_.Groups[1].Value })
# needed by every mode (status queries, token commit, artifact + ca download)
$cpFatal = @($cpMissing | Where-Object { $_ -in @('repo-dir', 'python3', 'base64', 'data-local/kumaboard.db') })
if (-not $VerifyOnly) { $cpFatal += @($cpMissing | Where-Object { $_ -in @('sha256sum', 'data-local/pki/ca.pem') }) }
if ($cpFatal.Count -gt 0) {
  throw "control plane $ControlPlane is missing: $($cpFatal -join ', ') (repo $cpRepo; check -ControlPlane / -ControlPlaneRepo)"
}

# One read-only status sample: @{ Ver; Reject; Fresh; Seen } or $null.
function Get-DeviceStatus {
  $vOut = Invoke-SshText $ControlPlane $verCmd -AllowFail
  $line = @($vOut -split "`n" | Where-Object { $_ -match '\|' }) | Select-Object -Last 1
  if (-not $line) { return $null }
  $parts = $line.Trim() -split '\|'
  if ($parts.Count -lt 4) { return $null }
  return @{ Ver = $parts[0]; Reject = $parts[1]; Fresh = $parts[2]; Seen = $parts[3] }
}

# Where to look next, per platform. Never "rerun the deploy".
function Write-NextSteps {
  Write-Host '  next: watch the dashboard, and read the agent log on the target:'
  if ($isWin -or -not $BinaryType) { Write-Host '    [cmd.exe] type C:\ProgramData\kuma-agent\agent.log' }
  if ($isMac -or -not $BinaryType) { Write-Host '    [macOS]   tail -n 50 /var/log/kuma-agent.log' }
  if ((-not $isWin -and -not $isMac) -or -not $BinaryType) { Write-Host '    [Linux]   journalctl -u kuma-agent -n 50 --no-pager' }
  Write-Host "  re-check status any time (read-only): .\deploy-agent.ps1 -VerifyOnly -DeviceName $DeviceName"
}

# --- verify-only: status report, no side effects --------------------------------
if ($VerifyOnly) {
  $regOut = Invoke-SshText $ControlPlane $regChk
  if ($regOut -notmatch 'KBREG:yes') { throw "'$DeviceName' is not registered on $ControlPlane" }
  $want = $Version.Trim()
  Step "status of '$DeviceName' (read-only; polling up to $VerifyWaitSec s for a fresh check-in)"
  $deadline = (Get-Date).AddSeconds($VerifyWaitSec)
  $ok = $false
  $st = $null
  while ($true) {
    $st = Get-DeviceStatus
    if ($st) {
      $ok = ($st.Fresh -eq '1') -and ($st.Reject -eq '')
      if (($want -ne 'latest') -and ($st.Ver -ne $want)) { $ok = $false }
    }
    if ($ok -or ((Get-Date) -ge $deadline)) { break }
    Start-Sleep -Seconds 5
  }
  if (-not $st) { throw "could not read status of '$DeviceName' from $ControlPlane" }
  Write-Host "  agent_version      $($st.Ver)$(if ($want -ne 'latest') { "   (want $want)" })"
  Write-Host "  last_reject_reason $($st.Reject)   (want empty)"
  Write-Host "  last_seen          $($st.Seen)   (fresh=$($st.Fresh), want 1 = within 90 s)"
  if ($ok) {
    Write-Host "RESULT: CONNECTED - $DeviceName is checking in." -ForegroundColor Green
    exit 0
  }
  Write-Host "RESULT: NOT CONNECTED - $DeviceName has no fresh, clean check-in." -ForegroundColor Red
  Write-NextSteps
  exit 1
}

$osPart   = ($BinaryType -split '_')[0]
$archPart = ($BinaryType -split '_')[1]
$homeWin  = ''
$scpStage = ''
if (-not $StageOnly) {
  Assert-SshReachable $TargetSsh 'target'

  # --- 1d. leftover control-plane scratch token -------------------------------
  # Present only if a run for this device is in progress right now, or one
  # was killed before its finally block could shred it.
  if ($cpChkOut -match 'KBLEFT:scratch-token') {
    Write-Host "  found a leftover plaintext token ${cpScratch}/${DeviceName}.token on the control plane" -ForegroundColor Yellow
    Write-Host '  (another deploy of this device may be running right now, or an earlier one was killed).' -ForegroundColor Yellow
    if (-not (Confirm-Override 'No other deploy of this device is running: shred it and continue?')) {
      throw "NOTHING CHANGED: leftover scratch token ${cpScratch}/${DeviceName}.token kept."
    }
    Invoke-SshText $ControlPlane "shred -u ${cpScratch}/${DeviceName}.token 2>/dev/null || rm -f ${cpScratch}/${DeviceName}.token" | Out-Null
  }

  # --- 1e. target platform, tools, leftovers (read-only) -----------------------
  Step "checking target $TargetSsh (os/arch, tools, earlier installs)"
  if ($isWin) {
    # the home dir is needed up front: leftovers live in <home>\kuma-deploy
    $homeOut = Invoke-SshText $TargetSsh (Get-WinCmd 'echo %USERPROFILE%') -AllowFail
    $homeWin = Get-LastLine $homeOut
    if ((-not $homeWin) -or ($homeWin -match '%USERPROFILE%') -or ($homeWin -notmatch '^[A-Za-z]:\\')) {
      throw ("could not resolve %USERPROFILE% on $TargetSsh (got '$homeWin'). Is it really a Windows host, with OpenSSH " +
        "DefaultShell cmd.exe or powershell.exe? (bash/other shells are not supported for windows targets)`n$homeOut")
    }
    # unquoted cmd.exe paths below (PS 5.1 cannot pass embedded quotes)
    if ($homeWin -notmatch '^[A-Za-z]:\\[A-Za-z0-9_.\\-]+$') { throw "%USERPROFILE% '$homeWin' on $TargetSsh has spaces/special characters; not supported" }
    # scp/sftp side: forward slashes (C:/Users/x/kuma-deploy) are accepted by
    # Win32-OpenSSH in both legacy scp and sftp mode
    $scpStage = ($homeWin -replace '\\', '/') + '/kuma-deploy'
    $tArchRaw = Get-LastLine (Invoke-SshText $TargetSsh (Get-WinCmd 'echo %PROCESSOR_ARCHITECTURE%') -AllowFail)
    $archMap = @{ AMD64 = 'amd64'; ARM64 = 'arm64' }
    $tArch = $archMap[$tArchRaw]
    if (-not $tArch) { throw "target $TargetSsh reported PROCESSOR_ARCHITECTURE '$tArchRaw'; expected AMD64 or ARM64" }
    $tOsDesc = "Windows $tArchRaw"

    $svcOut = Invoke-SshText $TargetSsh (Get-WinCmd 'sc query kuma-agent') -AllowFail
    if ($svcOut -match 'STATE\s*:\s*\d+\s+(\w+)') {
      Write-Host "  target already has a kuma-agent service ($($Matches[1])): the elevated step stops it, replaces its files and restarts it." -ForegroundColor Yellow
    }
    $dirOut = Invoke-SshText $TargetSsh (Get-WinCmd "dir /b $homeWin\kuma-deploy") -AllowFail
    $known = @('kuma-agent.exe', 'ca.pem', 'config.yaml', 'setup.ps1', 'sleep.ps1', 'token')
    $leftovers = @($dirOut -split "`n" | ForEach-Object { $_.Trim() } | Where-Object { $_ -in $known })
    # setup.ps1 MOVES the token away, so a staged token means that deploy's
    # elevated step never ran: ask. Without one the files are just copies
    # from a finished deploy and are replaced.
    if ($leftovers -contains 'token') {
      Write-Host "  $homeWin\kuma-deploy holds a staged token from an earlier deploy whose elevated setup.ps1 step never ran." -ForegroundColor Yellow
      if (-not (Confirm-Override "Discard that deploy and stage fresh files in $homeWin\kuma-deploy?")) {
        throw "NOTHING CHANGED: earlier staged files kept in $homeWin\kuma-deploy (run that deploy's elevated step, or rerun and answer y)."
      }
    } elseif ($leftovers.Count -gt 0) {
      Write-Host "  replacing files left from an earlier, completed deploy in $homeWin\kuma-deploy: $($leftovers -join ', ')"
    }
    # emptied in step 5a, so a later 'no' still leaves the target untouched
  } else {
    # single-quoted: $c / $(...) are for the remote shell, not PowerShell
    $unixChk = 'uname -s; uname -m; for c in sudo base64 install tr cmp; do command -v $c >/dev/null 2>&1 || echo KBMISS:$c; done; ' +
      'command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 || echo KBMISS:sha256sum; ' +
      'if [ $(uname -s) = Darwin ]; then command -v launchctl >/dev/null 2>&1 || echo KBMISS:launchctl; ' +
      'else command -v systemctl >/dev/null 2>&1 || echo KBMISS:systemctl; fi; ' +
      '[ -e /tmp/kbagent/token ] && echo KBLEFT:token; [ -e /tmp/kbagent ] && [ ! -O /tmp/kbagent ] && echo KBLEFT:foreign-owner; echo KBCHK:done'
    $unixOut = Invoke-SshText $TargetSsh $unixChk -AllowFail
    $ul = @($unixOut -split "`n" | ForEach-Object { $_.Trim() } | Where-Object { $_ })
    if ($unixOut -notmatch 'KBCHK:done' -or $ul.Count -lt 3) {
      throw "target $TargetSsh did not answer the Linux/macOS checks (a Windows host? then use -BinaryType windows_*):`n$unixOut"
    }
    $osMap = @{ Linux = 'linux'; Darwin = 'darwin' }
    $archMap = @{ x86_64 = 'amd64'; amd64 = 'amd64'; aarch64 = 'arm64'; arm64 = 'arm64' }
    $tOs = $osMap[$ul[0]]
    $tArch = $archMap[$ul[1]]
    $tOsDesc = "$($ul[0]) $($ul[1])"
    if ($tOs -ne $osPart) { throw "target $TargetSsh is $tOsDesc but -BinaryType is $BinaryType; pick ${tOs}_$tArch" }
    $tMissing = @([regex]::Matches($unixOut, 'KBMISS:(\S+)') | ForEach-Object { $_.Groups[1].Value })
    if ($tMissing.Count -gt 0) { throw "target $TargetSsh is missing required tools: $($tMissing -join ', ')" }
    if ($unixOut -match 'KBLEFT:foreign-owner') {
      throw "/tmp/kbagent on $TargetSsh belongs to another user; remove it there (sudo rm -rf /tmp/kbagent) and rerun"
    }
    if ($unixOut -match 'KBLEFT:token') {
      Write-Host "  /tmp/kbagent on $TargetSsh still holds a token from an earlier, unfinished deploy." -ForegroundColor Yellow
      if (-not (Confirm-Override 'Delete /tmp/kbagent and stage fresh files?')) {
        throw 'NOTHING CHANGED: leftover /tmp/kbagent kept on the target.'
      }
    }
  }
  if (-not $tArch) { throw "target $TargetSsh reported an unsupported architecture ($tOsDesc)" }
  if ($tArch -ne $archPart) {
    # only amd64-on-arm64 macOS (Rosetta 2) / Windows (x64 emulation) can run at all
    if (-not (($archPart -eq 'amd64') -and ($tArch -eq 'arm64') -and ($osPart -ne 'linux'))) {
      throw "target $TargetSsh is $tOsDesc; a $BinaryType binary cannot run there. Use -BinaryType ${osPart}_$tArch"
    }
    Write-Host "  target is $tOsDesc but -BinaryType is ${BinaryType}: this only runs under emulation (Rosetta 2 / Windows on ARM)." -ForegroundColor Yellow
    if (-not (Confirm-Override "Deploy $BinaryType to this $tArch target anyway?")) {
      throw "NOTHING CHANGED: architecture mismatch; use -BinaryType ${osPart}_$tArch"
    }
  }
  Write-Host "  target: $tOsDesc"
}

# --- 2. version resolve + artifact existence on control plane ------------------
New-Item -ItemType Directory -Force -Path $StagingDir | Out-Null

$cpBin = ''
$verList = ''
if ($FromBin) {
  # legacy path: build from HEAD on the control plane; go build -o bin/...
  # lands in repo bin/, NOT data-local/bin/
  $cpBin = "${cpRepo}/bin/kuma-agent.$BinaryType"
  Step 'building from repo HEAD on control plane (-FromBin; slow first time: npm ci + go build)'
  $buildCmd = "set -e; cd $cpRepo; export PATH=`$HOME/sdk/go/bin:`$HOME/sdk/node-v22.20.0-linux-x64/bin:`$PATH; if [ ! -d web/node_modules ]; then cd web; npm ci; npm run build; cd ..; fi; CGO_ENABLED=0 GOOS=$osPart GOARCH=$archPart go build -o bin/kuma-agent.$BinaryType ./cmd/kuma-agent"
  Invoke-SshText $ControlPlane $buildCmd | Out-Null
} else {
  Step "resolving release version on control plane (${cpData}/releases)"
  $listCmd = "cd ${cpData}/releases 2>/dev/null || exit 0; ls -1 | grep -E '^[0-9]+(\.[0-9]+)+' | sort -V | tail -1"
  $verList = Invoke-SshText $ControlPlane $listCmd -AllowFail
  $Version = $Version.Trim()
  if ($Version -eq 'latest') {
    $Version = (@($verList -split "`n" | ForEach-Object { $_.Trim() } | Where-Object { $_ -match '^[0-9]+(\.[0-9]+)+' }) | Select-Object -Last 1)
    if (-not $Version) { throw "no release versions under ${cpData}/releases on $ControlPlane - run 'kumaboard release ingest' there or pass -Version / -FromBin`n$verList" }
  }
  Step "release version: $Version"
  $cpBin = "${cpData}/releases/${Version}/${BinaryType}/$binName"
  $artOut = Invoke-SshText $ControlPlane "test -s $cpBin && echo KBART:ok || ls -1 ${cpData}/releases/${Version} 2>&1" -AllowFail
  if ($artOut -notmatch 'KBART:ok') {
    throw "release artifact $cpBin is missing or empty on $ControlPlane. Contents of release ${Version}:`n$artOut"
  }
}

# --- 3. registration check (read-only; registering is deferred to step 5c) -----
Step "checking registration of '$DeviceName' on control plane"
$regOut = Invoke-SshText $ControlPlane $regChk
$isRegistered = ($regOut -match 'KBREG:yes')
if (-not ($regOut -match 'KBREG:(yes|no)')) { throw "registration check returned no KBREG marker:`n$regOut" }
$targetMac = ''
if ($isRegistered) {
  if ($Mac) {
    Write-Host "  NOTE: '$DeviceName' is already registered; -Mac is only applied at registration. Change the MAC in the dashboard." -ForegroundColor Yellow
  }
  if (-not $StageOnly) {
    $st0 = Get-DeviceStatus
    if ($st0) {
      Write-Host "  '$DeviceName' is already registered (agent_version '$($st0.Ver)', last_seen $($st0.Seen), connected now: $(if ($st0.Fresh -eq '1') { 'yes' } else { 'no' }))." -ForegroundColor Yellow
    }
    Write-Host '  Re-keying mints a new token; its running agent (if any) is disconnected until the new token is installed' -ForegroundColor Yellow
    if ($isWin) { Write-Host '  (on windows: until you run the elevated setup.ps1 step on the target).' -ForegroundColor Yellow }
    if (-not (Confirm-Override "Re-key '$DeviceName' and redeploy it?" ([bool]$RotateToken))) {
      throw ("NOTHING CHANGED: '$DeviceName' is already registered and was not re-keyed. Check it with -VerifyOnly or the dashboard; " +
        'pass -RotateToken to pre-answer this prompt.')
    }
  }
} elseif ($StageOnly) {
  Write-Host "  NOT registered (StageOnly: nothing created - a real run auto-registers)" -ForegroundColor Yellow
} else {
  foreach ($need in 'bin/kumaboard', 'data-local/config.yaml') {
    if ($cpMissing -contains $need) { throw "cannot register '$DeviceName': ${cpRepo}/$need is missing on $ControlPlane" }
  }
  if ($Mac) {
    $targetMac = $Mac
  } elseif (-not $isWin) {
    Step "device not registered - reading real MAC from $TargetSsh (en0, fallback ip link)"
    $macOut = Invoke-SshText $TargetSsh $MAC_GET_BSH -AllowFail
    $macLines = @($macOut -split "`n" | ForEach-Object { $_.Trim() } | Where-Object { $_ -match '^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$' })
    if ($macLines.Count -gt 0) { $targetMac = $macLines[0] }
  }
  Step "device not registered - will register '$DeviceName' (mac: $(if ($targetMac) { $targetMac } else { 'unset, WoL off' })) after token delivery is verified"
}

# --- 4. artifact + config staging (release tree, caps, flatten fix: kept) ------
Step "downloading binary $BinaryType + ca.pem"
Invoke-Scp "$ControlPlane`:$cpBin" (Join-Path $StagingDir $binName)
if (-not (Test-Path (Join-Path $StagingDir $binName))) { throw "downloaded binary missing: $cpBin -> $StagingDir" }
# byte-exact copy: staged sha256 must equal the control plane's
$cpBinHash = ((Get-LastLine (Invoke-SshText $ControlPlane "sha256sum $cpBin")) -split '\s+')[0].ToLower()
$binHash = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $StagingDir $binName)).Hash.ToLower()
if ($binHash -ne $cpBinHash) { throw "staged $binName sha256 $binHash != control plane $cpBinHash ($cpBin); download corrupted, rerun" }
# a Windows exe starts with 'MZ', Linux ELF with 0x7F 'ELF', Mach-O with CF FA ED FE
$head = New-Object byte[] 4
$fs = [IO.File]::OpenRead((Resolve-Path -LiteralPath (Join-Path $StagingDir $binName)).Path)
try { [void]$fs.Read($head, 0, 4) } finally { $fs.Close() }
$magicOk = $false
if ($isWin) { $magicOk = ($head[0] -eq 0x4D -and $head[1] -eq 0x5A) }
elseif ($isMac) { $magicOk = ($head[0] -eq 0xCF -and $head[1] -eq 0xFA -and $head[2] -eq 0xED -and $head[3] -eq 0xFE) }
else { $magicOk = ($head[0] -eq 0x7F -and $head[1] -eq 0x45 -and $head[2] -eq 0x4C -and $head[3] -eq 0x46) }
if (-not $magicOk) { throw "$cpBin is not a $osPart executable (file header $(($head | ForEach-Object { $_.ToString('X2') }) -join ' ')); wrong artifact in the release tree?" }
Step "binary ok: sha256 $binHash"
Invoke-Scp "$ControlPlane`:${cpData}/pki/ca.pem" (Join-Path $StagingDir 'ca.pem')
if ((Get-Content -LiteralPath (Join-Path $StagingDir 'ca.pem') -Raw) -notmatch '-----BEGIN CERTIFICATE-----') {
  throw "${cpData}/pki/ca.pem on $ControlPlane is not a PEM certificate"
}

# capability caps: darwin/windows ship metrics + custom-commands + gpu;
# linux ships metrics + custom-commands, gpu only with -IncludeGpu.
$caps = @('metrics', 'custom-commands')
if ($isWin -or $isMac) {
  $caps += 'gpu'
} elseif ($IncludeGpu) {
  $caps += 'gpu'
}
# an explicit -Mac means the operator wants Wake-on-LAN for this device; the
# dashboard only offers Wake for devices declaring wol-target
if ($Mac) { $caps += 'wol-target' }
Step "writing config.yaml (device=$DeviceName, caps=$($caps -join ','))"
if ($isWin) {
  $paths = @{ ca = 'C:\ProgramData\kuma-agent\ca.pem'; token = 'C:\ProgramData\kuma-agent\token' }
} else {
  $paths = @{ ca = '/etc/kuma-agent/ca.pem'; token = '/etc/kuma-agent/token' }
}
$cfgLines = @(
  'device:',
  "  name: $DeviceName",
  '',
  'server:',
  "  url: $ServerUrl",
  "  ca_file: $($paths.ca)",
  '',
  "token_file: $($paths.token)",
  '',
  'capabilities:'
)
foreach ($c in $caps) { $cfgLines += "  - $c" }
$cfg = $cfgLines -join "`r`n"
if ($ConfigFile) {
  Step "using -ConfigFile $ConfigFile verbatim"
  $cfg = (Get-Content -LiteralPath $ConfigFile) -join "`r`n"
}
Set-Content -Path (Join-Path $StagingDir 'config.yaml') -Value $cfg -Encoding ascii
# The generated config has no commands (and no wol-target without -Mac): the
# dashboard then offers no sleep / custom commands and no Wake-on-LAN for
# this device. Say so instead of silently shipping a reduced config.
$tmpl = 'configs/kuma-agent.windows.example.yaml:14-21'
if ($cfg -notmatch '(?m)^commands:') {
  Write-Host "WARN: config.yaml has no 'commands:' block (no sleep / custom commands in the dashboard)." -ForegroundColor Yellow
  Write-Host "      template to merge: $tmpl ; deploy it with -ConfigFile <merged.yaml>" -ForegroundColor Yellow
}
if ($cfg -notmatch '(?m)^\s*-\s*wol-target\s*$') {
  Write-Host "WARN: config.yaml lacks the 'wol-target' capability (device will not be offered Wake-on-LAN; pass -Mac to add it)." -ForegroundColor Yellow
  Write-Host "      template to merge: $tmpl" -ForegroundColor Yellow
}

# launchd / systemd / windows kits: local copies when present, else auto-pull
# from the control plane repo. Flatten fix kept: scp of a directory into an
# existing directory nests one level - move the nested contents up.
$kitName = 'systemd'
if ($isWin) { $kitName = 'windows' }
elseif ($isMac) { $kitName = 'launchd' }
$kit = Join-Path $LocalKitDir $kitName
if ($isWin) {
  # the windows kit is flat: setup.ps1 + sleep.ps1 live in LocalKitDir itself
  foreach ($f in 'setup.ps1', 'sleep.ps1') {
    $p = Join-Path $LocalKitDir $f
    # repo checkout: the kit sits next to this script (deploy\windows), so a
    # run from any cwd uses the local copy, not the control plane's checkout
    if (-not (Test-Path $p) -and $PSScriptRoot) { $p = Join-Path $PSScriptRoot $f }
    if (-not (Test-Path $p)) {
      Step "kit file $f missing locally - pulling from control plane"
      Invoke-Scp "$ControlPlane`:${cpRepo}/deploy/windows/$f" $StagingDir
    } else {
      Copy-Item $p $StagingDir -Force
    }
    if (-not (Test-Path (Join-Path $StagingDir $f))) { throw "kit file $f not staged" }
  }
} else {
  # repo checkout: deploy\<kit> is a sibling of this script's deploy\windows
  if (-not (Test-Path (Join-Path $kit 'setup-agent.sh')) -and $PSScriptRoot) {
    $repoKit = Join-Path (Split-Path $PSScriptRoot -Parent) $kitName
    if (Test-Path (Join-Path $repoKit 'setup-agent.sh')) { $kit = $repoKit }
  }
  if (-not (Test-Path (Join-Path $kit 'setup-agent.sh'))) {
    Step "kit '$kitName' missing locally - pulling from control plane"
    Invoke-Scp "$ControlPlane`:${cpRepo}/deploy/$kitName" $LocalKitDir -Recurse
    $nested = Join-Path $kit $kitName
    if (-not (Test-Path (Join-Path $kit 'setup-agent.sh')) -and (Test-Path $nested)) {
      Get-ChildItem $nested | Move-Item -Destination $kit -Force
      Remove-Item $nested -Force -Recurse
    }
  }
  if (-not (Test-Path (Join-Path $kit 'setup-agent.sh'))) { throw "kit '$kitName' incomplete: setup-agent.sh missing in $kit" }
}

# StageOnly stops here: no token minted, target never contacted after
# preflight, no password prompt.
if ($StageOnly) {
  Step "staged in $StagingDir (binary, ca.pem, config.yaml, kit)."
  Write-Host 'Later: run the same command without -StageOnly for the install (it mints the token then).'
  return
}

# --- 4b. re-run safety: never clobber a different agent config silently -------
# macOS/Linux: /etc/kuma-agent/config.yaml is world-readable, so compare it
# now, BEFORE any token work. Windows: C:\ProgramData\kuma-agent is not
# readable without elevation; setup.ps1 -StageDir does the same check
# (backup + diff + refuse) as its very first step.
function Write-ConfigDiff([string[]]$Old, [string[]]$New) {
  Write-Host '--- diff (- installed, + new) ---'
  if ($Old.Count -eq 0 -or $New.Count -eq 0) {
    foreach ($l in $Old) { Write-Host "- $l" }
    foreach ($l in $New) { Write-Host "+ $l" }
    return
  }
  foreach ($d in (Compare-Object -ReferenceObject $Old -DifferenceObject $New)) {
    if ($d.SideIndicator -eq '<=') { Write-Host "- $($d.InputObject)" } else { Write-Host "+ $($d.InputObject)" }
  }
}
if (-not $isWin) {
  $probe = Invoke-SshText $TargetSsh 'if [ -f /etc/kuma-agent/config.yaml ]; then echo KBCFG:present; cat /etc/kuma-agent/config.yaml; else echo KBCFG:absent; fi' -AllowFail
  if ($probe -match 'KBCFG:present') {
    $cur = @((($probe -split 'KBCFG:present', 2)[1]) -split "`n" | ForEach-Object { $_.TrimEnd() } | Where-Object { $_ -ne '' })
    $new = @(($cfg -split "`n") | ForEach-Object { $_.TrimEnd() } | Where-Object { $_ -ne '' })
    if (($cur -join "`n") -ne ($new -join "`n")) {
      Write-Host "existing /etc/kuma-agent/config.yaml on $TargetSsh differs from the new one." -ForegroundColor Yellow
      Write-ConfigDiff $cur $new
      if (-not (Confirm-Override 'Replace the installed config.yaml (a timestamped backup is kept on the target)?' ([bool]$OverwriteConfig))) {
        throw ('NOTHING CHANGED: installed agent config differs (diff above). Merge local settings into a file and pass ' +
          '-ConfigFile <file>, or pass -OverwriteConfig to pre-answer this prompt.')
      }
      Write-Host '  install keeps a backup as config.yaml.bak-<timestamp>' -ForegroundColor Yellow
    }
  }
}

# --- 5. push, token (mint -> deliver -> verify -> commit), install --------------
# Everything that can leave plaintext behind runs inside try; the finally
# block below is the single cleanup point for all exit paths (throw,
# Ctrl+C, return).
$scratchFile   = "${cpScratch}/${DeviceName}.token"
$minted        = $false
$tokenOnTarget = $false
$tokenHandedOff = $false
$passVer       = $false
$lastVer       = ''
$lastReject    = ''
$lastSeenRaw   = ''
$fresh         = '0'
if ($isWin) {
  $targetToken = ''
} else {
  $targetToken = '/tmp/kbagent/token'
}
try {
  # 5a. push artifacts (no secrets)
  if ($isWin) {
    # Windows: NOT zero-touch. Reason printed below for the operator: the
    # kuma-agent service install goes through the SCM and requires LOCAL
    # elevation; the plain OpenSSH session has none. psexec/WinRM are out of
    # scope and not provisioned, so they are NOT attempted here.
    # $homeWin / $scpStage were resolved (and validated) in step 1e
    $targetToken = "$homeWin\kuma-deploy\token"
    Step "pushing to $TargetSsh ($homeWin\kuma-deploy)"
    # start from an empty dir (leftovers were confirmed in step 1e); cmd.exe
    # 'if exist' / 'if not exist' instead of swallowing rmdir/mkdir errors
    Invoke-SshText $TargetSsh (Get-WinCmd "if exist $homeWin\kuma-deploy rmdir /s /q $homeWin\kuma-deploy") | Out-Null
    Invoke-SshText $TargetSsh (Get-WinCmd "if not exist $homeWin\kuma-deploy mkdir $homeWin\kuma-deploy") | Out-Null
    foreach ($f in 'kuma-agent.exe', 'ca.pem', 'config.yaml', 'setup.ps1', 'sleep.ps1') {
      Invoke-Scp (Join-Path $StagingDir $f) "${TargetSsh}:${scpStage}/"
    }
  } else {
    Step "pushing artifacts + $kitName kit to ${TargetSsh}:/tmp/kbagent"
    Invoke-SshText $TargetSsh 'rm -rf /tmp/kbagent; mkdir -p /tmp/kbagent' | Out-Null
    Invoke-Scp (Join-Path $StagingDir $binName) "${TargetSsh}:/tmp/kbagent/$binName"
    Invoke-Scp (Join-Path $StagingDir 'ca.pem') "${TargetSsh}:/tmp/kbagent/"
    Invoke-Scp (Join-Path $StagingDir 'config.yaml') "${TargetSsh}:/tmp/kbagent/"
    Invoke-Scp (Join-Path $kit 'setup-agent.sh') "${TargetSsh}:/tmp/kbagent/setup-agent.sh"
    if ($isMac) {
      Invoke-Scp (Join-Path $kit 'com.kumaboard.agent.plist') "${TargetSsh}:/tmp/kbagent/com.kumaboard.agent.plist"
    } else {
      Invoke-Scp (Join-Path $kit 'kuma-agent.service') "${TargetSsh}:/tmp/kbagent/kuma-agent.service"
    }
  }
  # the pushed binary must be byte-identical to the verified staged one
  $binOnTarget = "/tmp/kbagent/$binName"
  if ($isWin) { $binOnTarget = "$homeWin\kuma-deploy\$binName" }
  $pushedHash = Get-TargetSha256 $binOnTarget
  if ($pushedHash -ne $binHash) { throw "pushed $binOnTarget on $TargetSsh has sha256 '$pushedHash', expected $binHash; nothing minted yet, rerun" }
  Step 'artifacts pushed; binary sha256 verified on target'

  # 5b. mint (scratch file only; live token_hash untouched)
  Step "minting candidate token on control plane (scratch only; DB not changed yet)"
  $minted = $true
  $mintOut = Invoke-SshText $ControlPlane (Get-CpPyCmd $MINT_PY "$cpScratch $DeviceName")
  $mintHash = ''
  if ($mintOut -match 'MINTED sha256=([0-9a-f]{64})') { $mintHash = $Matches[1] }
  if (-not $mintHash) { throw "token mint failed on control plane:`n$mintOut" }

  # deliver: scp -3 streams control plane -> target through this process
  # (no local file); source mode 0600 is kept for the new target file.
  Step "delivering token: scp -3 control plane -> $TargetSsh"
  $tokenOnTarget = $true
  if ($isWin) {
    Invoke-Scp "${ControlPlane}:$scratchFile" "${TargetSsh}:${scpStage}/token" -ThreeWay
  } else {
    Invoke-Scp "${ControlPlane}:$scratchFile" "${TargetSsh}:$targetToken" -ThreeWay
  }

  # verify ON TARGET: byte-exact sha256 match (implies non-empty; catches
  # truncation and any CR/LF mangling)
  if (-not $isWin) { Invoke-SshText $TargetSsh "chmod 600 $targetToken" | Out-Null }
  $gotHash = Get-TargetSha256 $targetToken
  if ($gotHash -ne $mintHash) {
    throw "token delivery NOT verified on $TargetSsh (want sha256 $mintHash, got '$gotHash'). DB unchanged; aborting before rotation."
  }
  Step 'token delivered and verified on target (sha256 match)'

  # 5c. commit: register if new, then make the verified token live
  if (-not $isRegistered) {
    $addCmd = "cd $cpRepo || exit 97; bin/kumaboard device add -config data-local/config.yaml"
    if ($targetMac) { $addCmd += " -mac $targetMac" }
    $addCmd += " $DeviceName"
    Step "registering '$DeviceName' on control plane"
    # 'device add' prints its own one-time token; it is superseded by the
    # commit right below and never used.
    $addOut = Invoke-SshText $ControlPlane $addCmd
    if ($addOut -notmatch 'registered') { throw "unexpected 'device add' output:`n$addOut" }
  }
  Step "committing verified token for '$DeviceName' (server checks sha256 of bearer token)"
  $comOut = Invoke-SshText $ControlPlane (Get-CpPyCmd $COMMIT_PY "data-local/kumaboard.db $DeviceName $cpScratch $mintHash")
  if ($comOut -notmatch 'COMMITTED rows=1') { throw "token commit failed for '$DeviceName':`n$comOut" }

  # 5d. install
  if ($isWin) {
    Write-Host ''
    Write-Host 'NOT ZERO-TOUCH (windows): the following needs an ELEVATED prompt.' -ForegroundColor Yellow
    Write-Host '  reason: kuma-agent service install uses the SCM (sc create/start),'
    Write-Host '  which requires local elevation; plain OpenSSH grants none, and'
    Write-Host '  psexec/WinRM are out of scope (not provisioned).'
    # Everything elevated is done by setup.ps1 -StageDir, in a safe order
    # with every step checked; the operator only pastes one block. Two
    # blocks, each in the syntax of its label (never paste cmd.exe lines
    # into PowerShell: there 'sc' is Set-Content, not sc.exe).
    $stage = "$homeWin\kuma-deploy"
    $setupCmd = "powershell.exe -NoProfile -ExecutionPolicy Bypass -File $stage\setup.ps1 -StageDir $stage"
    if ($OverwriteConfig) { $setupCmd += ' -OverwriteConfig' }
    Write-Host ''
    Write-Host 'setup.ps1 -StageDir does, in order (aborting on the first failure):'
    Write-Host '  1 validate the staged files; existing install or different config.yaml? -> show it + diff and'
    Write-Host '    ask y/N (overwrite keeps a timestamped backup; answering no changes nothing)'
    Write-Host '  2 kuma-agent account, logon rights, dirs + ACLs (idempotent)'
    Write-Host '  3 STOP the kuma-agent service, wait for the exe to exit'
    Write-Host '  4 THEN copy kuma-agent.exe, ca.pem, config.yaml, sleep.ps1; MOVE the token into place + lock its ACL'
    Write-Host '  5 register the service if missing, start it, print agent.log'
    Write-Host ''
    Write-Host 'Use ONE of these two blocks on the target, in an ELEVATED window of that kind:' -ForegroundColor Yellow
    Write-Host ''
    Write-Host '[PowerShell (admin)]' -ForegroundColor Cyan
    Write-Host $setupCmd
    Write-Host 'Get-Service -Name kuma-agent'
    Write-Host 'Get-Content -LiteralPath C:\ProgramData\kuma-agent\agent.log -Tail 20'
    Write-Host ''
    Write-Host '[cmd.exe (admin)]' -ForegroundColor Cyan
    Write-Host $setupCmd
    Write-Host 'sc.exe query kuma-agent'
    Write-Host 'type C:\ProgramData\kuma-agent\agent.log'
    Write-Host ''
    # the verified token in kuma-deploy is now the operator's input to the
    # elevated step: do NOT delete it in finally
    $tokenHandedOff = $true
  } else {
    # THE one prompt: the target's sudo password. SecureString -> plaintext
    # in process memory only; NEVER interpolated into any command string.
    # It flows via stdin through tr -d CR (kills CRLF injected by Windows
    # pipes) into 'sudo -S', which consumes the first stdin line as the
    # password, then runs the decoded install script. No terminal paste.
    $pass = Read-Host "sudo password for $TargetSsh (one prompt; stdin only)" -AsSecureString
    $bstr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($pass)
    $passPlain = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($bstr)
    [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr)
    if (-not $passPlain) { throw 'empty sudo password' }

    $installSh = $INSTALL_LINUX_BSH
    if ($isMac) { $installSh = $INSTALL_MAC_BSH }
    Invoke-SshText $TargetSsh "echo $(ConvertTo-B64 $installSh) | base64 --decode > /tmp/kbagent/install.sh" | Out-Null
    $remoteCmd = 'tr -d ''\r'' | sudo -S -p '''' bash /tmp/kbagent/install.sh'
    Step "remote install on $TargetSsh (sudo -S over stdin; INSTALL-OK required)"
    $installRes = Invoke-Native 'ssh' @('-o', 'BatchMode=yes', '-o', 'ConnectTimeout=10', $TargetSsh, $remoteCmd) -StdIn $passPlain
    $installOut = $installRes.Output
    $installRc = $installRes.ExitCode
    $passPlain = $null
    $pass = $null
    if (($installRc -ne 0) -or ($installOut -notmatch 'INSTALL-OK')) {
      $tail = ($installOut -split "`n" | Select-Object -Last 20) -join "`n"
      Write-Host '--- remote combined output (last 20 lines) ---' -ForegroundColor Yellow
      Write-Host $tail
      throw "remote install FAILED on $TargetSsh (exit $installRc, INSTALL-OK marker $(if ($installOut -match 'INSTALL-OK') {'present'} else {'missing'}))"
    }
    Step 'remote install reported INSTALL-OK'
  }

  # --- 6. verification loop --------------------------------------------------------
  # Poll the devices row on the control plane (ssh + python3, read-only URI),
  # up to 90 s. PASS = agent_version equals -Version (skipped for -FromBin
  # dev builds) AND last_reject_reason empty AND last_seen within 90 s.
  # The commit nulled last_seen, so PASS requires a NEW check-in with the NEW
  # token. Agent reconnect backoff starts at 1 s: healthy installs land in
  # seconds; 90 s is generous.
  if ($isWin) {
    Step 'verification: MANUAL for windows targets until the elevated checklist has run'
    Write-Host "  after the checklist '$DeviceName' should check in as v$Version (dashboard, or -VerifyOnly)."
  } else {
    Step "verifying '$DeviceName' on control plane (up to 90 s)"
    $deadline = (Get-Date).AddSeconds(90)
    while ((Get-Date) -lt $deadline) {
      $st = Get-DeviceStatus
      if ($st) {
        $lastVer = $st.Ver
        $lastReject = $st.Reject
        $fresh = $st.Fresh
        $lastSeenRaw = $st.Seen
        $verOk = $true
        if (-not $FromBin -and ($lastVer -ne $Version)) { $verOk = $false }
        if ($lastReject -ne '') { $verOk = $false }
        if ($fresh -ne '1') { $verOk = $false }
        if ($verOk) { $passVer = $true; break }
      }
      Start-Sleep -Seconds 5
    }
  }
} finally {
  # Single cleanup point for plaintext tokens, on every exit path. Each step
  # only warns, so a cleanup failure never masks the original error.
  $passPlain = $null
  $pass = $null
  if ($minted) {
    $r = Invoke-Native 'ssh' @('-o', 'BatchMode=yes', '-o', 'ConnectTimeout=10', $ControlPlane, "shred -u $scratchFile 2>/dev/null || rm -f $scratchFile")
    if ($r.ExitCode -ne 0) { Write-Warning "could not remove control-plane scratch token $scratchFile (exit $($r.ExitCode)); delete it by hand" }
  }
  if ($tokenOnTarget -and -not $tokenHandedOff) {
    if ($isWin) { $rmCmd = Get-WinCmd "if exist $targetToken del /f /q $targetToken" } else { $rmCmd = "rm -f $targetToken" }
    $r = Invoke-Native 'ssh' @('-o', 'BatchMode=yes', '-o', 'ConnectTimeout=10', $TargetSsh, $rmCmd)
    if ($r.ExitCode -ne 0) { Write-Warning "could not remove plaintext token $targetToken on $TargetSsh (exit $($r.ExitCode)); delete it by hand" }
  }
}

Write-Host ''
Write-Host 'VERIFICATION TABLE'
$verWant = $Version
if ($FromBin) { $verWant = "$Version (version equality skipped: -FromBin dev build)" }
if ($isWin) { $verWant = "$Version (pending elevated checklist)" }
Write-Host "  device               $DeviceName"
Write-Host "  agent_version      $lastVer          (want $verWant)"
Write-Host "  last_reject_reason $lastReject          (want empty)"
Write-Host "  last_seen          $lastSeenRaw   (fresh=$fresh, want 1 = within 90 s)"

if ($passVer) {
  Write-Host ''
  Write-Host "RESULT: PASS - $DeviceName v$Version installed and connected." -ForegroundColor Green
  Write-Host '  zero-touch complete: nothing left to do on the target.'
} elseif ($isWin) {
  Write-Host ''
  Write-Host "RESULT: MANUAL - run the elevated checklist above. Do NOT rerun this deploy to verify." -ForegroundColor Yellow
  Write-Host "  verified token waiting on target: $targetToken (setup.ps1 -StageDir moves it into place)"
  Write-NextSteps
} else {
  Write-Host ''
  Write-Host "RESULT: FAIL - $DeviceName did not check in as v$Version within 90 s" -ForegroundColor Red
  Write-Host "  reject reason: '$lastReject' (see table above)"
  # self-explanatory failure: kumaboard runs as a systemd USER unit under
  # the ssh user on the control plane; pull its journal filtered by device name.
  $logCmd = "journalctl --user -u kumaboard.service -n 200 --no-pager 2>/dev/null | grep -- $DeviceName | tail -20 || true"
  $ctrlLog = Invoke-SshText $ControlPlane $logCmd -AllowFail
  Write-Host '--- control plane kumaboard.service log (last 20 lines filtered by device) ---'
  Write-Host $ctrlLog
  Write-NextSteps
}

# --- 7. housekeeping ----------------------------------------------------------------
Write-Host ''
if ($passVer) {
  Write-Host "done. $DeviceName onboarded. (sudo secret cleared; plaintext token copies removed)"
} else {
  if ($isWin) {
    Write-Host 'done: elevated step pending (secrets cleared from this process).'
    exit 0
  }
  Write-Host 'done WITHOUT PASS. fix per reject reason / logs above, then check with -VerifyOnly. (secrets cleared)'
  exit 1
}
