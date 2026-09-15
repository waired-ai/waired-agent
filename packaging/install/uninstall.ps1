#Requires -Version 5.1
<#
.SYNOPSIS
    Waired uninstaller for Windows.

.DESCRIPTION
    Counterpart to install.ps1 (the `iwr ... | iex` one-liner installer).
    Two tiers, matching install.sh's apt remove / purge split:

      default   removes the Waired binaries + service registration, the
                machine-PATH entry, the tray autostart, Start Menu shortcuts,
                and the per-user Claude Code / coding-agent integration (managed
                settings, ~/.claude skills, opencode/openclaw plugins), but
                KEEPS local state (%ProgramData%\waired: identity, keys, settings).
      -Clean    also deletes state (%ProgramData%\waired and %APPDATA%\waired)
                and Ollama (binary + downloaded models). Destructive and
                irreversible -- guarded by a confirmation (see -Yes).

    Both tiers also best-effort DEREGISTER this device from the Control
    Plane (revoked -- removed from the account's device list and dropped
    from peers). That runs inside `waired-agent.exe uninstall`, which
    self-revokes before tearing the service down; it's best-effort, so an
    offline / CP-unreachable uninstall never blocks (remove the device from
    the web admin instead).

    The privileged removal logic lives in the binaries, not here: this
    script prefers `waired-agent.exe uninstall` (SCM + Event Log + Control
    Plane deregister) and `waired.exe proxy uninstall` (legacy hosts / CA /
    NODE_EXTRA_CA_CERTS), falling back to manual SCM cleanup only when the
    exe is already gone.

    Run it via:
        iwr -useb https://github.com/waired-ai/waired-agent/releases/latest/download/uninstall.ps1 | iex
    The default uninstall works piped. For -Clean, download then run (iex
    strips named parameters):
        iwr -useb .../uninstall.ps1 -OutFile uninstall.ps1; .\uninstall.ps1 -Clean

    If you installed Waired with the GUI installer (WairedSetup-*.exe),
    prefer Settings -> Apps -> Waired -> Uninstall; this script is safe to
    run either way.

.PARAMETER Clean
    Full wipe: also remove %ProgramData%\waired and Ollama (binary + models).

.PARAMETER Yes
    Assume "yes" to the -Clean confirmation (required to -Clean on a
    non-interactive / piped session).

.PARAMETER DryRun
    Show every change without making it. Skips elevation (no UAC prompt).

.PARAMETER Help
    Print help and exit.

.EXAMPLE
    PS> .\uninstall.ps1
    PS> .\uninstall.ps1 -Clean
    PS> .\uninstall.ps1 -DryRun
#>
[CmdletBinding()]
param(
    [switch]$Clean,
    [switch]$Yes,
    [switch]$DryRun,
    [switch]$Help,
    # Mask personal information (home dir, username) in the output -- for
    # screenshots and bug reports. Best-effort. Env form: WAIRED_PII_MASK=1.
    [switch]$MaskPII,
    # Internal: set on the re-elevated self-invoke so the child skips the
    # per-user teardown (it runs in the un-elevated parent, as the invoking
    # user, so HKCU / %APPDATA% / ~/.claude hit the right hive) and knows it
    # runs in a spawned console (transcript + pause-on-exit). waired#754.
    [switch]$FromElevation,
    # Internal: path the elevated child writes its Start-Transcript log to.
    # The un-elevated parent picks a path under its own %TEMP% (readable
    # without elevation) and forwards it. Mirrors install.ps1 (waired#748).
    [string]$LogPath
)

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'

# WAIRED_PII_MASK is the env-var form of -MaskPII; folded both ways so the
# elevated child and the waired.exe teardown helpers inherit the request.
if ($env:WAIRED_PII_MASK) { $MaskPII = $true }
if ($MaskPII) { $env:WAIRED_PII_MASK = '1' }

# -------------------------------------------------------------------
# Configuration (mirrors install.ps1)
# -------------------------------------------------------------------

# Install dir: WAIRED_INSTALL_DIR env > the HKLM registry value install.ps1 /
# the GUI installer recorded (-InstallDir relocations) > the historical
# %ProgramFiles%\Waired default.
$InstallDirRegKey = 'HKLM:\SOFTWARE\Waired'
$InstallDir = $env:WAIRED_INSTALL_DIR
if (-not $InstallDir) {
    try {
        $InstallDir = (Get-ItemProperty -Path $InstallDirRegKey -Name 'InstallDir' -ErrorAction Stop).InstallDir
    } catch { }
}
if (-not $InstallDir) { $InstallDir = Join-Path $env:ProgramFiles 'Waired' }
$ServiceName = 'waired-agent'
# SCM-mode state dir written by install.ps1 / the GUI installer.
$StateDir    = if ($env:WAIRED_STATE_DIR) { $env:WAIRED_STATE_DIR } `
               else { Join-Path $env:ProgramData 'waired' }
# Public mirror base for the elevated self-re-fetch (iex case). Mirrors
# install.ps1's WAIRED_INSTALL_BASE_URL default shape.
$BaseUrl     = if ($env:WAIRED_INSTALL_BASE_URL) { $env:WAIRED_INSTALL_BASE_URL } `
               else { 'https://github.com/waired-ai/waired-agent/releases' }

# Where the elevated child writes its Start-Transcript log so the uninstall
# output survives the spawned console closing (mirror of install.ps1,
# waired#748). Resolved in the un-elevated parent (its %TEMP% stays readable
# without elevation) and forwarded via -LogPath.
#
# One file PER RUN, for the reason install.ps1 documents at its own default
# (#314): a fixed name plus Start-Transcript -Force means the next run destroys
# the failed run's evidence. $PID disambiguates two runs starting in the same
# second; InvariantCulture keeps the stamp Gregorian and therefore sortable.
if (-not $LogPath) {
    $stamp = (Get-Date).ToString('yyyyMMdd-HHmmss', [Globalization.CultureInfo]::InvariantCulture)
    $LogPath = Join-Path $env:TEMP "waired-uninstall-$stamp-$PID.log"
}

# -------------------------------------------------------------------
# common_* helpers (mirror install.ps1 naming)
# -------------------------------------------------------------------

# Make emoji/box-drawing glyphs render on modern terminals (mirrors
# install.ps1; harmless when it fails on a legacy host).
try { [Console]::OutputEncoding = [Text.Encoding]::UTF8 } catch { }

# Emo <emoji> <ascii-fallback>: emoji on a UTF-8-capable console, else the
# ASCII fallback. WAIRED_NO_EMOJI forces the fallback. (Mirror of install.ps1.)
function Emo {
    param([string]$Emoji, [string]$Ascii)
    if ($env:WAIRED_NO_EMOJI) { return $Ascii }
    try {
        if ([Console]::OutputEncoding.CodePage -eq 65001) { return $Emoji }
    } catch { }
    return $Ascii
}

# Protect-PII masks the invoking user's home dir + username in one message
# when -MaskPII / WAIRED_PII_MASK is on (mirror of install.ps1's).
function Protect-PII {
    param([string]$Msg)
    if (-not $MaskPII) { return $Msg }
    if ($env:USERPROFILE -and $env:USERPROFILE.Length -ge 3) {
        $Msg = $Msg.Replace($env:USERPROFILE, '<home>')
    }
    if ($env:USERNAME -and $env:USERNAME.Length -ge 3) {
        $Msg = $Msg -replace "(?i)\b$([regex]::Escape($env:USERNAME))\b", '<user>'
    }
    return $Msg
}

function Common-Log  { param([string]$Msg) Write-Host "[waired] $(Protect-PII $Msg)" -ForegroundColor Cyan }
function Common-Warn { param([string]$Msg) Write-Host "[waired] Warning: $(Protect-PII $Msg)" -ForegroundColor Yellow }  # copy-ok: the one place the label is written

# Section prints a blank line + a horizontal-rule heading (mirror of
# install.ps1's Section; the U+2500 glyph is built at runtime so this file
# stays pure-ASCII on the wire -- scripts/install/encoding_test.go).
function Section {
    param([string]$Title)
    $d = Emo ([char]::ConvertFromUtf32(0x2500)) '-'
    $head = ($d * 3) + ' ' + $Title + ' '
    $fill = 56 - 4 - $Title.Length
    if ($fill -lt 3) { $fill = 3 }
    Write-Host ''
    Write-Host ($head + ($d * $fill)) -ForegroundColor DarkCyan
}

# Stop-TranscriptQuietly ends an active Start-Transcript without erroring
# when none is running (mirror of install.ps1, waired#748).
function Stop-TranscriptQuietly {
    try { Stop-Transcript -ErrorAction SilentlyContinue | Out-Null } catch { }
}

# True only inside the spawned elevated console (set in main when
# -FromElevation). Gates the transcript + pause-on-exit so that window never
# vanishes before its output can be read (the same waired#748 treatment
# install.ps1 got; previously the uninstall window closed the instant it
# finished and the user could not tell whether it succeeded).
$ElevatedConsole = $false

# Leave a one-line cause where the un-elevated parent can read it back: the
# spawned console closes with the child, and the parent otherwise has nothing
# but an exit code to report. The marker sits next to the transcript, in the
# parent's own %TEMP%, so it stays readable without elevation. Mirror of
# install.ps1's marker (#177), with one difference that matters: the path here
# is fixed rather than derived from a per-run workdir, so the parent MUST clear
# a stale one before elevating -- see Invoke-SelfElevate. Best-effort by
# design; a diagnostic must never become the failure.
function Write-UninstallStatus {
    param([string]$Text)
    if (-not $script:ElevatedConsole -or -not $script:LogPath) { return }
    try {
        [System.IO.File]::WriteAllText("$($script:LogPath).status", $Text,
            (New-Object System.Text.UTF8Encoding($false)))
    } catch { }
}

function Common-Die  {
    param([string]$Msg)
    Write-Host "[waired] $Msg" -ForegroundColor Red
    if ($script:ElevatedConsole) {
        # The trap below cannot see this: exit is not a terminating error, and
        # Common-Die is the path every ordinary elevated failure takes.
        Write-UninstallStatus $Msg
        if ($script:LogPath) { Write-Host "[waired] Full uninstall log: $($script:LogPath)" -ForegroundColor Red }
        Stop-TranscriptQuietly
        if (Test-InteractiveStdin) {
            Read-Host '[waired] Uninstall failed. Press Enter to close this window' | Out-Null
        }
    }
    exit 1
}

# Last resort for a terminating error that no try/catch and no Common-Die
# handled -- those would otherwise reach the elevated console as a stack trace
# on a window that closes, and reach the parent as a bare exit code. A trap is
# registered for the whole script scope regardless of where it is written, so
# this covers main below as well. It cannot catch a parse error or a parameter
# binding failure: both happen before the first statement runs, which is why
# the elevation argv is also quoted rather than merely diagnosed (#177).
# Deliberately self-contained rather than calling the helpers above: a trap is
# registered for the whole scope, so it can fire from the Configuration block
# too -- which runs before any of them is defined.
trap {
    $msg = "$($_.Exception.Message)"
    if ($script:ElevatedConsole -and $script:LogPath) {
        try {
            [System.IO.File]::WriteAllText("$($script:LogPath).status", $msg,
                (New-Object System.Text.UTF8Encoding($false)))
        } catch { }
    }
    Write-Host "[waired] uninstall failed: $msg" -ForegroundColor Red
    try { Stop-Transcript -ErrorAction SilentlyContinue | Out-Null } catch { }
    if ($script:ElevatedConsole) {
        try {
            if (-not [Console]::IsInputRedirected) {
                Read-Host '[waired] Uninstall failed. Press Enter to close this window' | Out-Null
            }
        } catch { }
    }
    exit 1
}

# Test-InteractiveStdin reports whether Read-Host will work without wedging
# (mirror of install.ps1, minus -NonInteractive which uninstall.ps1 lacks).
function Test-InteractiveStdin {
    try {
        return -not [Console]::IsInputRedirected
    } catch {
        return [Environment]::UserInteractive
    }
}

# Disable-QuickEdit clears conhost's QuickEdit mode in the spawned elevated
# window, where a stray click otherwise freezes all output until Enter/Esc
# (mirror of install.ps1; best-effort, transient console needs no restore).
function Disable-QuickEdit {
    try {
        Add-Type -Namespace WairedNative -Name ConsoleMode -MemberDefinition @'
[DllImport("kernel32.dll", SetLastError = true)]
public static extern IntPtr GetStdHandle(int nStdHandle);
[DllImport("kernel32.dll", SetLastError = true)]
public static extern bool GetConsoleMode(IntPtr hConsoleHandle, out uint lpMode);
[DllImport("kernel32.dll", SetLastError = true)]
public static extern bool SetConsoleMode(IntPtr hConsoleHandle, uint dwMode);
'@ -ErrorAction Stop
        $h = [WairedNative.ConsoleMode]::GetStdHandle(-10)  # STD_INPUT_HANDLE
        $mode = [uint32]0
        if ([WairedNative.ConsoleMode]::GetConsoleMode($h, [ref]$mode)) {
            $newMode = ($mode -band (-bnot [uint32]0x40)) -bor [uint32]0x80
            [void][WairedNative.ConsoleMode]::SetConsoleMode($h, $newMode)
        }
    } catch { }
}

# What this run actually did, so the closing summary can describe it instead
# of asserting it.
#
# Show-Done used to print "Waired fully removed (state wiped)." and "This
# device was deregistered from your Waired account" on every run, including a
# run on a machine where nothing was installed and no identity existed --
# claims with no object. -DryRun looked identical to a real run for the same
# reason: every step is existence-gated, so on an empty host none of them
# reached Common-Run and there was no [dry-run] line to tell the two apart
# (waired-agent#793).
#
# Common-Run and Skip-Absent are the two chokepoints every step passes
# through, so counting here cannot drift from what the steps did.
$script:DidCount = 0
$script:Deregistered = $false
# How many of those steps took out Claude Code settings Waired left behind
# (waired-agent#1398), so Show-Done can tell "Waired removed" from "Waired was
# already gone, and Claude Code still pointed at it".
$script:ClaudeLeftovers = 0

# Common-Run runs a scriptblock, or prints its description in dry-run mode.
function Common-Run {
    param([string]$Description, [scriptblock]$Action)
    $script:DidCount++
    if ($DryRun) { Write-Host "[dry-run] $Description" -ForegroundColor DarkGray; return }
    & $Action
}

function Test-IsAdmin {
    $id   = [Security.Principal.WindowsIdentity]::GetCurrent()
    $prin = New-Object Security.Principal.WindowsPrincipal($id)
    return $prin.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

# Quote one token per the CommandLineToArgvW rules. Start-Process joins
# -ArgumentList with single spaces and quotes NOTHING, for -Verb RunAs exactly
# as for a plain launch, so an unquoted path with a space arrives at the child
# split across two parameters. install.ps1 carries the same helper for the same
# reason (#177); the two scripts are downloaded and run independently, so each
# has to be self-contained.
function ConvertTo-NativeArg {
    param([string]$Value)
    if ($null -eq $Value) { $Value = '' }
    if ($Value -ne '' -and $Value -notmatch '[ \t"]') { return $Value }
    $sb = New-Object System.Text.StringBuilder
    [void]$sb.Append('"')
    for ($i = 0; $i -lt $Value.Length; $i++) {
        $slashes = 0
        while ($i -lt $Value.Length -and $Value[$i] -eq '\') { $i++; $slashes++ }
        if ($i -ge $Value.Length) {
            # Backslashes that run into the closing quote must be doubled,
            # or they escape the quote itself.
            [void]$sb.Append('\' * ($slashes * 2))
            break
        }
        if ($Value[$i] -eq '"') {
            [void]$sb.Append('\' * ($slashes * 2 + 1))
            [void]$sb.Append('"')
        } else {
            [void]$sb.Append('\' * $slashes)
            [void]$sb.Append($Value[$i])
        }
    }
    [void]$sb.Append('"')
    return $sb.ToString()
}

# Shared verbatim with install.ps1, which documents the reasoning in
# full. The two scripts are downloaded and run independently, so each has
# to be self-contained; installtest-windows.ps1 asserts the copies stay
# byte-identical, the same guard ConvertTo-NativeArg above already has.
# Get-ExitCodeReason turns a Windows process exit code into a plain cause, or
# '' when it is not one we recognise -- the caller then prints the raw code.
#
# Kept pure (int -> string, no script state, no Common-* calls) so
# installtest-windows.ps1 can lift it straight out of this file and table-test
# it, the way it already does with ConvertTo-NativeArg.
function Get-ExitCodeReason {
    param([int]$Code)
    # NTSTATUS values arrive as NEGATIVE Int32 -- Process.ExitCode is signed --
    # so match the signed literal. The hex in each comment is what
    # '{0:X8}' prints for it, because Int32 formats its two's-complement bit
    # pattern. Do NOT reach for [uint32] to "normalise" these: that conversion
    # is checked and throws on a negative value, on 5.1 and 7 alike.
    switch ($Code) {
        -1073741510 { return 'the Administrator window was closed, or Ctrl+C / Ctrl+Break was pressed, before setup finished' }  # 0xC000013A
        -1073741502 { return 'the elevated PowerShell couldn''t start (DLL initialization failed)' }                             # 0xC0000142
        -1073741819 { return 'the elevated installer stopped with an access violation' }                                         # 0xC0000005
        -1073741515 { return 'the elevated PowerShell couldn''t start (a required DLL was missing)' }                            # 0xC0000135
        -1073740791 { return 'the elevated installer was stopped by a security check (stack buffer overrun)' }                   # 0xC0000409
        default     { return '' }
    }
}

# Remove-OldRunLogs keeps the newest $Keep per-run transcripts (and their
# .status siblings) in %TEMP% and deletes the rest -- the bound the single
# fixed filename used to provide by clobbering, now that names are per-run.
#
# No locking, deliberately: Windows refuses to delete a file another process
# still holds open, so a concurrent run's transcript survives this by itself.
# Do not add a lockfile here.
function Remove-OldRunLogs {
    param([string]$Prefix, [int]$Keep = 5)
    try {
        # -LiteralPath plus -Filter, never -Path with a glob: %TEMP% lives
        # under the user profile, and a username containing [ or ] turns a
        # -Path wildcard into a character class that silently matches nothing.
        # -Filter itself goes through FindFirstFileEx, which also matches 8.3
        # short names, so the anchored -match is the real filter.
        $logs = @(Get-ChildItem -LiteralPath $env:TEMP -Filter "$Prefix-*.log" -File -ErrorAction SilentlyContinue |
            Where-Object { $_.Name -match "^$Prefix-\d{8}-\d{6}-\d+\.log$" } |
            Sort-Object -Property Name -Descending)
        foreach ($old in @($logs | Select-Object -Skip $Keep)) {
            Remove-Item -LiteralPath $old.FullName -Force -ErrorAction SilentlyContinue
            Remove-Item -LiteralPath "$($old.FullName).status" -Force -ErrorAction SilentlyContinue
        }
    } catch { }
}

function Show-Help {
@"
uninstall.ps1: remove Waired on Windows.

Usage:
  iwr -useb https://github.com/waired-ai/waired-agent/releases/latest/download/uninstall.ps1 | iex
  # For -Clean, download then run (iex strips named parameters):
  iwr -useb .../uninstall.ps1 -OutFile uninstall.ps1; .\uninstall.ps1 -Clean

By default removes the Waired binaries + service but KEEPS your local state
(%ProgramData%\waired: identity, keys, settings). Either tier also best-effort
deregisters this device from your Waired account (removed from your device list).

Options:
  -Clean    also delete state (%ProgramData%\waired) and Ollama (binary +
            downloaded models). Destructive; asks to confirm unless -Yes.
  -Yes      assume "yes" to the pre-uninstall confirmation and the -Clean
            confirmation (-Clean requires it when piped / non-interactive)
  -DryRun   show every change without making it (no elevation / UAC)
  -MaskPII  mask personal information (home dir, username) in the output -
            for screenshots and bug reports. Best-effort. Same as
            WAIRED_PII_MASK=1.
  -Help     print this help

If you installed Waired with the GUI installer (WairedSetup-*.exe), prefer
Settings -> Apps -> Waired -> Uninstall. This script targets the
'iwr ... | iex' install and is safe to run either way.

Environment variables:
  WAIRED_STATE_DIR         override the state dir removed by -Clean
                           (default %ProgramData%\waired)
  WAIRED_INSTALL_BASE_URL  mirror base for the elevated self-re-fetch
"@ | Write-Host
}

# Confirm-Uninstall shows what is about to be removed, then asks before
# ANYTHING runs (per-user teardown included). Default is NO -- uninstalling
# is destructive, so a bare Enter aborts. -Yes bypasses (the CI /
# already-consented path); -DryRun previews without asking. A
# non-interactive session proceeds for the plain tier (preserves piped
# `iwr | iex` uninstalls) but still refuses -Clean without -Yes, so a piped
# invocation can never silently wipe state. Runs in the un-elevated parent so
# the prompt reaches a real console before UAC hands the child a fresh stdin;
# the -FromElevation child never re-asks.
function Confirm-Uninstall {
    if ($FromElevation) { return }

    Section 'What this will remove'
    Write-Host "  * The Waired binaries under $InstallDir"
    Write-Host "  * The background service, the Start Menu entries and the Waired app's autostart"
    Write-Host "  * The Waired app, if it is open on this desktop (it is closed first)"
    Write-Host "  * The Claude Code / coding-agent integration for this user"
    Write-Host "  * This device's registration in your Waired account (best-effort)"
    if ($Clean) {
        Write-Host "  * All local state: config, keys, identity ($StateDir)" -ForegroundColor Yellow
        Write-Host "  * Ollama and its downloaded models (can't be undone)" -ForegroundColor Yellow
    } else {
        Write-Host "  (local state under $StateDir is kept; re-run with -Clean to wipe it)"
    }

    if ($Yes -or $DryRun) { return }
    if (-not (Test-InteractiveStdin)) {
        if ($Clean) {
            Common-Die "-Clean is destructive; re-run with -Yes to confirm on a non-interactive session"
        }
        Common-Log "No terminal detected. Proceeding without confirmation (pass -Yes to silence this notice)."
        return
    }
    Write-Host ''
    $reply = Read-Host '[waired] Proceed with the uninstall? [y/N] (Enter = No)'
    if ($reply -notmatch '^(y|yes)$') { Common-Die 'Aborted. Nothing was removed.' }
}

# Re-invoke this script elevated. SCM, HKLM PATH and cert stores all need
# admin. Consent was already obtained in the un-elevated parent
# (Confirm-Uninstall), so -Yes is forwarded to keep the child
# non-interactive. Mirrors install.ps1's Invoke-SelfElevate (no sudo.exe:
# Start-Process -Verb RunAs is universal back to Windows 10 1809). Like
# install.ps1, the `iwr | iex` case stages the fetched body to a temp .ps1
# and re-launches it with -File -- NOT an in-memory ScriptBlock cradle, which
# reads as a download-and-execute pattern to Defender's AMSI heuristics and
# can get the whole script blocked (#552); -File also binds the named
# passthrough params reliably.
function Invoke-SelfElevate {
    Common-Log "Requesting administrator rights (a UAC prompt will appear)..."
    $passthrough = @('-FromElevation', '-Yes', '-LogPath', $LogPath)
    if ($Clean)   { $passthrough += '-Clean' }
    if ($DryRun)  { $passthrough += '-DryRun' }
    if ($MaskPII) { $passthrough += '-MaskPII' }

    $psArgs = @('-NoProfile', '-ExecutionPolicy', 'Bypass')
    $tempScript = $null
    if ($PSCommandPath) {
        $psArgs += @('-File', $PSCommandPath) + $passthrough
    } else {
        # Sourced via `iwr | iex`: $PSCommandPath is null. Stage the body to
        # a temp .ps1 and re-launch with -File (see the function comment).
        $url = "$BaseUrl/latest/download/uninstall.ps1"
        $tempScript = Join-Path $env:TEMP "waired-uninstall-elevate-$([Guid]::NewGuid().ToString('N')).ps1"
        Invoke-WebRequest -Uri $url -OutFile $tempScript -UseBasicParsing
        $psArgs += @('-File', $tempScript) + $passthrough
    }

    # Both value-bearing tokens can contain a space: $PSCommandPath (the
    # uninstaller may sit anywhere, and install.ps1 -Clean invokes a sibling of
    # its own path) and $LogPath / $tempScript, which are %TEMP%-derived and so
    # carry the username. Unquoted, the child bound half a path and dropped the
    # rest, exactly as install.ps1 did before #177.
    $psArgs = @($psArgs | ForEach-Object { ConvertTo-NativeArg $_ })

    # The marker is derived from $LogPath, which is now per-run -- so a marker
    # from an earlier failed uninstall can no longer be mistaken for this
    # run's cause, and the explicit stale-marker delete this used to need is
    # gone with it (#314).
    $marker = "$LogPath.status"

    try {
        $proc = Start-Process -FilePath 'powershell.exe' `
            -ArgumentList $psArgs -Verb RunAs -PassThru -Wait
        if ($proc.ExitCode -ne 0) {
            # A child that died before its transcript existed still leaves the
            # marker; one that never started at all leaves nothing, and saying
            # so is itself the diagnosis.
            $why = ''
            if (Test-Path -LiteralPath $marker) {
                $why = ((Get-Content -LiteralPath $marker -Raw) -split "`r?`n")[0]
            }
            # No marker means the console was closed or the child never got
            # far enough to write one; decode what Windows reported instead of
            # printing a bare NTSTATUS. Shared verbatim with install.ps1 --
            # installtest-windows.ps1 asserts the two copies stay identical.
            if (-not $why) { $why = Get-ExitCodeReason -Code $proc.ExitCode }
            $code = "$($proc.ExitCode) (0x$('{0:X8}' -f [int]$proc.ExitCode))"
            $tail = if ($why) { " -- $why" } else { '' }
            Common-Die "The elevated uninstaller exited with code $code$tail. Full uninstall log: $LogPath"
        }
    } finally {
        # -Wait guarantees the elevated child finished reading the staged
        # script before we delete it.
        if ($tempScript) {
            Remove-Item -LiteralPath $tempScript -Force -ErrorAction SilentlyContinue
        }
    }
}

# Drop one entry from the machine PATH (case-insensitive). SetEnvironmentVariable
# against the Machine target broadcasts WM_SETTINGCHANGE, so new shells pick it up.
# Test-OnMachinePath -- is $Dir actually a machine PATH entry? Split out so the
# caller can decide whether to announce the removal (#630) rather than
# announcing it and then discovering there was nothing to do.
function Test-OnMachinePath {
    param([string]$Dir)
    $machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    if (-not $machinePath) { return $false }
    return (($machinePath -split ';') -contains $Dir)
}

function Remove-FromMachinePath {
    param([string]$Dir)
    $machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    if (-not $machinePath) { return }
    $entries = @($machinePath -split ';' | Where-Object { $_ -ne '' -and $_ -ne $Dir })
    $newPath = ($entries -join ';')
    if ($newPath -eq $machinePath) { return }
    Common-Run "system PATH -= $Dir" {
        [Environment]::SetEnvironmentVariable('Path', $newPath, 'Machine')
    }
}

# -------------------------------------------------------------------
# Claude Code settings Waired left behind (waired-agent#1398)
# -------------------------------------------------------------------
#
# `waired.exe claude disable` is what removes Waired from Claude Code's
# settings, and until #1398 this script ran nothing else: with waired.exe
# missing, refused by Windows (Smart App Control and the like), or too old to
# know a newer rule, the settings stayed. The one that matters is
# %ProgramFiles%\ClaudeCode\managed-settings.json: its ANTHROPIC_BASE_URL beats
# anything a user sets, so Claude Code went on sending every request to a
# 127.0.0.1 port nothing listened on, and failed with "Connection refused -- a
# firewall or proxy may be blocking it".
#
# So after the binary has had its turn (or when there is no binary), the
# functions below look at the files themselves and take out what is Waired's.
# The rules are the Go removers', copied: claudemanaged.RemoveWithOptions for
# the managed file, and runClaudeDisable's per-user steps for
# ~/.claude/settings.json. packaging/install/testdata/claude-leftovers holds
# the cases both copies are held to (scripts/install runs the Go side,
# installtest-pwsh.ps1 and installtest-windows.ps1 run these functions), and
# uninstall.sh carries the same copy for Linux and macOS.
#
# The JSON is read and written by the small parser below rather than
# ConvertFrom-Json / ConvertTo-Json, because those two differ between
# PowerShell 5.1 and 7 in ways that change a file they only round-trip: 5.1
# refuses keys that differ only by case, 7 turns date-looking strings into
# DateTime, both unroll one-element arrays and cap nesting depth. This parser
# keeps keys case-sensitive and in order, and numbers exactly as written.
#
# The parse and rule functions are pure (text in, result out; no Common-*, no
# files), so the harnesses lift them out of this file and run the corpus
# through them.

# ConvertFrom-ClaudeJson parses JSON text into OrderedDictionary (objects),
# ArrayList (arrays), string, bool, $null, and a PSCustomObject carrying the
# number as written. Throws on anything that is not a single JSON value.
function ConvertFrom-ClaudeJson {
    param([string]$Text)
    $state = @{ T = $Text; I = 0 }
    $value = Read-ClaudeJsonValue -S $state
    Skip-ClaudeJsonSpace -S $state
    if ($state.I -lt $state.T.Length) { throw "json: unexpected text at offset $($state.I)" }
    return ,$value
}

function Skip-ClaudeJsonSpace {
    param($S)
    while ($S.I -lt $S.T.Length -and " `t`r`n".IndexOf($S.T[$S.I]) -ge 0) { $S.I++ }
}

# Read-ClaudeJsonValue reads one value at $S.I. Every return goes through the
# comma operator: an ArrayList returned bare would be unrolled by the pipeline.
function Read-ClaudeJsonValue {
    param($S)
    Skip-ClaudeJsonSpace -S $S
    if ($S.I -ge $S.T.Length) { throw 'json: unexpected end of text' }
    $c = $S.T[$S.I]
    if ($c -eq [char]'{') {
        $S.I++
        $obj = New-Object System.Collections.Specialized.OrderedDictionary
        Skip-ClaudeJsonSpace -S $S
        if ($S.I -lt $S.T.Length -and $S.T[$S.I] -eq [char]'}') { $S.I++; return ,$obj }
        while ($true) {
            Skip-ClaudeJsonSpace -S $S
            if ($S.I -ge $S.T.Length -or $S.T[$S.I] -ne [char]'"') { throw "json: expected a key at offset $($S.I)" }
            $key = Read-ClaudeJsonString -S $S
            Skip-ClaudeJsonSpace -S $S
            if ($S.I -ge $S.T.Length -or $S.T[$S.I] -ne [char]':') { throw "json: expected a colon at offset $($S.I)" }
            $S.I++
            $member = Read-ClaudeJsonValue -S $S
            # Last one wins, as in encoding/json.
            if ($obj.Contains($key)) { $obj.Remove($key) }
            $obj.Add($key, $member)
            Skip-ClaudeJsonSpace -S $S
            if ($S.I -ge $S.T.Length) { throw 'json: unexpected end of text' }
            if ($S.T[$S.I] -eq [char]',') { $S.I++; continue }
            if ($S.T[$S.I] -eq [char]'}') { $S.I++; return ,$obj }
            throw "json: expected a comma or a closing brace at offset $($S.I)"
        }
    }
    if ($c -eq [char]'[') {
        $S.I++
        $list = New-Object System.Collections.ArrayList
        Skip-ClaudeJsonSpace -S $S
        if ($S.I -lt $S.T.Length -and $S.T[$S.I] -eq [char]']') { $S.I++; return ,$list }
        while ($true) {
            $item = Read-ClaudeJsonValue -S $S
            [void]$list.Add($item)
            Skip-ClaudeJsonSpace -S $S
            if ($S.I -ge $S.T.Length) { throw 'json: unexpected end of text' }
            if ($S.T[$S.I] -eq [char]',') { $S.I++; continue }
            if ($S.T[$S.I] -eq [char]']') { $S.I++; return ,$list }
            throw "json: expected a comma or a closing bracket at offset $($S.I)"
        }
    }
    if ($c -eq [char]'"') { return ,(Read-ClaudeJsonString -S $S) }
    foreach ($lit in @(@('true', $true), @('false', $false), @('null', $null))) {
        $word = [string]$lit[0]
        if ($S.I + $word.Length -le $S.T.Length -and
            [string]::CompareOrdinal($S.T, $S.I, $word, 0, $word.Length) -eq 0) {
            $S.I += $word.Length
            return ,$lit[1]
        }
    }
    $m = [regex]::Match($S.T.Substring($S.I), '^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?')
    if ($m.Success -and $m.Length -gt 0) {
        $S.I += $m.Length
        return ,([pscustomobject]@{ ClaudeJsonNumber = $m.Value })
    }
    throw "json: unexpected character at offset $($S.I)"
}

function Read-ClaudeJsonString {
    param($S)
    $S.I++
    $sb = New-Object System.Text.StringBuilder
    while ($true) {
        if ($S.I -ge $S.T.Length) { throw 'json: unterminated string' }
        $ch = $S.T[$S.I]
        if ($ch -eq [char]'"') { $S.I++; return $sb.ToString() }
        if ([int]$ch -lt 0x20) { throw "json: control character in a string at offset $($S.I)" }
        if ($ch -ne [char]'\') { [void]$sb.Append($ch); $S.I++; continue }
        if ($S.I + 1 -ge $S.T.Length) { throw 'json: unterminated string' }
        $esc = $S.T[$S.I + 1]
        $S.I += 2
        switch -CaseSensitive ([string]$esc) {
            '"' { [void]$sb.Append([char]'"') }
            '\' { [void]$sb.Append([char]'\') }
            '/' { [void]$sb.Append([char]'/') }
            'b' { [void]$sb.Append([char]8) }
            'f' { [void]$sb.Append([char]12) }
            'n' { [void]$sb.Append([char]10) }
            'r' { [void]$sb.Append([char]13) }
            't' { [void]$sb.Append([char]9) }
            'u' {
                if ($S.I + 4 -gt $S.T.Length) { throw 'json: short unicode escape' }
                $hex = $S.T.Substring($S.I, 4)
                if ($hex -notmatch '^[0-9a-fA-F]{4}$') { throw 'json: bad unicode escape' }
                [void]$sb.Append([char][Convert]::ToInt32($hex, 16))
                $S.I += 4
            }
            default { throw "json: bad escape at offset $($S.I - 1)" }
        }
    }
}

# ConvertTo-ClaudeJson writes what ConvertFrom-ClaudeJson reads, indented by
# two spaces the way encoding/json's MarshalIndent does.
function ConvertTo-ClaudeJson {
    param($Value, [int]$Depth = 0)
    if ($null -eq $Value) { return 'null' }
    if ($Value -is [bool]) { if ($Value) { return 'true' } else { return 'false' } }
    if ($Value -is [string]) { return (Format-ClaudeJsonString -Value $Value) }
    $pad = '  ' * ($Depth + 1)
    $end = '  ' * $Depth
    if ($Value -is [System.Collections.Specialized.OrderedDictionary]) {
        if ($Value.Count -eq 0) { return '{}' }
        $parts = New-Object System.Collections.ArrayList
        foreach ($k in @($Value.Keys)) {
            [void]$parts.Add($pad + (Format-ClaudeJsonString -Value ([string]$k)) + ': ' + (ConvertTo-ClaudeJson -Value $Value[$k] -Depth ($Depth + 1)))
        }
        return "{`n" + ($parts -join ",`n") + "`n$end}"
    }
    if ($Value -is [System.Collections.ArrayList]) {
        if ($Value.Count -eq 0) { return '[]' }
        $parts = New-Object System.Collections.ArrayList
        foreach ($item in $Value) {
            [void]$parts.Add($pad + (ConvertTo-ClaudeJson -Value $item -Depth ($Depth + 1)))
        }
        return "[`n" + ($parts -join ",`n") + "`n$end]"
    }
    if ($Value.PSObject.Properties['ClaudeJsonNumber']) { return [string]$Value.ClaudeJsonNumber }
    throw "json: can't write a value of type $($Value.GetType().FullName)"
}

function Format-ClaudeJsonString {
    param([string]$Value)
    $sb = New-Object System.Text.StringBuilder
    [void]$sb.Append([char]'"')
    foreach ($ch in $Value.ToCharArray()) {
        $n = [int]$ch
        if ($ch -eq [char]'"') { [void]$sb.Append('\"') }
        elseif ($ch -eq [char]'\') { [void]$sb.Append('\\') }
        elseif ($n -eq 10) { [void]$sb.Append('\n') }
        elseif ($n -eq 13) { [void]$sb.Append('\r') }
        elseif ($n -eq 9) { [void]$sb.Append('\t') }
        elseif ($n -lt 0x20) { [void]$sb.Append('\u' + $n.ToString('x4')) }
        else { [void]$sb.Append($ch) }
    }
    [void]$sb.Append([char]'"')
    return $sb.ToString()
}

# Test-WairedModelId is claudecode.IsWairedModelID: lower-cased, trimmed, every
# "[1m]" tier marker taken out, then any id containing "waired" is Waired's.
function Test-WairedModelId {
    param([string]$Id)
    $bare = $Id.Trim().ToLowerInvariant()
    while ($true) {
        $i = $bare.IndexOf('[1m]', [StringComparison]::Ordinal)
        if ($i -lt 0) { break }
        $bare = $bare.Remove($i, 4)
    }
    return $bare.Contains('waired')
}

# Test-ClaudeJsonObject / Test-ClaudeJsonList name the two container types the
# parser produces, so the rules below read as JSON shapes.
function Test-ClaudeJsonObject { param($Value) return ($Value -is [System.Collections.Specialized.OrderedDictionary]) }
function Test-ClaudeJsonList { param($Value) return ($Value -is [System.Collections.ArrayList]) }

# Test-ClaudeHookEntryIsWaired is claudemanaged's entryCommandAny: an entry is
# Waired's when any command inside its hooks list carries one of the markers.
function Test-ClaudeHookEntryIsWaired {
    param($Entry, [string[]]$Markers)
    if (-not (Test-ClaudeJsonObject $Entry) -or -not $Entry.Contains('hooks')) { return $false }
    $inner = $Entry['hooks']
    if (-not (Test-ClaudeJsonList $inner)) { return $false }
    foreach ($h in $inner) {
        if (-not (Test-ClaudeJsonObject $h) -or -not $h.Contains('command')) { continue }
        $cmd = $h['command']
        if (-not ($cmd -is [string])) { continue }
        foreach ($m in $Markers) {
            if ($cmd.Contains($m)) { return $true }
        }
    }
    return $false
}

# Get-ClaudePickerKind is claudecode.DetectPickerLineup on a parsed value:
# 'none', 'ours' (every row a Waired id), 'foreign', or 'unreadable' (a shape
# encoding/json would refuse to decode into the lineup).
function Get-ClaudePickerKind {
    param($Picker)
    if ($null -eq $Picker) { return 'none' }
    if (-not (Test-ClaudeJsonObject $Picker)) { return 'unreadable' }
    if ($Picker.Contains('replaceBuiltInOptions')) {
        $r = $Picker['replaceBuiltInOptions']
        if (-not ($null -eq $r -or $r -is [bool])) { return 'unreadable' }
    }
    if (-not $Picker.Contains('options') -or $null -eq $Picker['options']) { return 'none' }
    $options = $Picker['options']
    if (-not (Test-ClaudeJsonList $options)) { return 'unreadable' }
    $models = New-Object System.Collections.ArrayList
    foreach ($row in $options) {
        if ($null -eq $row) { [void]$models.Add(''); continue }
        if (-not (Test-ClaudeJsonObject $row)) { return 'unreadable' }
        foreach ($field in @('model', 'label', 'description')) {
            if ($row.Contains($field) -and -not ($null -eq $row[$field] -or $row[$field] -is [string])) { return 'unreadable' }
        }
        $model = ''
        if ($row.Contains('model') -and $row['model'] -is [string]) { $model = $row['model'] }
        [void]$models.Add($model)
    }
    if ($models.Count -eq 0) { return 'none' }
    foreach ($model in $models) {
        if (-not (Test-WairedModelId $model)) { return 'foreign' }
    }
    return 'ours'
}

# Edit-ClaudeLeftovers applies one kind of file's rules to its text.
#
#   -Kind managed  %ProgramFiles%\ClaudeCode\managed-settings.json
#   -Kind user     ~\.claude\settings.json
#   -Kind cache    ~\.claude\cache\gateway-models.json
#
# State is 'unchanged', 'rewrite' (Text is the new file), 'delete', or
# 'unreadable' (not a JSON object this can read; left alone). Removed names
# what was taken out, Kept names a value left behind that may be Waired's, and
# Wrapper says the status line was Waired's wrapper script, whose files the
# caller removes.
function Edit-ClaudeLeftovers {
    param([string]$Kind, [string]$Text)
    $removed = New-Object System.Collections.ArrayList
    $kept = New-Object System.Collections.ArrayList
    $result = [pscustomobject]@{ State = 'unchanged'; Text = ''; Removed = @(); Kept = @(); Wrapper = $false }
    if ($Text.Length -gt 0 -and [int]$Text[0] -eq 0xFEFF) { $Text = $Text.Substring(1) }
    if ($Text.Trim().Length -eq 0) { return $result }
    try {
        $doc = ConvertFrom-ClaudeJson -Text $Text
    } catch {
        $result.State = 'unreadable'
        return $result
    }
    if (-not (Test-ClaudeJsonObject $doc)) {
        # A bare null reads as an empty settings file to claudecode, and as
        # nothing to claudemanaged; neither changes it.
        if ($null -ne $doc) { $result.State = 'unreadable' }
        return $result
    }

    $loopback = 'http://127.0.0.1:'
    if ($Kind -eq 'managed') {
        if ($doc.Contains('env') -and (Test-ClaudeJsonObject $doc['env'])) {
            $envBlock = $doc['env']
            $envChanged = $false
            $url = $envBlock['ANTHROPIC_BASE_URL']
            if ($url -is [string] -and $url.StartsWith($loopback, [StringComparison]::Ordinal)) {
                $envBlock.Remove('ANTHROPIC_BASE_URL')
                [void]$removed.Add('ANTHROPIC_BASE_URL')
                $envChanged = $true
                foreach ($pair in @(@('CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY', '1'),
                                    @('CLAUDE_CODE_AUTO_COMPACT_WINDOW', '200000'),
                                    @('CLAUDE_CODE_MAX_CONTEXT_TOKENS', '250000'))) {
                    $cur = $envBlock[$pair[0]]
                    if ($cur -is [string] -and $cur -ceq $pair[1]) {
                        $envBlock.Remove($pair[0])
                        [void]$removed.Add($pair[0])
                    }
                }
                $window = $envBlock['CLAUDE_CODE_MAX_CONTEXT_TOKENS']
                if ($window -is [string]) { [void]$kept.Add("CLAUDE_CODE_MAX_CONTEXT_TOKENS=$window") }
            }
            $sub = $envBlock['CLAUDE_CODE_SUBAGENT_MODEL']
            if ($sub -is [string] -and $sub -ceq 'waired/subagent') {
                $envBlock.Remove('CLAUDE_CODE_SUBAGENT_MODEL')
                [void]$removed.Add('CLAUDE_CODE_SUBAGENT_MODEL')
                $envChanged = $true
            }
            if ($envChanged -and $envBlock.Count -eq 0) { $doc.Remove('env') }
        }
        if ($doc.Contains('hooks') -and (Test-ClaudeJsonObject $doc['hooks'])) {
            $hooks = $doc['hooks']
            $events = @(
                @('Stop', @('waired claude _fallback-hook')),
                @('SessionStart', @('waired claude _picker write --from-managed', 'waired claude _models-cache write --from-managed'))
            )
            foreach ($ev in $events) {
                $name = [string]$ev[0]
                if (-not $hooks.Contains($name) -or -not (Test-ClaudeJsonList $hooks[$name])) { continue }
                $keepEntries = New-Object System.Collections.ArrayList
                $dropped = 0
                foreach ($entry in $hooks[$name]) {
                    if (Test-ClaudeHookEntryIsWaired -Entry $entry -Markers $ev[1]) { $dropped++ } else { [void]$keepEntries.Add($entry) }
                }
                if ($dropped -eq 0) { continue }
                if ($keepEntries.Count -eq 0) { $hooks.Remove($name) } else { $hooks[$name] = $keepEntries }
                [void]$removed.Add("hooks.$name")
                if ($hooks.Count -eq 0) { $doc.Remove('hooks') }
            }
        }
    } elseif ($Kind -eq 'user') {
        if ($doc.Contains('statusLine')) {
            $line = $doc['statusLine']
            $cmd = ''
            $shapeOk = $null -eq $line -or (Test-ClaudeJsonObject $line)
            if (Test-ClaudeJsonObject $line) {
                if ($line.Contains('type') -and -not ($null -eq $line['type'] -or $line['type'] -is [string])) { $shapeOk = $false }
                if ($line.Contains('command')) {
                    if ($line['command'] -is [string]) { $cmd = $line['command'] }
                    elseif ($null -ne $line['command']) { $shapeOk = $false }
                }
            }
            if ($shapeOk -and $cmd.Contains('waired claude statusline')) {
                $doc.Remove('statusLine')
                $doc.Remove('waired_original_statusLine')
                [void]$removed.Add('statusLine')
            } elseif ($shapeOk -and $cmd.Contains('waired-statusline')) {
                if ($doc.Contains('waired_original_statusLine')) {
                    $doc['statusLine'] = $doc['waired_original_statusLine']
                } else {
                    $doc.Remove('statusLine')
                }
                $doc.Remove('waired_original_statusLine')
                [void]$removed.Add('statusLine')
                $result.Wrapper = $true
            }
        }
        if ($doc.Contains('modelPicker') -and (Get-ClaudePickerKind -Picker $doc['modelPicker']) -eq 'ours') {
            $doc.Remove('modelPicker')
            [void]$removed.Add('modelPicker')
        }
        if ($doc.Contains('model')) {
            $model = $doc['model']
            if ($model -is [string] -and $model -ne '' -and (Test-WairedModelId $model)) {
                $doc.Remove('model')
                [void]$removed.Add('model')
            }
        }
        if ($doc.Contains('env') -and (Test-ClaudeJsonObject $doc['env'])) {
            $envBlock = $doc['env']
            $allStrings = $true
            foreach ($k in @($envBlock.Keys)) { if (-not ($envBlock[$k] -is [string])) { $allStrings = $false } }
            $cur = ''
            if ($envBlock.Contains('CLAUDE_CODE_SUBAGENT_MODEL')) { $cur = [string]$envBlock['CLAUDE_CODE_SUBAGENT_MODEL'] }
            if ($allStrings -and ($cur -eq '' -or (Test-WairedModelId $cur))) {
                $envChanged = $false
                foreach ($k in @('CLAUDE_CODE_SUBAGENT_MODEL', 'CLAUDE_CODE_SUBAGENT_MODEL_FORCE')) {
                    if ($envBlock.Contains($k)) { $envBlock.Remove($k); [void]$removed.Add($k); $envChanged = $true }
                }
                if ($envChanged -and $envBlock.Count -eq 0) { $doc.Remove('env') }
            }
        }
    } elseif ($Kind -eq 'cache') {
        $base = ''
        if ($doc.Contains('baseUrl')) {
            if ($doc['baseUrl'] -is [string]) { $base = $doc['baseUrl'] }
            elseif ($null -ne $doc['baseUrl']) { return $result }
        }
        $models = $null
        if ($doc.Contains('models')) { $models = $doc['models'] }
        if ($null -ne $models -and -not (Test-ClaudeJsonList $models)) { return $result }
        if (-not $base.StartsWith($loopback, [StringComparison]::Ordinal) -or $null -eq $models -or $models.Count -eq 0) { return $result }
        foreach ($row in $models) {
            if ($null -eq $row) { return $result }
            if (-not (Test-ClaudeJsonObject $row)) { return $result }
            $id = ''
            if ($row.Contains('id')) {
                if ($row['id'] -is [string]) { $id = $row['id'] } elseif ($null -ne $row['id']) { return $result }
            }
            if (-not (Test-WairedModelId $id)) { return $result }
        }
        $result.State = 'delete'
        $result.Removed = @('gateway-models.json')
        return $result
    } else {
        throw "Edit-ClaudeLeftovers: unknown kind $Kind"
    }

    if ($removed.Count -eq 0) { return $result }
    $result.Removed = @($removed)
    $result.Kept = @($kept)
    if ($doc.Count -eq 0) {
        $result.State = 'delete'
    } else {
        $result.State = 'rewrite'
        $result.Text = (ConvertTo-ClaudeJson -Value $doc) + "`n"
    }
    return $result
}

# -------------------------------------------------------------------
# Removal steps
# -------------------------------------------------------------------

# MACHINE phase (elevated): remove the machine-scoped Claude Code managed
# settings (%ProgramFiles%\ClaudeCode\managed-settings.json) and sweep any
# residual retired-MITM proxy artifacts (hosts entry, Root-store CA,
# NODE_EXTRA_CA_CERTS), while waired.exe is still present. `claude disable` needs
# admin for the managed file. Replaces the removed `waired proxy uninstall`
# (waired#750/#754). The per-user half (~/.claude, HKCU) is Remove-UserIntegration.
function Remove-ClaudeManaged {
    $exe = Join-Path $InstallDir 'waired.exe'
    if (Test-Path -LiteralPath $exe) {
        Common-Log "Removing Claude Code managed settings (+ any retired MITM proxy artifacts)"
        # Output is NOT discarded here (it is in the per-user twin, which runs
        # unelevated and can only report the permission it does not have). This is
        # the elevated call that owns the machine-wide file, and when it cannot
        # confirm CLAUDE_CODE_MAX_CONTEXT_TOKENS as waired's it keeps the key and
        # says so on stderr -- a warning written for the uninstall transcript
        # (waired-agent#1174, measured leaking on macOS in waired-agent#1308).
        Common-Run "$exe claude disable" {
            $run = Invoke-WairedExe -Exe $exe -ArgList @('claude', 'disable') -Show
            Write-WairedExeProblem -Run $run -Command 'waired.exe claude disable'
        }
    }
    # Whatever the binary did or could not do, look at the file itself
    # (waired-agent#1398).
    Repair-ManagedClaudeSettings
}

# Invoke-WairedExe runs waired.exe and reports whether it ran and how it
# exited, instead of letting either question disappear into a catch.
#
# $ErrorActionPreference is 'Continue' for the call. Under this script's
# 'Stop', Windows PowerShell 5.1 turns the first stderr line of a native
# command into a terminating error, 2>$null or not -- and `claude disable`
# writes a warning to stderr before it gets to the per-user cleanup, so the
# old try { } catch { } could stop reading it half way through its work.
function Invoke-WairedExe {
    param([string]$Exe, [string[]]$ArgList, [switch]$Show)
    $ErrorActionPreference = 'Continue'
    try {
        $out = & $Exe @ArgList 2>&1
        $code = $LASTEXITCODE
        if ($Show) { foreach ($line in @($out)) { Write-Host "$line" } }
        return [pscustomobject]@{ Ran = $true; ExitCode = $code; Error = '' }
    } catch {
        # First line only: 5.1 appends the failing script line and a caret
        # marker to the message, which says nothing about waired.exe.
        $why = (($_.Exception.Message.Trim()) -split "`r?`n")[0]
        return [pscustomobject]@{ Ran = $false; ExitCode = -1; Error = $why }
    }
}

# Write-WairedExeProblem says when waired.exe couldn't start or failed, and that
# the settings are checked by hand next -- the same shape Remove-WairedService
# uses for waired-agent.exe.
function Write-WairedExeProblem {
    param($Run, [string]$Command)
    if (-not $Run.Ran) {
        Common-Warn "waired.exe couldn't run ($($Run.Error)). Checking Claude Code's settings by hand."
    } elseif ($Run.ExitCode -ne 0) {
        Common-Warn "$Command exited with code $($Run.ExitCode). Checking Claude Code's settings by hand."
    }
}

function Read-ClaudeSettingsText {
    param([string]$Path)
    # ReadAllText drops a UTF-8 BOM; Edit-ClaudeLeftovers drops one too, for
    # text that arrives some other way.
    return [System.IO.File]::ReadAllText($Path)
}

# Write-ClaudeSettingsText replaces the file through a temporary sibling, so a
# failure part way leaves the old file rather than half a new one. UTF-8
# without a BOM, as Claude Code and waired write it.
function Write-ClaudeSettingsText {
    param([string]$Path, [string]$Text)
    $tmp = "$Path.waired-tmp"
    [System.IO.File]::WriteAllText($tmp, $Text, (New-Object System.Text.UTF8Encoding($false)))
    Move-Item -LiteralPath $tmp -Destination $Path -Force
}

# Invoke-ClaudeLeftoverEdit reads one settings file, applies its rules, and
# makes the change through Common-Run, so -DryRun previews it and Show-Done
# counts it. Returns the edit, or $null when the file couldn't be read.
function Invoke-ClaudeLeftoverEdit {
    param([string]$Kind, [string]$Path)
    try {
        $text = Read-ClaudeSettingsText -Path $Path
    } catch {
        Common-Warn "Couldn't read $Path ($($_.Exception.Message.Trim())). If it still has Waired's settings, remove them by hand."
        return $null
    }
    $edit = Edit-ClaudeLeftovers -Kind $Kind -Text $text
    if ($edit.State -eq 'unreadable') {
        if ($text -match 'waired|127\.0\.0\.1') {
            Common-Warn "$Path isn't JSON the uninstaller can read, so it was left as it is. If it still has Waired's settings, remove them by hand."
        }
        return $edit
    }
    if ($edit.State -eq 'unchanged') { return $edit }
    $script:ClaudeLeftovers++
    Common-Log "Removing what Waired left in $Path ($($edit.Removed -join ', '))"
    $newText = $edit.Text
    if ($edit.State -eq 'delete') {
        Common-Run "Remove-Item $Path" { Remove-Item -LiteralPath $Path -Force }
    } else {
        Common-Run "rewrite $Path" { Write-ClaudeSettingsText -Path $Path -Text $newText }
    }
    foreach ($k in $edit.Kept) {
        Common-Warn "Left $k in $Path. It couldn't be confirmed as Waired's, so remove it by hand if you didn't set it."
    }
    return $edit
}

# Remove-ClaudeLeftoverPath deletes a file or directory Waired alone wrote,
# counted like the settings edits.
function Remove-ClaudeLeftoverPath {
    param([string]$Path)
    if (-not (Test-Path -LiteralPath $Path)) { return }
    $script:ClaudeLeftovers++
    Common-Log "Removing $Path, which Waired left behind"
    Common-Run "Remove-Item $Path" { Remove-Item -LiteralPath $Path -Recurse -Force -ErrorAction SilentlyContinue }
}

# Remove-EmptyDir removes a directory only when nothing is left in it, so a
# user's own skill or cache file under a name Waired also used keeps its home.
function Remove-EmptyDir {
    param([string]$Path)
    if ($DryRun) { return }
    if ((Test-Path -LiteralPath $Path -PathType Container) -and
        @(Get-ChildItem -LiteralPath $Path -Force -ErrorAction SilentlyContinue).Count -eq 0) {
        Remove-Item -LiteralPath $Path -Force -ErrorAction SilentlyContinue
    }
}

# Get-ClaudeManagedSettingsPath is claudemanaged's managedSettingsPath for
# Windows: %ProgramFiles%\ClaudeCode\managed-settings.json.
function Get-ClaudeManagedSettingsPath {
    $pf = $env:ProgramFiles
    if (-not $pf) { $pf = 'C:\Program Files' }
    return (Join-Path (Join-Path $pf 'ClaudeCode') 'managed-settings.json')
}

# MACHINE phase half of waired-agent#1398: the managed settings file.
function Repair-ManagedClaudeSettings {
    $path = Get-ClaudeManagedSettingsPath
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { return }
    # With waired.exe present, a dry run has already printed the step that
    # does this; previewing the same removals again would list them twice.
    if ($DryRun -and (Test-Path -LiteralPath (Join-Path $InstallDir 'waired.exe'))) { return }
    [void](Invoke-ClaudeLeftoverEdit -Kind managed -Path $path)
}

# PER-USER phase half of waired-agent#1398: this user's ~\.claude settings,
# the skills, the retired picker cache, and the retired Stop hook's markers.
function Repair-UserClaudeSettings {
    $userHome = $env:USERPROFILE
    if (-not $userHome) { return }
    if ($DryRun -and (Test-Path -LiteralPath (Join-Path $InstallDir 'waired.exe'))) { return }
    $claudeDir = Join-Path $userHome '.claude'

    $settings = Join-Path $claudeDir 'settings.json'
    if (Test-Path -LiteralPath $settings -PathType Leaf) {
        $edit = Invoke-ClaudeLeftoverEdit -Kind user -Path $settings
        if ($edit -and $edit.Wrapper) {
            foreach ($name in @('waired-statusline.ps1', 'waired-statusline.sh', 'waired-statusline.orig')) {
                Remove-ClaudeLeftoverPath -Path (Join-Path $claudeDir $name)
            }
        }
    }

    $skills = Join-Path $claudeDir 'skills'
    foreach ($name in @('waired-status', 'waired-doctor', 'waired-route')) {
        $dir = Join-Path $skills $name
        Remove-ClaudeLeftoverPath -Path (Join-Path $dir 'SKILL.md')
        Remove-EmptyDir -Path $dir
    }

    $configDir = $env:CLAUDE_CONFIG_DIR
    if (-not $configDir) { $configDir = $claudeDir }
    $cache = Join-Path (Join-Path $configDir 'cache') 'gateway-models.json'
    if (Test-Path -LiteralPath $cache -PathType Leaf) {
        [void](Invoke-ClaudeLeftoverEdit -Kind cache -Path $cache)
    }

    if ($env:LOCALAPPDATA) {
        $cacheRoot = Join-Path $env:LOCALAPPDATA 'waired'
        Remove-ClaudeLeftoverPath -Path (Join-Path $cacheRoot 'claude-fallback')
        Remove-EmptyDir -Path $cacheRoot
    }
}

# Unregister the waired-agent service. Prefer the binary's own uninstall
# (stops + deletes the SCM service and removes the Event Log source exactly
# as install registered them); fall back to native SCM cleanup when the exe
# is gone OR present-but-unrunnable -- e.g. blocked from launching by an
# Application Control Policy (Smart App Control / WDAC / AppLocker). The
# fallback is functionally equivalent (stop, sc.exe delete, DeleteEventSource)
# and launches no blocked exe, so it works even under app-control.
function Remove-WairedService {
    $agent = Join-Path $InstallDir 'waired-agent.exe'

    # Neither the exe nor a registration: there is no service to remove, and
    # saying "removing the service by hand" then running sc.exe delete against
    # nothing is the #630 defect. Note the fall-through below is deliberate
    # when EITHER exists -- an exe with no registration still gets the manual
    # sweep, because `waired-agent uninstall` may have half-finished.
    $registered = Test-Probe { Get-Service -Name $ServiceName -ErrorAction SilentlyContinue }
    if (-not (Test-Path -LiteralPath $agent) -and
        (Skip-Absent -What "the background service" -Present $registered)) { return }

    if (Test-Path -LiteralPath $agent) {
        Common-Log "Unregistering the background service"
        # This is the only step that deregisters the device: the Control Plane
        # call lives inside `waired-agent.exe uninstall`, which self-revokes
        # before tearing the service down. Reaching it is what entitles
        # Show-Done to say the device was deregistered (waired-agent#793).
        $script:Deregistered = $true
        $script:DidCount++
        if ($DryRun) {
            Write-Host "[dry-run] $agent uninstall (also deregisters this device)" -ForegroundColor DarkGray
            return
        }
        $failed = $false
        try {
            & $agent uninstall | Out-Null
            if ($LASTEXITCODE -ne 0) {
                $failed = $true
                Common-Warn "waired-agent.exe uninstall exited with code $LASTEXITCODE. Removing the service by hand."
            }
        } catch {
            $failed = $true
            Common-Warn "waired-agent.exe couldn't run ($($_.Exception.Message.Trim())). Removing the service by hand."
        }
        if (-not $failed) { return }
        # exe present but blocked / failed (e.g. Application Control Policy) - fall through
    } else {
        Common-Log "waired-agent.exe is missing. Removing the service by hand."
    }

    Common-Run "Stop-Service + sc.exe delete $ServiceName" {
        Get-Service -Name $ServiceName -ErrorAction SilentlyContinue |
            Stop-Service -Force -ErrorAction SilentlyContinue
        & sc.exe delete $ServiceName | Out-Null
        try { [System.Diagnostics.EventLog]::DeleteEventSource($ServiceName) } catch { }
    }
}

# Skip-Absent reports "<thing> not present -- skipping" and returns $true when
# there is nothing to do, so a step can announce a removal only when one is
# actually going to happen.
#
# uninstall.sh has gated this way from the start (linux_remove_ollama's
# "Ollama not present - skipping", the apt tier's "no Waired apt packages
# installed"); the PowerShell side announced the removal unconditionally and
# then quietly did nothing, so `-DryRun` previewed removals that could not
# happen and a -Clean transcript could not be read afterwards to tell what had
# actually been removed (#630). The wording is uninstall.sh's, verbatim -- the
# em dash is built at runtime because these scripts must stay pure-ASCII on the
# wire (scripts/install/encoding_test.go).
function Skip-Absent {
    param([string]$What, [bool]$Present)
    if ($Present) { return $false }
    Common-Log "${What}: not present. Skipping."
    return $true
}

# Test-Probe evaluates an existence probe and answers $false if the probe
# itself cannot run.
#
# It exists because these gates moved Windows-only calls out of Common-Run,
# where -DryRun had been skipping them. Get-Service does not exist off Windows
# and HKCU: is not a drive there, and neither failure is suppressible with
# -ErrorAction -- so the gates made the script unrunnable under a non-Windows
# pwsh, which installtest-pwsh.ps1 depends on: it spawns this file as a real
# child to prove `install.ps1 -Clean` delegates the wipe.
#
# "Cannot probe" means "cannot be present", which is the right answer both
# there (nothing is installed on a Linux fixture) and on any Windows host where
# a probe is somehow refused: the step is skipped rather than announced.
function Test-Probe {
    param([scriptblock]$Probe)
    try { return [bool](& $Probe) } catch { return $false }
}

# How long to wait for a terminated tray to actually be gone before moving on
# to deleting InstallDir. TerminateProcess is prompt, so this is a ceiling
# rather than a cost. Same figure as uninstall.sh's TRAY_STOP_GRACE, which
# needs the room for a real reason: there the stop is a signal the tray acts on
# itself, and it must outlast cmd/waired-tray's own shutdown budget.
$TrayStopGraceMs = 15000

# Format-TrayStopLine -- the one line all three uninstallers print when they
# stop a running Waired app, so the user help can quote it once.
#
# Pure, so installtest-windows.ps1 can lift it and pin it byte-for-byte against
# the literal in uninstall.sh's common_stop_tray. ASCII only: this file goes
# over the wire under `iwr|iex` (scripts/install/encoding_test.go).
function Format-TrayStopLine {
    param([int]$Id)
    return "Stopping the Waired app (waired-tray, PID $Id)"
}

# Get-TrayProcesses -- the running trays, or an empty list when the process
# table cannot be read at all. Separate from Test-Probe, which answers a
# yes/no and cannot hand back the objects.
function Get-TrayProcesses {
    try {
        return @(Get-Process -Name 'waired-tray' -ErrorAction SilentlyContinue)
    } catch {
        return @()
    }
}

# Stop-Tray ends a running tray -- so its exe is not locked when we delete
# InstallDir, and so the uninstall does not leave an app behind on the user's
# desktop running a binary that is no longer there (waired-agent#1031).
#
# It names each process it stops. That is the ratified behaviour rather than
# decoration: the owner ruling in
# docs/decisions/20260821/0228-uninstall-removes-what-is-running.md is to
# enumerate what is running, stop it, and say which -- which its sibling
# Stop-ProcessesUnder does and this one did not.
#
# Terminates. There is no graceful stop to try first on Windows, and the
# alternatives were measured rather than assumed (sv-evox2, 2026-08-27,
# waired-agent#1059):
#
#   tray mainwindow=[0] title=[]
#   CloseMainWindow returned=False        -> alive
#   taskkill /IM waired-tray.exe          -> "This process can only be
#                                            terminated forcefully (with /F)"
#
# waired-tray.exe is linked -H windowsgui and never shows a window, so
# Process.MainWindowHandle is 0 and CloseMainWindow can only answer $false;
# taskkill without /F finds nothing to post WM_CLOSE to either. fyne.io/systray
# does handle WM_CLOSE, but its window is never shown, which is what puts it out
# of both callers' reach.
#
# So the wind-down (suspend sharing, stop the engine) does NOT run here, unlike
# the POSIX side where SIGTERM reaches the tray. What frees the engine on
# Windows is the service teardown a few steps below, which happens either way.
# A Windows LOGOUT does wind down, through WM_ENDSESSION -> systray's onExit
# (waired-agent#1059); it is only this path, an outright kill, that cannot.
function Stop-Tray {
    $procs = @(Get-TrayProcesses)
    if (Skip-Absent -What 'waired-tray' -Present ($procs.Count -gt 0)) { return }
    foreach ($p in $procs) { Common-Log (Format-TrayStopLine -Id $p.Id) }
    Common-Run "Stop-Process -Force $(($procs | ForEach-Object { $_.Id }) -join ', ')" {
        foreach ($p in $procs) {
            Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
        }
        foreach ($p in $procs) {
            try { [void]$p.WaitForExit($TrayStopGraceMs) } catch { }
        }
    }
}

# Remove the per-user tray autostart Run key. MUST run in the un-elevated parent
# (as the invoking user): HKCU: resolves to whatever identity the process runs
# as, so doing this post-elevation used to delete the *admin's* key and leave the
# real user's autostart behind (waired#754). Called only from Remove-UserIntegration.
function Remove-TrayAutostart {
    $run = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run'
    $present = Test-Probe { Get-ItemProperty -Path $run -Name 'waired-tray' -ErrorAction SilentlyContinue }
    if (Skip-Absent -What 'the Waired app''s autostart entry' -Present $present) { return }
    Common-Log "Removing the Waired app's autostart entry (current user)"
    Common-Run "Remove-ItemProperty $run\waired-tray" {
        Remove-ItemProperty -Path $run -Name 'waired-tray' -ErrorAction SilentlyContinue
    }
}

# PER-USER phase (runs in the un-elevated parent, as the invoking user). Removes
# the Claude Code + coding-agent integration this user carries -- the managed
# ANTHROPIC_BASE_URL is admin-owned so `claude disable` here tolerates the
# permission miss and still scrubs ~/.claude (route skill + statusline); the
# elevated Remove-ClaudeManaged removes the managed file itself. `unlink` removes
# the ledger'd adapter artifacts (~/.claude skills, opencode/openclaw plugins).
# Plus the HKCU tray autostart and, under -Clean, this user's own state dir.
# waired#754.
function Remove-UserIntegration {
    $exe = Join-Path $InstallDir 'waired.exe'
    if (Test-Path -LiteralPath $exe) {
        Common-Log "Removing per-user Claude / coding-agent integration (as the current user)"
        Common-Run "$exe claude disable" {
            $run = Invoke-WairedExe -Exe $exe -ArgList @('claude', 'disable')
            Write-WairedExeProblem -Run $run -Command 'waired.exe claude disable'
        }
        Common-Run "$exe unlink" {
            [void](Invoke-WairedExe -Exe $exe -ArgList @('unlink'))
        }
    }
    # Whatever the binary did or could not do, look at this user's files
    # themselves (waired-agent#1398).
    Repair-UserClaudeSettings
    Remove-TrayAutostart
    if ($Clean) { Remove-UserStateDir }
}

# -Clean parity for the per-user state dir. Remove-State (elevated) wipes the
# service dir %ProgramData%\waired; a user who ran `waired` directly may also
# have %APPDATA%\waired. Runs as the invoking user so %APPDATA% is the right
# profile's (waired#754).
function Remove-UserStateDir {
    $userState = Join-Path $env:AppData 'waired'
    if (Test-Path -LiteralPath $userState) {
        Common-Log "Removing per-user state directory $userState"
        Common-Run "Remove-Item $userState" {
            Remove-Item -LiteralPath $userState -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}

# Remove the machine-wide "Waired" Start Menu group (best-effort). Both the GUI
# (.iss [Icons]) and the .ps1 installer (waired#755) create one under %ProgramData%.
function Remove-StartMenu {
    $groups = @(
        (Join-Path $env:ProgramData 'Microsoft\Windows\Start Menu\Programs\Waired'),
        (Join-Path $env:AppData    'Microsoft\Windows\Start Menu\Programs\Waired')
    )
    foreach ($g in $groups) {
        if (Test-Path -LiteralPath $g) {
            Common-Log "Removing Start Menu group $g"
            Common-Run "Remove-Item $g" {
                Remove-Item -LiteralPath $g -Recurse -Force -ErrorAction SilentlyContinue
            }
        }
    }
}

# Format-LockHolders -- the "still in use by" clause for a delete that did not
# happen, built from objects carrying Name and Id. Pure, so installtest can
# table-drive it the way it does ConvertTo-NativeArg / Get-ExitCodeReason.
function Format-LockHolders {
    param($Holders)
    # @(...) around the whole pipeline: a Where-Object that matches nothing
    # yields $null, and .Count on $null is an error under Set-StrictMode.
    $list = @(@($Holders) | Where-Object { $_ })
    if ($list.Count -eq 0) { return 'a process this uninstaller couldn''t identify' }
    return ($list | ForEach-Object { "$($_.Name) (PID $($_.Id))" }) -join ', '
}

# Get-LockHolders -- running processes whose image sits under $Path. Windows
# will not delete a running image, so this is what turns "could not be removed"
# into something an operator can act on. Best-effort: Get-Process cannot read
# .Path for a process owned by another user without rights, and those simply do
# not appear.
function Get-LockHolders {
    param([string]$Path)
    try {
        return @(Get-Process -ErrorAction SilentlyContinue | Where-Object {
                $_.Path -and $_.Path.StartsWith($Path, [StringComparison]::OrdinalIgnoreCase)
            } | Select-Object -Property Name, Id)
    } catch {
        return @()
    }
}

# Assert-Removed -- confirm a delete actually happened, and fail loudly when it
# did not.
#
# Every removal here is Remove-Item -ErrorAction SilentlyContinue, so a delete
# Windows refused left no trace: an orphaned `waired init` holding waired.exe
# open produced "Waired fully removed" and exit 0 with a 13 MB binary still on
# disk, and the next install then read that leftover as an existing install and
# declined to do anything -- two green exits and a machine with no service, no
# state and no PATH entry (waired-agent#660).
#
# Skipped under -DryRun, where nothing was deleted and there is nothing to
# verify.
function Assert-Removed {
    param([string]$Path)
    if ($DryRun) { return }
    if (-not (Test-Path -LiteralPath $Path)) { return }
    $who  = Format-LockHolders (Get-LockHolders -Path $Path)
    Common-Die "$Path couldn't be removed: it's still in use by $who. Close it and run this uninstaller again."
}

# Stop anything still running out of InstallDir, so the delete below is not
# racing a live image.
#
# Windows will not delete a running binary. rc9's real-hardware pass found
# that an orphaned `waired.exe init` holding waired.exe open did NOT actually
# block the uninstall -- unregistering the service first made the init exit,
# the lock released, and removal proceeded -- but nothing said so, and the
# outcome depended on the locker being a waired process that happens to quit
# when the daemon goes away. A third-party locker (an editor, an antivirus
# scan, a shell whose cwd is InstallDir) would still have reached
# Assert-Removed and failed the run.
#
# Owner ruling (2026-08-21, on waired-agent#793's request for triage): an
# uninstall must remove Waired even when Waired is running. So stop our own
# processes deliberately and say which, rather than relying on a side effect.
# Assert-Removed stays as the backstop for the locks this cannot clear -- it
# is what turns a refused delete into a named failure instead of a false
# "fully removed" (waired-agent#660).
function Stop-ProcessesUnder {
    param([string]$Path)
    $holders = @(Get-LockHolders -Path $Path)
    if ($holders.Count -eq 0) { return }
    foreach ($p in $holders) {
        Common-Log "$($p.Name) (PID $($p.Id)) is still running from $Path - stopping it before removal."
    }
    Common-Run "Stop-Process $(($holders | ForEach-Object { $_.Id }) -join ', ')" {
        foreach ($p in $holders) {
            Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
        }
    }
}

# Remove a directory, giving a process that is already on its way out the
# few seconds Windows needs to let go of its image.
#
# The service teardown above closes the daemon's job object, which
# terminates the engine tree it spawned -- but SCM reports "Stopped"
# before the kernel has finished the exits and released the image
# handles, so a delete fired immediately after it can still meet
# llama-server.exe holding runtimes\ollama. Measured on a Windows host
# running rc5 (waired-agent#1174): the first -Clean run failed on exactly
# that tree, and the process was gone one second later.
#
# Retry, not a longer sleep: the ordinary case is already free and must
# stay instant. The pattern is install.ps1's wait after sc.exe delete.
function Remove-DirWithGrace {
    param([string]$Path, [int]$GraceMs = 10000)
    Remove-Item -LiteralPath $Path -Recurse -Force -ErrorAction SilentlyContinue
    if (-not (Test-Path -LiteralPath $Path)) { return }
    $deadline = (Get-Date).AddMilliseconds($GraceMs)
    while ((Get-Date) -lt $deadline) {
        Start-Sleep -Milliseconds 250
        Remove-Item -LiteralPath $Path -Recurse -Force -ErrorAction SilentlyContinue
        if (-not (Test-Path -LiteralPath $Path)) { return }
    }
}

function Remove-InstallDir {
    Stop-ProcessesUnder -Path $InstallDir
    if (Test-OnMachinePath -Dir $InstallDir) {
        Common-Log "Removing $InstallDir from the system PATH"
        Remove-FromMachinePath -Dir $InstallDir
    } else {
        [void](Skip-Absent -What "$InstallDir on the system PATH" -Present $false)
    }
    if (Test-Path -LiteralPath $InstallDir) {
        Common-Log "Removing $InstallDir"
        Common-Run "Remove-Item $InstallDir" {
            Remove-DirWithGrace -Path $InstallDir
        }
        Assert-Removed -Path $InstallDir
    }
    # Drop the install-location record install.ps1 / the GUI installer wrote
    # (HKLM\SOFTWARE\Waired\InstallDir) so nothing points at the removed dir.
    if (Test-Path -LiteralPath $InstallDirRegKey) {
        Common-Run "Remove-Item $InstallDirRegKey" {
            Remove-Item -Path $InstallDirRegKey -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}

function Remove-State {
    if (Test-Path -LiteralPath $StateDir) {
        # The bundled engine lives under $StateDir, so the same treatment
        # $InstallDir has had since waired-agent#793: stop what is running
        # out of the directory, by name, before removing it. Nothing else
        # in this script stops the engine -- Remove-Ollama is about the
        # pre-#493 locations and runs after this (waired-agent#1174).
        Stop-ProcessesUnder -Path $StateDir
        Common-Log "Removing state directory $StateDir (identity, keys, settings)"
        Common-Run "Remove-Item $StateDir" {
            Remove-DirWithGrace -Path $StateDir
        }
        Assert-Removed -Path $StateDir
    }
}

# -Clean only: remove an Ollama at the pre-#493 locations, its machine-PATH
# entry, the OLLAMA_MODELS / OLLAMA_VULKAN / OLLAMA_IGPU_ENABLE machine env
# vars and the model stores. Best-effort + existence-gated throughout.
#
# waired's own engine is not here any more: it lives inside %ProgramData%\waired
# and goes with the state dir, which -Clean already wipes. What remains is
# migration cleanup for hosts installed before #493, plus the user's own
# per-user Ollama, which -Clean has always removed because -Clean means
# "including models".

# The pre-#493 Ollama install locations, its model stores, and the machine env
# vars the old PowerShell installer wrote. Named once and used by both the
# existence probe and the removal below, so the two can never disagree about
# what "present" means.
function Get-OllamaDirs {
    return @(
        (Join-Path $env:ProgramFiles  'Ollama'),
        (Join-Path $env:LOCALAPPDATA 'Programs\Ollama')
    )
}
function Get-OllamaModelHomes {
    return @(
        (Join-Path $env:USERPROFILE '.ollama'),
        'C:\Windows\System32\config\systemprofile\.ollama'
    )
}
$OllamaEnvVars = @('OLLAMA_MODELS', 'OLLAMA_VULKAN', 'OLLAMA_IGPU_ENABLE')

# Get-OllamaStageDirs -- the ~1.4 GB staging directories a killed engine
# download left behind (#191). Both temp roots: the elevated user's, and
# LocalSystem's, since the daemon-path setup executor can run the installer as
# SYSTEM.
function Get-OllamaStageDirs {
    $roots = @($env:TEMP, (Join-Path $env:SystemRoot 'Temp')) |
        Where-Object { $_ } | Select-Object -Unique
    $out = @()
    foreach ($t in $roots) {
        if (-not (Test-Path -LiteralPath $t)) { continue }
        foreach ($d in @(Get-ChildItem -LiteralPath $t -Directory -Filter 'ollama-stage-*' -ErrorAction SilentlyContinue)) {
            $out += $d.FullName
        }
    }
    return $out
}

# Test-OllamaPresent -- is there anything here for Remove-Ollama to do? Every
# trace it knows how to remove is probed, not just one path, so a "not present"
# answer is trustworthy rather than a guess.
function Test-OllamaPresent {
    if (Test-Probe { Get-Process -Name 'ollama*' -ErrorAction SilentlyContinue }) { return $true }
    foreach ($d in (Get-OllamaDirs))        { if (Test-Path -LiteralPath $d) { return $true } }
    foreach ($m in (Get-OllamaModelHomes))  { if (Test-Path -LiteralPath $m) { return $true } }
    foreach ($v in $OllamaEnvVars) {
        if (Test-Probe { [Environment]::GetEnvironmentVariable($v, 'Machine') }) { return $true }
    }
    # @(...) around the call, not just inside the function: a PowerShell
    # function returning an empty array yields nothing, so the caller sees
    # $null -- and .Count on $null is an error under Set-StrictMode, which
    # installtest-pwsh.ps1 has on when it runs this script in-process. Same
    # normalise-before-you-count rule install.ps1 records at its own count
    # sites.
    if (@(Get-OllamaStageDirs).Count -gt 0) { return $true }
    return $false
}

function Remove-Ollama {
    if (Skip-Absent -What 'Ollama' -Present (Test-OllamaPresent)) { return }
    Common-Log "Removing Ollama (binary, models, PATH, env)"
    if (Test-Probe { Get-Process -Name 'ollama*' -ErrorAction SilentlyContinue }) {
        Common-Run "Stop-Process ollama*" {
            Get-Process -Name 'ollama*' -ErrorAction SilentlyContinue |
                Stop-Process -Force -ErrorAction SilentlyContinue
        }
    }

    foreach ($d in (Get-OllamaDirs)) {
        Remove-FromMachinePath -Dir $d
        if (Test-Path -LiteralPath $d) {
            Common-Run "Remove-Item $d" {
                Remove-Item -LiteralPath $d -Recurse -Force -ErrorAction SilentlyContinue
            }
        }
    }

    # Model store: OLLAMA_MODELS (machine env), then the default per-profile
    # locations (the user's, and LocalSystem's when the service ran inference).
    $models = [Environment]::GetEnvironmentVariable('OLLAMA_MODELS', 'Machine')
    if ($models -and (Test-Path -LiteralPath $models)) {
        Common-Run "Remove-Item $models" {
            Remove-Item -LiteralPath $models -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
    # OLLAMA_MODELS, plus the GPU-backend flags the pre-#493 PowerShell
    # installer wrote at Machine scope (OLLAMA_VULKAN=1 + OLLAMA_IGPU_ENABLE=1).
    # Clearing those matters or a "clean" uninstall silently re-tunes any
    # later/other Ollama on this host; the agent supplies them at spawn now and
    # never writes them.
    #
    # One line per variable that is actually set: announcing a clear for a
    # variable that was never there is the same false report as announcing a
    # removal for a directory that does not exist (#630).
    foreach ($v in $OllamaEnvVars) {
        if (-not [Environment]::GetEnvironmentVariable($v, 'Machine')) { continue }
        Common-Run "clear $v (system environment)" {
            [Environment]::SetEnvironmentVariable($v, $null, 'Machine')
        }
    }
    foreach ($m in (Get-OllamaModelHomes)) {
        if (Test-Path -LiteralPath $m) {
            Common-Run "Remove-Item $m" {
                Remove-Item -LiteralPath $m -Recurse -Force -ErrorAction SilentlyContinue
            }
        }
    }

    # (#191) Staging directories left by killed engine downloads: ~1.4 GB
    # each, and until now nothing swept them -- not even a -Clean uninstall,
    # so the disk stayed occupied after every trace of Ollama was gone.
    #
    # Migration cleanup since #493: the Go installer stages inside the state
    # dir, on the volume it is about to extract onto, and sweeps its own
    # leftovers at the start of every run.
    foreach ($stage in (Get-OllamaStageDirs)) {
        Common-Run "Remove-Item $stage" {
            Remove-Item -LiteralPath $stage -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}

# Show-Done reports what happened. Every claim here is conditioned on a step
# having actually run: a machine with nothing installed used to be told
# "Waired fully removed (state wiped)" and "This device was deregistered",
# neither of which had an object, and a -DryRun run said the same words as a
# real one (waired-agent#793). Mirrors uninstall.sh's print_done.
function Show-Done {
    Section 'Done'
    $tag = ''
    if ($DryRun) { $tag = '[dry-run] ' }

    if ($script:DidCount -eq 0) {
        if ($DryRun) {
            Common-Log "${tag}Nothing would be removed: Waired isn't installed on this computer."
        } else {
            Common-Log "Nothing to remove: Waired wasn't installed on this computer."
        }
        return
    }

    # Everything this run did was Claude Code settings Waired left behind:
    # Waired itself was already gone (waired-agent#1398).
    if ($script:DidCount -eq $script:ClaudeLeftovers) {
        if ($DryRun) {
            Common-Log "${tag}Waired isn't installed, but Claude Code still has settings it left behind. They would be removed."
        } else {
            Common-Log "Waired wasn't installed, but Claude Code still had settings it left behind. They're removed. Restart Claude Code to pick that up."
        }
        return
    }

    if ($DryRun) {
        if ($Clean) {
            Common-Log "${tag}Waired would be removed, with its state."
        } else {
            Common-Log "${tag}Waired would be removed. Local state would be kept under $StateDir; re-run with -Clean to wipe it."
        }
    } elseif ($Clean) {
        Common-Log "Waired removed, with its state. Open a new shell to refresh the PATH."
    } else {
        Common-Log "Waired removed. Local state was kept under $StateDir; re-run with -Clean to wipe it."
    }

    if ($script:Deregistered) {
        if ($DryRun) {
            Common-Log "${tag}This device would be deregistered from your Waired account (best-effort)."
        } else {
            Common-Log "This device was deregistered from your Waired account (best-effort). If it was"
            Common-Log "offline during uninstall, remove it from the web admin device list."
        }
    } else {
        Common-Log "No Waired registration was found on this computer, so nothing was deregistered."
    }
}

# -------------------------------------------------------------------
# main
# -------------------------------------------------------------------

if ($Help) { Show-Help; exit 0 }

# Prune old per-run transcripts. Un-elevated parent only: the elevated child
# must never sweep the invoking user's %TEMP%, and this sits after -Help so a
# read-only invocation touches nothing. Mirrors install.ps1.
if (-not $FromElevation) { Remove-OldRunLogs -Prefix 'waired-uninstall' }

# The spawned elevated console closes the instant the script returns, taking
# every message with it. Make it liveable: kill conhost QuickEdit (a stray
# click otherwise freezes output), record a transcript, and (below) pause
# before exiting so the outcome is actually readable. waired#748 parity.
if ($FromElevation) {
    $script:ElevatedConsole = $true
    Disable-QuickEdit
    try { Start-Transcript -Path $LogPath -Force -ErrorAction SilentlyContinue | Out-Null } catch { }
}

# Review + confirm before ANY change (per-user teardown included) and before
# elevating, so the prompt reaches the real console. The elevated child
# skips it (consent was collected here).
Confirm-Uninstall

# Per-user teardown as the INVOKING user: run in this un-elevated parent (or
# inline when already admin) so HKCU / %APPDATA% / ~/.claude edits land in the
# right hive & profile, not the admin's after UAC. The re-elevated child sets
# -FromElevation and skips this (it would target the admin's profile). waired#754.
if (-not $FromElevation) {
    Remove-UserIntegration
}

# Elevate for the machine-scoped steps (skipped for -DryRun: just print).
if (-not $DryRun -and -not (Test-IsAdmin)) {
    Invoke-SelfElevate
    # The elevated window paused for the operator and closed; leave a recap in
    # THIS (persistent) console so the outcome survives it.
    #
    # NOT Show-Done: the machine-scoped steps ran in the child process, so
    # this one's counters are empty and Show-Done would report "nothing to
    # remove" over a completed uninstall. Say what this process actually
    # knows, the way install.ps1's post-elevation recap does; the child
    # printed the itemised summary in its own window and in the log.
    Section 'Done'
    Common-Log "Uninstall finished in the Administrator window (full log: $LogPath)."
    if ($script:ClaudeLeftovers -gt 0) {
        Common-Log "Settings Waired left in your Claude Code configuration were removed. Restart Claude Code to pick that up."
    }
    if ($Clean) {
        Common-Log "Open a new shell to refresh PATH."
    } else {
        Common-Log "Local state under $StateDir was kept; re-run with -Clean to wipe it."
    }
    exit 0
}

Section 'Removing Waired'
Common-Log "Uninstalling Waired..."
Remove-ClaudeManaged
Remove-WairedService
Stop-Tray
Remove-StartMenu
Remove-InstallDir
if ($Clean) {
    Remove-State
    Remove-Ollama
}
Show-Done

if ($FromElevation) {
    Stop-TranscriptQuietly
    if (Test-InteractiveStdin) {
        Read-Host '[waired] Uninstall complete. Press Enter to close this window' | Out-Null
    }
}

# Say 0 rather than falling off the end with whatever $LASTEXITCODE the last
# native command happened to leave. `sc.exe delete` returns 1060
# (ERROR_SERVICE_DOES_NOT_EXIST) on a host whose service is already gone, which
# is a perfectly successful uninstall -- and that 1060 became the script's exit
# code, so a caller checking it saw a failure where the transcript said "Waired
# fully removed". The mirror of #660: an exit code that does not mean what it
# says. Every real failure leaves through Common-Die or the trap, both exit 1.
exit 0
