#Requires -Version 5.1
<#
.SYNOPSIS
    Guard for install.ps1's Extract-Zip: putting new binaries into
    %ProgramFiles%\Waired must never leave the directory unusable (#819).

.DESCRIPTION
    `waired update` on Windows removed waired-agent.exe and waired-tray.exe,
    failed on waired.exe, and left a host whose service had no executable to
    start. The cause was `Expand-Archive -Force` straight onto the install
    directory: it clears the destination entries BEFORE it writes any of
    them, so one file it cannot write takes the whole directory with it. On
    the update path that file is always present — `waired update` is running
    %ProgramFiles%\Waired\waired.exe, and Windows will not let a mapped image
    be overwritten.

    These cases drive the real Extract-Zip against temp directories: no
    service, no mirror, no download, nothing installed. The functions are
    lifted out of install.ps1 by parsing it, so what runs is the shipped body
    rather than a copy that can drift.

    Two runners, on purpose. installtest-pwsh.ps1 calls this on the Linux
    matrix, where the held-open case cannot run — Unix has no mandatory
    locking — and is skipped. installtest-windows.ps1 calls it on a real
    Windows runner, where it does.

    What is NOT covered here, on any OS: the rename-aside branch for a file
    that is a RUNNING IMAGE. That combination — renameable but not
    replaceable — comes from the image section the memory manager holds, and
    no FileShare mode reproduces it (FileShare.None blocks the rename too;
    'Read, Delete' lets the replace succeed outright). It is verified on real
    hardware. These cases pin the floor beneath it: whatever happens, the
    other files are updated and nothing is deleted.

.PARAMETER InstallPs1
    Path to the install.ps1 under test. Defaults to the one in this worktree.
#>
[CmdletBinding()]
param([string]$InstallPs1)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if (-not $InstallPs1) {
    # Both callers pass -InstallPs1; this is for running the file by hand.
    # Reported rather than left to blow up on a null: `git rev-parse` declines
    # a repository it considers dubiously owned, and ".Trim() on null" is not
    # a useful way to learn that.
    $root = & git -C $PSScriptRoot rev-parse --show-toplevel 2>$null
    if (-not $root) {
        Write-Error "could not locate the repository root from $PSScriptRoot; pass -InstallPs1 <path>"
        exit 1
    }
    $InstallPs1 = Join-Path $root.Trim() 'packaging/install/install.ps1'
}
if (-not (Test-Path -LiteralPath $InstallPs1)) {
    Write-Error "install.ps1 not found: $InstallPs1"
    exit 1
}

# --- lift the functions under test out of install.ps1 -----------------------
$ast  = [System.Management.Automation.Language.Parser]::ParseFile($InstallPs1, [ref]$null, [ref]$null)
$defs = $ast.FindAll({ param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $true)
foreach ($fn in @('Extract-Zip', 'Move-IntoInstallDir', 'Clear-DisplacedFiles')) {
    $d = $defs | Where-Object { $_.Name -eq $fn } | Select-Object -First 1
    if (-not $d) { Write-Error "install.ps1 has no function $fn"; exit 1 }
    . ([scriptblock]::Create($d.Extent.Text))
}

# The names those functions close over. $DryRun stays false: unlike the rest
# of the installer matrix, these cases are about what lands on disk.
$StagingDirName  = '.waired-staging'
$DisplacedMarker = '.displaced-'
$DryRun = $false
function Common-Run { param([string]$D, [scriptblock]$B) & $B }
function Common-Log { param([string]$M) Write-Host "[swap]   $M" -ForegroundColor DarkGray }
# The real one logs and exits; throwing is the analogue that lets a case read
# the message instead of taking the run down with it.
function Common-Die { param([string]$M) throw $M }

# --- harness ----------------------------------------------------------------
$script:Pass = 0; $script:Fail = 0
function SwapNote { param([string]$M) Write-Host "[swap] $M" -ForegroundColor Cyan }
function SwapOk   { param([string]$M) Write-Host "[swap]  ok  $M" -ForegroundColor Green; $script:Pass++ }
function SwapBad  { param([string]$M) Write-Host "[swap] FAIL $M" -ForegroundColor Red;   $script:Fail++ }
function SwapEq   { param([string]$L, $Got, $Want)
    if ($Got -eq $Want) { SwapOk $L } else { SwapBad "$L -- got '$Got', want '$Want'" }
}

$Work = Join-Path ([IO.Path]::GetTempPath()) ("waired-swaptest-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $Work -Force | Out-Null

$Names = @('waired.exe', 'waired-agent.exe', 'waired-tray.exe', 'VERSION')

# New-SwapFixture builds an install dir of OLD-* files and a zip of NEW-*
# files for the same names.
function New-SwapFixture {
    param([string]$Name)
    $root = Join-Path $Work $Name
    $dest = Join-Path $root 'Waired'
    $src  = Join-Path $root 'src'
    New-Item -ItemType Directory -Path $dest, $src -Force | Out-Null
    foreach ($f in $Names) {
        Set-Content -LiteralPath (Join-Path $dest $f) -Value "OLD-$f" -NoNewline
        Set-Content -LiteralPath (Join-Path $src  $f) -Value "NEW-$f" -NoNewline
    }
    $zip = Join-Path $root 'new.zip'
    # Out-Null: on some hosts Compress-Archive's first call emits the
    # compression assembly it loads, which would land in this function's
    # output alongside the object below.
    Compress-Archive -Path (Join-Path $src '*') -DestinationPath $zip -Force | Out-Null
    [pscustomobject]@{ Dest = $dest; Zip = $zip }
}
function Get-SwapContent { param([string]$Dir, [string]$Name)
    $p = Join-Path $Dir $Name
    if (Test-Path -LiteralPath $p) { Get-Content -LiteralPath $p -Raw } else { $null }
}
function Get-SwapDisplaced { param([string]$Dir)
    @(Get-ChildItem -LiteralPath $Dir -Filter "*$DisplacedMarker*" -File -ErrorAction SilentlyContinue)
}

try {
    SwapNote "install.ps1 = $InstallPs1"

    # (a) The ordinary case: every file replaced, nothing left behind.
    $fx = New-SwapFixture 'happy'
    $InstallDir = $fx.Dest
    Extract-Zip -ZipPath $fx.Zip
    foreach ($f in $Names) { SwapEq "$f is replaced" (Get-SwapContent $fx.Dest $f) "NEW-$f" }
    if (Test-Path -LiteralPath (Join-Path $fx.Dest $StagingDirName)) {
        SwapBad 'the staging directory was left behind'
    } else { SwapOk 'the staging directory is cleaned up' }
    # @() at the call site, not only inside the function: PowerShell unwraps a
    # returned empty array to $null, and Set-StrictMode turns .Count on $null
    # into an error -- so the assertion would break precisely when it passes.
    if (@(Get-SwapDisplaced $fx.Dest).Count -ne 0) {
        SwapBad 'an unobstructed replace should displace nothing'
    } else { SwapOk 'nothing is displaced when nothing is in use' }

    # (b) A failed expand must not touch the install dir. A corrupt archive is
    #     the portable way to make the expand fail.
    #
    #     Note this case passes against the previous implementation too --
    #     Expand-Archive validates the archive before it clears anything. It is
    #     a pin on the staging boundary, not the #819 regression test. That is
    #     (c).
    $fx = New-SwapFixture 'corrupt'
    $InstallDir = $fx.Dest
    Set-Content -LiteralPath $fx.Zip -Value 'not a zip at all' -NoNewline
    $threw = $false
    try { Extract-Zip -ZipPath $fx.Zip } catch { $threw = $true }
    if ($threw) { SwapOk 'a bad archive fails loudly' } else { SwapBad 'a bad archive was swallowed' }
    foreach ($f in $Names) { SwapEq "$f survives a failed expand" (Get-SwapContent $fx.Dest $f) "OLD-$f" }

    # (c) #819 itself. One destination cannot be written; the others must be
    #     updated rather than deleted, and the failure must name the file.
    #
    #     Against the previous implementation this leaves ONLY the held-open
    #     file in the directory and deletes the rest -- measured, and exactly
    #     the state the reported host was found in.
    if ([System.Environment]::OSVersion.Platform -eq [System.PlatformID]::Win32NT) {
        $fx = New-SwapFixture 'held-open'
        $InstallDir = $fx.Dest
        $held = [System.IO.File]::Open((Join-Path $fx.Dest 'waired.exe'), 'Open', 'Read', 'None')
        try {
            $msg = ''
            try { Extract-Zip -ZipPath $fx.Zip } catch { $msg = "$($_.Exception.Message)" }
            if ($msg -match 'waired\.exe') { SwapOk 'an unwritable file fails by name' }
            else { SwapBad "the failure did not name the file -- '$msg'" }
            foreach ($f in @('waired-agent.exe', 'waired-tray.exe', 'VERSION')) {
                SwapEq "$f is updated even so" (Get-SwapContent $fx.Dest $f) "NEW-$f"
            }
        } finally { $held.Close() }
        # Read it only after the handle is gone: FileShare.None locks out this
        # script's own read too.
        SwapEq 'the unwritable file keeps its old bytes' (Get-SwapContent $fx.Dest 'waired.exe') 'OLD-waired.exe'
    } else {
        SwapNote 'skipping the held-open case (it needs Windows file locking)'
    }

    # (d) Whatever a previous run had to rename aside is swept by the next.
    $fx = New-SwapFixture 'sweep'
    $InstallDir = $fx.Dest
    Set-Content -LiteralPath (Join-Path $fx.Dest "waired.exe${DisplacedMarker}deadbeef") -Value 'stale' -NoNewline
    Extract-Zip -ZipPath $fx.Zip
    if (@(Get-SwapDisplaced $fx.Dest).Count -eq 0) {
        SwapOk 'a later run sweeps what an earlier one displaced'
    } else { SwapBad 'the displaced file was not swept' }
} finally {
    Remove-Item -LiteralPath $Work -Recurse -Force -ErrorAction SilentlyContinue
}

SwapNote "summary: $script:Pass passed, $script:Fail failed"
if ($script:Fail -gt 0) { exit 1 }
