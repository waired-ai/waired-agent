#Requires -Version 5.1
<#
.SYNOPSIS
    Smoke-tests the Waired Windows installer flow inside a Windows
    Server Core container.

.DESCRIPTION
    Intended to be exec'd inside a container started by
    `docker run mcr.microsoft.com/windows/servercore:ltsc2022 ...`
    with the worktree mounted at C:\waired-host.

    The container has no GUI, no UAC, and a single ContainerAdministrator
    user, so this script exercises only the parts of the install flow
    that don't require those: zip extract, SHA-256 verify,
    `waired-agent.exe install` (SCM register), service lookup,
    `waired-agent.exe uninstall` (SCM deregister).

    Things deliberately NOT tested (require a real Windows host):
        - UAC self-elevation
        - HKCU\...\Run autostart by waired-tray first launch
        - Mark-of-the-Web / SmartScreen behaviour on the Setup.exe
        - The Inno Setup wizard UI (silent install can be tested
          separately if iscc is available)

    Exits 0 on success, non-zero on the first failed assertion.

.PARAMETER ZipPath
    Path inside the container to waired-windows-amd64.zip.
    Default C:\waired-host\dist\waired-windows-amd64.zip.

.PARAMETER ShaPath
    Path to the matching .sha256 file. Default <ZipPath>.sha256.

.PARAMETER InstallDir
    Where to extract. Default C:\Program Files\Waired.

.PARAMETER LeaveInstalled
    Skip the uninstall step so the caller can poke at the
    registered service. Default: false (uninstall + cleanup).
#>
[CmdletBinding()]
param(
    [string]$ZipPath      = 'C:\waired-host\dist\waired-windows-amd64.zip',
    [string]$ShaPath,
    [string]$InstallDir   = (Join-Path $env:ProgramFiles 'Waired'),
    [switch]$LeaveInstalled
)

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'
if (-not $ShaPath) { $ShaPath = "$ZipPath.sha256" }

$ServiceName = 'waired-agent'
$failures    = 0

function Step  { param([string]$M) Write-Host "==> $M" -ForegroundColor Cyan }
function Pass  { param([string]$M) Write-Host "[OK] $M" -ForegroundColor Green }
function Fail  { param([string]$M) Write-Host "[FAIL] $M" -ForegroundColor Red; $script:failures++ }
function Note  { param([string]$M) Write-Host "[..] $M" -ForegroundColor DarkGray }

Step "Smoke test starting (PID=$PID, host=$env:COMPUTERNAME)"

# 1. Source artifacts present
if (-not (Test-Path -LiteralPath $ZipPath)) { Fail "zip not found: $ZipPath"; exit 1 }
if (-not (Test-Path -LiteralPath $ShaPath)) { Fail "sha not found: $ShaPath"; exit 1 }
Pass "source artifacts present"

# 2. SHA-256 verification (matches install.ps1's verification logic)
Step "Verifying SHA-256 of $ZipPath"
$expectedLine = (Get-Content -LiteralPath $ShaPath -First 1).Trim()
$expected = ($expectedLine -split '\s+')[0].ToLowerInvariant()
$actual   = (Get-FileHash -LiteralPath $ZipPath -Algorithm SHA256).Hash.ToLowerInvariant()
if ($expected -ne $actual) {
    Fail "SHA-256 mismatch: expected=$expected actual=$actual"
    exit 1
}
Pass "SHA-256 matches ($actual)"

# 3. Pre-existing service cleanup (idempotent)
Step "Removing any pre-existing $ServiceName"
$svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($svc) {
    if ($svc.Status -ne 'Stopped') {
        try { Stop-Service -Name $ServiceName -Force -ErrorAction Stop } catch {
            Note "Stop-Service threw: $($_.Exception.Message); continuing with sc.exe delete"
        }
    }
    & sc.exe delete $ServiceName | Out-Null
    if ($LASTEXITCODE -ne 0) { Fail "sc.exe delete returned $LASTEXITCODE" } else { Pass "removed pre-existing service" }
} else {
    Note "no pre-existing service"
}

# 4. Extract
Step "Extracting to $InstallDir"
if (-not (Test-Path -LiteralPath $InstallDir)) {
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
}
Expand-Archive -LiteralPath $ZipPath -DestinationPath $InstallDir -Force
$expected_exes = @('waired.exe','waired-agent.exe','waired-tray.exe')
foreach ($e in $expected_exes) {
    $p = Join-Path $InstallDir $e
    if (Test-Path -LiteralPath $p) { Pass "extracted $e" } else { Fail "missing $e at $p" }
}

# 5. Register the service (the Go side does SCM + Event Log + DACL work)
$agent = Join-Path $InstallDir 'waired-agent.exe'
Step "Running: $agent install"
& $agent install
if ($LASTEXITCODE -ne 0) {
    Fail "waired-agent install exited $LASTEXITCODE"
} else {
    Pass "waired-agent install exited 0"
}

# 6. Verify SCM state matches what release.yml's contract promises
Step "Inspecting SCM registration"
$svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if (-not $svc) {
    Fail "service $ServiceName not registered after install"
    exit 1
}
Pass "service registered (Name=$($svc.Name), StartType=$($svc.StartType), Status=$($svc.Status))"

# sc.exe qc gives ImagePath / start type / account, easier to grep than CIM
$qc = (& sc.exe qc $ServiceName) -join "`n"
Write-Host '--- sc qc output (filtered) ---' -ForegroundColor DarkGray
$qc -split "`n" |
    Where-Object { $_ -match 'BINARY_PATH_NAME|START_TYPE|SERVICE_START_NAME|DISPLAY_NAME' } |
    ForEach-Object { Write-Host "  $($_.Trim())" }
Write-Host '-------------------------------' -ForegroundColor DarkGray

if ($qc -notmatch [regex]::Escape($agent)) {
    Fail "ImagePath does not reference the expected agent exe: $agent"
} else {
    Pass "ImagePath references $agent"
}
# Windows SCM reports AutomaticDelayedStart as START_TYPE 2  AUTO_START + DELAYED
if ($qc -match 'AUTO_START.*DELAYED' -or $qc -match 'DELAYED.*AUTO_START') {
    Pass "StartType is AutomaticDelayedStart"
} elseif ($qc -match 'AUTO_START') {
    Note "StartType is AutomaticStart (delayed flag not visible via sc qc on this image)"
} else {
    Fail "StartType is not AUTO_START: $($qc -split "`n" | Where-Object { $_ -match 'START_TYPE' })"
}
if ($qc -match 'LocalSystem') {
    Pass "running as LocalSystem"
} else {
    Note "SERVICE_START_NAME line: $($qc -split "`n" | Where-Object { $_ -match 'SERVICE_START_NAME' })"
}

# 7. State directory + secrets DACL
$stateDir = Join-Path $env:ProgramData 'waired'
if (Test-Path -LiteralPath $stateDir) {
    Pass "state dir created: $stateDir"
    $secretsDir = Join-Path $stateDir 'secrets'
    if (Test-Path -LiteralPath $secretsDir) {
        Pass "secrets dir created: $secretsDir"
        Write-Host '--- icacls secrets ---' -ForegroundColor DarkGray
        & icacls.exe $secretsDir | ForEach-Object { Write-Host "  $_" }
        Write-Host '----------------------' -ForegroundColor DarkGray
    } else {
        Note "no secrets/ subdirectory (created lazily by daemon on first key write)"
    }
} else {
    Fail "state dir not created at $stateDir"
}

# 8. Uninstall round-trip
if (-not $LeaveInstalled) {
    Step "Running: $agent uninstall"
    & $agent uninstall
    if ($LASTEXITCODE -ne 0) {
        Fail "waired-agent uninstall exited $LASTEXITCODE"
    } else {
        Pass "waired-agent uninstall exited 0"
    }
    $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if ($svc) { Fail "service still present after uninstall" } else { Pass "service removed by uninstall" }
}

# 9. Summary
Write-Host ''
if ($failures -eq 0) {
    Write-Host '=== smoke test PASSED ===' -ForegroundColor Green
    exit 0
} else {
    Write-Host "=== smoke test FAILED ($failures assertion(s)) ===" -ForegroundColor Red
    exit 1
}
