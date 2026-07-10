#Requires -Version 5.1
<#
.SYNOPSIS
    Host-side driver: launch sandbox-smoke.ps1 inside a fresh Windows
    Sandbox and wait for its JSON result.

.DESCRIPTION
    Sandbox covers the parts of the installer experience that Windows
    containers can't reach: UAC self-elevation, HKCU\...\Run autostart
    written by waired-tray on first launch, and a real interactive user
    session (WDAGUtilityAccount). Sandbox is per-session ephemeral, so
    the host's state is never touched.

    Requires:
        - Windows 11 Pro / Enterprise
        - "Windows サンドボックス" / Containers-DisposableClientVM feature
          enabled
        - dist\waired-windows-amd64.zip artifacts staged in
          dist\windows-amd64\ (run `make dist-windows-installer` first)

    The Sandbox session is interactive: a UAC prompt fires inside the
    Sandbox once -- the user must click "Yes" for the elevation flow
    to be exercised. After that the script runs to completion and
    writes result.json into the worktree's dist\sandbox-out\ (mounted
    RW into the Sandbox).

.PARAMETER WorktreeRoot
    Repo / worktree root. Default: this script's grandparent dir.

.PARAMETER TimeoutSeconds
    How long to poll for result.json before declaring the test timed
    out. Default 600s (10 min) -- generous because Sandbox boot + UAC
    click + install steps can take a couple of minutes total.
#>
[CmdletBinding()]
param(
    [string]$WorktreeRoot,
    [int]$TimeoutSeconds = 600
)

$ErrorActionPreference = 'Stop'

if (-not $WorktreeRoot) {
    $WorktreeRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
}

function Die { param([string]$M) Write-Host "[host] $M" -ForegroundColor Red; exit 1 }
function Log { param([string]$M) Write-Host "[host] $M" -ForegroundColor Cyan }

# 1. Sanity-check artifacts.
$stage = Join-Path $WorktreeRoot 'dist\windows-amd64'
foreach ($e in @('waired.exe','waired-agent.exe','waired-tray.exe')) {
    $p = Join-Path $stage $e
    if (-not (Test-Path -LiteralPath $p)) { Die "missing $p (run: make dist-windows-installer)" }
}
$smoke = Join-Path $WorktreeRoot 'packaging\windows\sandbox-smoke.ps1'
if (-not (Test-Path -LiteralPath $smoke)) { Die "missing $smoke" }

# 2. WindowsSandbox.exe present?
$wsb = Get-Command -Name 'WindowsSandbox.exe' -ErrorAction SilentlyContinue
if (-not $wsb) {
    $wsb = 'C:\Windows\System32\WindowsSandbox.exe'
    if (-not (Test-Path -LiteralPath $wsb)) {
        Die "WindowsSandbox.exe not found -- enable 'Containers-DisposableClientVM' feature first"
    }
}
Log "worktree     : $WorktreeRoot"
$wsbPathDisp = if ($wsb -is [string]) { $wsb } else { $wsb.Source }
Log "WindowsSandbox: $wsbPathDisp"

# 3. Prepare the writable output directory and clear stale state.
$outDir   = Join-Path $WorktreeRoot 'dist\sandbox-out'
New-Item -ItemType Directory -Path $outDir -Force | Out-Null
$resultJson = Join-Path $outDir 'result.json'
$logTxt     = Join-Path $outDir 'sandbox.log'
Remove-Item -LiteralPath $resultJson, $logTxt -ErrorAction SilentlyContinue
Log "output dir   : $outDir"

# The Sandbox-side smoke script reads the staged binaries directly
# from C:\waired-host\dist\windows-amd64\ (the worktree mount); it
# does NOT exercise install.ps1's URL fetch path (file:// IWR is
# rejected by PowerShell 7+'s HttpClient, and standing up a real
# http server inside the Sandbox just to test the IWR path is more
# complexity than the test is worth -- the URL fetch path is
# exercised end-to-end by CI on actual release tags). The Sandbox
# test's unique value is the tray's HKCU\...\Run autostart write
# in a real interactive user session.

# 4. Build the .wsb config dynamically (absolute host paths embedded).
#    Windows Sandbox: docs at
#    https://learn.microsoft.com/en-us/windows/security/application-security/application-isolation/windows-sandbox/windows-sandbox-configure-using-wsb-file
$wsbPath = Join-Path $outDir 'sandbox-smoke.wsb'
$wsbXml = @"
<Configuration>
  <Networking>Default</Networking>
  <MappedFolders>
    <MappedFolder>
      <HostFolder>$WorktreeRoot</HostFolder>
      <SandboxFolder>C:\waired-host</SandboxFolder>
      <ReadOnly>true</ReadOnly>
    </MappedFolder>
    <MappedFolder>
      <HostFolder>$outDir</HostFolder>
      <SandboxFolder>C:\waired-out</SandboxFolder>
      <ReadOnly>false</ReadOnly>
    </MappedFolder>
  </MappedFolders>
  <LogonCommand>
    <Command>powershell -NoProfile -ExecutionPolicy Bypass -File C:\waired-host\packaging\windows\sandbox-smoke.ps1</Command>
  </LogonCommand>
</Configuration>
"@
Set-Content -LiteralPath $wsbPath -Value $wsbXml -Encoding UTF8
Log "wsb config   : $wsbPath"

# 5. Launch Sandbox. Don't -Wait: the Sandbox process stays alive until
#    the user closes it. We poll for result.json instead.
Log "starting Sandbox..."
Start-Process -FilePath $wsbPathDisp -ArgumentList $wsbPath

# 6. Poll for the result file. Print progress lines every 15s so the
#    caller knows we haven't hung.
$deadline = (Get-Date).AddSeconds($TimeoutSeconds)
$lastTick = Get-Date
$lastLogSize = 0
while ((Get-Date) -lt $deadline) {
    if (Test-Path -LiteralPath $resultJson) {
        Log "result.json appeared at $resultJson"
        break
    }
    # Stream the sandbox.log tail as it grows so the user can watch
    # progress inside the host terminal too.
    if (Test-Path -LiteralPath $logTxt) {
        $sz = (Get-Item -LiteralPath $logTxt).Length
        if ($sz -gt $lastLogSize) {
            $new = Get-Content -LiteralPath $logTxt -Tail 10
            foreach ($l in $new) { Write-Host "  [sandbox] $l" -ForegroundColor DarkGray }
            $lastLogSize = $sz
        }
    }
    if (((Get-Date) - $lastTick).TotalSeconds -ge 15) {
        Log "waiting for result.json ... (UAC prompt is interactive: click Yes inside the Sandbox)"
        $lastTick = Get-Date
    }
    Start-Sleep -Milliseconds 1000
}

if (-not (Test-Path -LiteralPath $resultJson)) {
    Die "timed out waiting for result.json after $TimeoutSeconds s"
}

$res = Get-Content -LiteralPath $resultJson -Raw | ConvertFrom-Json
Write-Host ''
Write-Host '=== Sandbox smoke result ==='
$res | ConvertTo-Json -Depth 5 | Write-Host
Write-Host ''
if ($res.ok) {
    Log "PASS"
    exit 0
} else {
    Log "FAIL"
    exit 1
}
