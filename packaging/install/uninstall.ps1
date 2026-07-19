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
if (-not $LogPath) { $LogPath = Join-Path $env:TEMP 'waired-uninstall.log' }

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

function Common-Log  { param([string]$Msg) Write-Host "[waired] $Msg" -ForegroundColor Cyan }
function Common-Warn { param([string]$Msg) Write-Host "[waired] $Msg" -ForegroundColor Yellow }

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

function Common-Die  {
    param([string]$Msg)
    Write-Host "[waired] $Msg" -ForegroundColor Red
    if ($script:ElevatedConsole) {
        if ($script:LogPath) { Write-Host "[waired] Full uninstall log: $($script:LogPath)" -ForegroundColor Red }
        Stop-TranscriptQuietly
        if (Test-InteractiveStdin) {
            Read-Host '[waired] Uninstall FAILED. Press Enter to close this window' | Out-Null
        }
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
    if ($Clean)  { $passthrough += '-Clean' }
    if ($DryRun) { $passthrough += '-DryRun' }

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

    try {
        $proc = Start-Process -FilePath 'powershell.exe' `
            -ArgumentList $psArgs -Verb RunAs -PassThru -Wait
        if ($proc.ExitCode -ne 0) {
            Common-Die "elevated uninstaller exited code $($proc.ExitCode). Full uninstall log: $LogPath"
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
function Remove-Service {
    $agent = Join-Path $InstallDir 'waired-agent.exe'

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

# Stop the tray process so its exe is not locked when we delete InstallDir.
function Stop-Tray {
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
# the ledger'd adapter artifacts (~/.claude skills, opencode/openclaw plugins).
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

function Remove-InstallDir {
    Common-Log "Removing $InstallDir from machine PATH"
    Remove-FromMachinePath -Dir $InstallDir
    if (Test-Path -LiteralPath $InstallDir) {
        Common-Log "Removing $InstallDir"
        Common-Run "Remove-Item $InstallDir" {
            Remove-Item -LiteralPath $InstallDir -Recurse -Force -ErrorAction SilentlyContinue
        }
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
    }
}

# -Clean only: remove an Ollama installed by ollama-windows.ps1 (or the
# official Windows installer), its machine-PATH entry, the OLLAMA_MODELS /
# OLLAMA_VULKAN / OLLAMA_IGPU_ENABLE machine env vars and the model stores.
# Best-effort + existence-gated throughout.
function Remove-Ollama {
    Common-Log "Removing Ollama (binary, models, PATH, env)"
    Common-Run "Stop-Process ollama*" {
        Get-Process -Name 'ollama*' -ErrorAction SilentlyContinue |
            Stop-Process -Force -ErrorAction SilentlyContinue
    }

    $dirs = @(
        (Join-Path $env:ProgramFiles  'Ollama'),
        (Join-Path $env:LOCALAPPDATA 'Programs\Ollama')
    )
    foreach ($d in $dirs) {
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
    Common-Run "clear OLLAMA_MODELS (machine env)" {
        [Environment]::SetEnvironmentVariable('OLLAMA_MODELS', $null, 'Machine')
    }
    # GPU-backend flags ollama-windows.ps1's Set-MachineVulkanFlag wrote at
    # Machine scope (OLLAMA_VULKAN=1 + OLLAMA_IGPU_ENABLE=1). Clear them too, or
    # a "clean" uninstall silently re-tunes any later/other Ollama on this host.
    Common-Run "clear OLLAMA_VULKAN (machine env)" {
        [Environment]::SetEnvironmentVariable('OLLAMA_VULKAN', $null, 'Machine')
    }
    Common-Run "clear OLLAMA_IGPU_ENABLE (machine env)" {
        [Environment]::SetEnvironmentVariable('OLLAMA_IGPU_ENABLE', $null, 'Machine')
    }
    $modelHomes = @(
        (Join-Path $env:USERPROFILE '.ollama'),
        'C:\Windows\System32\config\systemprofile\.ollama'
    )
    foreach ($m in $modelHomes) {
        if (Test-Path -LiteralPath $m) {
            Common-Run "Remove-Item $m" {
                Remove-Item -LiteralPath $m -Recurse -Force -ErrorAction SilentlyContinue
            }
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
Remove-Service
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
