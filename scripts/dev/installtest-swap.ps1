#Requires -Version 5.1
<#
.SYNOPSIS
    Guard for install.ps1's file swap: putting new binaries into
    %ProgramFiles%\Waired must never leave the directory unusable (#819),
    and an update that cannot run them must not happen at all (#1087).

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

    Cases (e) onward are waired-agent#1087: an update whose new programs
    Windows refuses to run stopped the service, swapped them in anyway, and
    left the host with nothing runnable. They drive the real
    Test-StagedBinaries, Backup-InstallDirFiles and Invoke-PendingRollback.
    A program that will not start is portable — a file that is not a valid
    executable throws on both OSes (Windows: not a valid Win32 application;
    Linux: Exec format error / "cannot run a document") — and one that will
    is a copy of an OS program the runner already has.

    What is NOT covered here: that a failure actually REACHES
    Invoke-PendingRollback. That wiring lives in Common-Die and the
    script-level trap, and Common-Die ends in `exit`, which would take this
    runner down with it. Case (j) asserts the wiring by reading the shipped
    file, and installtest-windows.ps1 proves it end to end against a real
    SCM service.

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
foreach ($fn in @('Extract-Zip', 'Expand-ToStaging', 'Move-StagedIntoInstallDir', 'Remove-StagingDir',
                  'Set-InstallDirFile', 'Move-IntoInstallDir', 'Clear-DisplacedFiles', 'Clear-RollbackDir',
                  'Get-StagedBinaryChecks', 'Test-BinaryRuns', 'Test-StagedBinaries',
                  'Backup-InstallDirFiles', 'Invoke-PendingRollback', 'Clear-RollbackArm')) {
    $d = $defs | Where-Object { $_.Name -eq $fn } | Select-Object -First 1
    if (-not $d) { Write-Error "install.ps1 has no function $fn"; exit 1 }
    . ([scriptblock]::Create($d.Extent.Text))
}

# The names those functions close over. $DryRun stays false: unlike the rest
# of the installer matrix, these cases are about what lands on disk.
$StagingDirName  = '.waired-staging'
$DisplacedMarker = '.displaced-'
$RollbackDirName = '.waired-rollback'
$DryRun  = $false
$NoTray  = $false
$BaseUrl = 'https://github.com/waired-ai/waired-agent/releases'
$ServiceName = 'waired-agent'
# Set-StrictMode turns a read of an unset variable into an error, and the
# armed rollback is read before it is ever written.
$script:RollbackPlan = $null
function Common-Run  { param([string]$D, [scriptblock]$B) & $B }
function Common-Log  { param([string]$M) Write-Host "[swap]   $M" -ForegroundColor DarkGray }
function Common-Warn { param([string]$M) Write-Host "[swap]   $M" -ForegroundColor DarkGray }
# The real one logs, rolls back an armed swap and exits; throwing is the
# analogue that lets a case read the message instead of taking the run down
# with it. It deliberately does NOT call Invoke-PendingRollback -- case (j)
# asserts that wiring against the shipped file instead of restating it here,
# where a copy would go on passing after the real one lost the call.
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

# A program this runner can actually start. Copying one the OS already ships
# is the only portable way to get a real, launchable image into a staging
# directory -- and the point of these cases is that a real one is launched.
# Its counterpart, a file that will not start, is what New-SwapFixture
# already writes: plain text under an .exe name.
$OnWindows       = [System.Environment]::OSVersion.Platform -eq [System.PlatformID]::Win32NT
$RunnableProgram = if ($OnWindows) { Join-Path $env:SystemRoot 'System32\cmd.exe' } else { '/bin/sh' }
$ArgsExitZero    = if ($OnWindows) { @('/c', 'exit', '0') } else { @('-c', 'exit 0') }
$ArgsExitThree   = if ($OnWindows) { @('/c', 'exit', '3') } else { @('-c', 'exit 3') }
# The shipped table, kept so the cases that swap it can put it back.
$ShippedChecks   = ($defs | Where-Object { $_.Name -eq 'Get-StagedBinaryChecks' } | Select-Object -First 1).Extent.Text
function Copy-RunnableInto { param([string]$Dir, [string]$Name)
    Copy-Item -LiteralPath $RunnableProgram -Destination (Join-Path $Dir $Name) -Force
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

    # (d) Whatever a previous run had to rename aside -- or copied aside and
    #     never came back for -- is swept by the next.
    $fx = New-SwapFixture 'sweep'
    $InstallDir = $fx.Dest
    Set-Content -LiteralPath (Join-Path $fx.Dest "waired.exe${DisplacedMarker}deadbeef") -Value 'stale' -NoNewline
    $stale = Join-Path $fx.Dest $RollbackDirName
    New-Item -ItemType Directory -Path $stale -Force | Out-Null
    Set-Content -LiteralPath (Join-Path $stale 'waired.exe') -Value 'stale' -NoNewline
    Extract-Zip -ZipPath $fx.Zip
    if (@(Get-SwapDisplaced $fx.Dest).Count -eq 0) {
        SwapOk 'a later run sweeps what an earlier one displaced'
    } else { SwapBad 'the displaced file was not swept' }
    if (-not (Test-Path -LiteralPath $stale)) {
        SwapOk 'a later run sweeps the copies an unfinished swap left'
    } else { SwapBad 'the rollback copies were not swept' }

    # ---- waired-agent#1087 -------------------------------------------------
    # An update whose new programs Windows refuses to run stopped the
    # service, swapped them in anyway, and left the host with nothing
    # runnable and no recovery named.

    # (e) A staged program that will not start stops the run, by name, with
    #     the install dir exactly as it was. Every file New-SwapFixture
    #     stages is text under an .exe name, which is precisely a file
    #     neither OS will start.
    $fx = New-SwapFixture 'refused'
    $InstallDir = $fx.Dest
    $staging = Expand-ToStaging -ZipPath $fx.Zip
    $msg = ''
    try {
        Test-StagedBinaries -Staging $staging -UnchangedNote 'nothing was changed'
    } catch { $msg = "$($_.Exception.Message)" }
    if ($msg -match 'waired\.exe') {
        SwapOk 'a program that will not run stops the run, and names it'
    } else { SwapBad "the refusal did not name the program -- '$msg'" }
    foreach ($f in $Names) {
        SwapEq "$f is untouched when the check refuses" (Get-SwapContent $fx.Dest $f) "OLD-$f"
    }
    if (-not (Test-Path -LiteralPath $staging)) {
        SwapOk 'the staging directory is cleared before the run stops'
    } else { SwapBad 'staging survived a refused check' }

    # (f) Programs that DO start pass, a refused non-fatal one only warns,
    #     and a fatal one that starts but exits non-zero is still a refusal.
    #
    #     The table is swapped, not the subject: the shipped one asks
    #     waired.exe for `version --json`, which only the real binary
    #     answers. Test-StagedBinaries and Test-BinaryRuns are the real
    #     ones, and what the shipped table itself says is case (i).
    $fx = New-SwapFixture 'runs'
    $InstallDir = $fx.Dest
    $staging = Expand-ToStaging -ZipPath $fx.Zip
    Copy-RunnableInto -Dir $staging -Name 'ok.exe'
    function Get-StagedBinaryChecks {
        return @(
            @{ Name = 'ok.exe';         Arguments = $ArgsExitZero; RequireZeroExit = $true;  Fatal = $true  },
            @{ Name = 'waired-tray.exe'; Arguments = @('-h');      RequireZeroExit = $false; Fatal = $false }
        )
    }
    $msg = ''
    try {
        Test-StagedBinaries -Staging $staging -UnchangedNote 'nothing was changed'
    } catch { $msg = "$($_.Exception.Message)" }
    if (-not $msg) {
        SwapOk 'a program that starts passes, and a refused non-fatal one only warns'
    } else { SwapBad "a runnable program was reported as refused -- '$msg'" }

    function Get-StagedBinaryChecks {
        return @(
            @{ Name = 'ok.exe'; Arguments = $ArgsExitThree; RequireZeroExit = $true; Fatal = $true }
        )
    }
    $msg = ''
    try {
        Test-StagedBinaries -Staging $staging -UnchangedNote 'nothing was changed'
    } catch { $msg = "$($_.Exception.Message)" }
    if ($msg -match 'ok\.exe') {
        SwapOk 'a program that starts but cannot answer is a refusal too'
    } else { SwapBad "a non-zero exit was accepted -- '$msg'" }
    . ([scriptblock]::Create($ShippedChecks))
    Remove-StagingDir -Staging $staging

    # (g) A failure after the swap puts every previous byte back, exactly
    #     once, and takes the copies with it.
    $fx = New-SwapFixture 'rollback'
    $InstallDir = $fx.Dest
    $staging = Expand-ToStaging -ZipPath $fx.Zip
    Backup-InstallDirFiles -Staging $staging -HadService $false -Version '0.0.0-before'
    Move-StagedIntoInstallDir -Staging $staging
    foreach ($f in $Names) {
        SwapEq "$f is the new one before the rollback" (Get-SwapContent $fx.Dest $f) "NEW-$f"
    }
    Invoke-PendingRollback
    foreach ($f in $Names) {
        SwapEq "$f is the previous one after the rollback" (Get-SwapContent $fx.Dest $f) "OLD-$f"
    }
    if (-not $script:RollbackPlan) { SwapOk 'the rollback disarms itself' }
    else { SwapBad 'the rollback stayed armed' }
    if (-not (Test-Path -LiteralPath (Join-Path $fx.Dest $RollbackDirName))) {
        SwapOk 'the copies are removed once they are back in place'
    } else { SwapBad 'the rollback copies were left behind' }
    Invoke-PendingRollback
    foreach ($f in $Names) {
        SwapEq "$f survives a second rollback call" (Get-SwapContent $fx.Dest $f) "OLD-$f"
    }
    Remove-StagingDir -Staging $staging

    # (h) Once the new programs are serving, the way back is deliberately
    #     dropped -- a later failure must not undo a finished update.
    $fx = New-SwapFixture 'disarm'
    $InstallDir = $fx.Dest
    $staging = Expand-ToStaging -ZipPath $fx.Zip
    Backup-InstallDirFiles -Staging $staging -HadService $false -Version '0.0.0-before'
    Move-StagedIntoInstallDir -Staging $staging
    Clear-RollbackArm
    Invoke-PendingRollback
    foreach ($f in $Names) {
        SwapEq "$f stays new once the swap is disarmed" (Get-SwapContent $fx.Dest $f) "NEW-$f"
    }
    if (-not (Test-Path -LiteralPath (Join-Path $fx.Dest $RollbackDirName))) {
        SwapOk 'disarming removes the copies'
    } else { SwapBad 'disarming left the copies behind' }
    Remove-StagingDir -Staging $staging

    # (i) What the SHIPPED table asks. The daemon and the CLI are both fatal
    #     (owner ruling, 2026-08-29); the app is not. And nothing asks
    #     waired-agent.exe a bare word: its flag parsing stops at the first
    #     non-flag token, so `waired-agent.exe version` starts the daemon in
    #     the foreground and never returns.
    $checks = @(Get-StagedBinaryChecks)
    SwapEq 'the shipped list covers three programs' $checks.Count 3
    foreach ($want in @(
        @{ Name = 'waired.exe'; Fatal = $true },
        @{ Name = 'waired-agent.exe'; Fatal = $true },
        @{ Name = 'waired-tray.exe'; Fatal = $false })) {
        $got = $checks | Where-Object { $_.Name -eq $want.Name } | Select-Object -First 1
        if (-not $got) { SwapBad "the shipped list has no entry for $($want.Name)"; continue }
        SwapEq "$($want.Name) is $(if ($want.Fatal) { 'fatal' } else { 'not fatal' })" $got.Fatal $want.Fatal
    }
    $agent = $checks | Where-Object { $_.Name -eq 'waired-agent.exe' } | Select-Object -First 1
    if ($agent -and @($agent.Arguments | Where-Object { $_ -notlike '-*' }).Count -eq 0) {
        SwapOk 'waired-agent.exe is never asked a bare word'
    } else { SwapBad 'waired-agent.exe is asked a bare word, which starts the daemon' }

    # (j) The wiring, read off the shipped file. Common-Die ends in `exit`,
    #     so a case that made it fire would take this runner down with it --
    #     and a copy of the wiring here would go on passing after the real
    #     one lost it. installtest-windows.ps1 proves the behaviour end to
    #     end against a real SCM service; this pins the calls.
    #     Every one of these reads CALLS out of the syntax tree rather than
    #     matching text: half of them name the function they are about in a
    #     comment one line above the call, and a text match would go on
    #     passing off the comment alone after the call was gone.
    function Get-SwapCalls { param($Node)
        @($Node.FindAll({ param($n) $n -is [System.Management.Automation.Language.CommandAst] }, $true))
    }
    function Get-SwapCall { param($Node, [string]$Name)
        Get-SwapCalls $Node | Where-Object { $_.GetCommandName() -eq $Name } | Select-Object -First 1
    }
    function Get-SwapFn { param([string]$Name)
        $defs | Where-Object { $_.Name -eq $Name } | Select-Object -First 1
    }
    $traps = @($ast.FindAll({ param($n) $n -is [System.Management.Automation.Language.TrapStatementAst] }, $true))
    if (@($traps | Where-Object { Get-SwapCall $_ 'Invoke-PendingRollback' }).Count -gt 0) {
        SwapOk 'the script trap rolls an armed swap back'
    } else { SwapBad 'no trap calls Invoke-PendingRollback' }
    if (Get-SwapCall (Get-SwapFn 'Common-Die') 'Invoke-PendingRollback') {
        SwapOk 'Common-Die rolls an armed swap back'
    } else { SwapBad 'Common-Die does not call Invoke-PendingRollback' }
    foreach ($pair in @(
        @{ Fn = 'Invoke-WairedUpdateSwap'; Stop = 'Stop-ServiceForUpdate' },
        @{ Fn = 'Invoke-InstallSteps';     Stop = 'Stop-ExistingService'  })) {
        $fn    = Get-SwapFn $pair.Fn
        $check = Get-SwapCall $fn 'Test-StagedBinaries'
        $stop  = Get-SwapCall $fn $pair.Stop
        if ($check -and $stop -and $check.Extent.StartOffset -lt $stop.Extent.StartOffset) {
            SwapOk "$($pair.Fn) checks the new programs before $($pair.Stop)"
        } else { SwapBad "$($pair.Fn) stops the service before it knows the new programs run" }
    }
    $swapFn = Get-SwapFn 'Invoke-WairedUpdateSwap'
    foreach ($call in @('Backup-InstallDirFiles', 'Clear-RollbackArm')) {
        if (Get-SwapCall $swapFn $call) { SwapOk "Invoke-WairedUpdateSwap calls $call" }
        else { SwapBad "Invoke-WairedUpdateSwap does not call $call" }
    }
} finally {
    Remove-Item -LiteralPath $Work -Recurse -Force -ErrorAction SilentlyContinue
}

SwapNote "summary: $script:Pass passed, $script:Fail failed"
if ($script:Fail -gt 0) { exit 1 }
