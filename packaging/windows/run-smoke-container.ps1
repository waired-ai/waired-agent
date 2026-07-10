#Requires -Version 5.1
<#
.SYNOPSIS
    Host-side driver that runs smoke-container.ps1 inside a fresh
    mcr.microsoft.com/windows/servercore:ltsc2022 container.

.DESCRIPTION
    Used to smoke-test the Windows installer flow without dirtying the
    host. Sequence:
        1. Validate dist\waired-windows-amd64.zip + .sha256 exist
           (run `make dist-windows-installer` or build them locally
           first).
        2. Start a Windows Server Core container with Hyper-V isolation
           and the worktree bind-mounted read-only at C:\waired-host.
        3. Exec smoke-container.ps1 inside, stream its output, propagate
           its exit code.

    The container is --rm so all state is cleaned up automatically. The
    host's own services are never touched.

    Requires:
        - Docker Desktop running in Windows containers mode
          (DockerCli.exe -SwitchDaemon)
        - mcr.microsoft.com/windows/servercore:ltsc2022 already pulled
          (docker pull is slow; do it once)
        - Containers Windows optional feature enabled

.PARAMETER WorktreeRoot
    Repo / worktree root to mount. Default: the worktree this script
    lives in (resolved via $PSScriptRoot\..\..).

.PARAMETER Image
    Container image. Default servercore:ltsc2022.

.PARAMETER LeaveInstalled
    Pass through to smoke-container.ps1: skip `waired-agent uninstall`
    at the end so you can `docker exec` in and poke at the registered
    service. Container is still --rm'd at the end so you have to be
    quick.
#>
[CmdletBinding()]
param(
    [string]$WorktreeRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path,
    [string]$Image = 'mcr.microsoft.com/windows/servercore:ltsc2022',
    [switch]$LeaveInstalled
)

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'

function Die { param([string]$M) Write-Host "[host] $M" -ForegroundColor Red; exit 1 }
function Log { param([string]$M) Write-Host "[host] $M" -ForegroundColor Cyan }

# 1. Sanity-check the worktree side.
$zip = Join-Path $WorktreeRoot 'dist\waired-windows-amd64.zip'
$sha = "$zip.sha256"
$smoke = Join-Path $WorktreeRoot 'packaging\windows\smoke-container.ps1'
foreach ($f in @($zip, $sha, $smoke)) {
    if (-not (Test-Path -LiteralPath $f)) { Die "missing: $f" }
}
Log "worktree root  : $WorktreeRoot"
Log "zip            : $zip"
Log "sha            : $sha"

# 2. Sanity-check docker mode.
$mode = (& docker info --format '{{.OSType}}' 2>$null).Trim()
if ($mode -ne 'windows') {
    Die "docker daemon is in '$mode' mode; switch with DockerCli.exe -SwitchDaemon"
}
Log "docker mode    : $mode"

# 3. Sanity-check image pulled.
$inspect = & docker image inspect $Image 2>$null
if ($LASTEXITCODE -ne 0) {
    Die "image not pulled: $Image  (run: docker pull $Image)"
}
Log "image          : $Image"

# 4. Build run args. Hyper-V isolation is required on Win11 Pro hosts;
#    process isolation only works on Windows Server hosts.
$leaveFlag = if ($LeaveInstalled) { '-LeaveInstalled' } else { '' }
$inContainerCmd = "powershell -NoProfile -ExecutionPolicy Bypass -File C:\waired-host\packaging\windows\smoke-container.ps1 $leaveFlag"

Log "starting container (--isolation=hyperv, --rm, bind-mount ro)..."
Log "in-container command: $inContainerCmd"

# 5. Run. Use --rm so cleanup is automatic, even on failure.
& docker run --rm --isolation=hyperv `
    -v "${WorktreeRoot}:C:\waired-host:ro" `
    $Image `
    cmd /c $inContainerCmd

$rc = $LASTEXITCODE
Log "container exited rc=$rc"
exit $rc
