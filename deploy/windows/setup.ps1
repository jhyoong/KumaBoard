#Requires -Version 5.1
#Requires -RunAsAdministrator
<#
.SYNOPSIS
  Elevated host preparation for kuma-agent on Windows. Idempotent: safe to
  run again on an already-prepared box (it never touches the token, the
  config or an installed service unless asked to).
  1. creates the local standard user 'kuma-agent' if missing (random
     password nobody knows; the service logon password is reset to a fresh
     random value by -InstallService, so it never has to be typed)
  2. grants SeServiceLogonRight and denies interactive + Remote Desktop logon
     for that account (secedit, verified by re-export)
  3. creates C:\ProgramData\kuma-agent (+ bin) and C:\scripts with ACLs, and
     pre-creates agent.log writable by the service account
  With -InstallService (after kuma-agent.exe was copied into bin): registers
  the kuma-agent service under that account.
  With -StageDir <dir> (what deploy-agent.ps1 prints): full install from a
  staging dir holding kuma-agent.exe, config.yaml, ca.pem, sleep.ps1 and
  token, in this order:
    a. checks FIRST, before anything changes: the staged files are
       validated; an existing, different config.yaml is diffed and a y/N
       prompt asks to overwrite it (backup config.yaml.bak-<timestamp>) or
       keep it (-OverwriteConfig / -KeepConfig pre-answer); an existing
       install (service or exe) is shown and a y/N prompt asks before it is
       replaced (-Yes pre-answers every prompt). Answering no changes nothing.
    b. stop the kuma-agent service and wait for the process to exit
    c. then replace kuma-agent.exe (never copied over a running exe)
    d. ca.pem, config.yaml, sleep.ps1; token MOVED (not copied) into place
       and locked to Administrators/SYSTEM full + kuma-agent read
    e. register the service if missing, start it, show agent.log
  Every native command is checked; any failure aborts with the failing
  command line and exit code.
#>
[CmdletBinding()]
param(
  # register the kuma-agent service (bin\kuma-agent.exe must already exist)
  [switch]$InstallService,
  # full install from this staging dir (see .SYNOPSIS)
  [string]$StageDir,
  # -StageDir: replace an existing config.yaml that differs (backup kept)
  [switch]$OverwriteConfig,
  # -StageDir: keep an existing config.yaml that differs, install the rest
  [switch]$KeepConfig,
  # answer yes to every y/N prompt (replace existing install, overwrite config)
  [switch]$Yes
)

$ErrorActionPreference = 'Stop'
$account = 'kuma-agent'
$base = 'C:\ProgramData\kuma-agent'
if ($OverwriteConfig -and $KeepConfig) { throw 'pass at most one of -OverwriteConfig / -KeepConfig' }

# y/N prompt before overriding anything that already exists. -Yes, or the
# matching specific switch passed as $Preset, answers yes up front. With no
# interactive console the answer is no, and the caller aborts.
function Confirm-Override {
  param([string]$Question, [bool]$Preset = $false)
  if ($Preset -or $Yes) {
    Write-Host "$Question -> yes (preset by switch)" -ForegroundColor Yellow
    return $true
  }
  if (-not [Environment]::UserInteractive -or [Console]::IsInputRedirected) {
    Write-Host "$Question -> no (no interactive console to ask)" -ForegroundColor Yellow
    return $false
  }
  while ($true) {
    $a = ([string](Read-Host "$Question [y/N]")).Trim().ToLower()
    if ($a -eq 'y' -or $a -eq 'yes') { return $true }
    if ($a -eq '' -or $a -eq 'n' -or $a -eq 'no') { return $false }
    Write-Host 'please answer y or n' -ForegroundColor Yellow
  }
}

# device.name from a config.yaml ('' if none)
function Get-CfgDeviceName([string]$Path) {
  $m = [regex]::Match((Get-Content -LiteralPath $Path -Raw), '(?m)^\s+name:\s*["'']?([A-Za-z0-9-]+)["'']?\s*$')
  if ($m.Success) { return $m.Groups[1].Value }
  return ''
}

if ($StageDir) {
  if (-not (Test-Path -LiteralPath $StageDir -PathType Container)) { throw "-StageDir '$StageDir' does not exist" }
  $StageDir = (Resolve-Path -LiteralPath $StageDir).Path
  foreach ($f in 'kuma-agent.exe', 'config.yaml', 'ca.pem') {
    $p = Join-Path $StageDir $f
    if (-not (Test-Path -LiteralPath $p)) { throw "$f missing in $StageDir" }
    if ((Get-Item -LiteralPath $p).Length -eq 0) { throw "$p is empty" }
  }
  # validate content before anything changes: a bad file would otherwise be
  # found only by an agent crash-looping under the SCM
  $hdr = New-Object byte[] 2
  $fs = [IO.File]::OpenRead((Join-Path $StageDir 'kuma-agent.exe'))
  try { [void]$fs.Read($hdr, 0, 2) } finally { $fs.Close() }
  if (-not ($hdr[0] -eq 0x4D -and $hdr[1] -eq 0x5A)) { throw "$StageDir\kuma-agent.exe is not a Windows executable (wrong -BinaryType?)" }
  if ((Get-Content -LiteralPath (Join-Path $StageDir 'ca.pem') -Raw) -notmatch '-----BEGIN CERTIFICATE-----') { throw "$StageDir\ca.pem is not a PEM certificate" }
  $stagedCfgText = Get-Content -LiteralPath (Join-Path $StageDir 'config.yaml') -Raw
  $stagedName = Get-CfgDeviceName (Join-Path $StageDir 'config.yaml')
  if (-not $stagedName) { throw "$StageDir\config.yaml has no device name" }
  if ($stagedCfgText -notmatch ('(?m)^token_file:\s*["'']?' + [regex]::Escape("$base\token"))) { throw "$StageDir\config.yaml: token_file must be $base\token" }
  $stagedToken = Join-Path $StageDir 'token'
  if (Test-Path -LiteralPath $stagedToken) {
    if ((Get-Item -LiteralPath $stagedToken).Length -eq 0) { throw "$stagedToken is empty" }
  } elseif (-not (Test-Path -LiteralPath "$base\token")) {
    throw "no token in $StageDir and none installed at $base\token"
  }
  $InstallService = $true
}

# Run a native exe, echo its output, abort on non-zero exit. EAP is relaxed
# in this function's scope only: under 'Stop', PS 5.1 turns redirected native
# stderr into a terminating error before $LASTEXITCODE could be checked.
function Invoke-Checked {
  param([string]$Exe, [string[]]$ArgList, [string]$Display)
  $ErrorActionPreference = 'Continue'
  $out = & $Exe @ArgList 2>&1
  $rc = $LASTEXITCODE
  $text = (@($out) | ForEach-Object { "$_" }) -join "`n"
  if ($text) { Write-Host $text }
  if (-not $Display) { $Display = "$Exe $($ArgList -join ' ')" }
  if ($rc -ne 0) { throw "FAILED (exit $rc): $Display" }
  # icacls reports per-file failures in its summary line
  if ($text -match 'Failed processing [1-9]') { throw "FAILED (per-file errors, exit $rc): $Display" }
}

# Throwaway password: 36 random bytes, base64. The fixed suffix guarantees
# the digit + symbol classes a complexity policy may require.
function New-RandomPassword {
  $bytes = New-Object byte[] 36
  $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
  $rng.GetBytes($bytes)
  $rng.Dispose()
  return [Convert]::ToBase64String($bytes) + '!9aA'
}

# Add the account SID to user-rights lines via secedit, preserving existing
# holders. Only the rights listed in the generated .inf are configured
# (/areas USER_RIGHTS), everything else in local policy is untouched. The
# result is verified by a second export. A domain GPO can still override
# local user rights; if verification fails, set them by hand in secpol.msc.
function Add-UserRights {
  param([string]$Sid, [string[]]$Rights)
  $tmp = Join-Path $env:TEMP ('kuma-secedit-' + [guid]::NewGuid().ToString('N'))
  $null = New-Item -ItemType Directory -Path $tmp
  try {
    $exp = Join-Path $tmp 'export.inf'
    Invoke-Checked 'secedit.exe' @('/export', '/cfg', $exp, '/areas', 'USER_RIGHTS', '/quiet')
    $lines = Get-Content -LiteralPath $exp
    $inf = @('[Unicode]', 'Unicode=yes', '[Version]', 'signature="$CHICAGO$"', 'Revision=1', '[Privilege Rights]')
    foreach ($r in $Rights) {
      $cur = @($lines | Where-Object { $_ -match ('^\s*' + $r + '\s*=') }) | Select-Object -First 1
      $vals = @()
      if ($cur) { $vals = @((($cur -split '=', 2)[1]).Split(',') | ForEach-Object { $_.Trim() } | Where-Object { $_ }) }
      if ($vals -notcontains "*$Sid") { $vals += "*$Sid" }
      $inf += "$r = " + ($vals -join ',')
    }
    $cfg = Join-Path $tmp 'rights.inf'
    Set-Content -LiteralPath $cfg -Value $inf -Encoding Unicode
    Invoke-Checked 'secedit.exe' @('/configure', '/db', (Join-Path $tmp 'rights.sdb'), '/cfg', $cfg, '/areas', 'USER_RIGHTS', '/quiet')
    $ver = Join-Path $tmp 'verify.inf'
    Invoke-Checked 'secedit.exe' @('/export', '/cfg', $ver, '/areas', 'USER_RIGHTS', '/quiet')
    # secedit wraps long privilege lines with a trailing backslash: unwrap
    # first, then match each right's full (continued) value. Matching only
    # the first physical line produced false 'not applied' aborts when the
    # SID landed after a wrap.
    $text = ((Get-Content -LiteralPath $ver) -join "`n") -replace "\`r?\\\`r?\n", ' '
    foreach ($r in $Rights) {
      $m = [regex]::Match($text, ('(?im)^[ \t]*' + $r + '[ \t]*=(.*)$'))
      # secedit exports members as *SID, but may render built-in/local
      # accounts as NAME or MACHINE\NAME -- accept SID or account name.
      # every interpolated piece is escaped: a raw '\' + 'kuma-agent' is the
      # regex \k (named backreference) and throws before any check runs.
      $sidRe  = '\*' + [regex]::Escape($Sid) + '(\s|,|$)'
      $nameRe = '(?i)(^|[,\s])\*?(' + [regex]::Escape($env:COMPUTERNAME) + '\\)?' + [regex]::Escape($account) + '(\s|,|$)'
      if (-not $m.Success -or (($m.Groups[1].Value -notmatch $sidRe) -and ($m.Groups[1].Value -notmatch $nameRe))) {
        throw ("user right $r not applied to $account (domain policy override?). Set it by hand: secpol.msc > Local Policies > " +
          "User Rights Assignment: add $account to 'Deny log on locally' and 'Deny log on through Remote Desktop Services' " +
          "and to 'Log on as a service'; do NOT add it to 'Deny log on as a service'.")
      }
    }
  } finally {
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
  }
}

# --- 0. -StageDir: every decision before ANY change ------------------------
# Decided up front so a refusal leaves the box exactly as it was.
$installConfig = $true
$cfgBackup = ''
if ($StageDir) {
  # 0a. existing install: show it and ask before replacing it
  $svc0 = Get-Service -Name 'kuma-agent' -ErrorAction SilentlyContinue
  $exe0 = "$base\bin\kuma-agent.exe"
  if ($svc0 -or (Test-Path -LiteralPath $exe0)) {
    Write-Host 'existing kuma-agent install found:' -ForegroundColor Yellow
    if ($svc0) {
      $svcPath = [string](Get-CimInstance -ClassName Win32_Service -Filter "Name='kuma-agent'").PathName
      Write-Host "  service: $($svc0.Status) ($svcPath)"
      if ($svcPath -and ($svcPath -notmatch [regex]::Escape($exe0))) {
        Write-Host "  WARNING: the service runs a different exe than $exe0; this install only replaces $exe0." -ForegroundColor Yellow
        Write-Host '  remove that service first (sc.exe delete kuma-agent) if it belongs to an old install.' -ForegroundColor Yellow
      }
    }
    if (Test-Path -LiteralPath $exe0) {
      $same = (Get-FileHash -LiteralPath $exe0).Hash -eq (Get-FileHash -LiteralPath (Join-Path $StageDir 'kuma-agent.exe')).Hash
      Write-Host "  exe:     $exe0 ($(if ($same) { 'identical to the staged one' } else { 'differs from the staged one: will be replaced' }))"
    }
    if (Test-Path -LiteralPath "$base\config.yaml") {
      Write-Host "  device:  $(Get-CfgDeviceName "$base\config.yaml") (installed config) -> $stagedName (staged)"
    }
    if ((Test-Path -LiteralPath "$base\token") -and (Test-Path -LiteralPath (Join-Path $StageDir 'token'))) {
      Write-Host '  token:   installed token will be replaced by the staged one'
    }
    if (-not (Confirm-Override 'Stop the service and replace this install?')) {
      throw 'NOTHING CHANGED: existing install kept. Rerun and answer y (or pass -Yes) to replace it.'
    }
  }

  # 0b. config: overwrite or keep a different installed config.yaml
  $newCfg = Join-Path $StageDir 'config.yaml'
  $curCfg = "$base\config.yaml"
  if (Test-Path -LiteralPath $curCfg) {
    $old = @(Get-Content -LiteralPath $curCfg | ForEach-Object { $_.TrimEnd() })
    $new = @(Get-Content -LiteralPath $newCfg | ForEach-Object { $_.TrimEnd() })
    if (($old -join "`n") -ne ($new -join "`n")) {
      Write-Host "existing $curCfg differs from the staged one" -ForegroundColor Yellow
      Write-Host '--- diff (- installed, + staged) ---'
      if ($old.Count -eq 0 -or $new.Count -eq 0) {
        foreach ($l in $old) { Write-Host "- $l" }
        foreach ($l in $new) { Write-Host "+ $l" }
      } else {
        foreach ($d in (Compare-Object -ReferenceObject $old -DifferenceObject $new)) {
          if ($d.SideIndicator -eq '<=') { Write-Host "- $($d.InputObject)" } else { Write-Host "+ $($d.InputObject)" }
        }
      }
      $curName = Get-CfgDeviceName $curCfg
      $nameDiffers = $curName -ne $stagedName
      if ($nameDiffers) {
        Write-Host "  device name changes: '$curName' -> '$stagedName'. The staged token belongs to '$stagedName';" -ForegroundColor Yellow
        Write-Host "  keeping the installed config would make the agent log in as '$curName' with it and be rejected." -ForegroundColor Yellow
      }
      Write-Host '  merge any local settings (e.g. commands:) into the staged file first if you still need them.'
      if ((-not $KeepConfig) -and (Confirm-Override "Overwrite $curCfg with the staged one (backup kept)?" ([bool]$OverwriteConfig))) {
        $cfgBackup = "$curCfg.bak-" + (Get-Date -Format 'yyyyMMdd-HHmmss')
      } elseif (Confirm-Override 'Keep the installed config.yaml and install everything else?' ([bool]$KeepConfig -and -not $nameDiffers)) {
        if ($nameDiffers -and (Test-Path -LiteralPath (Join-Path $StageDir 'token'))) {
          Write-Host "  WARNING: keeping '$curName' with the token minted for '$stagedName'; expect unknown_device / bad token rejects." -ForegroundColor Yellow
        }
        $installConfig = $false
      } else {
        throw ("NOTHING CHANGED: $curCfg differs from $newCfg (diff above). Merge any local settings into the staged file, " +
          'then run this again (-OverwriteConfig / -KeepConfig pre-answer the prompts).')
      }
    }
  }
}

# --- 1. service account ------------------------------------------------------
# Standard user, never an administrator, never SYSTEM. Created with a random
# password that is immediately forgotten: nobody logs on as this account, and
# -InstallService sets a fresh random one when it registers the service.
$user = Get-LocalUser -Name $account -ErrorAction SilentlyContinue
if (-not $user) {
  $pw = ConvertTo-SecureString (New-RandomPassword) -AsPlainText -Force
  $user = New-LocalUser -Name $account -Password $pw -AccountNeverExpires -PasswordNeverExpires `
    -UserMayNotChangePassword -Description 'KumaBoard agent service account'  # New-LocalUser caps Description at 48 chars
  $pw = $null
  # same membership 'net user /add' gives: plain Users, nothing more
  Add-LocalGroupMember -SID 'S-1-5-32-545' -Member $account
  Write-Host "created local user $account (standard user, password never expires)"
} else {
  Write-Host "local user $account exists"
}
$sid = $user.SID.Value

# --- 2. logon rights -----------------------------------------------------------
# Deny local + RDP logon (the account only ever runs the service). Grant
# 'Log on as a service' explicitly instead of relying on the SCM to add it.
Add-UserRights -Sid $sid -Rights @('SeDenyInteractiveLogonRight', 'SeDenyRemoteInteractiveLogonRight', 'SeServiceLogonRight')
Write-Host "$account : interactive + RDP logon denied, service logon granted"

# --- 3. directories + ACLs -----------------------------------------------------
foreach ($d in @($base, "$base\bin", 'C:\scripts')) {
  if (-not (Test-Path -LiteralPath $d)) { $null = New-Item -ItemType Directory -Path $d; Write-Host "created $d" }
}
Invoke-Checked 'icacls.exe' @($base, '/inheritance:r', '/grant:r', 'Administrators:(OI)(CI)F', 'SYSTEM:(OI)(CI)F', "${account}:(OI)(CI)RX")
Invoke-Checked 'icacls.exe' @("$base\bin", '/grant:r', "${account}:(OI)(CI)M")
Invoke-Checked 'icacls.exe' @('C:\scripts', '/inheritance:r', '/grant:r', 'Administrators:(OI)(CI)F', 'SYSTEM:(OI)(CI)F', "${account}:(OI)(CI)RX")

# The service opens C:\ProgramData\kuma-agent\agent.log with O_CREATE|O_APPEND
# (cmd/kuma-agent/platform_windows.go) BEFORE anything else; with only RX on
# the directory it cannot create the file, exits with code 2 and the SCM
# restart-loops it every 5 s. Pre-create the log and grant Modify on that one
# file rather than Modify on the whole directory: the agent must not be able
# to rewrite its own config.yaml / ca.pem / token (server URL, trust anchor).
# Everything else it writes (upgrade temp files, pending marker, .old) lives
# in bin, which already has Modify.
$log = "$base\agent.log"
if (-not (Test-Path -LiteralPath $log)) { $null = New-Item -ItemType File -Path $log; Write-Host "created $log" }
Invoke-Checked 'icacls.exe' @($log, '/grant:r', "${account}:M")

# --- 4. -StageDir: stop service, then replace files ---------------------------
$svcName = 'kuma-agent'
if ($StageDir) {
  $svc = Get-Service -Name $svcName -ErrorAction SilentlyContinue
  if ($svc -and $svc.Status -ne 'Stopped') {
    Write-Host "stopping service $svcName before replacing its exe"
    Stop-Service -Name $svcName -Force
  }
  # the SCM reports Stopped slightly before the process image is released
  for ($i = 0; $i -lt 30; $i++) {
    if (-not (Get-Process -Name 'kuma-agent' -ErrorAction SilentlyContinue)) { break }
    Start-Sleep -Seconds 1
  }
  if (Get-Process -Name 'kuma-agent' -ErrorAction SilentlyContinue) { throw 'kuma-agent.exe still running 30 s after stop; not replacing it' }

  Copy-Item -LiteralPath (Join-Path $StageDir 'kuma-agent.exe') -Destination "$base\bin\kuma-agent.exe" -Force
  Copy-Item -LiteralPath (Join-Path $StageDir 'ca.pem') -Destination "$base\ca.pem" -Force
  if ($installConfig) {
    if ($cfgBackup) {
      Copy-Item -LiteralPath "$base\config.yaml" -Destination $cfgBackup
      Write-Host "backed up the previous config.yaml to $cfgBackup"
    }
    Copy-Item -LiteralPath (Join-Path $StageDir 'config.yaml') -Destination "$base\config.yaml" -Force
  }
  if (Test-Path -LiteralPath (Join-Path $StageDir 'sleep.ps1')) {
    Copy-Item -LiteralPath (Join-Path $StageDir 'sleep.ps1') -Destination 'C:\scripts\sleep.ps1' -Force
  }
  if (Test-Path -LiteralPath (Join-Path $StageDir 'token')) {
    # moved, not copied: no plaintext token left in the staging dir
    Move-Item -LiteralPath (Join-Path $StageDir 'token') -Destination "$base\token" -Force
  } else {
    Write-Host "no staged token: keeping the installed $base\token" -ForegroundColor Yellow
  }
  # a same-volume move keeps the staging dir's ACEs: reset to the parent's,
  # strip inheritance, then grant exactly Administrators/SYSTEM F + agent R
  Invoke-Checked 'icacls.exe' @("$base\token", '/reset')
  Invoke-Checked 'icacls.exe' @("$base\token", '/setowner', 'Administrators')
  Invoke-Checked 'icacls.exe' @("$base\token", '/inheritance:r', '/grant:r', 'Administrators:F', 'SYSTEM:F', "${account}:R")
  Write-Host "installed exe, ca.pem, $(if ($installConfig) { 'config.yaml, ' })token into $base"
}

# --- 5. register the service ---------------------------------------------------
if ($InstallService) {
  $exe = "$base\bin\kuma-agent.exe"
  if (-not (Test-Path -LiteralPath $exe)) { throw "$exe missing: copy kuma-agent.exe into $base\bin first" }
  if (Get-Service -Name $svcName -ErrorAction SilentlyContinue) {
    Write-Host 'service kuma-agent already installed (logon password left unchanged)'
  } else {
    # Fresh random logon password, set on the account and handed to the SCM
    # in one go; it is never shown or stored. It passes through the
    # kuma-agent.exe command line for the life of that one call (visible to
    # other administrators only), which is why it is single-use.
    $plain = New-RandomPassword
    Set-LocalUser -Name $account -Password (ConvertTo-SecureString $plain -AsPlainText -Force)
    Invoke-Checked $exe @('install', '-account', ".\$account", '-password', $plain) "$exe install -account .\$account -password ***"
    $plain = $null
  }
}

# --- 6. -StageDir: start + show status -------------------------------------------
if ($StageDir) {
  $startErr = $null
  try { Start-Service -Name $svcName } catch { $startErr = $_ }
  Start-Sleep -Seconds 5
  $svc1 = Get-Service -Name $svcName
  $svc1 | Format-Table -AutoSize Name, Status, StartType
  Write-Host "--- $base\agent.log (last 20 lines) ---"
  Get-Content -LiteralPath "$base\agent.log" -Tail 20
  if ($startErr) { throw "service $svcName failed to start: $startErr" }
  # a config/token problem makes the agent exit right after start and the
  # SCM restart-loop it: still not Running 5 s later means look at the log
  if ($svc1.Status -ne 'Running') { throw "service $svcName is $($svc1.Status) 5 s after start; see agent.log above" }
  Write-Host 'Done. Check the dashboard; do not rerun deploy-agent.ps1 to verify (use its -VerifyOnly).'
} elseif (-not $InstallService) {
  Write-Host "Prepared. Next: copy kuma-agent.exe into $base\bin, config.yaml + ca.pem into $base, the token to $base\token,"
  Write-Host '  then run this script again with -InstallService.'
}
