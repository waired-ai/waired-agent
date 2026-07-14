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
    # user, so HKCU / %APPDATA% / ~/.claude hit the right hive). waired#754.
    [switch]$FromElevation
)

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'

# -------------------------------------------------------------------
# Configuration (mirrors install.ps1)
# -------------------------------------------------------------------

$InstallDir  = Join-Path $env:ProgramFiles 'Waired'
$ServiceName = 'waired-agent'
# SCM-mode state dir written by install.ps1 / the GUI installer.
$StateDir    = if ($env:WAIRED_STATE_DIR) { $env:WAIRED_STATE_DIR } `
               else { Join-Path $env:ProgramData 'waired' }
# Public mirror base for the elevated self-re-fetch (iex case). Mirrors
# install.ps1's WAIRED_INSTALL_BASE_URL default shape.
$BaseUrl     = if ($env:WAIRED_INSTALL_BASE_URL) { $env:WAIRED_INSTALL_BASE_URL } `
               else { 'https://github.com/waired-ai/waired-agent/releases' }

# -------------------------------------------------------------------
# common_* helpers (mirror install.ps1 naming)
# -------------------------------------------------------------------

function Common-Log  { param([string]$Msg) Write-Host "[waired] $Msg" -ForegroundColor Cyan }
function Common-Warn { param([string]$Msg) Write-Host "[waired] $Msg" -ForegroundColor Yellow }
function Common-Die  {
    param([string]$Msg)
    Write-Host "[waired] $Msg" -ForegroundColor Red
    exit 1
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
  -Yes      assume "yes" to the -Clean confirmation (required when piped /
            non-interactive)
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

# Confirm the destructive -Clean wipe. Skipped without -Clean. -Yes bypasses;
# a non-interactive session without -Yes aborts so a piped invocation can
# never silently wipe state. Runs in the un-elevated parent so the prompt
# reaches a real console before UAC hands the child a fresh stdin.
function Confirm-Clean {
    if (-not $Clean) { return }
    if ($Yes) { return }
    $interactive = $false
    try { $interactive = -not [Console]::IsInputRedirected }
    catch { $interactive = [Environment]::UserInteractive }
    if ($interactive) {
        Common-Warn "-Clean will PERMANENTLY delete Waired config, keys and state,"
        Common-Warn "and Ollama + its downloaded models."
        $reply = Read-Host "[waired] Continue? [y/N]"
        if ($reply -notmatch '^(y|yes)$') { Common-Die "aborted - nothing was removed" }
        return
    }
    Common-Die "-Clean is destructive; re-run with -Yes to confirm on a non-interactive session"
}

# Re-invoke this script elevated. SCM, HKLM PATH and cert stores all need
# admin. Consent for -Clean was already obtained in the un-elevated parent
# (Confirm-Clean), so -Yes is forwarded to keep the child non-interactive.
# Mirrors install.ps1's Invoke-SelfElevate (no sudo.exe: Start-Process
# -Verb RunAs is universal back to Windows 10 1809).
function Invoke-SelfElevate {
    Common-Log "Privileged step ahead -- requesting UAC..."
    $passthrough = @('-FromElevation')
    if ($Clean)  { $passthrough += @('-Clean', '-Yes') }
    if ($DryRun) { $passthrough += '-DryRun' }

    $psArgs = @('-NoProfile', '-ExecutionPolicy', 'Bypass')
    if ($PSCommandPath) {
        $psArgs += @('-File', $PSCommandPath) + $passthrough
    } else {
        # Sourced via `iwr | iex`: $PSCommandPath is null. Re-fetch the
        # script body and bind params through a call-operator block (iex
        # itself strips param() bindings).
        $url  = "$BaseUrl/latest/download/uninstall.ps1"
        $literal = $passthrough -join ' '
        $bootstrap = "`$r = (iwr -useb '$url').Content; if (`$r -is [byte[]]) { `$r = [System.Text.Encoding]::UTF8.GetString(`$r) }; & ([ScriptBlock]::Create(`$r)) $literal"
        $psArgs += @('-Command', $bootstrap)
    }

    $proc = Start-Process -FilePath 'powershell.exe' `
        -ArgumentList $psArgs -Verb RunAs -PassThru -Wait
    if ($proc.ExitCode -ne 0) {
        Common-Die "elevated uninstaller exited code $($proc.ExitCode)"
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
# official Windows installer), its machine-PATH entry, the OLLAMA_MODELS
# env var and the model stores. Best-effort + existence-gated throughout.
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

# Confirm before elevating so the prompt reaches the real console.
Confirm-Clean

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
    exit 0
}

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
