#!/usr/bin/env pwsh
<#
.SYNOPSIS
  Wait until Windows is actually refusing one of our programs, then run the
  control matrix while the refusal is still live.

.DESCRIPTION
  waired-ai/waired-agent#1191 hypothesis H3 asks what about a file's content
  decides the verdict. The matrix that answers it is only readable inside a
  window where the host is still refusing something -- outside one, every axis
  comes back allowed and the pass means nothing
  (docs/knowledges/20260904/0310-a-permissive-window-makes-every-test-pass.md).

  Those windows open and close on their own and cannot be induced. One was
  measured at 91 minutes, 234 refusals of a single file; a host has also gone
  142 hours refusing one hash. They are long enough to catch and far too
  irregular to sit and wait for by hand.

  So: poll the CodeIntegrity log, and the moment it refuses a file matching
  -Match, run scripts/dev/sac-control-matrix.ps1 with that exact file as the
  -RefusedControl. One pass, then stop.

  WHAT THIS DOES NOT DO. It does not make a window happen, it does not keep one
  open, and it cannot tell you the pass will be readable -- the refusal can lift
  between the trigger and the matrix's own control launch, which the matrix
  reports for itself. It only removes the waiting.

.PARAMETER Match
  Regular expression against the refused file's name. Default: our own
  programs. The matrix's own controls are excluded no matter what is passed
  here -- once the policy refuses those, they would retrigger the watch
  forever.

.PARAMETER Axes
  Passed through to the matrix, for running a subset of the axes.

.PARAMETER BinDir
  Passed through to the matrix. Read its -BinDir help before using it: on a
  host with no Go toolchain the axis binaries have to be staged in advance,
  and a binary that has been sitting on disk with real-time protection on is
  not necessarily the first sighting the method assumes. Where Go is present,
  leave this unset and the matrix builds at trigger time, which is the
  version of the method that holds.

.PARAMETER Since
  Also consider refusals at or after this time, not only ones that arrive
  while this is running. Use it when you started the watch because Windows had
  already told you something was blocked.

.PARAMETER PollSeconds
  How often to look. Default 30. A window measured in tens of minutes does not
  need better.

.PARAMETER TimeoutMinutes
  Give up after this long. Default 0 = wait indefinitely.

.EXAMPLE
  # on a host with Go: build at trigger time, which is the honest form
  powershell -NoProfile -File scripts\dev\sac-window-watch.ps1 -OutDir C:\sac-h3

.EXAMPLE
  # on a host without Go: axes cross-built elsewhere, with the caveat above
  powershell -NoProfile -File scripts\dev\sac-window-watch.ps1 `
      -BinDir C:\sac-axes -OutDir C:\sac-h3
#>
[CmdletBinding()]
param(
    [string]$Match = 'waired',
    [string]$BinDir,
    [string[]]$Axes,
    [string]$OutDir,
    [int]$PollSeconds = 30,
    [int]$TimeoutMinutes = 0,
    [string]$Since
)

$ErrorActionPreference = 'Continue'
$scriptDir = Split-Path -Parent $PSCommandPath
$matrix = Join-Path $scriptDir 'sac-control-matrix.ps1'
$CiLog = 'Microsoft-Windows-CodeIntegrity/Operational'

function Write-Note([string]$Text) { Write-Host "[sac-watch] $((Get-Date).ToUniversalTime().ToString('HH:mm:ss')) $Text" }

if (-not (Test-Path -LiteralPath $matrix)) { Write-Note "sac-control-matrix.ps1 not found next to this script"; exit 1 }
if (-not $OutDir) { $OutDir = Join-Path $env:TEMP ('sac-window-' + (Get-Date).ToString('yyyyMMddHHmmss')) }

# The matrix launches its own unsigned binaries. Once the policy starts
# refusing those, they are refusals matching almost any pattern -- and a watch
# that triggers on its own output never stops.
$selfPattern = '(?i)control-|refused-control|waired-sac-control'

$sacState = try { (Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\CI\Policy' -ErrorAction Stop).VerifiedAndReputablePolicyState } catch { $null }
Write-Note ("VerifiedAndReputablePolicyState = {0}" -f $(if ($null -eq $sacState) { '(absent)' } else { $sacState }))
if ($sacState -ne 1) {
    # Not fatal: an application-control policy can refuse things without Smart
    # App Control being the thing enforcing. But it is worth saying, because a
    # watch on a host that enforces nothing will wait forever.
    Write-Note 'this host is not enforcing Smart App Control. Nothing may ever refuse anything here.'
}

# A person starts this after seeing Windows' notification, by which time the
# refusal that prompted it is minutes old. -Since lets the window that is
# already open count.
$start = (Get-Date).AddSeconds(-1)
if ($Since) {
    try { $start = [datetime]::Parse($Since) } catch { Write-Note "could not read -Since '$Since'; watching from now" }
}
$deadline = if ($TimeoutMinutes -gt 0) { (Get-Date).AddMinutes($TimeoutMinutes) } else { $null }
Write-Note "watching $CiLog for a refusal matching '$Match' (poll ${PollSeconds}s)"

$trigger = $null
while (-not $trigger) {
    if ($deadline -and (Get-Date) -gt $deadline) {
        Write-Note "no refusal in $TimeoutMinutes minutes. Nothing was measured -- that is not the same as 'nothing is refused here'."
        exit 2
    }
    $ev = @()
    try { $ev = @(Get-WinEvent -FilterHashtable @{ LogName = $CiLog; Id = 3077, 3033; StartTime = $start } -ErrorAction Stop) } catch { }
    foreach ($e in ($ev | Sort-Object TimeCreated)) {
        $xml = [xml]$e.ToXml(); $d = @{}
        foreach ($n in $xml.Event.EventData.Data) { if ($n.Name) { $d[[string]$n.Name] = [string]$n.'#text' } }
        $f = $d['File Name']; if (-not $f) { $f = $d['FileNameBuffer'] }; if (-not $f) { $f = $d['FileName'] }
        if (-not $f) { continue }
        $leaf = Split-Path -Leaf $f
        if ($leaf -match $selfPattern) { continue }
        if ($leaf -notmatch $Match) { continue }
        # The NT device path is not openable; the matrix needs a real one.
        $dos = $f -replace '^\\Device\\HarddiskVolume\d+', $env:SystemDrive
        $trigger = [pscustomobject]@{ Utc = $e.TimeCreated.ToUniversalTime(); Id = $e.Id; Leaf = $leaf; NtPath = $f; Path = $dos }
        break
    }
    if (-not $trigger) { Start-Sleep -Seconds $PollSeconds }
}

Write-Note ("REFUSAL: {0} id={1} at {2}" -f $trigger.Leaf, $trigger.Id, $trigger.Utc.ToString('o'))
Write-Note ("  event path: {0}" -f $trigger.NtPath)

# A refused file is the control only if it can still be found. The device-path
# rewrite above assumes the system drive; when that guess is wrong, say so
# rather than handing the matrix a path it will reject.
if (-not (Test-Path -LiteralPath $trigger.Path)) {
    Write-Note ("  cannot resolve it on disk as {0}. The matrix needs a real path for -RefusedControl; pass one by hand." -f $trigger.Path)
    exit 3
}
Write-Note ("  using it as the refused control: {0}" -f $trigger.Path)

# NOT $args: that is an automatic variable, and writing to it is the kind of
# thing that works until something in the call path reads the real one.
$matrixArgs = @('-RefusedControl', $trigger.Path, '-OutDir', $OutDir)
if ($Axes) { $matrixArgs += @('-Axes', ($Axes -join ',')) }
if ($BinDir) {
    $matrixArgs += @('-BinDir', $BinDir)
    Write-Note 'axes come from -BinDir: they were built before the window, so "first sighting" is weaker for them than the method assumes. Say so when reporting the pass.'
} else {
    Write-Note 'axes are built now, inside the window -- the form of the method that holds.'
}
& powershell.exe -NoProfile -ExecutionPolicy Bypass -File $matrix @matrixArgs | Out-Host
Write-Note "matrix finished; output in $OutDir"
