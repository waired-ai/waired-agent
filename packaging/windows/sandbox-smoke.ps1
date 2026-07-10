#Requires -Version 5.1
<#
.SYNOPSIS
    Inside-Sandbox driver: validate the tray's first-launch
    HKCU\...\Run autostart write that Linux gets from the .deb's
    /etc/xdg/autostart/ but Windows has to register itself.

.DESCRIPTION
    Run by the Windows Sandbox LogonCommand. Sandbox is the cheapest
    place to exercise the "real interactive user session" parts of the
    installer flow that Windows containers can't reach.

    What this test does:
      1. Copies the staged Windows binaries from the mounted worktree
         (C:\waired-host\dist\windows-amd64\) to %ProgramFiles%\Waired\.
      2. Runs `waired-agent.exe install` (Go-side SCM + Event Log + DACL).
      3. Asserts the service is registered.
      4. Launches waired-tray.exe and waits up to 15s for the tray's
         ensureAutostartOnFirstLaunch() hook to write
         HKCU\Software\Microsoft\Windows\CurrentVersion\Run\waired-tray.
         This is the regression gate for internal/gui/tray/tray.go --
         without ensureAutostartOnFirstLaunch the tray requires an
         explicit menu click before the registry entry appears.
      5. Stops the tray, runs `waired-agent.exe uninstall`, writes a
         structured JSON result to C:\waired-out\result.json (mounted
         RW from host dist\sandbox-out\).

    What this does NOT cover (real-host territory):
      * install.ps1's late-UAC transition: Sandbox auto-elevates the
        LogonCommand so the un-elevated -> Start-Process -Verb RunAs
        path never fires. Test by hand on a normal Windows host.
      * SmartScreen / Mark-of-the-Web warnings on a downloaded
        Setup.exe.

.PARAMETER Elevated
    Internal flag: set when re-invoked by Phase A's Start-Process
    RunAs. Skips Phase A's elevation gate.
#>
[CmdletBinding()]
param(
    [switch]$Elevated
)

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'

$HostMount   = 'C:\waired-host'
$OutMount    = 'C:\waired-out'
$InstallDir  = Join-Path $env:ProgramFiles 'Waired'
$ServiceName = 'waired-agent'
$TrayAppName = 'waired-tray'
$RunKeyPath  = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run'

$LogPath     = Join-Path $OutMount 'sandbox.log'
$ResultPath  = Join-Path $OutMount 'result.json'

function Log {
    param([string]$Msg, [string]$Phase = 'main')
    $line = '[{0}] [{1}] {2}' -f (Get-Date -Format 'HH:mm:ss'), $Phase, $Msg
    Write-Host $line
    try { Add-Content -LiteralPath $LogPath -Value $line -ErrorAction SilentlyContinue } catch {}
}

function Test-Admin {
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    (New-Object Security.Principal.WindowsPrincipal($id)).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

# ------------- Phase A -------------
if (-not $Elevated) {
    if (-not (Test-Path -LiteralPath $OutMount)) {
        Write-Host "FATAL: $OutMount not mounted; aborting" -ForegroundColor Red
        exit 99
    }
    Remove-Item -LiteralPath $LogPath, $ResultPath -ErrorAction SilentlyContinue
    Log "Phase A; PID=$PID; user=$env:USERNAME; admin=$(Test-Admin)" 'PhaseA'

    if (Test-Admin) {
        Log "Sandbox LogonCommand already elevated; running Phase B inline" 'PhaseA'
        & $PSCommandPath -Elevated
        exit $LASTEXITCODE
    }
    try {
        $proc = Start-Process -FilePath 'powershell.exe' `
            -ArgumentList @('-NoProfile','-ExecutionPolicy','Bypass','-File',$PSCommandPath,'-Elevated') `
            -Verb RunAs -PassThru -ErrorAction Stop
        $proc.WaitForExit()
        exit $proc.ExitCode
    } catch {
        @{ ok = $false; phase = 'A'; reason = "UAC self-elevation failed: $($_.Exception.Message)" } |
            ConvertTo-Json | Set-Content -LiteralPath $ResultPath -Encoding UTF8
        exit 1
    }
}

# ------------- Phase B (elevated) -------------
$failures = @()
function Pass { param([string]$M) Log "[OK]   $M" 'PhaseB' }
function Fail { param([string]$M) Log "[FAIL] $M" 'PhaseB'; $script:failures += $M }

Log "Phase B; PID=$PID; user=$env:USERNAME; admin=$(Test-Admin)" 'PhaseB'
if (-not (Test-Admin)) {
    Fail "Phase B not actually elevated; aborting"
    @{ ok = $false; phase = 'B'; failures = $failures } |
        ConvertTo-Json | Set-Content -LiteralPath $ResultPath -Encoding UTF8
    exit 1
}

# 1. Stage binaries from the worktree mount.
$srcDir = Join-Path $HostMount 'dist\windows-amd64'
foreach ($e in @('waired.exe','waired-agent.exe','waired-tray.exe')) {
    if (-not (Test-Path -LiteralPath (Join-Path $srcDir $e))) {
        Fail "missing source $e at $srcDir"
    }
}
if ($failures.Count -gt 0) {
    @{ ok = $false; phase = 'B'; failures = $failures } |
        ConvertTo-Json | Set-Content -LiteralPath $ResultPath -Encoding UTF8
    exit 1
}
New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
Copy-Item (Join-Path $srcDir 'waired.exe')       (Join-Path $InstallDir 'waired.exe')       -Force
Copy-Item (Join-Path $srcDir 'waired-agent.exe') (Join-Path $InstallDir 'waired-agent.exe') -Force
Copy-Item (Join-Path $srcDir 'waired-tray.exe')  (Join-Path $InstallDir 'waired-tray.exe')  -Force
Pass "binaries staged at $InstallDir"

# 2. waired-agent install.
$agent = Join-Path $InstallDir 'waired-agent.exe'
$tray  = Join-Path $InstallDir 'waired-tray.exe'
Log "$agent install" 'PhaseB'
& $agent install 2>&1 | ForEach-Object { Log "  agent: $_" 'PhaseB' }
if ($LASTEXITCODE -ne 0) {
    Fail "waired-agent install exited $LASTEXITCODE"
} else {
    Pass "waired-agent install exited 0"
}

# 3. Service registered?
$svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if (-not $svc) {
    Fail "service $ServiceName not registered"
} else {
    Pass "service registered ($($svc.Name), StartType=$($svc.StartType))"
}

# 4. Launch tray; expect ensureAutostartOnFirstLaunch to write HKCU\Run.
Log "launching tray: $tray" 'PhaseB'
$trayProc = Start-Process -FilePath $tray -PassThru -WindowStyle Hidden

$entry = $null
$deadline = (Get-Date).AddSeconds(15)
while ((Get-Date) -lt $deadline) {
    Start-Sleep -Milliseconds 500
    try {
        $entry = (Get-ItemProperty -Path $RunKeyPath -Name $TrayAppName -ErrorAction Stop).$TrayAppName
        break
    } catch { continue }
}
if ($entry) {
    Pass "HKCU\Run\$TrayAppName = $entry"
    if ($entry -notmatch [regex]::Escape($tray)) {
        Fail "HKCU\Run\$TrayAppName does not reference $tray"
    } else {
        Pass "HKCU entry references installed tray path"
    }
} else {
    Fail "HKCU\Run\$TrayAppName never appeared within 15s -- ensureAutostartOnFirstLaunch regressed?"
}

if ($trayProc -and -not $trayProc.HasExited) {
    try { Stop-Process -Id $trayProc.Id -Force -ErrorAction Stop } catch {}
}

# 5. Cleanup.
Log "$agent uninstall" 'PhaseB'
& $agent uninstall 2>&1 | ForEach-Object { Log "  agent: $_" 'PhaseB' }
if ($LASTEXITCODE -ne 0) {
    Fail "waired-agent uninstall exited $LASTEXITCODE"
} else {
    Pass "waired-agent uninstall exited 0"
}

$ok = ($failures.Count -eq 0)
@{
    ok           = $ok
    phase        = 'B'
    failures     = $failures
    hkcu_value   = $entry
    timestamp    = (Get-Date).ToString('o')
    note         = 'install.ps1 late-UAC transition not exercised (Sandbox LogonCommand is auto-elevated). Test by hand on a normal Windows host.'
} | ConvertTo-Json -Depth 3 | Set-Content -LiteralPath $ResultPath -Encoding UTF8

if ($ok) {
    Log "=== sandbox smoke PASSED ===" 'PhaseB'
} else {
    Log "=== sandbox smoke FAILED ($($failures.Count) assertion(s)) ===" 'PhaseB'
}
Log "You may close the Sandbox window now." 'PhaseB'
Start-Sleep -Seconds 5
exit ([int](-not $ok))
