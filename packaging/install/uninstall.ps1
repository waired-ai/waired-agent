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
                settings, ~/.claude skills, the openclaw plugin, and the
                withdrawn opencode plugin), but
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
function Common-Warn { param([string]$Msg) Write-Host "[waired] $(Protect-PII $Msg)" -ForegroundColor Yellow }

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
            Read-Host '[waired] Uninstall FAILED. Press Enter to close this window' | Out-Null
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
                Read-Host '[waired] Uninstall FAILED. Press Enter to close this window' | Out-Null
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

# Common-Run runs a scriptblock, or prints its description in dry-run mode.
function Common-Run {
    param([string]$Description, [scriptblock]$Action)
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
        -1073741502 { return 'the elevated PowerShell could not start (DLL initialization failed)' }                             # 0xC0000142
        -1073741819 { return 'the elevated installer stopped with an access violation' }                                         # 0xC0000005
        -1073741515 { return 'the elevated PowerShell could not start (a required DLL was missing)' }                            # 0xC0000135
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
uninstall.ps1 - remove Waired on Windows.

Usage:
  iwr -useb https://github.com/waired-ai/waired-agent/releases/latest/download/uninstall.ps1 | iex
  # For -Clean, download then run (iex strips named parameters):
  iwr -useb .../uninstall.ps1 -OutFile uninstall.ps1; .\uninstall.ps1 -Clean

By default removes the Waired binaries + service but KEEPS your local state
(%ProgramData%\waired: identity, keys, settings). Either tier also best-effort
deregisters this device from your Waired account (removed from your device list).

Options:
  -Clean    also delete state (%ProgramData%\waired) and Ollama (binary +
            downloaded models). Destructive - asks to confirm unless -Yes.
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
    Write-Host "  * The waired-agent background service + Start Menu / tray entries"
    Write-Host "  * The Claude Code / coding-agent integration for this user"
    Write-Host "  * This device's registration in your Waired account (best-effort)"
    if ($Clean) {
        Write-Host "  * ALL local state: config, keys, identity ($StateDir)" -ForegroundColor Yellow
        Write-Host "  * Ollama and its downloaded models (PERMANENT)" -ForegroundColor Yellow
    } else {
        Write-Host "  (local state under $StateDir is KEPT; re-run with -Clean to wipe it)"
    }

    if ($Yes -or $DryRun) { return }
    if (-not (Test-InteractiveStdin)) {
        if ($Clean) {
            Common-Die "-Clean is destructive; re-run with -Yes to confirm on a non-interactive session"
        }
        Common-Log "No interactive console detected -- proceeding without confirmation (use -Yes to silence this notice)."
        return
    }
    Write-Host ''
    $reply = Read-Host '[waired] Proceed with the uninstall? [y/N] (Enter = No)'
    if ($reply -notmatch '^(y|yes)$') { Common-Die "aborted - nothing was removed" }
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
    Common-Log "Privileged step ahead -- requesting UAC..."
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
            Common-Die "elevated uninstaller exited code $code$tail. Full uninstall log: $LogPath"
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
    Common-Run "machine PATH -= $Dir" {
        [Environment]::SetEnvironmentVariable('Path', $newPath, 'Machine')
    }
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
    if (-not (Test-Path -LiteralPath $exe)) { return }
    Common-Log "Removing Claude Code managed settings (+ any retired MITM proxy artifacts)"
    Common-Run "$exe claude disable" {
        try { & $exe claude disable 2>$null | Out-Null } catch { }
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
        (Skip-Absent -What "the $ServiceName service" -Present $registered)) { return }

    if (Test-Path -LiteralPath $agent) {
        Common-Log "Unregistering the waired-agent service"
        if ($DryRun) {
            Write-Host "[dry-run] $agent uninstall" -ForegroundColor DarkGray
            return
        }
        $failed = $false
        try {
            & $agent uninstall | Out-Null
            if ($LASTEXITCODE -ne 0) {
                $failed = $true
                Common-Warn "waired-agent.exe uninstall exited $LASTEXITCODE - falling back to manual SCM cleanup"
            }
        } catch {
            $failed = $true
            Common-Warn "waired-agent.exe could not run ($($_.Exception.Message.Trim())) - falling back to manual SCM cleanup"
        }
        if (-not $failed) { return }
        # exe present but blocked / failed (e.g. Application Control Policy) - fall through
    } else {
        Common-Log "waired-agent.exe missing - removing the service by hand"
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
    $dash = Emo ([char]::ConvertFromUtf32(0x2014)) '-'
    Common-Log "$What not present $dash skipping"
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

# Stop the tray process so its exe is not locked when we delete InstallDir.
function Stop-Tray {
    if (Skip-Absent -What 'waired-tray' `
            -Present (Test-Probe { Get-Process -Name 'waired-tray' -ErrorAction SilentlyContinue })) { return }
    Common-Run "Stop-Process waired-tray" {
        Get-Process -Name 'waired-tray' -ErrorAction SilentlyContinue |
            Stop-Process -Force -ErrorAction SilentlyContinue
    }
}

# Remove the per-user tray autostart Run key. MUST run in the un-elevated parent
# (as the invoking user): HKCU: resolves to whatever identity the process runs
# as, so doing this post-elevation used to delete the *admin's* key and leave the
# real user's autostart behind (waired#754). Called only from Remove-UserIntegration.
function Remove-TrayAutostart {
    $run = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run'
    $present = Test-Probe { Get-ItemProperty -Path $run -Name 'waired-tray' -ErrorAction SilentlyContinue }
    if (Skip-Absent -What 'the waired-tray autostart entry' -Present $present) { return }
    Common-Log "Removing the waired-tray autostart entry (current user)"
    Common-Run "Remove-ItemProperty $run\waired-tray" {
        Remove-ItemProperty -Path $run -Name 'waired-tray' -ErrorAction SilentlyContinue
    }
}

# PER-USER phase (runs in the un-elevated parent, as the invoking user). Removes
# the Claude Code + coding-agent integration this user carries -- the managed
# ANTHROPIC_BASE_URL is admin-owned so `claude disable` here tolerates the
# permission miss and still scrubs ~/.claude (route skill + statusline); the
# elevated Remove-ClaudeManaged removes the managed file itself. `unlink` removes
# the ledger'd adapter artifacts (~/.claude skills, the openclaw plugin) plus the
# withdrawn OpenCode integration's leftovers (waired-agent#333, drop one release
# after it shipped).
# Plus the HKCU tray autostart and, under -Clean, this user's own state dir.
# waired#754.
function Remove-UserIntegration {
    $exe = Join-Path $InstallDir 'waired.exe'
    if (Test-Path -LiteralPath $exe) {
        Common-Log "Removing per-user Claude / coding-agent integration (as the current user)"
        Common-Run "$exe claude disable" {
            try { & $exe claude disable 2>$null | Out-Null } catch { }
        }
        Common-Run "$exe unlink" {
            try { & $exe unlink 2>$null | Out-Null } catch { }
        }
    }
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
    if ($list.Count -eq 0) { return 'a process this uninstaller could not identify' }
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
    $dash = Emo ([char]::ConvertFromUtf32(0x2014)) '-'
    $who  = Format-LockHolders (Get-LockHolders -Path $Path)
    Common-Die "$Path could not be removed $dash it is still in use by $who. Close it and run this uninstaller again."
}

function Remove-InstallDir {
    if (Test-OnMachinePath -Dir $InstallDir) {
        Common-Log "Removing $InstallDir from machine PATH"
        Remove-FromMachinePath -Dir $InstallDir
    } else {
        [void](Skip-Absent -What "$InstallDir on the machine PATH" -Present $false)
    }
    if (Test-Path -LiteralPath $InstallDir) {
        Common-Log "Removing $InstallDir"
        Common-Run "Remove-Item $InstallDir" {
            Remove-Item -LiteralPath $InstallDir -Recurse -Force -ErrorAction SilentlyContinue
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
        Common-Log "Removing state directory $StateDir (identity, keys, settings)"
        Common-Run "Remove-Item $StateDir" {
            Remove-Item -LiteralPath $StateDir -Recurse -Force -ErrorAction SilentlyContinue
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
        Common-Run "clear $v (machine env)" {
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

function Show-Done {
    Section 'Done'
    if ($Clean) {
        Common-Log "Waired fully removed (state wiped). Open a new shell to refresh PATH."
    } else {
        Common-Log "Waired removed. Local state kept under $StateDir; re-run with -Clean to wipe it."
    }
    Common-Log "This device was deregistered from your Waired account (best-effort). If it was"
    Common-Log "offline during uninstall, remove it from the web admin device list."
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
    # The elevated window paused for the operator and closed; repeat the
    # outcome in THIS (persistent) console so it survives.
    Show-Done
    Common-Log "Full uninstall log: $LogPath"
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
