#Requires -Version 5.1
<#
.SYNOPSIS
    Installs waired-agent as a Windows Service.

.DESCRIPTION
    Copies waired-agent.exe to %ProgramFiles%\Waired\ and registers it with
    the Service Control Manager by invoking the binary's own `install`
    sub-command (implemented in internal/platform/service/service_windows.go,
    landed in PR #52). The Go-side install handler:

        * CreateService with StartType=Automatic-Delayed, LocalSystem
        * Recovery actions (3 restarts with 5s/15s/30s backoff)
        * Registers an Event Log source under "Application"
        * Creates <StateDir> and <StateDir>\secrets with restrictive DACLs
          via internal/platform/secrets

    This script is the *developer-facing* installer: it expects a
    pre-built local exe (via -Binary or -Build) and is intended for use
    from a repo checkout while iterating on the agent.

    End users go through packaging/install/install.ps1 instead, which
    downloads the release zip from the public mirror and converges on
    the same downstream registration path (waired-agent.exe install).
    Keep the Go-side install handler the single source of truth — do
    not duplicate SCM / DACL logic into either PowerShell script.

    Idempotent: re-running with an existing registration is a no-op unless
    -Force is passed.

.PARAMETER Binary
    Path to a pre-built waired-agent.exe. Defaults to <repo>\bin\waired-agent.exe.
    Pass an explicit path when shipping from an installer.

.PARAMETER InstallDir
    Permanent install directory. Defaults to %ProgramFiles%\Waired. The exe
    is copied here before `install` is invoked, because the SCM bakes the
    binary's absolute path into the ImagePath at registration time.

.PARAMETER StateDir
    Override for --state-dir. Empty means the Go side resolves the platform
    default (%ProgramData%\waired).

.PARAMETER MgmtAddr
    Optional --mgmt override (loopback management bind). Empty means default.

.PARAMETER Build
    Run `go build` to produce bin\waired-agent.exe before installing. Only
    valid when running from a repo checkout with the Go toolchain available.

.PARAMETER Force
    If the service is already registered, stop and `sc.exe delete` it before
    re-installing. The Go-side `waired-agent uninstall` would be cleaner
    but requires the previously-installed binary to be intact; `sc.exe
    delete` works even if the old exe is missing.

.PARAMETER NoStart
    Skip Start-Service after registration. Useful in installer contexts
    that defer service start to first reboot.

.EXAMPLE
    PS> .\waired-agent-windows.ps1 -Build

.EXAMPLE
    PS> .\waired-agent-windows.ps1 -Binary D:\artifacts\waired-agent.exe -Force
#>
[CmdletBinding()]
param(
    [string]$Binary,
    [string]$InstallDir = (Join-Path $env:ProgramFiles 'Waired'),
    [string]$StateDir,
    [string]$MgmtAddr,
    [switch]$Build,
    [switch]$Force,
    [switch]$NoStart
)

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'

$ServiceName = 'waired-agent'
$RepoRoot    = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)  # scripts/install/<this>.ps1 -> repo

function Assert-Admin {
    $id   = [System.Security.Principal.WindowsIdentity]::GetCurrent()
    $prin = New-Object System.Security.Principal.WindowsPrincipal($id)
    if (-not $prin.IsInRole([System.Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'This script must run as Administrator (SCM operations require elevation).'
    }
}

function Get-WairedAgentService {
    Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
}

function Invoke-Build {
    Write-Host "Building $RepoRoot\bin\waired-agent.exe (GOOS=windows GOARCH=amd64)"
    $env:CGO_ENABLED = '0'
    $env:GOOS        = 'windows'
    $env:GOARCH      = 'amd64'
    & go build -trimpath -ldflags="-s -w" -o (Join-Path $RepoRoot 'bin\waired-agent.exe') ./cmd/waired-agent
    if ($LASTEXITCODE -ne 0) {
        throw "go build failed (exit $LASTEXITCODE)."
    }
}

function Resolve-Binary {
    if ($Binary) {
        if (-not (Test-Path -LiteralPath $Binary)) {
            throw "Binary not found: $Binary"
        }
        return (Resolve-Path -LiteralPath $Binary).Path
    }
    $default = Join-Path $RepoRoot 'bin\waired-agent.exe'
    if (-not (Test-Path -LiteralPath $default)) {
        throw "Default binary not found at $default. Re-run with -Build or pass -Binary <path>."
    }
    return $default
}

function Uninstall-Existing {
    $svc = Get-WairedAgentService
    if (-not $svc) { return }

    Write-Host "Existing service detected (status: $($svc.Status)); removing..."
    if ($svc.Status -ne 'Stopped') {
        try {
            Stop-Service -Name $ServiceName -Force -ErrorAction Stop
        } catch {
            Write-Warning "Stop-Service failed: $($_.Exception.Message); continuing with sc.exe delete"
        }
    }
    # sc.exe delete works regardless of whether the registered exe still
    # exists on disk; waired-agent.exe uninstall would also remove the
    # Event Log source, but on re-install the Go side tolerates an
    # already-registered source via errEventlogExists.
    & sc.exe delete $ServiceName | Out-Null
    if ($LASTEXITCODE -ne 0) {
        throw "sc.exe delete $ServiceName exited with code $LASTEXITCODE."
    }

    # Wait for SCM to actually drop the entry (a few hundred ms in practice).
    $deadline = (Get-Date).AddSeconds(10)
    while ((Get-Date) -lt $deadline) {
        if (-not (Get-WairedAgentService)) { return }
        Start-Sleep -Milliseconds 200
    }
    throw "Service still present 10s after sc.exe delete."
}

function Install-WairedAgent {
    param([string]$Source)

    if (-not (Test-Path -LiteralPath $InstallDir)) {
        New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    }
    $dest = Join-Path $InstallDir 'waired-agent.exe'
    Write-Host "Copying $Source -> $dest"
    Copy-Item -LiteralPath $Source -Destination $dest -Force

    # Hand off to the Go-side install handler. It picks up its own exe
    # path via os.Executable() so the SCM ImagePath references $dest, not
    # the staging location we copied from.
    $installArgs = @('install')
    if ($StateDir) { $installArgs += "-state-dir=$StateDir" }
    if ($MgmtAddr) { $installArgs += "-mgmt=$MgmtAddr" }
    Write-Host "Running: $dest $($installArgs -join ' ')"
    & $dest @installArgs
    if ($LASTEXITCODE -ne 0) {
        throw "waired-agent install exited with code $LASTEXITCODE."
    }
    return $dest
}

function Start-WairedAgent {
    Write-Host 'Starting service...'
    try {
        Start-Service -Name $ServiceName -ErrorAction Stop
    } catch {
        Write-Warning "Start-Service threw: $($_.Exception.Message)"
        Show-LastEventlogErrors
        return
    }

    # StartType=AutomaticDelayedStart implies SCM may delay actual start,
    # but Start-Service forces immediate transition. Still poll because
    # the daemon's foreground startup (identity load, ollama probe,
    # control-plane enrol attempts) can take several seconds before the
    # SCM reports Running.
    $deadline = (Get-Date).AddSeconds(30)
    while ((Get-Date) -lt $deadline) {
        $svc = Get-Service -Name $ServiceName
        if ($svc.Status -eq 'Running') { break }
        Start-Sleep -Milliseconds 500
    }
    $svc = Get-Service -Name $ServiceName
    if ($svc.Status -ne 'Running') {
        Write-Warning "Service did not reach Running within 30s (current: $($svc.Status))."
        Show-LastEventlogErrors
        return
    }
    Write-Host 'Service reached Running. Confirming stays-running for 5s...'

    # The daemon may exit immediately (e.g. no identity, no controlplane
    # config). Re-poll briefly so the verification block reflects the
    # actual steady-state, not a momentary Running transition.
    Start-Sleep -Seconds 5
    $svc = Get-Service -Name $ServiceName
    if ($svc.Status -eq 'Running') {
        Write-Host 'Service is steady-state Running.'
    } else {
        Write-Warning "Service exited after start (Status: $($svc.Status)). This is expected on fresh machines before `waired init`. SCM Recovery will retry per its policy."
        Show-LastEventlogErrors
    }
}

function Show-LastEventlogErrors {
    Write-Host '--- Recent Application/waired-agent errors ---'
    $events = Get-WinEvent -FilterHashtable @{LogName='Application'; ProviderName=$ServiceName; Level=2} `
        -MaxEvents 3 -ErrorAction SilentlyContinue
    if (-not $events) { Write-Host '  (no events; Event Log source may not yet be registered)'; return }
    foreach ($ev in $events) {
        Write-Host ("  [{0}] {1}" -f $ev.TimeCreated, ($ev.Message -replace '\s+', ' '))
    }
}

function Show-VerificationSummary {
    Write-Host ''
    Write-Host '=== Verification ==='
    $svc = Get-Service -Name $ServiceName
    Write-Host ("Service: {0}  StartType: {1}  Status: {2}" -f $svc.Name, $svc.StartType, $svc.Status)

    $qc = & sc.exe qc $ServiceName 2>&1
    $qc | Where-Object { $_ -match 'BINARY_PATH_NAME|START_TYPE|SERVICE_START_NAME|DISPLAY_NAME' } | ForEach-Object {
        Write-Host ("  {0}" -f $_.Trim())
    }

    # Resolve the effective state dir from `sc qc` for the final check.
    $stateFromImagePath = $null
    foreach ($line in $qc) {
        if ($line -match 'BINARY_PATH_NAME\s*:\s*(.+)$') {
            $imgPath = $matches[1].Trim()
            if ($imgPath -match '-state-dir=([^\s"]+)') {
                $stateFromImagePath = $matches[1]
            }
        }
    }
    if ($stateFromImagePath -and (Test-Path -LiteralPath $stateFromImagePath)) {
        Write-Host "StateDir: $stateFromImagePath"
        $secretsDir = Join-Path $stateFromImagePath 'secrets'
        if (Test-Path -LiteralPath $secretsDir) {
            Write-Host "  secrets\ exists (DACL via icacls):"
            & icacls.exe $secretsDir | Select-Object -First 6 | ForEach-Object { Write-Host "  $_" }
        }
    }
}

# --- main ---

Assert-Admin

if ($Build) {
    Invoke-Build
}

$existing = Get-WairedAgentService
if ($existing -and -not $Force) {
    Write-Host "Service $ServiceName is already installed (Status: $($existing.Status))."
    Write-Host '(pass -Force to uninstall and reinstall)'
    Show-VerificationSummary
    return
}

if ($Force) {
    Uninstall-Existing
}

$src = Resolve-Binary
Install-WairedAgent -Source $src | Out-Null

if (-not $NoStart) {
    Start-WairedAgent
}

Show-VerificationSummary
Write-Host 'Done.'
