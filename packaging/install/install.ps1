#Requires -Version 5.1
<#
.SYNOPSIS
    Waired one-liner installer for Windows.

.DESCRIPTION
    End-user-facing entry point. Designed to be hosted on the public
    waired-agent GitHub Releases and run via:

        iwr -useb $BaseUrl/latest/download/install.ps1 | iex

    ($BaseUrl is the public mirror base -- see the assignment near the top
    of the script. The examples below write it that way deliberately: a
    contiguous literal "iwr -useb <full-url> | iex" inside the script body
    reads as an in-memory download-and-execute cradle to Windows Defender's
    AMSI heuristics and can get the whole script blocked. The live `-Help`
    output, packaging/install/README.md, and the docs site show the full,
    copy-pasteable URL.)

    The script:
        1. Re-launches itself elevated when not already Administrator
           (UAC prompt).
        2. Downloads `waired-windows-amd64.zip` + `.sha256` from the
           public mirror, verifies the hash.
        3. Stops any existing `waired-agent` service.
        4. Extracts the zip to %ProgramFiles%\Waired\.
        5. Hands off to `waired-agent.exe install`, which registers the
           Windows Service, the Event Log source, and applies the
           restrictive DACL on the state directory. SCM register logic
           is NOT duplicated here.
        6. Prints next-step instructions that mirror the Linux
           install.sh "Next steps" block.

    The Linux counterpart is packaging/install/install.sh -- keep this
    script's UX (env vars, --dry-run, --help) parallel to it.

    For developers building from a repo checkout, see
    scripts/install/waired-agent-windows.ps1 instead -- that script takes
    a pre-built local exe and skips the download path.

.PARAMETER DryRun
    Print every privileged command without running it.

.PARAMETER Help
    Print help and exit.

.PARAMETER Dev
    Pre-configure this install for the built-in dogfood Control Plane
    (https://app.dev.waired.net). The URL is substituted into the Next-steps
    `waired.exe init --control "<URL>"` command so enrolment is single-step.

.PARAMETER Control
    Same as -Dev but with an explicit URL. Takes precedence over -Dev when
    both are given.

.PARAMETER Edge
    Install the latest main build (same as WAIRED_VERSION=edge) -- rebuilt on
    every merge to main; NOT a stable release. -Latest is an alias.

.PARAMETER Stable
    Install/switch to the latest stable release. On -Update/-Check this
    overrides the default, which preserves the channel the host already tracks
    (edge stays edge, stable stays stable).

.PARAMETER Clean
    Clean install: run the uninstaller with -Clean first (PERMANENTLY deletes
    config, keys, state, and Ollama + its models), then install fresh.
    Destructive -- asks to confirm unless -Yes. Env-var form: WAIRED_CLEAN=1
    (how the piped `iwr | iex` one-liner opts in, since iex strips switches).

.EXAMPLE
    iwr -useb $BaseUrl/latest/download/install.ps1 | iex

.EXAMPLE
    # Latest main build (edge channel)
    $env:WAIRED_VERSION = 'edge'
    iwr -useb $BaseUrl/latest/download/install.ps1 | iex

.EXAMPLE
    # Clean install: full wipe (uninstall.ps1 -Clean), then reinstall fresh
    $env:WAIRED_CLEAN = '1'
    iwr -useb $BaseUrl/latest/download/install.ps1 | iex

.EXAMPLE
    # Dogfood (dev-main Control Plane). Save to a file first: this avoids the
    # Windows PowerShell 5.1 octet-stream byte[] pitfall and lets the UAC
    # self-elevation re-launch the same file with -Dev preserved.
    $f = "$env:TEMP\waired-install.ps1"
    iwr -useb $BaseUrl/latest/download/install.ps1 -OutFile $f
    & $f -Dev

.EXAMPLE
    # Pin to a specific release (the leading v is optional)
    $env:WAIRED_VERSION = '1.2.3-rc1'
    iwr -useb $BaseUrl/latest/download/install.ps1 | iex

.EXAMPLE
    # Headless server: skip tray
    $env:WAIRED_NO_TRAY = '1'
    iwr -useb $BaseUrl/latest/download/install.ps1 | iex
#>
[CmdletBinding(PositionalBinding=$false)]
param(
    [switch]$DryRun,
    [switch]$Help,
    [switch]$Dev,
    [string]$Control = '',
    # Skip the Ollama engine install. The engine is no longer installed by
    # this script: `waired init` owns both the decision (its "run local
    # inference?" answers) and the install, which runs elevated inside
    # Phase 2 right after the questions. -SkipOllama resolves into
    # WAIRED_NO_OLLAMA=1 for the init child, which then skips the engine;
    # finish later from an elevated prompt (`waired runtimes install
    # ollama`). Same semantics as install.sh's --skip-ollama.
    [switch]$SkipOllama,
    # Skip the post-SCM `waired init` invocation. When -Control / -Dev
    # resolved a CP URL the installer normally runs `waired init` so
    # enrolment is single-step; -SkipInit reverts to the manual-Next-
    # steps block.
    [switch]$SkipInit,
    # Skip enabling the transparent Claude proxy after enrolment. By default
    # (mirroring the Linux installer) a successful `waired init` enables it:
    # Claude Code managed settings point ANTHROPIC_BASE_URL at local inference
    # (no credential, subscription preserved; fallback: real Anthropic). Pass
    # -SkipClaudeProxy (or WAIRED_NO_CLAUDE_PROXY=1) to leave Claude Code routed
    # straight to Anthropic; enable later with an elevated `waired claude enable`.
    [switch]$SkipClaudeProxy,
    # Force `waired init --non-interactive`. Auto-detected when stdin is
    # redirected (CI / piped through iex with a non-console stdin).
    [switch]$NonInteractive,
    # Mask personal information (home dir, username) in this script's own
    # output, and set WAIRED_PII_MASK=1 so `waired init` masks its output
    # too (home dir, username, hostname, account email) -- for screenshots
    # and bug reports. Best-effort, not a security boundary. Env form:
    # WAIRED_PII_MASK=1 (the piped one-liner cannot bind switches).
    [switch]$MaskPII,
    # -Check reports whether a newer waired is available and exits;
    # -Update applies it; -Yes assumes "yes" to the update prompt
    # (required to update on a non-interactive / no-TTY host). See #292.
    [switch]$Check,
    [switch]$Update,
    [switch]$Yes,
    # Clean install: run uninstall.ps1 -Clean first (full wipe: config, keys,
    # state, Ollama + models), then install fresh. Confirmed in the
    # un-elevated parent (Confirm-CleanInstall); -Yes bypasses the prompt.
    # Equivalent env var: WAIRED_CLEAN=1 (resolved into $Clean below) -- the
    # only way the piped `iwr | iex` one-liner can opt in, and the mirror of
    # install.sh's --clean / WAIRED_CLEAN. Deliberately NOT forwarded by
    # Invoke-SelfElevate: the wipe runs once, in Phase 1.
    [switch]$Clean,
    # Install the latest main build (same as WAIRED_VERSION=edge) -- rebuilt
    # on every merge to main; NOT a stable release. -Latest is an alias.
    # Resolved into $Version + $env:WAIRED_VERSION below so the edge
    # prerelease assets are fetched and the elevated re-invoke tracks the
    # same channel.
    [switch]$Edge,
    [switch]$Latest,
    # Force the stable channel on -Update/-Check, overriding the channel-
    # preservation that otherwise keeps an edge host on edge. The counterpart
    # to -Edge; resolved into $Version below.
    [switch]$Stable,
    # GPU mode for the engine install `waired init` performs: auto / rocm /
    # vulkan / cuda-only / cpu-only. It decides whether the ~300 MB AMD ROCm
    # overlay is fetched; the base archive already carries CUDA, Vulkan and
    # CPU. Forwarded as WAIRED_OLLAMA_GPU_MODE.
    [string]$OllamaGpuMode    = 'auto',
    # Install location. Resolution order: this param > WAIRED_INSTALL_DIR env
    # > the HKLM registry value a previous install recorded (so -Update and
    # re-runs find a relocated install) > an interactive prompt on a fresh
    # install > %ProgramFiles%\Waired. The env form exists because the piped
    # `iwr | iex` one-liner cannot bind parameters.
    [string]$InstallDir = '',
    # Force `waired init --inference-enabled=<true|false>`. Empty = no
    # override (the prompt or hardware-based default decides). Validated in
    # Resolve-InitAnswers (main) so a typo fails before UAC.
    [string]$InferenceEnabled = '',
    # Force `waired init --share-with-mesh=<true|false>`. Empty = no
    # override.
    [string]$ShareWithMesh = '',
    # Internal: non-empty when re-invoked elevated by Phase 1 after the
    # download has already happened. Skips re-download and goes straight
    # to the privileged install steps. Not documented in -Help -- callers
    # never set this directly.
    [string]$StagedZipPath,
    # Internal: path to the JSON document Phase 1 wrote with its fully
    # resolved state (every WAIRED_* value plus every resolved parameter).
    # It is THE configuration channel across the UAC boundary -- see the
    # handoff block below for why the environment cannot be one (#192).
    # Not documented in -Help -- callers never set this directly.
    [string]$StateFile,
    # Internal: path the elevated Phase-2 child writes its Start-Transcript
    # log to. The un-elevated parent picks a path under its own %TEMP% (so the
    # log stays readable without elevation) and forwards it; empty -> the child
    # defaults it. Not documented in -Help. (waired#748)
    [string]$LogPath,
    # Start the agent at this log verbosity: debug|info|warn|error (default
    # info). The Windows service has no EnvironmentFile, so this is baked into
    # the SCM service's ExecStart as --log-level. Same as WAIRED_LOG_LEVEL.
    # Change it later at runtime (no reinstall) with `waired config log-level`.
    # Mirrors install.sh --log-level.
    #
    # The two forms are one value: this default folds the env into the param,
    # and Resolve-LogLevel (main) folds the param back into the env so every
    # child of this process sees it. Across UAC it travels in -StateFile --
    # the environment does not survive that boundary (#192), which is why the
    # flag stayed silently dropped on the self-elevating path after #164.
    [string]$LogLevel = $env:WAIRED_LOG_LEVEL,
    # Catch-all for stray tokens. PowerShell can't bind install.sh-style
    # `--dev` / `--control <url>` long options to the -Dev / -Control params
    # (they arrive as plain string values), so with PositionalBinding=$false
    # they land here and Normalize-ExtraArgs folds the supported ones into
    # their -Xxx switches. Anything unrecognised dies loudly instead of
    # silently mis-binding to -Control and running `init --control --dev`
    # (waired#746).
    [Parameter(ValueFromRemainingArguments=$true)]
    [string[]]$ExtraArgs
)

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'

# -------------------------------------------------------------------
# Phase 1 -> Phase 2 handoff (#192, #177)
# -------------------------------------------------------------------
#
# These helpers live HERE, immediately after param(), and not with the
# other functions below, because -StateFile has to be loaded BEFORE the
# Configuration block resolves $BaseUrl / $Version / $NoTray / $StateDir /
# $DevControlUrl / $InstallDir / $LogPath out of the environment.
# PowerShell runs top-level statements in order, so a function defined
# further down does not exist yet at that point.
#
# Why a file and not the environment: Start-Process -Verb RunAs forces
# UseShellExecute, so the elevated child is created by the AppInfo service,
# which builds a FRESH environment block via CreateEnvironmentBlock. The
# parent's environment is not inherited and -Environment is ignored for
# RunAs, so every WAIRED_* value Phase 1 resolved was silently dropped
# (#192) -- WAIRED_LOG_LEVEL, WAIRED_STATE_DIR, WAIRED_NO_TRAY (which has
# no parameter form, i.e. no workaround), WAIRED_CONTROL_URL,
# WAIRED_DEV_CONTROL_URL, WAIRED_NO_EMOJI.
#
# install.sh hits the same wall at sudo's env_reset and answers it by
# threading `env VAR=value` onto the argv (linux_maybe_init /
# darwin_maybe_init). Windows has no equivalent, so Phase 1 writes its
# resolved state to a file and passes exactly one -StateFile token.
# Collapsing the argv to two tokens is also what makes #177 tractable:
# less to quote, and nothing left that a space can split.

# Schema of the -StateFile document. A mismatch means the two phases came
# from different installer versions; that is a hard error, never a
# half-applied configuration.
$InstallStateSchema = 1

# Leave a one-line cause next to the state file. The elevated console closes
# the instant the script exits, so this is what the un-elevated parent reads
# back and prints when the child exits non-zero (#177). Both the trap below
# and Common-Die write it -- Common-Die calls exit, which no trap can see.
# Best-effort by design: a diagnostic must never become the failure.
function Write-InstallStatus {
    param([string]$Text)
    $p = if ($StateFile) { "$StateFile.status" } elseif ($LogPath) { "$LogPath.status" } else { '' }
    if (-not $p) { return }
    try {
        [System.IO.File]::WriteAllText($p, $Text, (New-Object System.Text.UTF8Encoding($false)))
    } catch { }
}

# Breadcrumbs: one machine token per milestone reached, appended beside the
# state file -- the same parent-owned workdir the .status marker uses, and
# readable back for the same reason (see Invoke-SelfElevate).
#
# The marker above only exists when the trap or Common-Die runs. Closing the
# elevated console runs NEITHER (#314): the parent got a bare NTSTATUS and had
# no way to tell a half-done install from a total failure, nor a finished one
# from an abandoned one. These breadcrumbs are the signal that survives a kill,
# because each AppendAllText is durable the moment it returns -- so a killed
# console leaves an intact prefix rather than an empty or truncated file.
# Append, never rewrite: a truncate-then-write has a window where a kill leaves
# nothing at all, and ordering is the whole point.
#
# Tokens are machine-readable ASCII by construction -- no paths, no usernames --
# so this file can never carry PII and the parent owns every word the user
# sees. Readers must ignore tokens they do not know: Invoke-WairedUpdate shares
# the elevation path with a different (swap-only) step list.
# Best-effort by design: a diagnostic must never become the failure.
function Write-InstallProgress {
    param([string]$Token)
    if (-not $StateFile) { return }
    try {
        [System.IO.File]::AppendAllText("$StateFile.progress", "$Token`n",
            (New-Object System.Text.UTF8Encoding($false)))
    } catch { }
}

# Quote one token per the CommandLineToArgvW rules.
# Start-Process -ArgumentList joins its elements with a single space and
# quotes NOTHING (the claim that -Verb RunAs auto-quotes them was simply
# wrong), so an unquoted 'C:\Program Files\Waired' arrived at the child as
# two arguments and killed it before any diagnostics were armed (#177).
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

# Serialize everything Phase 1 resolved. After this file is written the
# argv carries no configuration at all, so anything omitted here is lost
# -- which is exactly the bug this replaces. WAIRED_CLEAN and -Check are
# deliberately absent: the wipe and the check are Phase-1-only branches
# (gated on -not $StagedZipPath) and restoring them would arm a step that
# must never run twice.
function Export-InstallState {
    param([string]$Path)
    $state = [ordered]@{
        schema = $InstallStateSchema
        env    = [ordered]@{
            WAIRED_LOG_LEVEL         = $env:WAIRED_LOG_LEVEL
            WAIRED_STATE_DIR         = $env:WAIRED_STATE_DIR
            WAIRED_NO_TRAY           = $env:WAIRED_NO_TRAY
            WAIRED_CONTROL_URL       = $env:WAIRED_CONTROL_URL
            WAIRED_DEV_CONTROL_URL   = $env:WAIRED_DEV_CONTROL_URL
            WAIRED_NO_EMOJI          = $env:WAIRED_NO_EMOJI
            WAIRED_VERSION           = $env:WAIRED_VERSION
            WAIRED_INSTALL_BASE_URL  = $env:WAIRED_INSTALL_BASE_URL
            WAIRED_INSTALL_REPO      = $env:WAIRED_INSTALL_REPO
            WAIRED_INSTALL_DIR       = $env:WAIRED_INSTALL_DIR
            WAIRED_PII_MASK          = $env:WAIRED_PII_MASK
            WAIRED_NO_OLLAMA         = $env:WAIRED_NO_OLLAMA
            WAIRED_NO_CLAUDE_PROXY   = $env:WAIRED_NO_CLAUDE_PROXY
        }
        params = [ordered]@{
            # $InstallDirExplicit and $ControlUrl are not carried: the
            # Configuration block and Resolve-ControlUrl re-derive both from
            # the values above, after the import, and would overwrite them.
            InstallDir         = [string]$InstallDir
            LogPath            = [string]$LogPath
            LogLevel           = [string]$LogLevel
            Control            = [string]$Control
            OllamaGpuMode      = [string]$OllamaGpuMode
            InferenceEnabled   = [string]$InferenceEnabled
            ShareWithMesh      = [string]$ShareWithMesh
            Dev                = [bool]$Dev
            DryRun             = [bool]$DryRun
            Update             = [bool]$Update
            Yes                = [bool]$Yes
            SkipOllama         = [bool]$SkipOllama
            SkipInit           = [bool]$SkipInit
            SkipClaudeProxy    = [bool]$SkipClaudeProxy
            NonInteractive     = [bool]$NonInteractive
            MaskPII            = [bool]$MaskPII
        }
    }
    # UTF-8 with NO BOM: Set-Content -Encoding UTF8 writes one on Windows
    # PowerShell 5.1 and ConvertFrom-Json rejects the BOM'd first key.
    [System.IO.File]::WriteAllText($Path, ($state | ConvertTo-Json -Depth 4),
        (New-Object System.Text.UTF8Encoding($false)))
}

# Rehydrate the child from the state file. Runs before the Configuration
# block, so restoring the environment is enough for everything derived
# there -- except a parameter whose default was already evaluated at BIND
# time (-LogLevel :$env:WAIRED_LOG_LEVEL), which is why every param is
# assigned explicitly below.
# Common-Die does not exist yet, so failures throw and the trap picks them
# up.
function Import-InstallState {
    param([string]$Path)
    if (-not (Test-Path -LiteralPath $Path)) {
        throw "install state file not found at $Path (the un-elevated parent may have crashed)"
    }
    try {
        # NOT Get-Content -Raw: on Windows PowerShell 5.1 it decodes a BOM-less
        # file with the system ANSI code page, which mangles a non-ASCII install
        # dir or user profile path. Read as UTF-8 explicitly, matching the write.
        $state = [System.IO.File]::ReadAllText($Path, [System.Text.Encoding]::UTF8) | ConvertFrom-Json
    } catch {
        throw "install state file $Path is not readable JSON: $($_.Exception.Message)"
    }
    if (-not $state -or -not $state.schema) {
        throw "install state file $Path has no schema field"
    }
    if ([int]$state.schema -ne $InstallStateSchema) {
        throw "install state file schema $($state.schema) != expected $InstallStateSchema (the two phases are different installer versions)"
    }
    foreach ($e in $state.env.PSObject.Properties) {
        # Name filter, not decoration: this runs in an ELEVATED process reading a
        # file from the un-elevated caller's %TEMP%. Without it a tampered
        # document could set PSModulePath or ComSpec here. The caller already
        # owns the staged zip, so this is defence in depth rather than the only
        # thing standing in the way -- but the restore has no business touching
        # anything outside our own namespace.
        if ($e.Name -notmatch '^WAIRED_[A-Za-z0-9_]+$') { continue }
        $val = [string]$e.Value
        if ($val -eq '') { $val = $null }   # $null removes the variable
        [Environment]::SetEnvironmentVariable($e.Name, $val)
    }
    $p = $state.params
    $script:InstallDir         = [string]$p.InstallDir
    $script:LogPath            = [string]$p.LogPath
    $script:LogLevel           = [string]$p.LogLevel
    $script:Control            = [string]$p.Control
    $script:OllamaGpuMode      = [string]$p.OllamaGpuMode
    $script:InferenceEnabled   = [string]$p.InferenceEnabled
    $script:ShareWithMesh      = [string]$p.ShareWithMesh
    $script:Dev                = [bool]$p.Dev
    $script:DryRun             = [bool]$p.DryRun
    $script:Update             = [bool]$p.Update
    $script:Yes                = [bool]$p.Yes
    $script:SkipOllama         = [bool]$p.SkipOllama
    $script:SkipInit           = [bool]$p.SkipInit
    $script:SkipClaudeProxy    = [bool]$p.SkipClaudeProxy
    $script:NonInteractive     = [bool]$p.NonInteractive
    $script:MaskPII            = [bool]$p.MaskPII
}

# The elevated console closes the instant the script exits, so an uncaught
# terminating error left nothing behind at all (waired#748's transcript is
# armed further down, and a failure before that point missed it entirely).
# Drop a marker next to the state file, which lives in the un-elevated
# parent's own workdir, so the parent can read the cause back and print it.
#
# Deliberately self-contained: it must work before Common-Die /
# Test-InteractiveStdin exist. It also cannot catch a parse error or a
# parameter-binding failure -- both happen before the first statement runs.
# Those are covered by the other half of this change: an argv short enough
# and quoted well enough that it cannot mis-bind.
trap {
    $msg = "$($_.Exception.Message)"
    Write-InstallStatus $msg
    Write-Host "[waired] install failed: $msg" -ForegroundColor Red
    # The other half of the armed rollback (waired-agent#1087): a terminating
    # error that never reached a Common-Die -- a native program refusing to
    # launch is one, and it is how #1087 was reported -- still has to put the
    # previous version back. $script:RollbackPlan is only set by the swap, so
    # by the time this is non-null every function below is defined.
    if ($script:RollbackPlan) { try { Invoke-PendingRollback } catch { } }
    try { Stop-Transcript -ErrorAction SilentlyContinue | Out-Null } catch { }
    if ($script:ElevatedConsole) {
        # Test-InteractiveStdin is ~1000 lines below and may not exist yet;
        # this is its inlined equivalent.
        try {
            if (-not $NonInteractive -and -not [Console]::IsInputRedirected) {
                Read-Host '[waired] Install FAILED. Press Enter to close this window' | Out-Null
            }
        } catch { }
    }
    exit 1
}

# Restore the resolved Phase-1 state before anything derives from the
# environment below.
if ($StateFile) { Import-InstallState -Path $StateFile }

# -------------------------------------------------------------------
# Configuration (overridable via environment, mirrors install.sh)
# -------------------------------------------------------------------

# Public GitHub Releases of `waired-ai/waired-agent` host install.ps1
# itself plus the per-tag Windows release assets (zip + sha256 +
# Setup.exe). Each `v*` tag publishes its assets there via release.yml.
$BaseUrl    = if ($env:WAIRED_INSTALL_BASE_URL) { $env:WAIRED_INSTALL_BASE_URL } `
              else { 'https://github.com/waired-ai/waired-agent/releases' }
$Version    = if ($env:WAIRED_VERSION) { $env:WAIRED_VERSION } else { 'latest' }
# -Edge / -Latest: the latest main build. Mirror install.sh's --edge by
# setting the channel both on $Version (this process) and $env:WAIRED_VERSION
# (inherited by the elevated re-invoke, which re-resolves $Version from it).
if ($Edge -or $Latest) {
    $Version = 'edge'
    $env:WAIRED_VERSION = 'edge'
}
# -Stable forces the stable channel, overriding channel-preservation and any
# inherited WAIRED_VERSION=edge (so it wins over -Edge if both are given, like
# install.sh's --stable). Clearing $env:WAIRED_VERSION also unpins the elevated
# re-invoke, which re-resolves $Version from the (now empty) env.
if ($Stable) {
    $Version = 'latest'
    $env:WAIRED_VERSION = $null
}
# GitHub repo (owner/name) whose Releases API resolves the stable
# 'latest' version during -Check / -Update. Mirror of install.sh's
# WAIRED_INSTALL_REPO. Override alongside WAIRED_INSTALL_BASE_URL for a
# private/staging mirror.
$InstallRepo = if ($env:WAIRED_INSTALL_REPO) { $env:WAIRED_INSTALL_REPO } else { 'waired-ai/waired-agent' }
$NoTray     = [bool]$env:WAIRED_NO_TRAY
$StateDir   = $env:WAIRED_STATE_DIR

# WAIRED_NO_OLLAMA is the env-var form of -SkipOllama (mirrors install.sh,
# where --skip-ollama and WAIRED_NO_OLLAMA are equivalent). Fold it into
# the switch here so every downstream check (the summary, the elevation
# re-invoke that forwards -SkipOllama, the init env below) sees one
# resolved value regardless of which form the operator used. The env block
# is also inherited by the elevated child, so the resolution holds across
# phases. The engine install itself now happens inside `waired init`.
if ($env:WAIRED_NO_OLLAMA) { $SkipOllama = $true }

# WAIRED_CLEAN is the env-var form of -Clean (mirrors install.sh's --clean /
# WAIRED_CLEAN). It exists because the piped `iwr | iex` one-liner cannot
# bind switches. The env block is inherited by the elevated Phase-2 child,
# but the wipe itself is gated on Phase 1 (no -StagedZipPath), so it never
# runs twice.
if ($env:WAIRED_CLEAN) { $Clean = $true }

# WAIRED_NO_CLAUDE_PROXY is the env-var form of -SkipClaudeProxy (mirrors the
# Linux installer's WAIRED_NO_CLAUDE_PROXY / --skip-proxy). Folded into the
# switch so every downstream check + the elevation re-invoke see one value.
if ($env:WAIRED_NO_CLAUDE_PROXY) { $SkipClaudeProxy = $true }

# WAIRED_PII_MASK is the env-var form of -MaskPII (mirrors install.sh's
# --mask-pii). Folded both ways: the env sets the switch, and the switch sets
# the env so every child (the elevated Phase 2, `waired init` and the engine
# installer it runs) inherits the masking request.
if ($env:WAIRED_PII_MASK) { $MaskPII = $true }
if ($MaskPII) { $env:WAIRED_PII_MASK = '1' }

# Built-in dogfood Control Plane URL surfaced via -Dev. Script-level only;
# never compiled into the waired binary (spec section 10.4 -- binary hash stays
# identical across environments).
$DevControlUrl = if ($env:WAIRED_DEV_CONTROL_URL) { $env:WAIRED_DEV_CONTROL_URL } `
                 else { 'https://app.dev.waired.net' }
$ControlUrl    = ''   # resolved by Resolve-ControlUrl after param parsing.
$InitRan       = $false  # set by Invoke-WairedInit; read by Show-NextSteps.
# $LocalAIDown: `waired init` signed this device in and then reported that
# local inference is not running here -- the engine could not be installed, or it
# installed and would not stay up. Sign-in SUCCEEDED, so this is not the
# "enrolment did not complete" case: $InitRan stays true and the done
# banner adds a line rather than changing what it says (#310).
$LocalAIDown   = $false
# Mirrors exitLocalAIDown in cmd/waired/main.go, and install.sh's
# $WAIRED_INIT_LOCAL_AI_DOWN. Named on both sides so a reader can grep the
# constant across the three files that have to agree on it.
$WairedInitLocalAIDown = 3
$OllamaStatus  = ''      # set by Set-OllamaEnvForInit; read by Show-NextSteps
                         # (Windows analog of install.sh's $ollama_status line).
# True only inside the spawned elevated Phase-2 console (set in Phase 2). Gates
# the transcript-path + pause-on-exit behaviour so that window never vanishes
# before its output can be read (waired#748).
$ElevatedConsole = $false

# Registry key where Phase 2 records the resolved install dir so the
# uninstaller and later -Update / re-runs find a relocated install.
$InstallDirRegKey = 'HKLM:\SOFTWARE\Waired'

# Install dir resolution: -InstallDir param > WAIRED_INSTALL_DIR env >
# registry (a previous install's recorded location) > %ProgramFiles%\Waired.
# A fresh interactive install may still override the default via the
# Request-InstallDir prompt (Phase 1 only). $InstallDirExplicit remembers
# whether the operator pinned it, so the prompt is offered only for the
# default.
$InstallDirExplicit = [bool]$InstallDir
if (-not $InstallDir -and $env:WAIRED_INSTALL_DIR) {
    $InstallDir = $env:WAIRED_INSTALL_DIR
    $InstallDirExplicit = $true
}
if (-not $InstallDir) {
    try {
        $regDir = (Get-ItemProperty -Path $InstallDirRegKey -Name 'InstallDir' -ErrorAction Stop).InstallDir
        if ($regDir) { $InstallDir = $regDir; $InstallDirExplicit = $true }
    } catch { }
}
if (-not $InstallDir) { $InstallDir = Join-Path $env:ProgramFiles 'Waired' }
$ServiceName = 'waired-agent'
$ZipName     = 'waired-windows-amd64.zip'
$ShaName     = "$ZipName.sha256"
# SCM-mode state dir; agent.json + identity land here so the
# LocalSystem-spawned waired-agent service finds them on boot.
$AgentStateDir = Join-Path $env:ProgramData 'waired'

# Where the elevated Phase-2 child (and the already-admin inline path) write a
# Start-Transcript log so the install output survives the console closing
# (waired#748). The un-elevated parent resolves it under its own %TEMP% and
# forwards -LogPath to the elevated child, so the file stays readable by the
# invoking (non-admin) user afterwards.
#
# One file PER RUN. The name used to be fixed, and Start-Transcript -Force
# meant the next run destroyed the failed run's evidence -- on one reviewed
# host that is exactly what happened, and the only surviving log was of a run
# nobody cared about (#314). $PID disambiguates two runs that start inside the
# same second: the loser of that race would otherwise hit a sharing violation
# that the transcript's own `catch {}` swallows, leaving a silently log-less
# run. It is the PARENT's pid, since the child inherits this path through the
# state file, so both phases still write one file.
# InvariantCulture, not Get-Date -Format: the current culture can supply a
# non-Gregorian calendar, which changes the year and breaks the ordering the
# prune below depends on.
if (-not $LogPath) {
    $stamp = (Get-Date).ToString('yyyyMMdd-HHmmss', [Globalization.CultureInfo]::InvariantCulture)
    $LogPath = Join-Path $env:TEMP "waired-install-$stamp-$PID.log"
}

# -------------------------------------------------------------------
# common_* helpers (mirror install.sh naming)
# -------------------------------------------------------------------

# Make emoji in the friendly banners render on modern terminals. Wrapped
# in try/catch because legacy hosts (or redirected output) may not accept
# the assignment; Emo falls back to ASCII when emoji can't be shown.
try { [Console]::OutputEncoding = [Text.Encoding]::UTF8 } catch { }

# Emo <emoji> <ascii-fallback>: emoji on a UTF-8-capable console, else the
# ASCII fallback. WAIRED_NO_EMOJI forces the fallback.
function Emo {
    param([string]$Emoji, [string]$Ascii)
    if ($env:WAIRED_NO_EMOJI) { return $Ascii }
    try {
        if ([Console]::OutputEncoding.CodePage -eq 65001) { return $Emoji }
    } catch { }
    return $Ascii
}

# Protect-PII masks the invoking user's home dir + username in one message
# when -MaskPII / WAIRED_PII_MASK is on (screenshots / bug reports;
# best-effort). The Go binary masks its own output via the same env var --
# this only covers the script's log lines. Longest token (the home dir,
# which contains the username) is replaced first.
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

# Get-FailureReason -- the reason out of a caught error, without this
# installer's own position in it.
#
# PowerShell appends its position ("At line:N char:M") and the offending source
# line to the message of a native-command failure, and it appends the position
# to the SAME line -- measured on Windows PowerShell 5.1, so taking the first
# line does not remove it, and a user-facing message ends "... At line:2129
# char:9". The innermost exception is the OS's own words on its own
# ("An Application Control policy has blocked this file"), which is what a
# reader needs; where there is no inner exception the position is cut by
# matching InvocationInfo.PositionMessage, which is the text PowerShell
# appended in the first place, so this needs no knowledge of the console
# language.
function Get-FailureReason {
    param($ErrorRecord)
    $ex = $ErrorRecord.Exception
    while ($ex -and $ex.InnerException) { $ex = $ex.InnerException }
    $msg = if ($ex) { "$($ex.Message)" } else { "$ErrorRecord" }
    $pos = $null
    if ($ErrorRecord.InvocationInfo) {
        $pos = ("$($ErrorRecord.InvocationInfo.PositionMessage)" -split "`r?`n" |
                    Where-Object { $_.Trim() } | Select-Object -First 1)
    }
    if ($pos) {
        $at = $msg.IndexOf($pos.Trim())
        if ($at -ge 0) { $msg = $msg.Substring(0, $at) }
    }
    return (($msg -split "`r?`n")[0]).Trim()
}

# Section prints a blank line + a horizontal-rule heading so a run reads as
# distinct steps (several tools write to this console; the rules make it easy
# to see where one step ends, the next begins, and which output belongs to a
# prompt). The U+2500 rule glyph is built at runtime via Glyph so the file
# stays pure-ASCII on the wire (scripts/install/encoding_test.go); non-UTF-8
# consoles / WAIRED_NO_EMOJI fall back to '-'. Mirrors install.sh's section().
function Section {
    param([string]$Title)
    $d = Emo ([char]::ConvertFromUtf32(0x2500)) '-'
    $head = ($d * 3) + ' ' + $Title + ' '
    $fill = 56 - 4 - $Title.Length
    if ($fill -lt 3) { $fill = 3 }
    Write-Host ''
    Write-Host ($head + ($d * $fill)) -ForegroundColor DarkCyan
}

# Disable-QuickEdit clears the console's QuickEdit mode. In the spawned
# elevated conhost window (Win10; Windows Terminal is unaffected) a stray
# click otherwise enters text-selection mode and FREEZES all output until the
# user presses Enter/Esc -- which looks like a hung installer. Best-effort:
# fails silently when output is redirected or the console API is unavailable.
# No restore needed; the spawned console is transient.
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
            # Clear ENABLE_QUICK_EDIT_MODE (0x40); ENABLE_EXTENDED_FLAGS (0x80)
            # must be set for the QuickEdit bit to be honoured.
            $newMode = ($mode -band (-bnot [uint32]0x40)) -bor [uint32]0x80
            [void][WairedNative.ConsoleMode]::SetConsoleMode($h, $newMode)
        }
    } catch { }
}

# Stop-TranscriptQuietly ends an active Start-Transcript without erroring when
# none is running (Stop-Transcript throws in that case). Used by the Phase-2
# transcript logging added for waired#748.
function Stop-TranscriptQuietly {
    try { Stop-Transcript -ErrorAction SilentlyContinue | Out-Null } catch { }
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

function Common-Die  {
    param([string]$Msg)
    Write-Host "[waired] $Msg" -ForegroundColor Red
    # A swap that got part-way is put back here, not in a catch around the
    # swap: this function ends in `exit`, which is not an exception, so a
    # catch would never see the failures that come through it
    # (waired-agent#1087). Guarded on the plan, which only the swap arms --
    # by then Invoke-PendingRollback is defined.
    if ($script:RollbackPlan) {
        try { Invoke-PendingRollback } catch {
            Write-Host "[waired] could not put the previous version back: $($_.Exception.Message)" -ForegroundColor Red
        }
    }
    # In the spawned elevated Phase-2 console the window closes the instant the
    # script exits, taking every message with it. Surface the transcript path
    # and pause so the failure is actually readable (waired#748). Guarded on
    # $ElevatedConsole so Phase-1 / parent dies stay unchanged (their console
    # persists). Runs here (not just in a try/finally) because install steps
    # call Common-Die -> exit 1, which can bypass a wrapping finally.
    if ($script:ElevatedConsole) {
        # The trap cannot see this: exit is not a terminating error. Without
        # this line the most common elevated failure -- any Common-Die inside
        # Phase 2 -- would still reach the parent as a bare exit code (#177).
        Write-InstallStatus $Msg
        if ($script:LogPath) { Write-Host "[waired] Full install log: $($script:LogPath)" -ForegroundColor Red }
        Stop-TranscriptQuietly
        if (Test-InteractiveStdin) {
            Read-Host '[waired] Install FAILED. Press Enter to close this window' | Out-Null
        }
    }
    exit 1
}

# Arm the elevated-console diagnostics as early as the helpers allow (#177).
# The spawned Phase-2 console closes the instant the script exits, so
# anything failing before this point vanished with it -- which is exactly how
# a mis-bound -InstallDir died: Normalize-ExtraArgs -> Common-Die, with the
# transcript still ~1500 lines further down the file. Everything between here
# and main is function definitions, which cannot fail.
#
# Gated on $StagedZipPath, the Phase-2 discriminator, so Phase 1 and the
# already-admin inline path (which wraps its own transcript around the install
# steps) are untouched. $LogPath is resolved by now -- and after the -StateFile
# load -- so it is the path the un-elevated parent picked under its own %TEMP%,
# where it stays readable without elevation (waired#748).
if ($StagedZipPath) {
    $script:ElevatedConsole = $true
    # QuickEdit first: a stray click in this spawned window would otherwise
    # freeze all output (it looks like a hung install) until Enter/Esc.
    Disable-QuickEdit
    try { Start-Transcript -Path $LogPath -Force -ErrorAction SilentlyContinue | Out-Null } catch { }
}

# Normalize-ExtraArgs folds install.sh-style long options that PowerShell left
# unbound in $ExtraArgs (because `--foo` tokens arrive as plain string values,
# not parameters) into the native -Xxx parameters, so `--dev` / `--control
# <url>` work for parity instead of silently mis-binding to -Control. Any token
# it does not recognise dies loudly -- the whole point of
# PositionalBinding=$false is that a stray arg is never swallowed again
# (waired#746). Must run before anything consumes the switches.
function Normalize-ExtraArgs {
    if (-not $ExtraArgs) { return }
    $i = 0
    while ($i -lt $ExtraArgs.Count) {
        $tok = [string]$ExtraArgs[$i]
        $val = $null
        # Support --opt=value as well as --opt value.
        $eq = $tok.IndexOf('=')
        if ($tok -match '^--?[A-Za-z]' -and $eq -gt 0) {
            $val = $tok.Substring($eq + 1)
            $tok = $tok.Substring(0, $eq)
        }
        $key = $tok.TrimStart('-').ToLowerInvariant()
        switch ($key) {
            'dev'               { $script:Dev = $true }
            'control' {
                if ($null -eq $val) {
                    if ($i + 1 -ge $ExtraArgs.Count) { Common-Die "--control requires a URL argument (e.g. --control https://<host>)." }
                    $i++
                    $val = [string]$ExtraArgs[$i]
                }
                $script:Control = $val
            }
            'install-dir' {
                if ($null -eq $val) {
                    if ($i + 1 -ge $ExtraArgs.Count) { Common-Die "--install-dir requires a path argument (e.g. --install-dir D:\Waired)." }
                    $i++
                    $val = [string]$ExtraArgs[$i]
                }
                $script:InstallDir = $val
                $script:InstallDirExplicit = $true
            }
            'log-level' {
                if ($null -eq $val) {
                    if ($i + 1 -ge $ExtraArgs.Count) { Common-Die "--log-level requires an argument (debug|info|warn|error)." }
                    $i++
                    $val = [string]$ExtraArgs[$i]
                }
                $script:LogLevel = $val
            }
            'inference-enabled' {
                if ($null -eq $val) {
                    if ($i + 1 -ge $ExtraArgs.Count) { Common-Die "--inference-enabled requires an argument (true|false)." }
                    $i++
                    $val = [string]$ExtraArgs[$i]
                }
                $script:InferenceEnabled = $val
            }
            'share-with-mesh' {
                if ($null -eq $val) {
                    if ($i + 1 -ge $ExtraArgs.Count) { Common-Die "--share-with-mesh requires an argument (true|false)." }
                    $i++
                    $val = [string]$ExtraArgs[$i]
                }
                $script:ShareWithMesh = $val
            }
            'skip-ollama'       { $script:SkipOllama = $true }
            'skip-init'         { $script:SkipInit = $true }
            'skip-claude-proxy' { $script:SkipClaudeProxy = $true }
            'skip-proxy'        { $script:SkipClaudeProxy = $true }
            'non-interactive'   { $script:NonInteractive = $true }
            'mask-pii'          { $script:MaskPII = $true; $env:WAIRED_PII_MASK = '1' }
            'dry-run'           { $script:DryRun = $true }
            'yes'               { $script:Yes = $true }
            'clean'             { $script:Clean = $true }
            'check'             { $script:Check = $true }
            'update'            { $script:Update = $true }
            # Channel flags set the derived $Version / WAIRED_VERSION directly
            # (mirroring the -Edge/-Latest/-Stable resolution above), since this
            # runs after that block.
            'edge'              { $script:Version = 'edge';   $env:WAIRED_VERSION = 'edge' }
            'latest'            { $script:Version = 'edge';   $env:WAIRED_VERSION = 'edge' }
            'stable'            { $script:Version = 'latest'; $env:WAIRED_VERSION = $null }
            'help'              { $script:Help = $true }
            default {
                if ($ExtraArgs[$i] -match '^https?://') {
                    Common-Die "unexpected URL argument '$($ExtraArgs[$i])'. Pass the Control Plane URL as -Control https://<host> (or --control https://<host>)."
                }
                Common-Die "unknown argument '$($ExtraArgs[$i])'. Windows uses -Dev / -Control <url> / -SkipOllama etc. (run with -Help). The install.sh --dev and --control <url> spellings are also accepted."
            }
        }
        $i++
    }
}

# Resolve-LogLevel validates the requested verbosity and republishes it to the
# environment. Both halves matter, and both used to be missing (#164):
#
#   * Validation here, at parameter-resolution time, mirrors install.sh
#     (which validates right after its arg loop). It used to live inside
#     Invoke-AgentInstall -- i.e. AFTER UAC, inside the elevated window that
#     closes on exit, so a typo was reported where nobody could read it.
#
#   * The $env write folds the flag and the WAIRED_LOG_LEVEL form onto one
#     value, so every child of THIS process (`waired-agent install`,
#     `waired init`) sees it without a second passthrough. What it is not is
#     the UAC carrier: -Verb RunAs rebuilds the environment block, so across
#     elevation the value rides -StateFile like everything else (#192).
#     Claiming otherwise is why `-LogLevel debug` stayed silently dropped on
#     the ordinary self-elevating path after #164 was called fixed -- the one
#     flag you reach for when reproducing a pre-release bug.
#
# Must run after Normalize-ExtraArgs so the install.sh-style `--log-level LVL`
# spelling is covered as well as the -LogLevel param and the env form.
function Resolve-LogLevel {
    if (-not $LogLevel) { return }
    $lvl = $LogLevel.ToLowerInvariant()
    if ($lvl -notin @('debug','info','warn','error')) {
        Common-Die "-LogLevel must be one of: debug info warn error (got: $LogLevel)"
    }
    $script:LogLevel    = $lvl
    $env:WAIRED_LOG_LEVEL = $lvl
}

# Resolve-InitAnswers validates the two pre-answered setup questions. `waired
# init`'s --inference-enabled / --share-with-mesh are Go bool flags, so the
# only value spellings that reach them are `true` / `false` (see
# Get-WairedInitArgs for why the `=` form is mandatory). Checking here means a
# typo dies before UAC instead of surfacing as a flag-parse error inside the
# elevated init.
function Resolve-InitAnswers {
    if ($InferenceEnabled) {
        $v = $InferenceEnabled.ToLowerInvariant()
        if ($v -notin @('true','false')) {
            Common-Die "-InferenceEnabled must be true or false (got: $InferenceEnabled)"
        }
        $script:InferenceEnabled = $v
    }
    if ($ShareWithMesh) {
        $v = $ShareWithMesh.ToLowerInvariant()
        if ($v -notin @('true','false')) {
            Common-Die "-ShareWithMesh must be true or false (got: $ShareWithMesh)"
        }
        $script:ShareWithMesh = $v
    }
}

# Show-Banner prints the WAIRED "GATE" splash at the start of a run.
# Two tiers, mirroring install.sh:
#   * rich  -- a block WAIRED wordmark + GATE emblem ( o ) with a
#             blue->cyan 24-bit gradient, on a UTF-8 console that supports
#             virtual-terminal sequences and is wide enough.
#   * plain -- a figlet ASCII wordmark in cyan, otherwise.
# Colour is suppressed when output is redirected or NO_COLOR is set. The
# 24-bit gradient is emitted as raw VT sequences (Write-Host only knows
# the 16 console colours). PS 5.1 compatible; row text is single-quoted
# so the literal "$0" is not treated as a variable.
# Glyph / Utf8FromB64 build the non-ASCII banner + emoji glyphs at runtime so
# install.ps1 stays pure-ASCII on the wire. `iwr | iex` coerces the downloaded
# script through the system ANSI code page, which turns any literal non-ASCII
# byte into "?" (the mojibake seen on non-UTF-8 / non-English hosts). Numeric
# code points and Base64 are ASCII and survive that round-trip intact.
function Glyph([int]$cp) { [char]::ConvertFromUtf32($cp) }
function Utf8FromB64([string]$b64) { [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($b64)) }

function Show-Banner {
    $utf8 = $false
    try { $utf8 = ([Console]::OutputEncoding.CodePage -eq 65001) } catch { }
    if ($env:WAIRED_NO_EMOJI) { $utf8 = $false }

    $tty = $true
    try { $tty = -not [Console]::IsOutputRedirected } catch { }
    $useColor = $tty -and (-not $env:NO_COLOR)
    $vt = $false
    try { $vt = [bool]$Host.UI.SupportsVirtualTerminal } catch { }

    $cols = 80
    try { $cols = [int][Console]::WindowWidth } catch { }
    if ($cols -lt 1) { $cols = 80 }

    if ($utf8 -and $cols -ge 60) {
        $e = [char]27
        $rows = @(
            @(127,233,255,'ICAgICAgIMK3ICDin6gg4pePIOKfqSAgwrc='),
            @(72,105,140,'ICAg4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE'),
            @(143,189,240,'IOKWiOKWiOKVlyAgICDilojilojilZcg4paI4paI4paI4paI4paI4pWXIOKWiOKWiOKVl+KWiOKWiOKWiOKWiOKWiOKWiOKVlyDilojilojilojilojilojilojilojilZfilojilojilojilojilojilojilZcg'),
            @(140,198,243,'IOKWiOKWiOKVkSAgICDilojilojilZHilojilojilZTilZDilZDilojilojilZfilojilojilZHilojilojilZTilZDilZDilojilojilZfilojilojilZTilZDilZDilZDilZDilZ3ilojilojilZTilZDilZDilojilojilZc='),
            @(137,207,246,'IOKWiOKWiOKVkSDilojilZcg4paI4paI4pWR4paI4paI4paI4paI4paI4paI4paI4pWR4paI4paI4pWR4paI4paI4paI4paI4paI4paI4pWU4pWd4paI4paI4paI4paI4paI4pWXICDilojilojilZEgIOKWiOKWiOKVkQ=='),
            @(134,215,249,'IOKWiOKWiOKVkeKWiOKWiOKWiOKVl+KWiOKWiOKVkeKWiOKWiOKVlOKVkOKVkOKWiOKWiOKVkeKWiOKWiOKVkeKWiOKWiOKVlOKVkOKVkOKWiOKWiOKVl+KWiOKWiOKVlOKVkOKVkOKVnSAg4paI4paI4pWRICDilojilojilZE='),
            @(130,224,252,'IOKVmuKWiOKWiOKWiOKVlOKWiOKWiOKWiOKVlOKVneKWiOKWiOKVkSAg4paI4paI4pWR4paI4paI4pWR4paI4paI4pWRICDilojilojilZHilojilojilojilojilojilojilojilZfilojilojilojilojilojilojilZTilZ0='),
            @(127,233,255,'ICDilZrilZDilZDilZ3ilZrilZDilZDilZ0g4pWa4pWQ4pWdICDilZrilZDilZ3ilZrilZDilZ3ilZrilZDilZ0gIOKVmuKVkOKVneKVmuKVkOKVkOKVkOKVkOKVkOKVkOKVneKVmuKVkOKVkOKVkOKVkOKVkOKVnSA='),
            @(72,105,140,'ICAg4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE4pSE'),
            @(150,160,175,'ICAgTG9jYWwtZmlyc3QgQUkgZ2F0ZXdheSAgwrcgICQwIHBlciB0b2tlbg=='),
            @(112,120,134,'ICAgQ2xhdWRlIENvZGUgwrcgT3BlbkNvZGUgwrcgT3BlbkNsYXcg4oCUIHlvdXIgb3duIG1hY2hpbmU=')
        )
        foreach ($r in $rows) {
            $txt = Utf8FromB64 ([string]$r[3])
            if ($useColor -and $vt) {
                Write-Host ("{0}[38;2;{1};{2};{3}m{4}{0}[0m" -f $e, $r[0], $r[1], $r[2], $txt)
            } elseif ($useColor) {
                Write-Host $txt -ForegroundColor Cyan
            } else {
                Write-Host $txt
            }
        }
        Write-Host ""
    } else {
        $art = @'
__        ___    ___ ____  _____ ____
\ \      / / \  |_ _|  _ \| ____|  _ \
 \ \ /\ / / _ \  | || |_) |  _| | | | |
  \ V  V / ___ \ | ||  _ <| |___| |_| |
   \_/\_/_/   \_\___|_| \_\_____|____/
'@
        if ($useColor) { Write-Host $art -ForegroundColor Cyan } else { Write-Host $art }
        Write-Host "   Local-first AI gateway`n"
    }
}

# Either run the script-block or, in dry-run mode, print a description.
function Common-Run {
    param(
        [string]$Description,
        [scriptblock]$Action
    )
    if ($DryRun) {
        Write-Host "[dry-run] $Description" -ForegroundColor DarkGray
        return
    }
    & $Action
}

function Show-Help {
@"
install.ps1 -- install Waired for Windows.

Usage:
  iwr -useb $BaseUrl/latest/download/install.ps1 | iex

  # Or, with options (save to a file first so -Dev / -Control bind and the
  # UAC self-elevation re-launches the same file):
  `$f = "`$env:TEMP\waired-install.ps1"
  iwr -useb $BaseUrl/latest/download/install.ps1 -OutFile `$f
  & `$f -Dev

Switches:
  -DryRun           Print every privileged command without executing it.
  -Dev              Pre-configure for the built-in dogfood Control Plane
                    ($DevControlUrl); the installer enrols this device
                    against that CP automatically (UAC + browser sign-in).
                    Persists WAIRED_CONTROL_URL to the agent env file
                    (%ProgramData%\waired\agent.env) so a later
                    `waired init` with no -Control still finds this CP --
                    parity with install.sh on Linux/macOS.
  -Control <URL>    Same as -Dev but with an explicit URL; takes precedence
                    over -Dev when both are given. A scheme-less host
                    (dev.waired.net) is accepted and normalised by
                    `waired init`. (The install.sh spellings --dev and
                    --control <URL> also work; a stray flag / junk value is
                    rejected.)
  -Edge, -Latest    Install/switch to the latest main build (same as
                    WAIRED_VERSION=edge) -- rebuilt on every merge to main;
                    NOT a stable release. Fetches the edge prerelease
                    assets from the mirror.
  -Stable           Install/switch to the latest stable release. On
                    -Update/-Check this overrides the default, which
                    preserves the channel the host already tracks (edge
                    stays edge, stable stays stable).
  -SkipOllama       Tell `waired init` not to install the Ollama engine
                    (same as WAIRED_NO_OLLAMA=1). Add it later from an
                    elevated prompt: waired runtimes install ollama.
  -SkipInit         Skip the post-install `waired init` invocation; finish
                    with the manual-Next-steps block instead.
  -SkipClaudeProxy  Leave Claude Code routed straight to the Anthropic API.
                    Forwarded to `waired init --skip-claude-route`, which is the
                    single place routing is decided (default: on -- init writes
                    managed settings pointing ANTHROPIC_BASE_URL at local
                    inference, no credential). Same as WAIRED_NO_CLAUDE_PROXY=1;
                    enable later with an elevated `waired claude enable`.
  -NonInteractive   Never prompt: forward `--non-interactive` to `waired
                    init` (skip the install-time inference role prompts)
                    AND attempt sign-in even when no terminal is
                    available -- the default there is to skip sign-in and
                    tell you to finish later. Same as install.sh's
                    --non-interactive.
  -MaskPII          Mask personal information (home dir, username; the
                    sign-in step also masks hostname + account email) in
                    the output -- for screenshots and bug reports.
                    Best-effort. Same as WAIRED_PII_MASK=1.
  -LogLevel LVL     Start the agent at this log verbosity: debug, info,
                    warn, or error (default info). Use -LogLevel debug for
                    pre-release debugging. Same as WAIRED_LOG_LEVEL=LVL.
                    Change it later without reinstalling via
                    `waired config log-level <level>`.
  -Check            Report whether a newer waired is available, then exit.
                    Read-only: no download and no UAC prompt.
  -Update           Update an existing install to the latest release for
                    the active channel (WAIRED_VERSION): stops the
                    service, swaps the binaries in place, restarts. The
                    SCM registration and the state/identity under
                    %ProgramData%\waired are preserved; the inference engine is
                    not touched. Re-running install.ps1 on a host that
                    already has waired offers this automatically.
  -Yes              Assume "yes" to every prompt: the pre-install
                    confirmation, the update prompt (required to update on a
                    non-interactive / no-TTY host), the -Clean confirmation,
                    and `waired init`'s own prompts (it is run with
                    --non-interactive). Also accepts the default install
                    location without asking. Does NOT make sign-in run on a
                    host with no terminal -- see -NonInteractive.
  -Clean            Clean install: run the uninstaller with -Clean first
                    (PERMANENTLY deletes config, keys, state, and Ollama +
                    its models), then install fresh. Destructive -- asks to
                    confirm unless -Yes. Expect two UAC prompts (wipe +
                    install). Same as WAIRED_CLEAN=1, which is how the piped
                    `iwr | iex` one-liner opts in. Cannot be combined with
                    -Check/-Update.
  -Help             Print this help.

Parameters:
  -InstallDir <path>         Install location (default: %ProgramFiles%\Waired;
                             a fresh interactive install also offers a prompt).
                             Recorded in the registry (HKLM\SOFTWARE\Waired) so
                             updates and the uninstaller find it. Env form:
                             WAIRED_INSTALL_DIR (for the piped one-liner).
  -OllamaGpuMode <mode>      auto | rocm | vulkan | cuda-only | cpu-only
                             (default: auto). Forwarded to the engine install
                             that `waired init` performs (WAIRED_OLLAMA_GPU_MODE).
                             It selects the GPU runtime; it does not change
                             where the engine is installed.
  -InferenceEnabled <bool>   true | false to force `waired init
                             --inference-enabled`. Empty = prompt. Same as
                             install.sh's --inference-enabled.
  -ShareWithMesh <bool>      true | false to force `waired init
                             --share-with-mesh`. Empty = prompt. Same as
                             install.sh's --share-with-mesh.

Environment variables:
  WAIRED_VERSION           Pin a specific release (e.g. 1.2.3, 1.2.3-rc1, or
                           v1.2.3-rc1 -- the leading v is optional), or 'edge'
                           for the latest main build (same as -Edge). Default: latest.
  WAIRED_NO_TRAY           If set, skip waired-tray.exe.
  WAIRED_NO_OLLAMA         If set, `waired init` skips the Ollama engine
                           install (same as -SkipOllama).
  WAIRED_CLEAN             If set, same as -Clean (full wipe first, then a
                           fresh install). The env form exists because the
                           piped `iwr | iex` one-liner cannot bind switches.
  WAIRED_NO_CLAUDE_PROXY   If set, skip configuring Claude Code managed settings (same as -SkipClaudeProxy).
  WAIRED_INSTALL_DIR       Install location (same as -InstallDir; the env form
                           works with the piped one-liner).
  WAIRED_PII_MASK          If set, mask personal information in the output
                           (same as -MaskPII; works with the piped one-liner).
  WAIRED_STATE_DIR         Override on-disk state location. Default: %ProgramData%\waired.
  WAIRED_CONTROL_URL       Control Plane URL written to agent.env when
                           -Dev / -Control are not given (lower-priority
                           fallback for per-org installer wrappers).
  WAIRED_DEV_CONTROL_URL   Override the URL -Dev resolves to.
                           Default: https://app.dev.waired.net.
  WAIRED_INSTALL_BASE_URL  Override the mirror base URL (tests / staging).
                           Hosts the waired binaries only. (WAIRED_OLLAMA_WINDOWS_URL
                           and -OllamaModelsDir are both retired: the engine is
                           downloaded by `waired init` / `waired runtimes install
                           ollama` from a pinned URL, into waired's own folder
                           under %ProgramData%\waired.)

Diagnostics:
  Get-Service waired-agent
  %ProgramData%\waired\logs\waired-agent.log  (the agent's own log)
  Get-WinEvent -ProviderName waired-agent -LogName Application -MaxEvents 20
                                              (warnings and errors only)

Uninstall:
  - Settings -> Apps -> Waired -> Uninstall (when the GUI installer was used)
  - or: & "C:\Program Files\Waired\waired-agent.exe" uninstall
"@ | Write-Host
}

# Resolve the Control Plane URL using [-Control > -Dev preset > env]
# precedence and store it in $script:ControlUrl. An empty result is fine
# -- Show-NextSteps falls back to a placeholder URL in that case.
function Resolve-ControlUrl {
    if ($Control -and $Dev) {
        Common-Warn "-Control overrides -Dev (both were given)"
    }
    if ($Control) {
        $script:ControlUrl = $Control
    } elseif ($Dev) {
        if (-not $DevControlUrl) {
            Common-Die "-Dev requires WAIRED_DEV_CONTROL_URL but it is empty"
        }
        $script:ControlUrl = $DevControlUrl
    } elseif ($env:WAIRED_CONTROL_URL) {
        $script:ControlUrl = $env:WAIRED_CONTROL_URL
    }
    # Reject a Control URL that is really a stray flag or multi-token junk --
    # e.g. the old failure mode where `--dev` bound to -Control and enrolment
    # ran against `https://--dev` (waired#746), or `--control --dev` slipping a
    # flag into the value. A scheme-less host (dev.waired.net) is intentionally
    # allowed: `waired init` normalises it (https for remote, http for
    # loopback), matching install.sh, whose resolve_control_url does no URL
    # validation of its own -- so this stays lenient to avoid a Windows-only
    # divergence.
    if ($script:ControlUrl -and ($script:ControlUrl -match '^-' -or $script:ControlUrl -match '\s')) {
        Common-Die "Control Plane URL '$($script:ControlUrl)' looks like a stray flag, not a host/URL. Use -Dev for the dogfood Control Plane, or -Control https://<host> (a scheme-less host such as dev.waired.net is also accepted)."
    }
}

# -------------------------------------------------------------------
# detect_* -- OS / arch validation
# -------------------------------------------------------------------

function Detect-Platform {
    $arch = $env:PROCESSOR_ARCHITECTURE
    if ($arch -ne 'AMD64') {
        Common-Die "unsupported CPU architecture: $arch. Waired ships windows/amd64 today."
    }
    $os = [Environment]::OSVersion
    # Windows 10 1809 (build 17763) is the minimum for the path /
    # service / DACL APIs the agent relies on.
    if ($os.Version.Build -lt 17763) {
        Common-Die "Windows 10 1809 (build 17763) or newer is required. Detected build $($os.Version.Build)."
    }
    Common-Log "Detected Windows $($os.Version) ($arch)"
}

# -------------------------------------------------------------------
# Self-elevation
# -------------------------------------------------------------------

function Test-Admin {
    $id   = [Security.Principal.WindowsIdentity]::GetCurrent()
    $prin = New-Object Security.Principal.WindowsPrincipal($id)
    return $prin.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

# Re-invoke this script elevated, AFTER the un-elevated download +
# checksum-verify have already finished. The staged zip path is passed
# along so the elevated child does not re-download (one UAC prompt
# total, no double-fetch), and the resolved configuration travels in the
# -StateFile document -- NOT in the environment, which -Verb RunAs
# rebuilds from scratch (see the handoff block at the top, #192).
# Two cases for re-invocation source:
#   (a) Running from a .ps1 on disk (`powershell -File install.ps1`):
#       $PSCommandPath gives the absolute script path; re-launch
#       powershell.exe -File against it with -StagedZipPath.
#   (b) Sourced via `iwr | iex`: $PSCommandPath is null. Re-fetch the
#       script body to a temp .ps1 and re-launch powershell.exe -File
#       against it with -StagedZipPath, exactly like case (a). We
#       deliberately do NOT rebuild an in-memory download-then-compile
#       cradle here (fetch the body, ScriptBlock-Create it, invoke): that
#       contiguous download-decode-execute literal reads as malware to
#       Windows Defender AMSI and gets the whole script blocked (#552).
#       Running from a file also sidesteps the Windows PowerShell 5.1
#       octet-stream byte[] pitfall the cradle was working around.
#
# We deliberately do NOT use sudo.exe: it ships only on Windows 11
# 24H2+ Pro builds and is not present on the majority of supported
# targets. Start-Process -Verb RunAs is universal back to Windows 10
# 1809.
#
# The argv is deliberately tiny -- powershell's own switches, -File, and the
# two internal handoff parameters. Everything else travels in the state file
# (#192). Every token still goes through ConvertTo-NativeArg, because
# Start-Process quotes nothing and both the script path and %TEMP% can
# contain spaces (#177).
function Get-ElevateArgs {
    param([string]$ScriptPath, [string]$ZipPath, [string]$StatePath)
    $argv = @(
        '-NoProfile', '-ExecutionPolicy', 'Bypass',
        '-File', $ScriptPath,
        '-StagedZipPath', $ZipPath,
        '-StateFile', $StatePath
    )
    return @($argv | ForEach-Object { ConvertTo-NativeArg $_ })
}

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

# Read-InstallProgress returns the breadcrumb tokens the elevated child left,
# oldest first, or an empty array. Tolerant on purpose: the file may not exist,
# the child can be killed mid-append and leave a torn final line, and the
# update flow shares this elevation path with a different step list -- so
# anything that is not a well-formed token is dropped rather than reported.
function Read-InstallProgress {
    param([string]$Path)
    if (-not (Test-Path -LiteralPath $Path)) { return @() }
    try {
        $raw = @(Get-Content -LiteralPath $Path -Encoding UTF8 -ErrorAction Stop)
    } catch {
        return @()
    }
    return @($raw | Where-Object { $_ -match '^[a-z][a-z0-9-]*$' })
}

# Watch-ElevatedConsole mirrors the elevated child's transcript into THIS
# console while the child runs.
#
# Before this, the parent blocked in Start-Process -Wait and printed nothing
# for the entire elevated phase -- which on the reviewed hosts meant many
# minutes of silence while sign-in and a multi-gigabyte engine download ran in
# a window the parent never described (#314). A silent parent next to a
# seemingly-stuck child is what got the child closed.
function Watch-ElevatedConsole {
    param($Process, [string]$Path)
    $reader = $null
    $seps   = 0
    $shown  = $false
    $hinted = $false
    $exited = $false
    try {
        while ($true) {
            if ($null -eq $reader -and (Test-Path -LiteralPath $Path)) {
                try {
                    # FileShare::ReadWrite is the load-bearing part: the child's
                    # Start-Transcript holds this file open, and a reader that
                    # asks for less sharing fails outright -- which is exactly
                    # why File.ReadAllText (used elsewhere in this script for
                    # the state file) must not be used on the transcript.
                    $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open,
                        [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
                    $reader = New-Object System.IO.StreamReader($stream, [System.Text.Encoding]::UTF8, $true)
                } catch { }
            }
            if ($reader) {
                while ($null -ne ($line = $reader.ReadLine())) {
                    if ($line -match '^\*{5,}$') { $seps++; continue }
                    # PowerShell brackets the transcript header between the
                    # first two separator lines and the footer between the last
                    # two, so only $seps -eq 2 is the run's own output. That
                    # header carries Username / RunAs User / Machine, and
                    # mirroring it would leak the elevating account into this
                    # console -- including an administrator OTHER than the
                    # invoking user, whom Protect-PII cannot mask. Counting
                    # separators rather than matching the field names is
                    # deliberate: those labels are localized.
                    if ($seps -ne 2) { continue }
                    if (-not $shown) {
                        Common-Log '--- live view of the Administrator window (type THERE, not here) ---'
                        $shown = $true
                    }
                    Write-Host "  | $(Protect-PII $line)" -ForegroundColor DarkGray
                }
            }
            # Phase 2 stops its transcript BEFORE its final "Press Enter to
            # close this window" pause, so that prompt is never mirrored: the
            # live view would simply stop dead while the elevated window waits
            # for a keypress, leaving this console silent at the exact moment
            # the operator has to act. That is the same failure as #314, just
            # seconds long instead of minutes, so say it out loud once. The
            # third separator is the transcript footer, i.e. "the child is
            # done talking but has not exited".
            if ($seps -ge 3 -and $shown -and -not $hinted -and -not $Process.HasExited) {
                Common-Log 'The Administrator window is waiting for you -- press Enter THERE to close it (setup has finished).'
                $hinted = $true
            }
            # One full read pass AFTER the child exits, so the tail of the
            # transcript is never dropped.
            if ($exited) { break }
            if ($Process.HasExited) { $exited = $true; continue }
            # Deliberately no wall-clock timeout in this loop: sign-in plus a
            # multi-gigabyte engine download legitimately runs for many
            # minutes, and a timeout here would recreate the bug being fixed.
            Start-Sleep -Milliseconds 250
        }
    } catch {
        # Mirroring is a convenience; the child owns the install and the
        # transcript on disk is the record. Never let it break the run.
    } finally {
        if ($reader) { $reader.Dispose() }
        if ($shown) { Common-Log '--- end of live view ---' }
    }
}

# Show-InterruptedInstall reports what is actually on the machine after the
# elevated console went away part-way. The old code called any non-zero exit a
# total failure, which was wrong in the common case: files extracted, service
# installed and running, and only sign-in missing (#314).
function Show-InterruptedInstall {
    param([string[]]$Steps)
    # Framed, not suppressed, under -DryRun: the probes below read the real
    # host (Phase 1 already calls Get-InstalledVersion unconditionally, so
    # this adds no side effect), but a dry run changed none of it -- and its
    # child still emits init-ok, which would otherwise read as a real sign-in.
    if ($DryRun) {
        Common-Warn 'Dry run -- the state below is the machine as it already was, not the result of this run.'
    }

    # Normalise before anything indexes or counts: Set-StrictMode turns both a
    # $null .Count and an out-of-range [-1] into terminating errors, and an
    # unreached elevated child legitimately leaves no breadcrumbs at all.
    if ($null -eq $Steps) { $Steps = @() }
    $last = if ($Steps.Count) { $Steps[-1] } else { '' }
    # The step AFTER the last one that completed is the one it stopped in.
    $stopped = switch ($last) {
        ''                  { 'the first step (installing files)' }
        'files-ok'          { 'installing the background service' }
        'service-installed' { 'starting the background service' }
        'service-running'   { 'updating PATH' }
        'path-ok'           { 'sign-in' }
        'init-start'        { 'sign-in' }
        default             { '' }
    }
    if ($stopped) { Common-Warn "It stopped during: $stopped" }

    $version = Get-InstalledVersion
    $files = if ($version) { "installed ($version)" } else { 'not installed' }
    $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    $service = if (-not $svc) { 'not registered' }
               elseif ($svc.Status -eq 'Running') { 'registered and running' }
               else { "registered, not running ($($svc.Status))" }
    # Sign-in comes from the breadcrumbs, never from a filesystem probe. The
    # state dir's DACL grants SYSTEM, Administrators and the SID of whoever
    # elevated, so a Test-Path there succeeds when the user elevated their own
    # filtered token and fails when an administrator typed credentials over
    # their shoulder. A probe whose answer depends on which of those happened
    # is worse than no probe -- do not "fix" this by adding one.
    $signin = if ($Steps -contains 'init-ok') { 'completed' }
              elseif ($Steps -contains 'init-no-ai') { 'completed, but local inference is not running' }
              elseif ($Steps -contains 'init-failed') { 'did not complete' }
              elseif ($Steps -contains 'init-start') { 'started, did not finish' }
              elseif ($Steps -contains 'init-skipped') { 'skipped' }
              else { 'not reached' }

    Common-Log 'What is on this machine now:'
    Common-Log "  Waired files:        $files"
    Common-Log "  Background service:  $service"
    Common-Log "  Sign in:             $signin"
    Common-Log 'Re-run this installer to resume from where it stopped.'
}

function Invoke-SelfElevate {
    param([string]$ZipPath)

    Common-Log "Privileged step ahead -- requesting UAC..."

    # The state file lives beside the staged zip, in the workdir the
    # un-elevated parent created -- so the caller's existing finally already
    # cleans it up, and the parent can read the failure marker back afterwards.
    # The elevated token can read it: the default user-profile ACL grants
    # Administrators full control, which is already what lets the child open
    # the staged zip at all.
    $stateFile = Join-Path (Split-Path -Parent $ZipPath) 'install-state.json'
    Export-InstallState -Path $stateFile

    $tempScript = $null
    if ($PSCommandPath) {
        $scriptPath = $PSCommandPath
    } else {
        $url = "$(Resolve-ReleaseBase)/install.ps1"
        # No on-disk path (sourced via iwr|iex): stage the script body to a
        # temp .ps1 and re-launch it with -File, which binds the named
        # handoff params just like case (a). Writing to a file -- rather
        # than ScriptBlock-Create on the fetched bytes -- keeps install.ps1
        # out of Defender's in-memory download-and-execute AMSI heuristic
        # (#552) and reads the body back as text, so the Windows PowerShell
        # 5.1 octet-stream byte[] pitfall cannot occur.
        $tempScript = Join-Path $env:TEMP "waired-install-elevate-$([Guid]::NewGuid().ToString('N')).ps1"
        Invoke-WebRequest -Uri $url -OutFile $tempScript -UseBasicParsing
        # The fetched script comes from the RELEASE, which a pinned
        # WAIRED_VERSION can make older than this one. An older body has no
        # -StateFile parameter, and a binding failure happens before the first
        # statement runs -- no trap, no transcript, just a UAC window closing.
        # Refuse here, where the operator can still read it.
        if ((Get-Content -LiteralPath $tempScript -Raw) -notmatch '\[string\]\$StateFile\b') {
            Remove-Item -LiteralPath $tempScript -Force -ErrorAction SilentlyContinue
            Common-Die "the pinned release's install.ps1 ($Version) is older than this one and cannot receive the elevated handoff. Unset WAIRED_VERSION, or download install.ps1 and run it from disk."
        }
        $scriptPath = $tempScript
    }

    $psArgs = Get-ElevateArgs -ScriptPath $scriptPath -ZipPath $ZipPath -StatePath $stateFile

    try {
        $proc = $null
        try {
            # NOT -Wait: the parent mirrors the child's transcript while it
            # runs (Watch-ElevatedConsole), so it has to own the wait loop.
            # -PassThru on its own still blocks here until the UAC dialog is
            # answered, so a declined prompt is still reported below.
            $proc = Start-Process -FilePath 'powershell.exe' `
                -ArgumentList $psArgs -Verb RunAs -PassThru
        } catch {
            # ShellExecute failing is a TERMINATING error, so only try/catch
            # sees it -- -ErrorAction does nothing here.
            #
            # We cannot name the cause more precisely than this, and the
            # attempt is a trap: Start-Process catches the Win32Exception and
            # rethrows a bare InvalidOperationException with NO InnerException,
            # so ERROR_CANCELLED (1223) is gone before this catch runs, on
            # Windows PowerShell 5.1 and pwsh 7 alike. Verified on both, not
            # assumed. So do NOT "improve" this into `catch [Win32Exception]`
            # or an InnerException/NativeErrorCode walk -- both compile, read
            # correctly, and silently never fire. The OS message is localized
            # too, so it cannot be matched on either; quote it verbatim and
            # let the operator read it.
            Common-Warn "The Administrator step did not start, so nothing was installed."
            Common-Warn "Windows reported: $($_.Exception.Message)"
            Common-Log  "The usual cause is choosing No on the Administrator (UAC) prompt."
            Common-Die  "re-run and choose Yes, or open an Administrator PowerShell and run this installer there."
        }
        if ($null -eq $proc) {
            Common-Die "Windows did not start the elevated installer and gave no reason. Try running this installer from an Administrator PowerShell."
        }
        # Pin the process handle NOW. Without -Wait, Start-Process returns a
        # Process object whose ExitCode is only readable while that object
        # still holds an open handle; touching .Handle caches it before the
        # child can exit and its id be recycled.
        $null = $proc.Handle
        Watch-ElevatedConsole -Process $proc -Path $LogPath
        $proc.WaitForExit()

        # Read the breadcrumbs and the marker HERE: both live in the workdir,
        # which the caller's finally deletes the moment this returns.
        $steps  = Read-InstallProgress -Path "$stateFile.progress"
        $marker = "$stateFile.status"

        if ($proc.ExitCode -ne 0) {
            if ($steps -contains 'done') {
                # Setup ran to the end and the operator closed the window
                # rather than pressing Enter at the final pause. Windows
                # reports that as STATUS_CONTROL_C_EXIT, and treating it as a
                # failure -- which is what used to happen -- was the single
                # most common false alarm in #314.
                Common-Log "The Administrator window was closed before you pressed Enter, but setup had already finished."
                return
            }
            # A child that died before its transcript existed still leaves the
            # trap's marker behind (#177); a closed console leaves neither the
            # marker nor a transcript tail, which is what the decode is for.
            $why = ''
            if (Test-Path -LiteralPath $marker) {
                $why = ((Get-Content -LiteralPath $marker -Raw) -split "`r?`n")[0]
            }
            if (-not $why) { $why = Get-ExitCodeReason -Code $proc.ExitCode }
            $code = "$($proc.ExitCode) (0x$('{0:X8}' -f [int]$proc.ExitCode))"
            if ($why) {
                Common-Warn "The Administrator step stopped: $why"
                Common-Warn "Windows exit code $code."
            } else {
                Common-Warn "The Administrator step stopped with Windows exit code $code."
            }
            Show-InterruptedInstall -Steps $steps
            Common-Die "setup did not finish. Full install log: $LogPath"
        }
    } finally {
        # WaitForExit above guarantees the elevated child finished reading the
        # staged script before we delete it -- the guarantee -Wait used to
        # give. (PowerShell runs finally on exit, so Common-Die still cleans
        # up.)
        if ($tempScript) {
            Remove-Item -LiteralPath $tempScript -Force -ErrorAction SilentlyContinue
        }
    }
}

# -------------------------------------------------------------------
# Clean install (-Clean / WAIRED_CLEAN): full wipe, then fresh install
# -------------------------------------------------------------------

# Confirm the destructive -Clean wipe before anything runs. Mirrors
# uninstall.ps1's Confirm-Clean (-Yes bypass; a non-interactive session
# without -Yes aborts so a piped invocation can never silently wipe
# state) with the clean-INSTALL framing added. Runs in the un-elevated
# Phase-1 parent so the prompt reaches a real console before any UAC.
function Confirm-CleanInstall {
    if ($Yes) { return }
    $interactive = $false
    try { $interactive = -not [Console]::IsInputRedirected }
    catch { $interactive = [Environment]::UserInteractive }
    if ($interactive) {
        Common-Warn "-Clean will PERMANENTLY delete Waired config, keys and state,"
        Common-Warn "and Ollama + its downloaded models, then reinstall Waired fresh."
        $reply = Read-Host "[waired] Continue? [y/N]"
        if ($reply -notmatch '^(y|yes)$') { Common-Die "aborted - nothing was removed" }
        return
    }
    Common-Die "-Clean is destructive; re-run with -Yes to confirm on a non-interactive session (save the script to a file so -Clean -Yes bind)"
}

# -------------------------------------------------------------------
# Pre-install review: install-location prompt + summary + confirmation
# (fresh installs, Phase 1 only). Nothing -- no download, no UAC -- runs
# before the operator has seen what the script will do and agreed to it.
# -------------------------------------------------------------------

# Request-InstallDir offers the install location on a fresh interactive
# install when the operator did not pin one (-InstallDir / WAIRED_INSTALL_DIR
# / --install-dir / a previous install's registry record). Enter keeps the
# default. Validates that a custom path is absolute and warns when it lives
# under the user profile (the service runs as LocalSystem, so a profile path
# is fragile: profile ACLs, roaming, folder redirection).
function Request-InstallDir {
    if ($InstallDirExplicit) { return }
    if ($Yes -or -not (Test-InteractiveStdin)) { return }
    Write-Host ''
    $reply = Read-Host "[waired] Install location [$InstallDir] (Enter = default)"
    $reply = ([string]$reply).Trim().Trim('"')
    if (-not $reply) { return }
    if (-not [IO.Path]::IsPathRooted($reply)) {
        Common-Die "install location must be an absolute path (got '$reply')"
    }
    if ($env:USERPROFILE -and $reply.ToLowerInvariant().StartsWith($env:USERPROFILE.ToLowerInvariant())) {
        Common-Warn "that path is inside your user profile; the background service runs as LocalSystem and a profile path can break on ACL/roaming changes. Continuing anyway."
    }
    $script:InstallDir = $reply
    $script:InstallDirExplicit = $true
}

# Show-InstallSummary tells the operator what a fresh install is about to do,
# BEFORE anything runs. Content mirrors install.sh's show_install_summary.
function Show-InstallSummary {
    Section 'What this will do'
    $verLabel = if ($Version -eq 'latest') { 'latest stable release' }
                elseif ($Version -eq 'edge') { 'latest edge (main) build' }
                else { $Version }
    Write-Host "  * Download Waired ($verLabel) and install it to:"
    Write-Host "      $InstallDir"
    Write-Host "  * Register the waired-agent background service (starts at boot)"
    # Sign-in comes BEFORE the engine, because that is the order the install
    # runs in: the engine install moved into `waired init` (Set-OllamaEnvForInit
    # below), which asks whether this computer should run models first. Mirrors
    # install.sh's show_install_summary, which said the same thing while Linux
    # still pre-installed the engine behind the question's back (#138).
    if (-not $SkipInit) {
        Write-Host "  * Sign you in (opens your web browser)"
    }
    if (-not $SkipOllama) {
        Write-Host "  * Install the Ollama inference engine during sign-in, only if you"
        Write-Host "    choose to run models here (a few GB download)"
    }
    if (-not (Test-Admin)) {
        Write-Host "  * Ask for administrator rights (a Windows UAC prompt will appear)"
    }
    if ($ControlUrl) {
        Write-Host "  * Enrol this device against: $ControlUrl"
    }
}

# Confirm-Proceed is the single go / no-go gate for a fresh install. Runs in
# Phase 1 only (the elevated Phase-2 child never re-asks), after the summary.
# Skips: -Yes / -DryRun (preview) / -Clean (Confirm-CleanInstall already
# collected consent) / a non-interactive session (proceeds with a notice so
# CI one-liners keep working; pass -Yes to silence it).
function Confirm-Proceed {
    if ($Yes -or $DryRun -or $Clean) { return }
    if (-not (Test-InteractiveStdin)) {
        Common-Log "No interactive console detected -- proceeding without confirmation (use -Yes / -NonInteractive to silence this notice)."
        return
    }
    Write-Host ''
    $reply = Read-Host '[waired] Proceed with the install? [Y/n] (Enter = Yes)'
    if ($reply -match '^(n|no)$') {
        Common-Die 'aborted - nothing was installed'
    }
}

# Invoke-CleanWipe -- the wipe half of -Clean: delegate to uninstall.ps1
# (published as a release asset next to install.ps1 on both channels)
# rather than re-implementing the purge here. Prefers a sibling
# uninstall.ps1 when install.ps1 runs from a file (a checkout); the piped
# `iwr | iex` case fetches it from the same mirror/channel the install
# assets come from. uninstall.ps1 self-elevates for its privileged steps
# and runs its per-user teardown un-elevated as the invoking user
# (waired#754) -- which is exactly why the wipe is a child process here
# in un-elevated Phase 1, not a step inside our elevated Phase 2. That
# costs a second UAC prompt (wipe + install), the same two an operator
# clicking through uninstall.ps1 then install.ps1 by hand would see.
# Consent was already collected by Confirm-CleanInstall, so the child
# gets -Yes; under -DryRun it previews its own wipe commands. Any
# failure aborts before install work starts.
function Invoke-CleanWipe {
    $wipeScript = $null
    $fetched    = $null
    if ($PSCommandPath) {
        $sibling = Join-Path (Split-Path -Parent $PSCommandPath) 'uninstall.ps1'
        if (Test-Path -LiteralPath $sibling) { $wipeScript = $sibling }
    }
    if (-not $wipeScript) {
        $url = "$(Resolve-ReleaseBase)/uninstall.ps1"
        $fetched = Join-Path $env:TEMP "waired-clean-uninstall-$([Guid]::NewGuid().ToString('N')).ps1"
        Common-Log "Fetching the uninstaller from $url"
        Invoke-WebRequest -Uri $url -OutFile $fetched -UseBasicParsing
        $wipeScript = $fetched
    }
    Common-Log "Clean install: wiping the existing Waired install first"
    if (-not $DryRun) {
        Common-Log "(expect two UAC prompts total: one for the wipe, one for the install)"
    }
    # Quoted for the same reason as the elevation argv: -ArgumentList does no
    # quoting of its own, and $wipeScript sits next to a script path that can
    # contain spaces (#177).
    $wipeArgs = @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $wipeScript, '-Clean', '-Yes')
    if ($DryRun) { $wipeArgs += '-DryRun' }
    $wipeArgs = @($wipeArgs | ForEach-Object { ConvertTo-NativeArg $_ })
    try {
        $proc = Start-Process -FilePath 'powershell.exe' `
            -ArgumentList $wipeArgs -NoNewWindow -PassThru -Wait
        if ($proc.ExitCode -ne 0) {
            Common-Die "clean uninstall exited code $($proc.ExitCode) -- aborting the install (nothing was installed)"
        }
    } finally {
        if ($fetched) {
            Remove-Item -LiteralPath $fetched -Force -ErrorAction SilentlyContinue
        }
    }
}

# -------------------------------------------------------------------
# Asset download + verification
# -------------------------------------------------------------------

# Resolve-ReleaseTag -- the release TAG that names a pinned version. Tags
# carry a leading "v" ("v1.2.3"); the version itself does not, and
# `waired version` prints it without one. Both spellings of a pin are
# accepted so WAIRED_VERSION='1.2.3' does not 404 on
# releases/download/1.2.3 while WAIRED_VERSION='v1.2.3' works
# (waired-agent#781) -- install.sh accepts both too, and its help
# documents the bare form. The moving 'edge' prerelease is a tag already
# and takes no "v". Mirror of install.sh release_tag_for_pin.
function Resolve-ReleaseTag {
    param([string]$Pin)
    if ($Pin -match '^[0-9]') { return "v$Pin" }
    return $Pin
}

function Resolve-ReleaseBase {
    if ($Version -eq 'latest') {
        return "$BaseUrl/latest/download"
    }
    return "$BaseUrl/download/$(Resolve-ReleaseTag $Version)"
}

# Invoke-DownloadWithProgress streams $Url to $OutFile with a SINGLE in-place
# progress line on an interactive console (sparse fresh lines when output is
# redirected). Same implementation as scripts/install/ollama-windows.ps1's --
# see there for the full rationale (waired#747: the silent Invoke-WebRequest
# looked like a hang; a fresh line per few percent scrolled a wall of rows).
function Invoke-DownloadWithProgress {
    param(
        [Parameter(Mandatory)][string]$Url,
        [Parameter(Mandatory)][string]$OutFile
    )
    $interactive = $true
    try { $interactive = -not [Console]::IsOutputRedirected } catch { $interactive = $false }
    try {
        [Net.ServicePointManager]::SecurityProtocol =
            [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    } catch { }

    $req = [System.Net.WebRequest]::Create($Url)
    if ($req -is [System.Net.HttpWebRequest]) {
        $req.UserAgent         = 'waired-installer'
        $req.AllowAutoRedirect = $true
        $req.Timeout           = 60000
        $req.ReadWriteTimeout  = 120000
    }

    $resp = $null; $rs = $null; $fs = $null
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    try {
        $resp    = $req.GetResponse()
        $total   = [int64]$resp.ContentLength
        $totalMB = if ($total -gt 0) { $total / 1MB } else { 0 }
        $rs = $resp.GetResponseStream()
        $fs = [System.IO.File]::Create($OutFile)
        $buf      = [byte[]]::new(1MB)
        $done     = [int64]0
        $lastPct  = -100
        $lastTick = [double]0
        $read     = 0
        while (($read = $rs.Read($buf, 0, $buf.Length)) -gt 0) {
            $fs.Write($buf, 0, $read)
            $done   += $read
            $elapsed = $sw.Elapsed.TotalSeconds
            $pct     = if ($total -gt 0) { [int]($done * 100 / $total) } else { -1 }
            $rate    = if ($elapsed -gt 0) { ($done / 1MB) / $elapsed } else { 0 }
            if ($interactive) {
                if (($elapsed - $lastTick) -ge 0.25) {
                    $line = if ($total -gt 0) {
                        "  {0,3}%  ({1,7:N1} / {2:N1} MB)  {3:N1} MB/s" -f $pct, ($done / 1MB), $totalMB, $rate
                    } else {
                        "  {0:N1} MB downloaded  {1:N1} MB/s" -f ($done / 1MB), $rate
                    }
                    [Console]::Write("`r" + $line.PadRight(72))
                    $lastTick = $elapsed
                }
            } elseif ((($pct -ge 0) -and ($pct -ge $lastPct + 10)) -or (($elapsed - $lastTick) -ge 5)) {
                if ($total -gt 0) {
                    Write-Host ("  {0,3}%  ({1,7:N1} / {2:N1} MB)  {3:N1} MB/s" -f `
                        $pct, ($done / 1MB), $totalMB, $rate)
                    $lastPct = $pct
                } else {
                    Write-Host ("  {0:N1} MB downloaded  {1:N1} MB/s" -f ($done / 1MB), $rate)
                }
                $lastTick = $elapsed
            }
        }
        $fs.Flush()
    } finally {
        if ($fs)   { $fs.Close() }
        if ($rs)   { $rs.Close() }
        if ($resp) { $resp.Close() }
        $sw.Stop()
    }
    if ($interactive) {
        [Console]::Write("`r" + (' ' * 72) + "`r")
    }
    Write-Host ("  done: {0:N1} MB in {1:N0}s" -f `
        ((Get-Item -LiteralPath $OutFile).Length / 1MB), $sw.Elapsed.TotalSeconds)
}

function Get-AssetWithChecksum {
    param([string]$WorkDir)

    $releaseBase = Resolve-ReleaseBase
    $zipPath = Join-Path $WorkDir $ZipName
    $shaPath = Join-Path $WorkDir $ShaName

    Common-Log "Downloading $ZipName from $releaseBase"
    Common-Run "download $releaseBase/$ZipName -> $zipPath" {
        Invoke-DownloadWithProgress -Url "$releaseBase/$ZipName" -OutFile $zipPath
    }
    Common-Log "Downloading $ShaName"
    Common-Run "Invoke-WebRequest $releaseBase/$ShaName -> $shaPath" {
        Invoke-WebRequest -Uri "$releaseBase/$ShaName" -OutFile $shaPath -UseBasicParsing
    }

    if ($DryRun) { return $zipPath }

    # Expect a line of the shape "<hex>  waired-windows-amd64.zip"
    $expectedLine = (Get-Content -LiteralPath $shaPath -First 1).Trim()
    if (-not $expectedLine) {
        Common-Die "checksum file is empty: $shaPath"
    }
    $expected = ($expectedLine -split '\s+')[0].ToLowerInvariant()
    $actual   = (Get-FileHash -LiteralPath $zipPath -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($expected -ne $actual) {
        Common-Die "SHA-256 mismatch for ${ZipName}: expected $expected, got $actual"
    }
    Common-Log "Checksum OK ($actual)"
    return $zipPath
}

# -------------------------------------------------------------------
# Service install
# -------------------------------------------------------------------

function Stop-ExistingService {
    $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if (-not $svc) { return }

    Common-Log "Existing $ServiceName found (Status: $($svc.Status)); removing before re-install"
    if ($svc.Status -ne 'Stopped') {
        Common-Run "Stop-Service $ServiceName" {
            try { Stop-Service -Name $ServiceName -Force -ErrorAction Stop } catch {
                Common-Warn "Stop-Service failed: $($_.Exception.Message); falling back to sc.exe delete"
            }
        }
    }
    Common-Run "sc.exe delete $ServiceName" {
        $null = & sc.exe delete $ServiceName
        if ($LASTEXITCODE -ne 0) {
            Common-Die "sc.exe delete $ServiceName exited with code $LASTEXITCODE"
        }
        $deadline = (Get-Date).AddSeconds(10)
        while ((Get-Date) -lt $deadline) {
            if (-not (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue)) { return }
            Start-Sleep -Milliseconds 200
        }
        Common-Die "service still present 10s after sc.exe delete"
    }
}

# Where Extract-Zip expands before it touches anything, what it renames a
# file it cannot replace to, and where it keeps the previous binaries while
# a swap is in progress. All three live inside $InstallDir so every move
# below stays on one volume (a rename, not a copy), and all three are swept.
$StagingDirName  = '.waired-staging'
$DisplacedMarker = '.displaced-'
$RollbackDirName = '.waired-rollback'

# The armed rollback, or $null. Set by Backup-InstallDirFiles once the old
# binaries are safe, cleared by Clear-RollbackArm when the new ones are
# serving. While it is set, EVERY way this script can fail runs
# Invoke-PendingRollback on the way out -- see it for why that is armed
# rather than wrapped in a try/catch (waired-agent#1087).
$script:RollbackPlan = $null

# Extract-Zip puts the archive's files into $InstallDir without ever leaving
# it in a state the host cannot start from.
#
# NOT `Expand-Archive -Force` onto $InstallDir directly. That clears the
# destination entries BEFORE it writes any of them, so a single file it
# cannot write takes the whole directory with it. That is #819: an update
# removed waired-agent.exe and waired-tray.exe, failed on waired.exe, and
# left a host whose service had no executable to start. Measured on Windows
# PowerShell 5.1 against a temp directory with one destination file held
# open exclusively -- the run reports that file, and the OTHER destination
# files are gone. Not replaced, not left at their previous contents. Gone.
#
# So: expand into a staging directory, then move each file into place one
# at a time. A failure before the first move leaves the install exactly as
# it was; a failure during them leaves every other file updated and the
# failed one at its previous bytes. Either way the host still has binaries.
#
# One destination is expected to be unreplaceable. On the update path
# `waired update` is itself running $InstallDir\waired.exe, and a
# tray-initiated update is additionally running waired-tray.exe; Windows
# will not let a mapped image be overwritten. It will let one be RENAMED,
# which is how Move-IntoInstallDir gets the new file into place. The
# displaced copy goes on serving the process still running from it and is
# swept at the start of the next run.
#
# install.sh's darwin_install_binaries has always had this shape -- verify,
# unpack to a temp dir, then `install` one binary at a time. This is the
# Windows half catching up, not a new design.
#
# The expand and the move are two calls rather than one because the question
# waired-agent#1087 asks -- "will these new programs run on this computer?" --
# can only be put to files that already exist, and has to be answered while
# the running install is still untouched. Extract-Zip is the pair of them,
# for callers with nothing to check.

# Extract-Zip: expand, place, clean up.
function Extract-Zip {
    param([string]$ZipPath)
    $staging = Expand-ToStaging -ZipPath $ZipPath
    try {
        Move-StagedIntoInstallDir -Staging $staging
    } finally {
        Remove-StagingDir -Staging $staging
    }
}

# Expand-ToStaging unpacks the archive next to where its files are going and
# returns that directory. Nothing the host is using is touched: on the update
# path the service is still running at this point, deliberately.
function Expand-ToStaging {
    param([string]$ZipPath)
    $staging = Join-Path $InstallDir $StagingDirName
    Common-Run "Expand-Archive $ZipPath -> $InstallDir (staged, then moved file by file)" {
        if (-not (Test-Path -LiteralPath $InstallDir)) {
            New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
        }
        Clear-DisplacedFiles
        Clear-RollbackDir
        if (Test-Path -LiteralPath $staging) {
            Remove-Item -LiteralPath $staging -Recurse -Force
        }
        New-Item -ItemType Directory -Path $staging -Force | Out-Null
        Expand-Archive -LiteralPath $ZipPath -DestinationPath $staging -Force
    }
    return $staging
}

# Move-StagedIntoInstallDir places the expanded files, one at a time.
function Move-StagedIntoInstallDir {
    param([string]$Staging)
    Common-Run "move the staged files into $InstallDir (one at a time)" {
        # Deliberate order, not whatever the directory listing gives:
        # the files most likely to be in use go last, so if one of them
        # cannot be placed, everything else is already updated. waired.exe
        # is last of all -- on the update path it is the binary running
        # this -- and the tray is next-to-last, since a tray-initiated
        # update is running that one.
        $staged = Get-ChildItem -LiteralPath $Staging -File -Recurse | Sort-Object @{ Expression = {
            switch ($_.Name) { 'waired.exe' { 2 } 'waired-tray.exe' { 1 } default { 0 } }
        } }, Name
        foreach ($src in @($staged)) {
            $rel = $src.FullName.Substring($Staging.Length).TrimStart('\', '/')
            Move-IntoInstallDir -Source $src.FullName -Destination (Join-Path $InstallDir $rel)
        }
    }
}

# Remove-StagingDir clears the staging directory. Best effort: a leftover is
# swept by the next run, and failing an otherwise finished install over one
# would be worse than the leftover.
function Remove-StagingDir {
    param([string]$Staging)
    if ($DryRun -or -not $Staging) { return }
    Remove-Item -LiteralPath $Staging -Recurse -Force -ErrorAction SilentlyContinue
}

# Set-InstallDirFile places one file at $Destination, replacing whatever is
# there. Returns '' when it landed, or the reason it could not.
#
# [IO.File]::Replace is the atomic form: the destination is swapped for the
# new file or it is not, never truncated half-way, and the destination's
# ACL survives. It needs both paths on one volume (they are -- staging and
# the rollback copy are both inside $InstallDir) and it fails, without
# touching anything, when the destination cannot be opened for replacement.
# That failure is the running image, and the answer to it is to rename the
# old file aside: a mapped image cannot be replaced but can be moved.
#
# It reports rather than dies because it has two callers with opposite
# needs: Move-IntoInstallDir, which fails the install, and
# Invoke-PendingRollback, which is already handling a failure and has to
# put back as much as it can and then say what it managed.
function Set-InstallDirFile {
    param([string]$Source, [string]$Destination)

    $parent = Split-Path -Parent $Destination
    if ($parent -and -not (Test-Path -LiteralPath $parent)) {
        New-Item -ItemType Directory -Path $parent -Force | Out-Null
    }
    if (-not (Test-Path -LiteralPath $Destination)) {
        try {
            Move-Item -LiteralPath $Source -Destination $Destination -Force
            return ''
        } catch {
            return "$($_.Exception.Message)"
        }
    }
    try {
        # [NullString]::Value, not $null: PowerShell converts a bare $null
        # to "" for a [string] parameter, and Replace rejects that with
        # "The value cannot be an empty string" on every platform -- which
        # would send every file down the displacement path below.
        [System.IO.File]::Replace($Source, $Destination, [NullString]::Value)
        return ''
    } catch {
        # Fall through. Any reason the destination could not be replaced is
        # handled the same way, and displacing is safe even when the guess
        # about why is wrong.
    }
    $displaced = "$Destination$DisplacedMarker" + [Guid]::NewGuid().ToString('N').Substring(0, 8)
    Common-Log ("{0} is in use; renaming it aside as {1}" -f `
        (Split-Path -Leaf $Destination), (Split-Path -Leaf $displaced))
    try {
        Move-Item -LiteralPath $Destination -Destination $displaced -Force
        Move-Item -LiteralPath $Source -Destination $Destination -Force
        return ''
    } catch {
        return "$($_.Exception.Message)"
    }
}

# Move-IntoInstallDir places one staged file, and fails the install when it
# cannot.
function Move-IntoInstallDir {
    param([string]$Source, [string]$Destination)
    $why = Set-InstallDirFile -Source $Source -Destination $Destination
    if ($why) {
        # Nothing was removed getting here, so the file is still the one
        # that was there before and the host still runs. Name it, because
        # "install failed" over a path the operator cannot place is what
        # made #819 hard to read.
        Common-Die ("could not replace $Destination -- it is held open by a running process " +
                    "and could not be renamed aside either ($why). " +
                    "Close it, or reboot, and re-run the update.")
    }
}

# Clear-DisplacedFiles removes what an earlier run had to rename aside, and
# what ReplaceFile left behind when it could not tidy up after itself.
#
# The second pattern is Windows', not ours: [IO.File]::Replace moves the
# destination to a temporary "<name>~RF<hex>.TMP" and removes it on the way
# out, and when it cannot, that copy of the previous binary simply stays.
# Measured on a Windows host running edge (2026-08-29): four of them, old
# waired.exe and waired-tray.exe, sitting in the install directory.
# waired-agent#1087's report names one of these, because copying it was the
# only way back that host had -- which is why the rollback here keeps a copy
# of its own rather than counting on them.
#
# Best effort on purpose, for both patterns: one may still be the image of a
# process that has not exited, or the working temporary of a replace running
# right now, and Windows refuses to delete either. Neither is a reason to
# fail an install.
function Clear-DisplacedFiles {
    foreach ($pattern in @("*$DisplacedMarker*", '*~RF*.TMP')) {
        Get-ChildItem -LiteralPath $InstallDir -Filter $pattern -File -ErrorAction SilentlyContinue |
            ForEach-Object { Remove-Item -LiteralPath $_.FullName -Force -ErrorAction SilentlyContinue }
    }
}

# Clear-RollbackDir removes the previous binaries a run that never finished
# left behind. Same best-effort reasoning as Clear-DisplacedFiles: they are
# only disk, and they are only ever read by the run that wrote them.
function Clear-RollbackDir {
    $dir = Join-Path $InstallDir $RollbackDirName
    if (Test-Path -LiteralPath $dir) {
        Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction SilentlyContinue
    }
}

# ---- Will these files run here? (waired-agent#1087) ------------------------
#
# Waired's programs are not signed with a certificate Windows recognises
# (#759 deferred the signing), so Smart App Control and other
# application-control policies can refuse to execute them. A downloaded,
# checksum-verified archive is therefore not evidence that its programs can
# run, and the only way to find out is to run one.
#
# The refusal is per FILE, not per build and not per path. Measured on one
# Windows 11 Pro host with Smart App Control on (2026-08-29): the edge
# build's waired.exe was refused while the SAME archive's waired-agent.exe
# ran, and a release build's verdicts were the exact opposite. It also moves
# over days -- files that host refused on 2026-08-27 ran on 2026-08-29.
#
# So this asks before anything is stopped, and the rollback below covers the
# case the question cannot: a verdict that changes between the check and the
# service start.

# Get-StagedBinaryChecks -- the files an install or update places that
# Windows can refuse, what to ask each one, and whether a refusal is fatal.
#
# Fatal for waired.exe and waired-agent.exe, on both paths -- owner ruling,
# docs/decisions/20260829/1730-installer-refuses-programs-that-cannot-run.md:
# without the daemon there is no product, and without the CLI there is no
# `waired init`, `waired doctor` or `waired update`, so nobody could finish
# or diagnose the install. Not fatal for the app: a refused waired-tray.exe
# costs the Waired app, not the computer.
#
# What each call asks is "did Windows start this image", not what it
# printed. `waired-agent.exe -h` exits 1 by design (its flag set is
# flag.ContinueOnError, so -h prints usage and returns), and asserting on
# that usage text would abort a good update the day someone gives it a
# custom Usage. waired.exe is asked for `version --json` -- the same call
# Get-InstalledVersion makes -- and its exit code IS checked, because a
# program that starts and then cannot report its own version is not one to
# install either.
#
# Never a bare word as an argument: waired-agent's flag parsing stops at the
# first non-flag token, so `waired-agent.exe version` would start the daemon
# in the foreground and sit there.
function Get-StagedBinaryChecks {
    return @(
        @{ Name = 'waired.exe';       Arguments = @('version', '--json'); RequireZeroExit = $true;  Fatal = $true  },
        @{ Name = 'waired-agent.exe'; Arguments = @('-h');                RequireZeroExit = $false; Fatal = $true  },
        @{ Name = 'waired-tray.exe';  Arguments = @('-h');                RequireZeroExit = $false; Fatal = $false }
    )
}

# Test-BinaryRuns runs one file and reports '' when it ran, or the reason it
# could not.
function Test-BinaryRuns {
    param([string]$Path, [string[]]$Arguments, [bool]$RequireZeroExit)
    if (-not (Test-Path -LiteralPath $Path)) { return 'it is not in the downloaded archive' }
    # $ErrorActionPreference is 'Stop' for this whole script, and under it
    # ANY line a native command writes to stderr is raised as a terminating
    # error. Measured on Windows PowerShell 5.1: `waired-agent.exe -h`
    # prints its usage to stderr and threw, with the message "Usage of
    # waired-agent:" -- which would read here as a refusal. 'Continue' for
    # the length of the call separates the two: a program that RAN and wrote
    # to stderr is silent, while one Windows would not START still throws
    # ApplicationFailedException (measured under both settings).
    $previous = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        & $Path @Arguments 2>$null | Out-Null
        if ($RequireZeroExit -and $LASTEXITCODE -ne 0) {
            return "it ran but exited with code $LASTEXITCODE"
        }
        return ''
    } catch {
        return (Get-FailureReason $_)
    } finally {
        $ErrorActionPreference = $previous
    }
}

# Test-StagedBinaries runs the newly expanded programs and stops the run,
# with nothing on the computer touched, when one that matters is refused.
# $UnchangedNote says what the caller left untouched -- the sentence is the
# caller's because only it knows.
function Test-StagedBinaries {
    param([string]$Staging, [string]$UnchangedNote)
    if ($DryRun) {
        Common-Run "run the staged programs to check this computer will execute them" { }
        return
    }
    Common-Log 'Checking the new programs run on this computer before replacing anything'
    foreach ($check in @(Get-StagedBinaryChecks)) {
        if ($NoTray -and $check.Name -eq 'waired-tray.exe') { continue }
        $why = Test-BinaryRuns -Path (Join-Path $Staging $check.Name) `
                               -Arguments $check.Arguments -RequireZeroExit $check.RequireZeroExit
        if (-not $why) { continue }
        if (-not $check.Fatal) {
            Common-Warn ("the Waired app ({0}) will not run on this computer: {1}" -f $check.Name, $why)
            Common-Warn 'Setup continues; the background service and the waired command are not affected, but the app will not open until Windows accepts that file.'
            continue
        }
        # Named before dying, so the last thing on screen is the state the
        # computer is in rather than a stack of policy prose.
        Common-Warn ("Windows will not run the new {0} on this computer:" -f $check.Name)
        Common-Warn "  $why"
        Common-Warn "Waired's programs are not signed with a certificate Windows recognises, so Smart App Control"
        Common-Warn '(or another application-control policy) can refuse to run them. The refusal is per file and'
        Common-Warn 'can change on its own, so a later build -- or the same one, later -- may be accepted.'
        if ($UnchangedNote) { Common-Warn $UnchangedNote }
        Remove-StagingDir -Staging $Staging
        Common-Die ("stopped before replacing anything: Windows refused to run the new {0}" -f $check.Name)
    }
}

# ---- Putting it back (waired-agent#1087) -----------------------------------

# Backup-InstallDirFiles copies aside every file the staged archive is about
# to overwrite, and arms the rollback.
function Backup-InstallDirFiles {
    param([string]$Staging, [bool]$HadService, [string]$Version)
    $backup = Join-Path $InstallDir $RollbackDirName
    Common-Run "copy the current programs aside into $backup" {
        Clear-RollbackDir
        New-Item -ItemType Directory -Path $backup -Force | Out-Null
        foreach ($src in @(Get-ChildItem -LiteralPath $Staging -File -Recurse)) {
            $rel     = $src.FullName.Substring($Staging.Length).TrimStart('\', '/')
            $current = Join-Path $InstallDir $rel
            if (-not (Test-Path -LiteralPath $current)) { continue }
            $to     = Join-Path $backup $rel
            $parent = Split-Path -Parent $to
            if ($parent -and -not (Test-Path -LiteralPath $parent)) {
                New-Item -ItemType Directory -Path $parent -Force | Out-Null
            }
            # A copy, not [IO.File]::Replace's backup slot: Set-InstallDirFile
            # has two placement branches and only one of them can leave a
            # backup behind, so a copy is the single shape the restore reads.
            # Copying a running image is fine -- Windows refuses to overwrite
            # one, not to read it.
            Copy-Item -LiteralPath $current -Destination $to -Force
        }
    }
    if (-not $DryRun) {
        $script:RollbackPlan = @{ BackupDir = $backup; HadService = $HadService; Version = $Version }
    }
}

# Invoke-PendingRollback puts the previous programs back, restarts the
# service, and says what it managed.
#
# ARMED, not wrapped. The obvious shape -- try { swap } catch { put back } --
# does not work here: Common-Die ends in `exit`, which is not an exception,
# so a catch around the swap would miss every failure that goes through one
# (Move-IntoInstallDir's, Invoke-AgentInstall's). The two funnels that every
# Phase-2 failure already passes through call this instead: Common-Die, and
# the script-level trap for anything that never reached a Common-Die.
function Invoke-PendingRollback {
    $plan = $script:RollbackPlan
    if (-not $plan) { return }
    # Once. A failure inside the rollback must not re-enter it through a
    # Common-Die on the way out.
    $script:RollbackPlan = $null

    Common-Warn 'The update did not finish. Putting the previous version back.'
    $failed = @()
    foreach ($src in @(Get-ChildItem -LiteralPath $plan.BackupDir -File -Recurse -ErrorAction SilentlyContinue)) {
        $rel = $src.FullName.Substring($plan.BackupDir.Length).TrimStart('\', '/')
        $why = Set-InstallDirFile -Source $src.FullName -Destination (Join-Path $InstallDir $rel)
        if ($why) { $failed += "$rel ($why)" }
    }
    $serviceWhy = ''
    if ($plan.HadService) {
        try {
            Start-Service -Name $ServiceName -ErrorAction Stop
        } catch {
            $serviceWhy = "$($_.Exception.Message)"
        }
    }
    if ($failed.Count -eq 0 -and -not $serviceWhy) {
        if ($plan.HadService) {
            Common-Log ("Waired {0} is back in place and its background service is running again." -f $plan.Version)
        } else {
            Common-Log ("Waired {0} is back in place." -f $plan.Version)
        }
    } else {
        foreach ($f in $failed) { Common-Warn "could not put back $f" }
        if ($serviceWhy) { Common-Warn "could not restart ${ServiceName}: $serviceWhy" }
        Common-Warn 'Repair this computer by re-running the installer for the version it was on:'
        Common-Warn (("  `$env:WAIRED_VERSION='{0}'; iwr -useb {1}/latest/download/install.ps1 | iex") -f `
            $plan.Version, $BaseUrl)
    }
    Remove-Item -LiteralPath $plan.BackupDir -Recurse -Force -ErrorAction SilentlyContinue
}

# Clear-RollbackArm -- the new programs are serving, so the previous ones
# stop being a way back worth keeping.
function Clear-RollbackArm {
    $script:RollbackPlan = $null
    Clear-RollbackDir
}

# Remove waired-tray.exe after extraction when WAIRED_NO_TRAY is set.
function Remove-TrayIfRequested {
    if (-not $NoTray) { return }
    $tray = Join-Path $InstallDir 'waired-tray.exe'
    Common-Log "WAIRED_NO_TRAY set -- skipping tray binary"
    Common-Run "Remove-Item $tray" {
        if (Test-Path -LiteralPath $tray) {
            Remove-Item -LiteralPath $tray -Force
        }
    }
}

# ---- Tray surfacing (waired#755) -----------------------------------------
# install.ps1 historically neither created a Start Menu entry nor launched the
# tray, so its per-user autostart (HKCU\...\Run\waired-tray, written on the
# tray's first run) never registered -- unlike the .exe installer, which does
# both. These two helpers close that gap.

# Create the machine-wide "Waired" Start Menu group, mirroring the .exe
# installer's [Icons]: a "Waired" tray launcher + a "Waired (CLI)" help shortcut.
# Runs elevated (writes under %ProgramData%). This is the surface the installtest
# #755 contract asserts, and it gives users a discoverable launcher.
function New-StartMenuShortcuts {
    if ($NoTray) { return }
    $group = Join-Path $env:ProgramData 'Microsoft\Windows\Start Menu\Programs\Waired'
    $tray  = Join-Path $InstallDir 'waired-tray.exe'
    Common-Log "Creating Start Menu group: $group"
    Common-Run "create Start Menu shortcuts under $group" {
        # Best-effort: a WScript.Shell COM hiccup must not fail the whole
        # (elevated) install -- the tray still runs from $InstallDir.
        try {
            New-Item -ItemType Directory -Path $group -Force | Out-Null
            $ws = New-Object -ComObject WScript.Shell
            if (Test-Path -LiteralPath $tray) {
                $lnk = $ws.CreateShortcut((Join-Path $group 'Waired.lnk'))
                $lnk.TargetPath  = $tray
                $lnk.Description  = 'Waired system-tray app'
                $lnk.Save()
            }
            $cli = $ws.CreateShortcut((Join-Path $group 'Waired (CLI).lnk'))
            $cli.TargetPath = Join-Path $env:SystemRoot 'System32\cmd.exe'
            $cli.Arguments  = "/k `"$InstallDir\waired.exe`" --help"
            $cli.Description = 'Waired command-line help'
            $cli.Save()
        } catch {
            Common-Warn "could not create the Start Menu shortcuts ($($_.Exception.Message.Trim()))"
        }
    }
}

# Best-effort launch of the tray as the ORIGINAL (de-elevated) desktop user, so
# its first run registers HKCU autostart in the *logged-in* user's hive rather
# than the elevating admin's (waired#755) -- the install.ps1 analog of the .exe
# installer's `runasoriginaluser` [Run] flag. Interactive-only (mirrors the .iss
# `skipifsilent`): a silent / -NonInteractive / CI install never spawns the GUI
# and just leaves the tray to start at next logon (the Start Menu shortcut above
# is enough to surface it). explorer.exe runs as the interactive shell user, so
# the child it launches inherits that de-elevated token.
function Start-TrayAsOriginalUser {
    if ($NoTray) { return }
    if (-not (Test-InteractiveStdin)) {
        # Was a bare `return`. A silent skip here is what made the elevated /
        # SSH install look like it had surfaced the tray when it had not: the
        # banner below still claimed autostart, and nothing in the transcript
        # said the launch had not happened (waired-agent#832). The autostart
        # registration is independent of this and runs either way.
        Common-Log "No interactive desktop detected (SSH or service session) - not launching the tray now."
        return
    }
    $tray = Join-Path $InstallDir 'waired-tray.exe'
    if (-not (Test-Path -LiteralPath $tray)) { return }
    Common-Run "launch waired-tray as the original user (via explorer.exe)" {
        try {
            # Quoted: Start-Process does no quoting of its own, and the default
            # install dir is 'C:\Program Files\Waired' -- always a space. Unlike
            # the elevation argv (#177) this token is consumed by explorer.exe,
            # which takes a shell path rather than a CommandLineToArgvW argv, so
            # a failure here would surface as "the tray silently never starts"
            # rather than a parameter error. Explorer's tolerance of the
            # unquoted form is undocumented; the quoted form is the documented
            # one, and for a real file path (no embedded quote, no trailing
            # backslash) the two quoting rules coincide.
            Start-Process -FilePath (Join-Path $env:SystemRoot 'explorer.exe') `
                -ArgumentList (ConvertTo-NativeArg $tray) -ErrorAction Stop
        } catch {
            Common-Warn "could not auto-launch the tray ($($_.Exception.Message.Trim())); start `"$tray`" yourself or it runs at next logon"
        }
    }
}

# ---- Tray autostart, registered by the installer (waired-agent#832) -------
#
# Until now the ONLY writer of the HKCU Run value was the tray's own first
# run (internal/gui/tray/tray.go ensureAutostartOnFirstLaunch). That is fine
# on a desktop install, where Start-TrayAsOriginalUser hands the launch to
# Explorer and the tray registers itself. It is not fine anywhere else: an
# elevated or SSH-driven install has no interactive desktop for Explorer to
# hand off to, the launch silently did not happen, the tray therefore never
# ran, and the Run value was never written -- for anybody. The closing banner
# claimed autostart regardless, so the machine shipped with no tray icon, no
# autostart, and a transcript saying otherwise.
#
# So the installer writes the value itself, and the tray's first-run
# registration stays as the idempotent backstop (IsEnabled() only checks that
# a value is present, so whichever wrote it first wins and neither fights the
# other).
#
# It writes into HKEY_USERS\<console-user-SID>, never HKCU:, because HKCU:
# resolves to whoever this PROCESS is running as. Post-elevation that is the
# elevating administrator -- who may not even be the person at the keyboard,
# with over-the-shoulder UAC -- and writing there is the exact hazard
# uninstall.ps1's Remove-TrayAutostart comment names (waired#754). The
# interactive console user is the one whose desktop the tray belongs to,
# whether this install came from their own shell or from an SSH session
# alongside them.

# The management endpoint the tray is launched with. Must match the tray's
# own default (cmd/waired-tray/main.go -mgmt, from
# internal/management.DefaultListen); Get-TrayAutostartCommand's contract
# test in scripts/dev/installtest-windows.ps1 pins the pair.
$script:TrayMgmtUrl = 'http://127.0.0.1:9476'

# What Register-TrayAutostart decided, read by Show-NextSteps. Defaulted so a
# path that reaches the banner without the registration step (an update, a
# future caller) says nothing about autostart rather than something untrue.
$script:TrayAutostartPlan = 'skip:no-tray'
$script:TrayAutostartUser = ''

# Get-ConsoleUser identifies the user logged in at this machine's interactive
# desktop. Returns $null when nobody is -- a headless server, a CI runner, or
# a machine sitting at the logon screen -- which is a real answer, not a
# failure: there is no desktop for a tray icon to appear on.
#
# Win32_ComputerSystem.UserName is the console user specifically, not "the
# user of this process", which is why it survives elevation. The hive check
# is what makes the SID usable: HKEY_USERS only carries a subkey for a
# profile that is currently loaded, and a logged-on user always has one.
function Get-ConsoleUser {
    $name = $null
    try {
        $name = (Get-CimInstance -ClassName Win32_ComputerSystem -ErrorAction Stop).UserName
    } catch {
        return $null
    }
    if ([string]::IsNullOrWhiteSpace($name)) { return $null }
    $sid = $null
    try {
        $sid = (New-Object System.Security.Principal.NTAccount($name)).Translate(
            [System.Security.Principal.SecurityIdentifier]).Value
    } catch {
        return $null
    }
    if (-not (Test-Path -LiteralPath "Registry::HKEY_USERS\$sid")) { return $null }
    return [pscustomobject]@{ Name = $name; Sid = $sid }
}

# Get-TrayAutostartPlan is the whole decision, as a pure function of facts
# gathered elsewhere, so it can be lifted out of this file and table-tested
# without a UAC prompt, an SSH session or an interactive desktop -- none of
# which the CI runner can provide (CLAUDE.md "Test discipline": put the seam
# below the behaviour under test).
function Get-TrayAutostartPlan {
    param([bool]$NoTray, [bool]$TrayShipped, [string]$ConsoleUserSid)
    if ($NoTray)      { return 'skip:no-tray' }
    if (-not $TrayShipped) { return 'skip:not-shipped' }
    if ([string]::IsNullOrWhiteSpace($ConsoleUserSid)) { return 'skip:no-console-user' }
    return 'register'
}

# Get-TrayAutostartCommand builds the Run value byte-for-byte as the tray's
# own registration would (internal/platform/autostart/autostart_windows.go
# quoteCommand, with tray.go's "-mgmt <url>" args), so the two writers cannot
# produce two different entries for the same thing. ConvertTo-NativeArg is
# the fuller CommandLineToArgvW quoting and coincides with the Go helper for
# every input reachable here: an absolute .exe path and a URL, neither of
# which carries an embedded quote or a trailing backslash.
function Get-TrayAutostartCommand {
    param([string]$TrayPath, [string]$MgmtUrl)
    return (ConvertTo-NativeArg $TrayPath) + ' -mgmt ' + (ConvertTo-NativeArg $MgmtUrl)
}

# Get-TrayBannerLines renders what Show-NextSteps says about the tray, from
# what actually happened. Pure, for the same reason as Get-TrayAutostartPlan:
# the banner asserting autostart on a run that could not register it is the
# defect, so the wording has to be testable without a desktop.
function Get-TrayBannerLines {
    param(
        [string]$Plan,
        [string]$ConsoleUser,
        [string]$CurrentUser,
        [string]$InstallDir
    )
    $launch = "       Launch it from the Start Menu, or now: & `"$InstallDir\waired-tray.exe`""
    switch ($Plan) {
        'skip:no-tray' { return @() }
        'register' {
            # Win32_ComputerSystem.UserName is DOMAIN\user; [Environment]::
            # UserName is the bare account name. Compare the account halves,
            # but show the qualified name -- on a domain-joined machine the
            # bare name is not the identity the user recognises.
            $consoleAccount = ($ConsoleUser -split '\\')[-1]
            if ($consoleAccount -and $CurrentUser -and
                $consoleAccount.ToLowerInvariant() -ne $CurrentUser.ToLowerInvariant()) {
                return @(
                    "Tray:  a `"Waired`" Start Menu shortcut was created; the tray auto-starts when $ConsoleUser next signs in.",
                    $launch
                )
            }
            return @(
                'Tray:  a "Waired" Start Menu shortcut was created; the tray auto-starts at each logon.',
                $launch
            )
        }
        'skip:no-console-user' {
            return @(
                'Tray:  a "Waired" Start Menu shortcut was created. No signed-in desktop user was found,',
                '       so auto-start could not be registered - open Waired from the Start Menu once and',
                '       it will start at every logon after that.',
                $launch
            )
        }
        default {
            # skip:not-shipped -- an older zip with no waired-tray.exe.
            return @('Tray:  not installed (this build does not ship the Waired app).')
        }
    }
}

# ---- Replacing the running tray on an update (waired-agent#1046) ---------
#
# An update swaps the binaries and restarts the service, and until now did
# nothing at all about the app the user is looking at. Extract-Zip is in fact
# built AROUND that: it sorts waired-tray.exe next-to-last "since a
# tray-initiated update is running that one", and Move-IntoInstallDir renames a
# held image aside rather than replacing it. So the old tray went on running
# from a displaced copy until the next logon -- which means the app that just
# reported "Updated" was the previous build, and its About box said so.
#
# Get-TrayRestartPlan is the whole decision, as a pure function of facts, so it
# is table-testable without a desktop (the Get-TrayAutostartPlan idiom above).
#
# WasRunning is what keeps this honest: an update puts back what it took away
# and nothing more. A user who had closed the app does not get it reopened,
# which is the same restraint darwin_tray_autostart_notice describes on the
# macOS side -- an update must not decide for the user whether Waired is on
# their desktop.
function Get-TrayRestartPlan {
    param([bool]$NoTray, [bool]$TrayShipped, [bool]$WasRunning, [bool]$SameSession)
    if ($NoTray)           { return 'skip:no-tray' }
    if (-not $WasRunning)  { return 'skip:not-running' }
    if (-not $TrayShipped) { return 'skip:not-shipped' }
    # Same session as the app being replaced. Necessary, because Start-Process
    # reaches only the caller's session -- measured on sv-evox2 (2026-08-27),
    # where an ssh login lands in session 0 and the desktop is session 2. And
    # sufficient, because the app is drawn on that session: if we are in it,
    # there is a desktop to reopen into. When the reopen is out of reach the app
    # is left alone entirely, which is what shipped before this.
    #
    # Deliberately NOT Get-ConsoleUser, which was the first answer here and the
    # wrong question. It reads Win32_ComputerSystem.UserName, which is empty
    # while a session is logged on but DISCONNECTED -- the ordinary state of a
    # server someone RDPs into, and the state sv-evox2 was in when the first
    # version of this silently skipped a restart it should have made. Whose
    # desktop it is, is answered by the process being replaced.
    if (-not $SameSession) { return 'skip:other-session' }
    return 'restart'
}

# What Stop-TrayForUpdate decided, read by Start-TrayAfterUpdate. The two are
# separated because the stop has to happen BEFORE Extract-Zip -- that is the
# point, so the exe is replaced rather than displaced -- and the start after
# the service is back up.
$script:TrayRestartPlan = 'skip:no-tray'

function Get-RunningTrays {
    try { return @(Get-Process -Name 'waired-tray' -ErrorAction SilentlyContinue) }
    catch { return @() }
}

# Stop-TrayForUpdate terminates the running tray so Extract-Zip can replace its
# exe instead of renaming it aside.
#
# Terminate, not ask: Windows has no graceful stop for this process at all --
# it is linked -H windowsgui, so MainWindowHandle is 0, CloseMainWindow answers
# $false and taskkill without /F refuses (measured, waired-agent#1059). The
# wind-down therefore does not run, which is right here anyway: this is a
# restart, and planShutdown's causeRestart says a restart must not stop the
# engine only to start it again.
function Stop-TrayForUpdate {
    $procs = @(Get-RunningTrays)
    $tray  = Join-Path $InstallDir 'waired-tray.exe'

    # Compare against the tray's own session, not Explorer's: it is the process
    # being replaced, so it is the one whose desktop has to be reachable.
    $same = $false
    if ($procs.Count -gt 0) {
        try { $same = ($procs[0].SessionId -eq (Get-Process -Id $PID).SessionId) } catch { $same = $false }
    }

    $script:TrayRestartPlan = Get-TrayRestartPlan -NoTray:$NoTray `
        -TrayShipped:(Test-Path -LiteralPath $tray) `
        -WasRunning:($procs.Count -gt 0) -SameSession:$same
    if ($script:TrayRestartPlan -ne 'restart') {
        if ($script:TrayRestartPlan -eq 'skip:other-session') {
            Common-Log "The Waired app is open on a desktop this session cannot reach - leaving it running. It picks up the new version at the next sign-in."
        }
        return
    }

    foreach ($p in $procs) { Common-Log "Closing the Waired app (waired-tray, PID $($p.Id)) so the update can replace it" }
    Common-Run "Stop-Process -Force $(($procs | ForEach-Object { $_.Id }) -join ', ')" {
        foreach ($p in $procs) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue }
        foreach ($p in $procs) { try { [void]$p.WaitForExit(15000) } catch { } }
    }
}

# Start-TrayAfterUpdate reopens the app on the new binary, through explorer.exe
# so it lands in the console user's session with their own token rather than
# this elevated one -- the same hop Start-TrayAsOriginalUser uses.
#
# Deliberately NOT gated on Test-InteractiveStdin the way the fresh-install
# launch is. That predicate asks whether THIS process has a console, and a
# tray-initiated update reaches the installer through elevation with none --
# so the gate that fits a `curl | iex` install would have made every
# tray-initiated update close the app and leave it closed. What matters here is
# whether there is a desktop to reopen into, which is Get-ConsoleUser.
function Start-TrayAfterUpdate {
    if ($script:TrayRestartPlan -ne 'restart') { return }
    $tray = Join-Path $InstallDir 'waired-tray.exe'
    if (-not (Test-Path -LiteralPath $tray)) { return }
    Common-Run "reopen waired-tray (via explorer.exe)" {
        try {
            Start-Process -FilePath (Join-Path $env:SystemRoot 'explorer.exe') `
                -ArgumentList (ConvertTo-NativeArg $tray) -ErrorAction Stop
        } catch {
            Common-Warn "could not reopen the Waired app ($($_.Exception.Message.Trim())); open it from the Start Menu, or it returns at your next sign-in"
        }
    }
}

# Register-TrayAutostart carries out the plan and records what happened for
# the banner. Best-effort throughout: a machine that ends up without an
# autostart entry is a machine the user opens from the Start Menu, which is
# not worth failing an otherwise complete install over -- but it IS worth
# saying, which is what $script:TrayAutostartPlan is for.
function Register-TrayAutostart {
    $tray = Join-Path $InstallDir 'waired-tray.exe'
    $user = $null
    if (-not $NoTray) { $user = Get-ConsoleUser }
    $sid  = ''
    if ($user) { $sid = $user.Sid }

    $plan = Get-TrayAutostartPlan -NoTray:$NoTray `
        -TrayShipped:(Test-Path -LiteralPath $tray) -ConsoleUserSid $sid
    $script:TrayAutostartPlan = $plan
    $script:TrayAutostartUser = ''
    if ($user) { $script:TrayAutostartUser = $user.Name }

    if ($plan -ne 'register') {
        if ($plan -eq 'skip:no-console-user') {
            Common-Log "No user is signed in at this computer's desktop - not registering the tray autostart."
        }
        return
    }

    $key = "Registry::HKEY_USERS\$sid\Software\Microsoft\Windows\CurrentVersion\Run"
    $cmd = Get-TrayAutostartCommand -TrayPath $tray -MgmtUrl $script:TrayMgmtUrl
    Common-Log "Registering the tray autostart for $($user.Name)"
    Common-Run "set $key\waired-tray" {
        try {
            if (-not (Test-Path -LiteralPath $key)) {
                New-Item -Path $key -Force | Out-Null
            }
            Set-ItemProperty -Path $key -Name 'waired-tray' -Value $cmd -ErrorAction Stop
        } catch {
            Common-Warn "could not register the tray autostart ($($_.Exception.Message.Trim())); the Waired app registers it itself the first time you open it"
            $script:TrayAutostartPlan = 'skip:no-console-user'
        }
    }
}

function Invoke-AgentInstall {
    $exe = Join-Path $InstallDir 'waired-agent.exe'
    # NOTE: do NOT name this `$args` -- that is a PowerShell automatic
    # variable holding the un-bound positional arguments of the enclosing
    # scope. The Common-Run scriptblock below is evaluated via `& $Action`
    # inside Common-Run's own scope, where `$args` resolves to
    # Common-Run's (empty) automatic, NOT to this function's assignment.
    # The result was `& $exe @args` = `& $exe` (no args), so
    # waired-agent.exe was invoked WITHOUT the `install` subcommand,
    # fell through to the foreground daemon path, and exited with
    # `no identity at <user APPDATA>` -- which looked like an install
    # failure but was really an automatic-variable scoping bug. The
    # developer-facing scripts/install/waired-agent-windows.ps1 already
    # uses `$installArgs` for exactly this reason; match it.
    # $LogLevel was validated + normalised by Resolve-LogLevel in main, before
    # UAC (both phases run main, so the elevated child re-validates too).
    $installArgs = @('install')
    if ($StateDir) { $installArgs += "-state-dir=$StateDir" }
    # No --log-level here, deliberately (waired-agent#801). Everything after
    # `--` is baked into the SCM ImagePath, and an agent flag outranks
    # agent.json at every boot -- so the install-time level survived every
    # restart while `waired config log-level` did not, and an updated host
    # silently went back to whatever it was installed with. The level is a
    # persisted setting now: Set-PersistedLogLevel writes it through the
    # running daemon once the service is up.
    Common-Log "Running: $exe $($installArgs -join ' ')"
    Common-Run "& $exe $($installArgs -join ' ')" {
        & $exe @installArgs
        if ($LASTEXITCODE -ne 0) {
            Common-Die "waired-agent install exited with code $LASTEXITCODE"
        }
    }
}

# Get-AgentStateDir returns the state dir this install actually uses:
# $WAIRED_STATE_DIR when set, else %ProgramData%\waired. It was copy-pasted
# into four call sites (init argv, the re-run hint, Next steps, the update
# summary) plus the new agent.env writer below -- and every one of them has to
# agree, or `waired init` reads a control URL from a directory the installer
# never wrote to. One expression, one place.
function Get-AgentStateDir {
    if ($StateDir) { $StateDir } else { $AgentStateDir }
}

# Write-ControlUrlEnvFile persists the resolved Control Plane URL to
# <state dir>\agent.env, the Windows analog of Linux's /etc/waired/agent.env
# and macOS's <state dir>/agent.env (install.sh's linux_apt_write_control_url /
# darwin_write_control_url). `waired init` reads it back as the --control
# default via controlurl.PlatformDefault (internal/controlurl).
#
# Why it matters (#42): Get-WairedInitArgs passes --control only when init runs
# here. On any install where it does not enroll -- -SkipInit, a cancelled or
# failed sign-in, or the `iwr | iex` form where -Dev / -Control cannot bind --
# a later bare `waired init` had nothing to recover the URL from and silently
# fell back to the baked production Control Plane. Linux and macOS never had
# that hole. The daemon reads the same file for sign-in from the app (#174):
# there is no SCM EnvironmentFile, so reading it directly is the only way
# daemon-driven login can honour a -Dev/-Control install.
#
# Overwrite, unlike darwin_write_control_url's "already set -- leaving it
# as-is". That rule exists because on Linux agent.env is an operator-editable
# file (NOT a .deb conffile, as this comment used to say -- nothing in
# packaging/nfpm/waired.yaml.tmpl is marked `type: config`; the .deb ships
# agent.env.example and packaging/debian/waired/postinst copies it into place,
# so dpkg holds no md5sum for it), and it also clears $CONTROL_URL so init drops
# --control. Copying it here would make `install.ps1 -Control https://new` on a
# host that still has an old agent.env (a non-clean uninstall keeps the state
# dir) silently enrol against the OLD URL -- a regression against today's
# behaviour. On Windows the file is installer-owned and single-purpose, so the
# rule is simply: it always reflects what THIS install enrols against.
#
# Must run after Invoke-AgentInstall, which creates the state dir and applies
# its SYSTEM + Administrators DACL (secrets.SecureDir). The file inherits that
# DACL, exactly as identity.json does -- no explicit ACL work here.
function Write-ControlUrlEnvFile {
    if (-not $ControlUrl) { return }
    # NOT named $stateDir: PowerShell variable names are case-insensitive, so
    # that would shadow the script-scoped $StateDir for the rest of this
    # function.
    $agentState = Get-AgentStateDir
    $envFile    = Join-Path $agentState 'agent.env'

    if ($DryRun) {
        Common-Log "  (dry-run) would write WAIRED_CONTROL_URL=$ControlUrl to $envFile"
        return
    }

    # Never create the directory here: an un-ACLed %ProgramData%\waired would
    # defeat the lockdown waired-agent install applies. If it is missing the
    # install already failed louder than this.
    if (-not (Test-Path -LiteralPath $agentState)) {
        Common-Warn "$agentState not present after install -- skipping control-URL auto-config"
        return
    }

    $existing = @()
    if (Test-Path -LiteralPath $envFile) {
        $existing = @(Get-Content -LiteralPath $envFile -ErrorAction SilentlyContinue)
    }
    $prior = $existing | Where-Object { $_ -match '^\s*WAIRED_CONTROL_URL\s*=\s*\S' } | Select-Object -Last 1
    if ($prior) {
        $priorUrl = ($prior -replace '^\s*WAIRED_CONTROL_URL\s*=\s*', '').Trim()
        if ($priorUrl -ne $ControlUrl) {
            Common-Warn "$envFile had WAIRED_CONTROL_URL=$priorUrl; replacing it with $ControlUrl"
        }
    }
    # Keep any other keys a future version (or an operator) put there.
    $lines = @($existing | Where-Object { $_ -notmatch '^\s*WAIRED_CONTROL_URL\s*=' })
    $lines += "WAIRED_CONTROL_URL=$ControlUrl"

    Common-Log "Writing WAIRED_CONTROL_URL=$ControlUrl to $envFile"
    try {
        # UTF-8 WITHOUT a BOM, explicitly. `Set-Content -Encoding UTF8` emits a
        # BOM on Windows PowerShell 5.1, and the Go reader scans raw lines --
        # it would see a BOM'd first key and never match WAIRED_CONTROL_URL.
        # ASCII would dodge the BOM but mangle a non-ASCII -Control value.
        [IO.File]::WriteAllText($envFile, (($lines -join "`r`n") + "`r`n"),
            (New-Object Text.UTF8Encoding $false))
    } catch {
        # Best-effort, like Set-InstallDirRegistry: the install itself is fine
        # and this run still passes --control to init. Only a LATER bare
        # `waired init` loses the URL, and saying so beats failing the install.
        Common-Warn "could not write $envFile ($($_.Exception.Message.Trim())); a later 'waired init' will need --control $ControlUrl"
    }
}

# Add-InstallDirToPath appends $InstallDir to the machine PATH so `waired` and
# `waired-agent` resolve as bare commands in newly-opened shells (the original
# install left them callable only by full path). Runs only in the elevated
# phase -- the machine PATH lives under HKLM. Idempotent: a no-op when the dir
# is already present (case-insensitive `-contains`). SetEnvironmentVariable with
# the Machine target broadcasts WM_SETTINGCHANGE, so freshly-launched shells
# pick it up; shells already open when the installer ran still need a restart.
function Add-InstallDirToPath {
    $machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    $entries = @($machinePath -split ';' | Where-Object { $_ -ne '' })
    if ($entries -contains $InstallDir) {
        Common-Log "machine PATH already contains $InstallDir"
        return
    }
    Common-Log "Adding $InstallDir to machine PATH (open a new shell to use 'waired')."
    Common-Run "machine PATH += $InstallDir" {
        [Environment]::SetEnvironmentVariable(
            'Path', "$($machinePath.TrimEnd(';'));$InstallDir", 'Machine')
        # Update this process's PATH too so a same-window retry sees it.
        $env:PATH = "$($env:PATH.TrimEnd(';'));$InstallDir"
    }
}

# Set-InstallDirRegistry records the resolved install dir under
# HKLM\SOFTWARE\Waired so the uninstaller and later -Update / re-runs find a
# relocated install (-InstallDir). Runs in the elevated phase (HKLM). The
# GUI installer (waired-setup.iss) writes the same value. Best-effort: a
# registry hiccup must not fail the install -- the default-location fallback
# still works.
function Set-InstallDirRegistry {
    Common-Run "registry: $InstallDirRegKey\InstallDir = $InstallDir" {
        try {
            if (-not (Test-Path -LiteralPath $InstallDirRegKey)) {
                New-Item -Path $InstallDirRegKey -Force | Out-Null
            }
            Set-ItemProperty -Path $InstallDirRegKey -Name 'InstallDir' -Value $InstallDir
        } catch {
            Common-Warn "could not record the install dir in the registry ($($_.Exception.Message.Trim())); uninstall/update will assume the default location"
        }
    }
}

# Test-InteractiveStdin reports whether Read-Host will work without
# wedging. Honours -NonInteractive, then [Console]::IsInputRedirected
# (CI / `iwr | iex` with a redirected stdin), and falls back to
# UserInteractive on hosts that don't expose IsInputRedirected.
function Test-InteractiveStdin {
    if ($NonInteractive) { return $false }
    try {
        return -not [Console]::IsInputRedirected
    } catch {
        return [Environment]::UserInteractive
    }
}

# Set-OllamaEnvForInit resolves the Ollama knobs into the environment the
# `waired init` child inherits. The engine install itself lives INSIDE init
# (it asks "run local inference?" first, then installs when the answer calls
# for one) -- installing here, before init, made init re-detect waired's own
# install as a "foreign" Ollama. The outcome line for Show-NextSteps is set
# here too (mirror of install.sh's $ollama_status).
function Set-OllamaEnvForInit {
    if ($SkipOllama) {
        $env:WAIRED_NO_OLLAMA = '1'
        $script:OllamaStatus = 'skipped (-SkipOllama / WAIRED_NO_OLLAMA; install the engine later from an elevated prompt: waired runtimes install ollama)'
        return
    }
    if ($OllamaGpuMode -and $OllamaGpuMode -ne 'auto') { $env:WAIRED_OLLAMA_GPU_MODE = $OllamaGpuMode }
    $script:OllamaStatus = if ($SkipInit) {
        'not installed yet (installed during sign-in: waired init)'
    } else {
        'decided at sign-in (installed by waired init when local inference is on)'
    }
}

# Get-WairedInitArgs builds the `waired init` argv. Split out of
# Invoke-WairedInit so the WAIRED_ARGTEST seam can print the exact argv a run
# would use without installing anything -- the argv is where the
# --inference-enabled spelling bug lived, and an assert needs to see it.
function Get-WairedInitArgs {
    $initArgs = @('init', '--state-dir', (Get-AgentStateDir))
    # waired init self-defaults the Control Plane URL (machine env var /
    # baked production default), so --control is only passed when we have an
    # explicit one. This is why init no longer needs a URL to run.
    if ($ControlUrl) { $initArgs += @('--control', $ControlUrl) }
    # -Yes means "assume yes at every prompt", which install.sh --help has
    # always spelled out as covering init's prompts too; Windows never applied
    # it there. Folded in HERE rather than into $NonInteractive globally --
    # that would also suppress the Phase-2 "Press Enter to close this window"
    # pause (waired#748) and take the elevated window's output with it.
    if ($Yes -or -not (Test-InteractiveStdin)) { $initArgs += '--non-interactive' }
    # -SkipClaudeProxy is forwarded into `waired init` (the single decider of
    # Claude Code routing) rather than being applied by a separate post-init
    # `waired claude enable` step -- an unconditional enable there used to
    # override an interactive "no" to the routing prompt (issue: routing left
    # on despite declining). Env form WAIRED_NO_CLAUDE_PROXY is already folded
    # into $SkipClaudeProxy above, so this one line covers both.
    if ($SkipClaudeProxy) { $initArgs += '--skip-claude-route' }
    # --inference-enabled / --share-with-mesh are Go BOOL flags, and Go's flag
    # parser does not consume the following token for a bool. The space form
    # `--inference-enabled true` therefore set the flag to true and left "true"
    # as a positional argument, which `waired init` (cobra.NoArgs) rejected:
    # `unknown command "true"`, exit 1 -- so BOTH true and false killed
    # enrolment outright. Emit the single-token `=` form, the spelling every
    # other caller in the repo already uses.
    if ($InferenceEnabled) { $initArgs += "--inference-enabled=$InferenceEnabled" }
    if ($ShareWithMesh)    { $initArgs += "--share-with-mesh=$ShareWithMesh" }
    return $initArgs
}

# Invoke-WairedInit runs `waired.exe init` so enrolment happens inside
# the installer instead of as a manual post-install step. The elevated
# PS console opened by Start-Process -Verb RunAs has its own stdin, so
# the OAuth flow and the install-time inference role prompt work
# normally. State always lives under $AgentStateDir
# (= %ProgramData%\waired) so the SCM-mode agent picks it up -- also
# side-steps the agent-side state-dir mismatch tracked in issue #113.
# Only runs when -Control / -Dev resolved a CP URL.
function Invoke-WairedInit {
    # Records the enrolment outcome in the script-scoped $InitRan flag instead
    # of returning it. The `& $exe @initArgs` below writes waired.exe's stdout
    # to the success stream, so a caller that ASSIGNED this function's result
    # (`$x = Invoke-WairedInit`) would fold that stdout into the value and turn
    # it into an Object[] -- which then can't bind to [bool]$InitRan in
    # Show-NextSteps. Keeping the boolean out-of-band lets the callers invoke
    # this as a bare statement, so waired init's stdout (the actionable
    # "couldn't reach the Control Plane" hint, the deploy plan, and the
    # interactive OAuth prompts) reaches the real console untouched.
    # Breadcrumbs at every exit, not just at the end: $script:InitRan is a
    # single bool and cannot tell "skipped" from "failed", and the parent needs
    # that distinction to say something true about sign-in after the elevated
    # console was closed (#314).
    $script:InitRan = $false
    if ($SkipInit) {
        Common-Log "-SkipInit set; not running waired init."
        Write-InstallProgress 'init-skipped'
        return
    }
    # No terminal -> skip sign-in, the same call install.sh has always made
    # (linux_maybe_init / darwin_maybe_init gate on tty_available and print a
    # "finish later" note). Windows used to attempt it regardless, so an
    # unattended image build ended up in a different state per OS. Sign-in is
    # browser-driven and interactive; --non-interactive only means "take
    # hardware-derived defaults for the setup questions", it does not make
    # sign-in headless. -NonInteractive is the explicit override for callers
    # that do want the attempt -- also mirroring install.sh.
    if (-not $NonInteractive -and -not (Test-InteractiveStdin)) {
        Common-Log "No terminal detected -- sign-in skipped. To finish setup:"
        Common-Log "  - run:  waired init"
        Common-Log "  - or open the tray app and pick `"Sign in...`""
        Common-Log "  - or re-run the installer with -NonInteractive to attempt it anyway"
        Write-InstallProgress 'init-skipped'
        return
    }

    $exe = Join-Path $InstallDir 'waired.exe'
    if (-not (Test-Path -LiteralPath $exe)) {
        Common-Warn "waired.exe not found at $exe; cannot run `waired init`."
        Write-InstallProgress 'init-skipped'
        return
    }

    $stateForInit = Get-AgentStateDir
    $initArgs = Get-WairedInitArgs

    Common-Log "Running: $exe $($initArgs -join ' ')"
    # Emitted BEFORE the call: this is the long interactive step (browser
    # sign-in, then the engine download), so it is where an operator who
    # thinks the installer has hung closes the window. "It stopped during
    # sign-in" is only sayable if the breadcrumb precedes the wait.
    Write-InstallProgress 'init-start'
    if ($DryRun) {
        Common-Run "& $exe $($initArgs -join ' ')" { }
        $script:InitRan = $true
        Write-InstallProgress 'init-ok'
        return
    }
    & $exe @initArgs
    if ($LASTEXITCODE -eq $WairedInitLocalAIDown) {
        # Enrolment DID complete. Telling the operator to re-run `waired
        # init` here would be wrong advice -- it would sign in a device
        # that is already signed in, and change nothing about the engine.
        $script:InitRan     = $true
        $script:LocalAIDown = $true
        Write-InstallProgress 'init-no-ai'
        return
    }
    if ($LASTEXITCODE -ne 0) {
        Common-Warn "waired init exited with code $LASTEXITCODE -- enrolment did not complete."
        Common-Warn "Re-run manually: & `"$exe`" init --state-dir `"$stateForInit`""
        Write-InstallProgress 'init-failed'
        return
    }
    $script:InitRan = $true
    Write-InstallProgress 'init-ok'
}

function Show-NextSteps {
    param([bool]$InitRan = $false)
    $cpHint  = Get-AgentStateDir
    $url     = if ($ControlUrl) { $ControlUrl } else { 'https://your-cp.example.com' }
    $haveUrl = [bool]$ControlUrl
    Section 'Done'
    Write-Host ''
    Write-Host "$(Emo (Glyph 0x1F389) '*') Waired is installed." -ForegroundColor Green
    if ($haveUrl) {
        Write-Host "Control Plane URL: $url" -ForegroundColor Green
    }
    Write-Host ''
    if ($InitRan) {
        Write-Host "$(Emo (Glyph 0x2705) '[ok]') Enrolled - the agent service is running." -ForegroundColor Green
        Write-Host "  Check it:  & `"$InstallDir\waired.exe`" status   (try: & `"$InstallDir\waired.exe`" infer `"hello, world!`")"
        # Mirrors install.sh's set_local_ai_note, same wording. The headline
        # above stays "Waired is installed", because it is: the files are on
        # disk, the service is running, and the device is signed in. Only
        # local inference is missing (#310).
        if ($script:LocalAIDown) {
            Write-Host ''
            Write-Host "$(Emo (Glyph 0x26A0) '!')  Local inference is not running on this device." -ForegroundColor Yellow
            Write-Host '    Sign-in is finished; only local inference is missing.'
            Write-Host '    Details:      waired doctor'
        }
    } else {
        Write-Host "$(Emo (Glyph 0x1F527) '*') The agent service is running - ready for sign-in."
        Write-Host "  Sign in:   & `"$InstallDir\waired.exe`" init"
        Write-Host '             (or right-click the waired-tray icon and pick "Sign in...")'
        Write-Host "  Verify:    & `"$InstallDir\waired.exe`" status"
    }
    Write-Host ''
    Write-Host 'The agent service is enabled at boot and running now.'
    Write-Host ''
    # What the tray lines say depends on what Register-TrayAutostart managed
    # to do. They used to assert autostart unconditionally, on a run that had
    # just silently failed to register it (waired-agent#832).
    # @() because PowerShell unrolls a one-element array on return, and the
    # single-line arms would otherwise reach .Count as a bare string.
    $trayLines = @(Get-TrayBannerLines -Plan $script:TrayAutostartPlan `
        -ConsoleUser $script:TrayAutostartUser `
        -CurrentUser ([Environment]::UserName) -InstallDir $InstallDir)
    if ($trayLines.Count -gt 0) {
        foreach ($l in $trayLines) { Write-Host $l }
        Write-Host ''
    }
    # Ollama status line, mirroring install.sh's `Ollama: $ollama_status`
    # summary (set by Set-OllamaEnvForInit). Guarded so callers that never
    # reach the install step (e.g. -Update) don't print a blank label.
    if ($script:OllamaStatus) {
        Write-Host "Ollama:            $script:OllamaStatus"
    }
    Write-Host "State / identity:  $cpHint"
    Write-Host "PATH:              $InstallDir (added to PATH; open a NEW shell to run 'waired' directly)"
    # The agent's own log file, not the Event Log query this used to name.
    # internal/platform/logsink mirrors Warn and above to the Event Log, so
    # that query answers "was there an error" but never "what was the daemon
    # doing": every INFO and DEBUG record is in the file (#636). $cpHint is
    # the resolved state dir (Get-AgentStateDir), so a -StateDir install
    # points at its own path rather than the default.
    Write-Host "Diagnostics:       waired doctor   (logs: $cpHint\logs\waired-agent.log)"
    Write-Host "Uninstall:         & `"$InstallDir\waired-agent.exe`" uninstall"
    Write-Host 'More:              waired init --help'
    Write-Host 'Quickstart:        https://docs.waired.ai/quickstart/'
    Write-Host ''
}

# Invoke-InstallSteps is the privileged fresh-install step sequence, shared
# by the elevated Phase-2 child and the already-admin inline path (they used
# to be two copy-pasted blocks). Section headings split the console output
# into readable steps. Invoke-WairedInit runs as a bare statement (not
# assigned) so waired init's stdout reaches the console; it records the
# outcome in $script:InitRan.
function Invoke-InstallSteps {
    param([string]$ZipPath)
    Section 'Installing files'
    # Expand and CHECK before Stop-ExistingService, which sc.exe-DELETES the
    # registered service: a host whose new programs Windows refuses to run
    # must not lose the install it has to find that out (waired-agent#1087).
    # On a host with nothing installed the same order simply means an
    # abandoned install rather than a half-made one.
    $staging = Expand-ToStaging -ZipPath $ZipPath
    try {
        Test-StagedBinaries -Staging $staging -UnchangedNote 'Nothing has been installed, removed or replaced.'
        Stop-ExistingService
        Move-StagedIntoInstallDir -Staging $staging
    } finally {
        Remove-StagingDir -Staging $staging
    }
    Remove-TrayIfRequested
    Write-InstallProgress 'files-ok'
    Section 'Background service'
    Invoke-AgentInstall
    Write-InstallProgress 'service-installed'
    # Persist the resolved CP URL now that waired-agent install has created +
    # locked down the state dir, and BEFORE Invoke-WairedInit -- the same
    # ordering install.sh uses (darwin_register_agent -> darwin_write_control_url
    # -> darwin_maybe_init). It is what lets a later bare `waired init` find the
    # right Control Plane when this run does not enrol (#42).
    Write-ControlUrlEnvFile
    # Start the service now, BEFORE `waired init`: with the agent already
    # running, init attaches to it and takes the daemon-driven onboarding
    # path (browser sign-in + setup; waired#835 section 11.2) than the legacy
    # standalone enroll. Safe before sign-in -- the daemon boots
    # identity-less and idles until login (#177); macOS starts its
    # LaunchDaemon (RunAtLoad) before init for the same reason.
    Ensure-AgentRunning
    Write-InstallProgress 'service-running'
    # After the service is up, not inside Invoke-AgentInstall: the level is
    # persisted through the running daemon rather than baked into the SCM
    # ImagePath, so `waired config log-level` is what decides it from here on
    # (waired-agent#801). No-op unless -LogLevel / $env:WAIRED_LOG_LEVEL was
    # given. No new Write-InstallProgress token -- Read-InstallProgress and
    # Watch-ElevatedConsole key off the existing set.
    Set-PersistedLogLevel
    Add-InstallDirToPath
    Set-InstallDirRegistry
    Write-InstallProgress 'path-ok'
    Section 'Sign in and set up'
    # The Ollama engine is installed by `waired init` itself (after its
    # inference questions); we only resolve the knobs into env for it.
    Set-OllamaEnvForInit
    Invoke-WairedInit
    $initRan = $script:InitRan
    # Claude Code routing is decided entirely inside `waired init` (it asks
    # interactively, or writes managed settings on --non-interactive unless
    # -SkipClaudeProxy forwarded --skip-claude-route). No separate enable step
    # here -- an unconditional post-init `waired claude enable` used to override
    # an interactive "no" to init's routing prompt.
    New-StartMenuShortcuts
    # Registration before the launch: whichever runs, the Run value exists,
    # and the tray's own first-run registration then finds it already set.
    Register-TrayAutostart
    Start-TrayAsOriginalUser
    Show-NextSteps -InitRan:$initRan
}

# -------------------------------------------------------------------
# update_* -- manual update (#292). Mirrors install.sh's --check /
# --update flow: detect the installed version, resolve the latest for
# the active channel, gate on a version compare, then swap the binaries
# in place and restart the service. The version-compare semantics match
# internal/version (Go) so the installer, `waired update` (#293) and the
# auto-check (#294) all agree on "is X older than Y".
# -------------------------------------------------------------------

# ConvertTo-WairedVersion -- normalize arbitrary versionish text and split
# it into the dotted-numeric release core and the prerelease that follows.
# Returns $null when no core is present.
#
# Normalization drops, in order: a Debian epoch ("1:"), a leading "v", and
# SemVer build metadata ("+abc1234", not part of precedence). It then
# rewrites Debian's "~" prerelease separator to SemVer's "-", which is
# what makes the .deb and SemVer spellings of one release compare equal
# (waired-agent#780). Mirror of install.sh version_normalize.
#
# A [version] cast cannot carry the prerelease, which is why this returns
# a shape rather than a [version]: "0.6.3-rc1" and "0.6.3" both cast to
# 0.6.3, and every rc-to-rc update compared equal (waired-agent#781).
function ConvertTo-WairedVersion {
    param([string]$Text)
    if (-not $Text) { return $null }
    $s = $Text.Trim()
    $s = [regex]::Replace($s, '^[0-9]+:', '')
    if ($s -match '^[vV]') { $s = $s.Substring(1) }
    $plus = $s.IndexOf('+')
    if ($plus -ge 0) { $s = $s.Substring(0, $plus) }
    $s = $s.Replace('~', '-')

    $dash = $s.IndexOf('-')
    $coreText = if ($dash -ge 0) { $s.Substring(0, $dash) } else { $s }
    $pre = if ($dash -ge 0) { $s.Substring($dash + 1) } else { '' }
    # The core tolerates a trailing non-numeric tail that is NOT introduced
    # by a separator -- ".post1" and friends. That tail is a POST-release,
    # above the release, so it must not be read as a prerelease.
    $m = [regex]::Match($coreText, '^[0-9]+(\.[0-9]+)*')
    if (-not $m.Success) { return $null }
    $parts = @($m.Value.TrimEnd('.').Split('.') | ForEach-Object { [int]$_ })
    return [pscustomobject]@{ Core = $parts; Pre = $pre }
}

# Compare-WairedVersion -- -1 / 0 / +1 for $A older / same / newer than
# $B. Mirror of install.sh version_lt and internal/version's Compare, and
# it is dpkg's ordering: on Linux the other side of this comparison is an
# apt candidate that dpkg itself picked, and the three implementations
# have to agree with each other and with it.
#
# Release core first (dotted numeric, shorter side zero-padded); on a tie
# a prerelease sorts below the release it leads to, and two prereleases
# are read as alternating runs of non-digits and digits -- digits
# numerically, everything else by dpkg's character ranking. See
# internal/version/dotted.go comparePre for why that rules out SemVer
# section 11's lexical rule (it would place rc10 below rc2, and this
# repository has shipped an rc18).
function Compare-WairedVersion {
    param([Parameter(Mandatory)]$A, [Parameter(Mandatory)]$B)
    $n = [Math]::Max($A.Core.Count, $B.Core.Count)
    for ($i = 0; $i -lt $n; $i++) {
        $x = if ($i -lt $A.Core.Count) { $A.Core[$i] } else { 0 }
        $y = if ($i -lt $B.Core.Count) { $B.Core[$i] } else { 0 }
        if ($x -ne $y) { return $(if ($x -lt $y) { -1 } else { 1 }) }
    }
    if ($A.Pre -eq '' -and $B.Pre -eq '') { return 0 }
    if ($A.Pre -eq '') { return 1 }   # A is the release, B a prerelease of it
    if ($B.Pre -eq '') { return -1 }
    return Compare-WairedPrerelease $A.Pre $B.Pre
}

# Compare-WairedPrerelease -- dpkg's comparison of the part after a "~".
function Compare-WairedPrerelease {
    param([string]$A, [string]$B)
    while ($A.Length -gt 0 -or $B.Length -gt 0) {
        $ra = Get-WairedVersionRun $A $false
        $rb = Get-WairedVersionRun $B $false
        $c = Compare-WairedVersionRunes $ra $rb
        if ($c -ne 0) { return $c }
        $A = $A.Substring($ra.Length); $B = $B.Substring($rb.Length)

        $ra = Get-WairedVersionRun $A $true
        $rb = Get-WairedVersionRun $B $true
        $c = Compare-WairedVersionDigits $ra $rb
        if ($c -ne 0) { return $c }
        $A = $A.Substring($ra.Length); $B = $B.Substring($rb.Length)
    }
    return 0
}

# Get-WairedVersionRun -- the leading run of digits ($Digits) or
# non-digits (-not $Digits) in $S.
function Get-WairedVersionRun {
    param([string]$S, [bool]$Digits)
    $i = 0
    while ($i -lt $S.Length -and ([char]::IsDigit($S[$i]) -eq $Digits)) { $i++ }
    return $S.Substring(0, $i)
}

# Compare-WairedVersionRunes -- dpkg's character ranking for the
# non-digit runs: the separator sorts before anything including the end
# of the run, then the end of the run, then letters, then everything
# else.
function Compare-WairedVersionRunes {
    param([string]$A, [string]$B)
    $n = [Math]::Max($A.Length, $B.Length)
    for ($i = 0; $i -lt $n; $i++) {
        $x = if ($i -lt $A.Length) { Get-WairedVersionRank $A[$i] } else { 0 }
        $y = if ($i -lt $B.Length) { Get-WairedVersionRank $B[$i] } else { 0 }
        if ($x -ne $y) { return $(if ($x -lt $y) { -1 } else { 1 }) }
    }
    return 0
}

function Get-WairedVersionRank {
    param([char]$C)
    if ($C -eq '-' -or $C -eq '~') { return -1 }
    if ([char]::IsLetter($C)) { return [int]$C }
    return ([int]$C) + 256
}

# Compare-WairedVersionDigits -- two runs of decimal digits by value,
# without an integer width limit (an edge build's timestamp run is
# already 14 digits). Leading zeros are insignificant; an absent run is
# zero.
function Compare-WairedVersionDigits {
    param([string]$A, [string]$B)
    $a = $A.TrimStart('0'); $b = $B.TrimStart('0')
    if ($a.Length -ne $b.Length) { return $(if ($a.Length -lt $b.Length) { -1 } else { 1 }) }
    $c = [string]::CompareOrdinal($a, $b)
    if ($c -eq 0) { return 0 }
    return $(if ($c -lt 0) { -1 } else { 1 })
}

# Test-WairedOlder -- $true iff $Installed < $Latest. An unparseable /
# empty $Latest returns $false ("can't tell -> don't offer"); an
# unparseable / empty $Installed returns $true ("offer the update").
# Mirror of install.sh version_lt.
function Test-WairedOlder {
    param([string]$Installed, [string]$Latest)
    $b = ConvertTo-WairedVersion $Latest
    if (-not $b) { return $false }
    $a = ConvertTo-WairedVersion $Installed
    if (-not $a) { return $true }
    return ((Compare-WairedVersion $a $b) -lt 0)
}

# Get-InstalledVersion -- the installed waired version, or $null when no
# binary is present. Primary source is `waired.exe version --json`
# (.version); falls back to a VERSION file beside the binary, then
# 'unknown' for a binary too old to report a version (no `version`
# subcommand -- treated as "older" so the update is offered). Mirror of
# install.sh darwin_detect_installed.
function Get-InstalledVersion {
    $exe = Join-Path $InstallDir 'waired.exe'
    if (-not (Test-Path -LiteralPath $exe)) { return $null }
    try {
        $out = & $exe version --json 2>$null
        if ($LASTEXITCODE -eq 0 -and $out) {
            $v = ($out | ConvertFrom-Json).version
            if ($v) { return [string]$v }
        }
    } catch { }
    $verFile = Join-Path $InstallDir 'VERSION'
    if (Test-Path -LiteralPath $verFile) {
        $v = (Get-Content -LiteralPath $verFile -First 1).Trim()
        if ($v) { return $v }
    }
    return 'unknown'
}

# Test-AgentServiceRegistered -- does the SCM know about waired-agent? Reads
# only; no elevation needed. The same question waired-setup.iss asks in Pascal
# (AgentServiceExists, `sc.exe query` / 1060).
function Test-AgentServiceRegistered {
    try {
        return [bool](Get-Service -Name $ServiceName -ErrorAction SilentlyContinue)
    } catch {
        return $false
    }
}

# Test-InstallComplete -- true only when this host carries a COMPLETE install:
# the binary AND a registered service. Pure (facts in, verdict out) so
# installtest can table-drive it.
#
# Get-InstalledVersion alone is the wrong signal for the install-vs-update
# dispatch, for the same reason install.sh does not use darwin_detect_installed
# there: an install that aborted, or an uninstall that could not delete a
# running waired.exe, leaves a binary with a perfectly good version string
# behind. A bare re-run was then dispatched to the update path, which installs
# none of the missing pieces -- so the host reported "Update declined." and
# exit 0 while carrying no service, no state dir, no registry key and no PATH
# entry, and could never converge no matter how often it ran (#660).
#
# A leftover binary is a broken install to repair, not an install to decline
# updating. Mirror of install.sh darwin_install_complete.
function Test-InstallComplete {
    param([string]$Version, [bool]$ServiceRegistered)
    if (-not $Version) { return $false }
    return $ServiceRegistered
}

# Get-GitHubLatestTag -- resolve the stable 'latest' release tag via the
# public mirror's GitHub Releases API. Returns a stripped version (no
# leading v) or $null on any failure (non-fatal; the caller leaves the
# install unchanged). Unauthenticated api.github.com (60 req/hr/IP) is
# plenty for an installer. Mirror of install.sh resolve_latest_version
# (stable arm).
function Get-GitHubLatestTag {
    try {
        [Net.ServicePointManager]::SecurityProtocol = `
            [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    } catch { }
    $api = "https://api.github.com/repos/$InstallRepo/releases/latest"
    try {
        $resp = Invoke-RestMethod -Uri $api -UseBasicParsing `
            -Headers @{ 'User-Agent' = 'waired-installer' }
        if ($resp.tag_name) { return ([string]$resp.tag_name -replace '^v', '') }
    } catch {
        Common-Warn "could not query the latest version ($($_.Exception.Message)); leaving the current install unchanged."
    }
    return $null
}

# Resolve-LatestVersion -- the latest version for the active channel
# (from WAIRED_VERSION / $Version): unset|latest -> stable (GitHub API),
# edge -> the moving 'edge' prerelease (compare degrades to "always
# offer"), explicit vX.Y.Z -> that pin verbatim (no network call).
# Mirror of install.sh channel_from_env + resolve_latest_version.
function Resolve-LatestVersion {
    switch -Regex ($Version) {
        '^(latest)?$' { return Get-GitHubLatestTag }
        '^edge$'      { return 'edge' }
        default       { return ($Version -replace '^v', '') }
    }
}

# Confirm-WairedUpdate -- $true to proceed. -Yes forces yes; a
# non-interactive shell without -Yes reports and declines (safe,
# reversible); otherwise an interactive [Y/n] prompt defaulting to yes.
# Mirror of install.sh prompt_update.
function Confirm-WairedUpdate {
    param([string]$Installed, [string]$Latest)
    if ($Yes) { return $true }
    if (-not (Test-InteractiveStdin)) {
        Common-Warn "Update available: $Installed -> $Latest. Re-run with -Update -Yes to apply (non-interactive)."
        return $false
    }
    $reply = Read-Host "[waired] Update waired $Installed -> $Latest? [Y/n]"
    if ($reply -match '^(n|no)$') { return $false }
    return $true
}

# Stop-ServiceForUpdate -- stop (but do NOT delete) the waired-agent
# service so its on-disk binaries can be overwritten in place. Unlike
# Stop-ExistingService (the fresh-install path, which sc.exe-deletes so
# `waired-agent install` re-registers from scratch), the update path
# keeps the SCM registration + state-dir DACL intact -- the binary path
# is unchanged, so there is nothing to re-register. Returns $true when
# the service existed.
function Stop-ServiceForUpdate {
    $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if (-not $svc) { return $false }
    if ($svc.Status -ne 'Stopped') {
        Common-Log "Stopping $ServiceName for in-place update"
        Common-Run "Stop-Service $ServiceName" {
            Stop-Service -Name $ServiceName -Force -ErrorAction Stop
        }
    }
    return $true
}

# Start-AgentService -- (re)start the service after the swap.
function Start-AgentService {
    Common-Run "Start-Service $ServiceName" {
        Start-Service -Name $ServiceName -ErrorAction Stop
    }
}

# Ensure-AgentRunning -- best-effort start of the registered service on a
# fresh install. Runs BEFORE `waired init` so init attaches to the running
# agent and takes the daemon-driven onboarding path (waired#835 section
# 11.2). The SCM service is registered StartType=Automatic by
# `waired-agent install`,
# and the daemon boots identity-less safely (#177), so starting it before
# sign-in is safe -- and also lets a non-admin user finish setup via the
# tray when sign-in is skipped. Never aborts the install: a start failure is
# a warning.
function Ensure-AgentRunning {
    if ($DryRun) {
        Common-Run "Start-Service $ServiceName" { }
        return
    }
    try {
        Start-Service -Name $ServiceName -ErrorAction Stop
        Common-Log "$ServiceName is running."
    } catch {
        Common-Warn "could not start ${ServiceName}: $_ -- start it with: Start-Service $ServiceName"
    }
}

# Converge-Engine -- bring an ALREADY-INSTALLED bundled Ollama up to the
# version this build serves with, by calling the freshly-swapped CLI (#826).
# Mirror of install.sh's common_converge_engine.
#
# waired serves only with the engine it installed itself (#489) and only at
# the exact pinned version, so an agent update that moves the pin leaves
# every host behind -- the state a user reads as "needs ollama >= X
# (running Y)" in `waired models ls --detail`. Called after the zip is
# extracted and before the service starts, so the service comes up on the
# converged engine.
#
# Never installs an engine on a host that has none: `waired init` owns that
# decision (#138) and `waired runtimes upgrade` enforces it.
#
# Non-fatal on purpose: an update that fails because GitHub was slow is
# worse than one that finishes and leaves the warning the product already
# prints.
function Converge-Engine {
    $exe = Join-Path $InstallDir 'waired.exe'
    if (-not $DryRun -and -not (Test-Path -LiteralPath $exe)) {
        Common-Warn "waired.exe not found at $exe after the swap; skipping the engine check."
        return
    }
    Common-Run "$exe runtimes upgrade ollama --quiet" {
        # "Non-fatal" has to survive the CLI not running at all. A program
        # Windows refuses to launch -- and, under $ErrorActionPreference =
        # 'Stop', a program that merely writes to stderr -- raises a
        # terminating error rather than setting $LASTEXITCODE, and this step
        # had no catch: on a Smart App Control host the refusal escaped to
        # the script trap, printed "install failed", and left the service
        # stopped even though it would have started (measured on a Windows 11
        # Pro host, 2026-08-29; waired-agent#1087).
        try {
            & $exe runtimes upgrade ollama --quiet
            $rc = $LASTEXITCODE
        } catch {
            Common-Warn ("could not run the engine check ({0}). Run it by hand: waired runtimes upgrade ollama" -f (Get-FailureReason $_))
            return
        }
        if ($rc -ne 0) {
            Common-Warn "could not bring the bundled engine to the pinned version. Run it by hand: waired runtimes upgrade ollama"
        }
    }
}

# Wait-AgentDaemon -- wait until the daemon answers the log-level read over
# its local named pipe, which is the same path Set-PersistedLogLevel's write
# takes. Returns $true when it does.
#
# `waired config log-level` separates the three states that matter:
#
#   "Log level: info"                                       daemon answered
#   "Log level: info (persisted; waired-agent not running)"  daemon down
#   non-zero exit                                           pipe not up yet
#     (/log/level is not on the loopback-TCP read allow-list, so a TCP
#      attempt is refused rather than answered)
#
# Only the first is safe to write through, which is why this polls the real
# read rather than the cheaper /waired/v1/status probe: status IS served over
# TCP, so it would go green while the pipe the write needs is still absent.
# Mirror of install.sh's common_daemon_owns_log_level.
function Wait-AgentDaemon {
    param([string]$Exe, [int]$TimeoutSec = 30)
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
        $out = ''
        try { $out = (& $Exe config log-level 2>&1 | Out-String) } catch { $out = '' }
        if ($LASTEXITCODE -eq 0 -and $out -match 'Log level: ' -and $out -notmatch 'not running') {
            return $true
        }
        Start-Sleep -Seconds 1
    }
    return $false
}

# Set-PersistedLogLevel -- persist -LogLevel as the agent's log verbosity.
#
# It goes through the RUNNING daemon on purpose (waired-agent#801). `waired
# config log-level` also has a daemon-is-down branch that writes agent.json
# directly, and reaching for it here would be a trap twice over:
#
#   * an agent.json that exists before the daemon's first boot permanently
#     disables the hardware-aware bundled-model selection -- that gate is
#     `!agentJSONExists` (cmd/waired-agent/bundled_model_select.go,
#     waired#756) -- so a below-spec host would boot with inference on and
#     pull the full default model;
#   * it would be written by the elevated installer rather than by the
#     service account that owns the state dir.
#
# So: wait for the daemon, let it do the write, and if it never answers, say
# so and leave the level alone rather than writing the file ourselves. A
# level that was not applied is recoverable with one command; neither of the
# two failures above is visible at all.
#
# Mirror of install.sh's common_seed_log_level.
function Set-PersistedLogLevel {
    if (-not $LogLevel) { return }
    $exe  = Join-Path $InstallDir 'waired.exe'
    $hint = "set it later with: waired config log-level $LogLevel"
    if ($DryRun) {
        Common-Log "  (dry-run) would: $exe config log-level $LogLevel"
        return
    }
    if (-not (Test-Path -LiteralPath $exe)) {
        Common-Warn "could not set the log level (waired.exe not found at $exe); $hint"
        return
    }
    if (-not (Wait-AgentDaemon -Exe $exe)) {
        Common-Warn "could not set the log level (the background service did not answer); $hint"
        return
    }
    Common-Log "Setting the agent log level to $LogLevel (persisted; change it later with: waired config log-level <level>)"
    # Same guard as Converge-Engine: a level that was not applied is one
    # command away, and must never be what fails an install
    # (waired-agent#1087).
    try {
        & $exe config log-level $LogLevel | Out-Null
        $rc = $LASTEXITCODE
    } catch {
        Common-Warn ("could not set the log level ({0}); {1}" -f (Get-FailureReason $_), $hint)
        return
    }
    if ($rc -ne 0) {
        Common-Warn "could not set the log level (the background service did not answer); $hint"
    }
}

# Show-UpdateResult -- closing summary for the update path.
# Get-TrayAutostartNotice tells the console user, on an update, that the
# Waired app will not come back when they sign in.
#
# The update path deliberately does NOT register the autostart the way a fresh
# install does (waired-agent#832 put that in Invoke-InstallSteps). It cannot:
# turning off "Start Waired on login" in the app deletes the same Run value
# (internal/platform/autostart/autostart_windows.go Disable), and nothing
# distinguishes "never registered" from "the user switched it off" -- the tray
# infers first launch from the value's presence alone, with no marker. Writing
# it here would silently overturn that choice on every update.
#
# So it says something instead. The host this is for is one installed over SSH
# before #832, where the tray never ran and never registered itself: the user
# has no reason to suspect anything is missing, and one click fixes it.
#
# Silent unless it POSITIVELY knows the entry is missing: no console user
# (nothing to be missing for), an unreadable hive (an un-elevated update by a
# different user), or an entry already there all produce nothing. Never guess
# at somebody's machine.
function Get-TrayAutostartNotice {
    param([string]$ConsoleUser, [string]$State)
    if (-not $ConsoleUser) { return @() }
    if ($State -ne 'absent')  { return @() }
    return @(
        "Tray:     the Waired app is not set to start when $ConsoleUser signs in.",
        '          Open Waired once and tick "Start Waired on login" to change that.'
    )
}

# Test-TrayAutostartState reports 'present', 'absent' or 'unknown' for the
# console user's Run value. 'unknown' is the honest answer when the hive
# cannot be read, and Get-TrayAutostartNotice stays quiet on it.
function Test-TrayAutostartState {
    param([string]$ConsoleUserSid)
    if ([string]::IsNullOrWhiteSpace($ConsoleUserSid)) { return 'unknown' }
    $key = "Registry::HKEY_USERS\$ConsoleUserSid\Software\Microsoft\Windows\CurrentVersion\Run"
    try {
        if (-not (Test-Path -LiteralPath $key)) { return 'absent' }
        $v = Get-ItemProperty -Path $key -Name 'waired-tray' -ErrorAction Stop
        if ($v) { return 'present' }
        return 'absent'
    } catch [System.Management.Automation.ItemNotFoundException] {
        return 'absent'
    } catch [System.Management.Automation.PSArgumentException] {
        return 'absent'
    } catch {
        # Access denied, hive unloaded mid-read: cannot tell, so do not say.
        return 'unknown'
    }
}

function Show-UpdateResult {
    param([string]$From, [string]$To)
    Write-Host ''
    # $From and $To are both measured (Invoke-WairedUpdateSwap reads
    # Get-InstalledVersion either side of the swap), so when they agree
    # nothing moved. Announcing "updated: X -> X" for that is the signature
    # waired-agent#1006 is about: a version on both sides of an arrow that
    # a user cannot tell apart from a real update.
    if ($From -eq $To) {
        Write-Host ("Waired is unchanged at {0}." -f $To) -ForegroundColor Yellow
    } else {
        Write-Host ("Waired updated: {0} -> {1}." -f $From, $To) -ForegroundColor Green
    }
    if (-not $DryRun) {
        $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
        if ($svc) {
            Write-Host "Service:  $ServiceName is $($svc.Status)."
        } else {
            Write-Host "Service:  $ServiceName is not registered; run `"$InstallDir\waired-agent.exe`" install."
        }
    }
    Write-Host "State:    $(Get-AgentStateDir) (identity/config preserved)."
    if (-not $DryRun -and -not $NoTray) {
        $cu = Get-ConsoleUser
        $sid = ''
        if ($cu) { $sid = $cu.Sid }
        foreach ($l in @(Get-TrayAutostartNotice -ConsoleUser $(if ($cu) { $cu.Name } else { '' }) `
                -State (Test-TrayAutostartState -ConsoleUserSid $sid))) {
            Write-Host $l
        }
    }
    Write-Host ''
}

# Invoke-WairedUpdate -- Phase 1 (un-elevated) of the update path:
# detect installed + latest, gate, and on a real update download +
# verify the zip, then hand the swap to Phase 2 (elevated). -Check is
# read-only: it reports and returns without a UAC prompt or a download.
# Mirror of install.sh darwin_update's gate.
function Invoke-WairedUpdate {
    param([string]$Installed)
    Common-Log ("waired (Windows): installed={0} channel={1}" -f `
        $(if ($Installed) { $Installed } else { 'not installed' }), $Version)

    $latest = Resolve-LatestVersion
    if (-not $latest) {
        Common-Warn "could not determine the latest version; nothing to do."
        return
    }

    $pinned = [bool]$env:WAIRED_VERSION
    if (-not $pinned -and $Installed -and $Installed -ne 'unknown' -and -not (Test-WairedOlder $Installed $latest)) {
        Common-Log "waired $Installed is already up to date."
        return
    }

    if ($Check) {
        Common-Log ("Update available: {0} -> {1}" -f `
            $(if ($Installed) { $Installed } else { 'not installed' }), $latest)
        return
    }

    $from = if ($Installed) { $Installed } else { 'unknown' }
    if (-not (Confirm-WairedUpdate -Installed $from -Latest $latest)) {
        Common-Log "Update declined."
        return
    }

    # Download + verify un-elevated (zero wasted UAC clicks on a bad
    # mirror / hash), then elevate just for the in-place swap.
    $workDir = Join-Path $env:TEMP "waired-update-$([Guid]::NewGuid().ToString('N'))"
    New-Item -ItemType Directory -Path $workDir -Force | Out-Null
    try {
        $stagedZip = Get-AssetWithChecksum -WorkDir $workDir
        if (Test-Admin) {
            Invoke-WairedUpdateSwap -StagedZip $stagedZip
        } else {
            Invoke-SelfElevate -ZipPath $stagedZip
            # Recap in this persistent console too -- the elevated window
            # paused and closed, and its summary vanished with it.
            Common-Log "Update finished in the elevated window (full log: $LogPath)."
        }
    } finally {
        Common-Run "Remove-Item -Recurse $workDir" {
            Remove-Item -LiteralPath $workDir -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}

# Invoke-WairedUpdateSwap -- Phase 2 (elevated) of the update path: stop
# the service, overwrite the binaries in place (same %ProgramFiles%
# path, so the SCM registration stays valid), then restart. Falls back
# to a full `waired-agent install` only when no service was registered.
# State under %ProgramData%\waired and enrolment are left untouched, and
# Ollama / `waired init` are NOT re-run (mirror of install.sh
# darwin_update: swap + restart only).
function Invoke-WairedUpdateSwap {
    param([string]$StagedZip)
    if (-not $DryRun -and -not (Test-Path -LiteralPath $StagedZip)) {
        Common-Die "staged zip not found at $StagedZip (parent installer may have crashed)"
    }
    $before = Get-InstalledVersion
    # Expand and CHECK first, with the service still running: an update that
    # cannot run its own new programs must be an update that did not happen,
    # not a stopped service and a swapped-in binary Windows refuses
    # (waired-agent#1087).
    $staging = Expand-ToStaging -ZipPath $StagedZip
    try {
        Test-StagedBinaries -Staging $staging -UnchangedNote (
            "Nothing has been changed: Waired $before is still installed and its background service is untouched.")
        $hadService = Stop-ServiceForUpdate
        # Before the move, deliberately: the app is being reopened a few steps
        # below, and closing it first is what keeps the reopen from racing the
        # swap. It also spares Move-IntoInstallDir from having to rename a held
        # image aside -- though measured on sv-evox2 (2026-08-27) the replace
        # succeeded against a running tray anyway (no .displaced- file was left),
        # so that is a bonus rather than the mechanism. What actually caused the
        # version skew was simply that nothing restarted the process
        # (waired-agent#1046).
        Stop-TrayForUpdate
        # From here on the install dir is being changed, so every exit from
        # this script puts it back until Clear-RollbackArm says otherwise.
        Backup-InstallDirFiles -Staging $staging -HadService $hadService -Version $before
        Move-StagedIntoInstallDir -Staging $staging
        Remove-TrayIfRequested
        Converge-Engine
        if ($hadService) {
            Start-AgentService
        } else {
            Common-Warn "$ServiceName was not registered; running waired-agent install to register it."
            Invoke-AgentInstall
            Start-AgentService
        }
        Clear-RollbackArm
    } finally {
        Remove-StagingDir -Staging $staging
    }
    # After the service is back: the tray polls it, so reopening first would
    # only paint a daemon-down menu for a second or two.
    Start-TrayAfterUpdate
    $after = Get-InstalledVersion
    Show-UpdateResult -From $(if ($before) { $before } else { 'unknown' }) `
                      -To   $(if ($after)  { $after }  else { 'updated' })
}

# -------------------------------------------------------------------
# main
# -------------------------------------------------------------------

# Fold any install.sh-style long options (--dev / --control <url> / --skip-*
# ...) that PowerShell left unbound in $ExtraArgs into their -Xxx params, or
# die loudly on a stray token, before -Help / the banner / Resolve-ControlUrl /
# Invoke-SelfElevate read any of them (waired#746).
Normalize-ExtraArgs

if ($Help) {
    Show-Help
    return
}

# Value-bearing options are validated + normalised HERE, in Phase 1, before any
# download / UAC / privileged step -- mirroring install.sh, which validates
# right after its arg loop (a typo must not cost a UAC click, nor be reported
# inside the elevated window that closes on exit). Resolve-LogLevel also
# republishes the level to $env:WAIRED_LOG_LEVEL, which is what carries it into
# the elevated Phase 2 (#164).
Resolve-LogLevel
Resolve-InitAnswers

# -Clean always wipes and installs fresh, so the read-only -Check and the
# in-place -Update contradict it (mirror of install.sh's --clean guard).
if ($Clean -and ($Check -or $Update)) {
    Common-Die "-Clean cannot be combined with -Check/-Update (a clean install always installs fresh)"
}

# Banner-only self-test seam for the CI banner-render guard
# (scripts/dev/installtest-banner-render.ps1, #571). WAIRED_BANNER_SELFTEST is
# never set on a user host, so this is inert in production: it renders the same
# Show-Banner a user sees, then returns before any Resolve-ControlUrl / download
# / SCM work. Kept pure-ASCII so the file stays wire-safe under `iwr|iex`
# (scripts/install/encoding_test.go).
if ($env:WAIRED_BANNER_SELFTEST) { Show-Banner; return }

Resolve-ControlUrl

# Arg-parsing self-test seam (waired#746). WAIRED_ARGTEST is never set on a user
# host; when set, print the resolved arg state after Normalize-ExtraArgs +
# Resolve-ControlUrl and return before any download / UAC / SCM work, so a unit
# test can assert --dev / --control parity (and that a bad URL / unknown token
# dies) without doing privileged work. Kept pure-ASCII (wire-safe under iwr|iex,
# scripts/install/encoding_test.go).
#
# EnvLogLevel is deliberately separate from LogLevel: it is the value every
# child of THIS process inherits. Fields are append-only -- the harness matches
# on positions up to ShareWithMesh.
#
# NoTray / StateDir / DevControlUrl / InstallDir are the values #192 showed were
# dropped across UAC, so they are what a -StateFile round-trip assert reads.
# WAIRED_ARGTEST_STATEFILE additionally writes the real state document, so a
# test can hand it to a second process whose environment has been scrubbed --
# the only way to observe the boundary without elevation, which the runner
# cannot provide. ElevateArgs prints the argv the elevated re-invoke would get,
# for the quoting assert (#177) -- real paths where this invocation has them, so
# the printed vector is one a test can execute as-is. InitArgs prints the argv
# Get-WairedInitArgs would hand to `waired init`.
#
# Admin is Test-Admin, i.e. WHICH branch the install would take: elevated ->
# Invoke-InstallSteps inline, non-elevated -> Invoke-SelfElevate. The CI runner
# is always Administrator, so that decision was never observed under a real
# restricted token (#195); the harness now runs this seam under a standard user
# and a filtered token and asserts it flips, along with the argv THAT token
# builds. Printing the predicate is the whole seam -- Invoke-SelfElevate itself
# still raises a real UAC prompt and is never reached from here.
if ($env:WAIRED_ARGTEST) {
    Write-Host ("ARGTEST Dev={0} Control={1} ControlUrl={2} Version={3} SkipOllama={4} SkipInit={5} SkipClaudeProxy={6} NonInteractive={7} DryRun={8} Update={9} Check={10} Yes={11} Clean={12} LogLevel={13} EnvLogLevel={14} InferenceEnabled={15} ShareWithMesh={16} NoTray={17} StateDir={18} InstallDir={19} DevControlUrl={20} Admin={21}" -f `
        [bool]$Dev, $Control, $ControlUrl, $Version, [bool]$SkipOllama, [bool]$SkipInit, `
        [bool]$SkipClaudeProxy, [bool]$NonInteractive, [bool]$DryRun, [bool]$Update, [bool]$Check, [bool]$Yes, [bool]$Clean, `
        $LogLevel, $env:WAIRED_LOG_LEVEL, $InferenceEnabled, $ShareWithMesh, `
        [bool]$NoTray, $StateDir, $InstallDir, $DevControlUrl, [bool](Test-Admin))
    Write-Host ("ARGTEST InitArgs=[{0}]" -f ((Get-WairedInitArgs) -join ' '))
    if ($env:WAIRED_ARGTEST_STATEFILE) {
        Export-InstallState -Path $env:WAIRED_ARGTEST_STATEFILE
    }
    # Real paths where we have them, so the printed vector is one a test can
    # actually execute; a spaced placeholder otherwise, since the token that
    # has to survive quoting is the one with a space in it.
    Write-Host ("ARGTEST ElevateArgs=[{0}]" -f ((Get-ElevateArgs `
        -ScriptPath $(if ($PSCommandPath) { $PSCommandPath } else { 'C:\Program Files\Waired\install.ps1' }) `
        -ZipPath    'C:\Temp Dir\waired.zip' `
        -StatePath  $(if ($env:WAIRED_ARGTEST_STATEFILE) { $env:WAIRED_ARGTEST_STATEFILE } else { 'C:\Temp Dir\install-state.json' })) -join ' '))
    return
}

# Welcome banner -- Phase 1 only ($StagedZipPath set => elevated Phase 2
# child, which would otherwise print it a second time). Printed before
# Detect-Platform so the first thing an operator sees is the wordmark, not a
# detection log line.
if (-not $StagedZipPath) { Show-Banner }

# Prune old per-run transcripts. Phase 1 only: the elevated child must never
# sweep the invoking user's %TEMP%, and this sits after the -Help / banner /
# ARGTEST returns so a read-only invocation touches nothing.
if (-not $StagedZipPath) { Remove-OldRunLogs -Prefix 'waired-install' }

Detect-Platform

# Clean install: collect consent, then wipe via uninstall.ps1. Phase 1
# only -- the elevated Phase-2 child inherits WAIRED_CLEAN in its env
# block, but $StagedZipPath being set keeps it out of this branch, so
# the wipe can never run twice. Runs before Get-InstalledVersion so the
# freshly wiped host takes the fresh-install path below.
if ($Clean -and -not $StagedZipPath) {
    Confirm-CleanInstall
    Invoke-CleanWipe
}

# -Check / -Update, or a bare re-run that detects an existing install,
# routes through the update flow instead of a fresh install (mirror of
# install.sh main()'s dispatch). The elevated child carries -Update, so
# $StagedZipPath being set means "already in Phase 2" -- exclude it from
# the bare-re-run auto-detect so the child doesn't re-enter Phase 1.
# -Clean is excluded too: under -DryRun the (not actually wiped) host
# would still look installed and misleadingly preview the update path.
#
# The bare-re-run arm asks whether the install is COMPLETE, not merely whether
# a binary is present: a leftover exe is something to repair, and routing it to
# the update path left it unrepairable (#660). -Check / -Update are explicit
# operator requests and still honoured on a partial install.
$installedVersion = Get-InstalledVersion
$installComplete  = Test-InstallComplete -Version $installedVersion `
    -ServiceRegistered (Test-AgentServiceRegistered)
$updateRequested  = $Check -or $Update -or ($installComplete -and -not $StagedZipPath -and -not $Clean)

# Say so when the fresh-install path is being taken to repair wreckage rather
# than to install onto a clean host -- otherwise the run looks like a first
# install that inexplicably found files already there.
if ($installedVersion -and -not $installComplete -and -not $StagedZipPath -and -not $Clean -and -not $Check -and -not $Update) {
    $dash = Emo ([char]::ConvertFromUtf32(0x2014)) '-'
    Common-Warn ("Found Waired's program files but no registered background service $dash " +
        'the last install did not finish. Installing again to repair it.')
}

# Channel preservation (Phase 1 only; the elevated Phase 2 just swaps the
# already-staged zip). When an update names no channel and none is pinned,
# stay on whatever channel this host already tracks -- an edge build
# (version contains "edge") updates to the latest edge instead of silently
# moving onto stable. -Stable / -Edge / WAIRED_VERSION already decided the
# channel above, so they short-circuit this. Mirrors install.sh main().
#
# Keyed on $installedVersion rather than on $updateRequested since #660: the
# repair path is a fresh install by dispatch but an existing host by fact, and
# repairing an edge machine must not silently move it onto stable. -Clean
# cannot reach here with a version -- Invoke-CleanWipe runs before
# Get-InstalledVersion, so a wiped host reads as $null.
if (-not $StagedZipPath -and -not $Stable `
        -and -not $Edge -and -not $Latest -and -not $env:WAIRED_VERSION `
        -and $installedVersion -and $installedVersion -match 'edge') {
    $Version = 'edge'
    $env:WAIRED_VERSION = 'edge'
}

# Two phases. Both run the same script, distinguished by whether
# -StagedZipPath was passed:
#
#   Phase 1 (un-elevated): runs the download + sha256 verify in the
#     calling user's context. No UAC prompt yet. If anything fails
#     (no network, bad mirror, hash mismatch) the user wastes zero
#     UAC clicks. On success, re-invokes self via Start-Process
#     -Verb RunAs with -StagedZipPath pointing at the verified zip.
#
#   Phase 2 (elevated): launched by Phase 1 through UAC. Reads the
#     already-verified zip from the path passed by the parent, stops
#     any old service, extracts to %ProgramFiles%\Waired, and runs
#     `waired-agent.exe install`. Does NOT re-download.
#
# This is the "defer elevation until actually needed" pattern: the
# UAC dialog appears once, immediately before the first privileged
# operation, with the script body unchanged across the boundary.

if (-not $StagedZipPath) {
    # ---- Phase 1: un-elevated ----
    if ($updateRequested) {
        # -Check is read-only and returns before any download / UAC.
        # -Update (or a bare re-run on an existing install) gates on the
        # version compare, then downloads + verifies here and elevates
        # only for the in-place swap.
        Invoke-WairedUpdate -Installed $installedVersion
        return
    }
    if (Test-Admin) {
        Common-Warn "already running elevated; doing download + install in one go (UAC was unnecessary)"
    }

    # Pre-install review: offer the install location, show what is about to
    # happen, and ask before ANY work (download, UAC) starts. Phase 1 only --
    # the elevated child never re-asks.
    Request-InstallDir
    Show-InstallSummary
    Confirm-Proceed

    $workDir = Join-Path $env:TEMP "waired-install-$([Guid]::NewGuid().ToString('N'))"
    New-Item -ItemType Directory -Path $workDir -Force | Out-Null
    $stagedZip = $null
    try {
        Section 'Downloading Waired'
        $stagedZip = Get-AssetWithChecksum -WorkDir $workDir
        if (Test-Admin) {
            # Already elevated -> skip the self-re-exec and just run
            # Phase 2 inline so we don't pop a no-op UAC dialog. Log to a
            # transcript too so a record exists even here (waired#748); no
            # pause -- this is the user's own console, it does not vanish.
            try { Start-Transcript -Path $LogPath -Force -ErrorAction SilentlyContinue | Out-Null } catch { }
            try {
                Invoke-InstallSteps -ZipPath $stagedZip
            } finally {
                Stop-TranscriptQuietly
            }
        } else {
            Section 'Administrator step'
            # Say where the work happens BEFORE the UAC dialog steals focus.
            # The reviewed hosts closed the elevated window because nothing
            # ever told them it was the one doing the work, or how long that
            # work legitimately takes (#314). Lives here rather than inside
            # Invoke-SelfElevate because the update path shares that function
            # and its elevated step is a fast swap, not this.
            Common-Log 'A new Administrator window is opening. The rest of setup runs THERE:'
            Common-Log 'sign-in, the inference engine download, and the first model.'
            Common-Log 'That can take several minutes. Do NOT close that window -- closing it'
            Common-Log 'stops setup part-way. This window mirrors its output and waits.'
            Common-Log "Install log: $LogPath"
            Invoke-SelfElevate -ZipPath $stagedZip
            # The elevated window paused for the operator and closed; leave a
            # recap in THIS (persistent) console too, so the outcome is
            # readable even after that window is gone.
            Section 'Done'
            Common-Log "Install finished in the elevated window (full log: $LogPath)."
            Common-Log "Open a NEW shell to use 'waired' directly (PATH was updated)."
        }
    } finally {
        # Only the un-elevated parent owns the workdir lifecycle. The
        # elevated child reads the zip and exits; the parent then
        # cleans up. If the elevated child crashes, the workdir leaks
        # under %TEMP% and the next install gets a fresh GUID dir --
        # acceptable.
        Common-Run "Remove-Item -Recurse $workDir" {
            Remove-Item -LiteralPath $workDir -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
    return
}

# ---- Phase 2: elevated ----
if (-not (Test-Admin)) {
    Common-Die "internal error: -StagedZipPath set but not running elevated"
}
if (-not (Test-Path -LiteralPath $StagedZipPath)) {
    Common-Die "staged zip not found at $StagedZipPath (parent installer may have crashed)"
}

# The transcript, the QuickEdit fix and $ElevatedConsole (which makes
# Common-Die pause) are armed far earlier now -- right after Common-Die is
# defined -- so that a failure between here and there is still recorded.
# Arming them here was too late: a mis-bound parameter died in
# Normalize-ExtraArgs and the window closed with nothing on disk (#177).

if ($Update) {
    # Elevated swap-only path (the parent already gated + downloaded).
    Invoke-WairedUpdateSwap -StagedZip $StagedZipPath
    Stop-TranscriptQuietly
    # 'done' BEFORE the pause, for the same reason as the install path below.
    Write-InstallProgress 'done'
    if (Test-InteractiveStdin) { Read-Host '[waired] Update complete. Press Enter to close this window' | Out-Null }
    return
}

try {
    Common-Log "elevated phase: installing from $StagedZipPath"
    Invoke-InstallSteps -ZipPath $StagedZipPath
} catch {
    # A terminating error that was NOT a Common-Die (those exit + pause on their
    # own). Route it through Common-Die for the same log-path + pause + exit 1.
    Common-Die "install failed: $($_.Exception.Message)"
}

Stop-TranscriptQuietly
# The success sentinel, and it MUST be written before the pause below.
#
# This is the most common way #314 was actually hit, and the issue text does
# not name it: the install succeeds, the window says "Install complete", and
# the operator closes it instead of pressing Enter -- which is the natural
# thing to do with a window that says it is done. Windows then reports
# STATUS_CONTROL_C_EXIT, and the parent used to Common-Die on a run that had
# completely succeeded. With the sentinel already on disk the parent can tell
# "closed after finishing" (exit 0) from "closed part-way" (report + recap).
Write-InstallProgress 'done'
if (Test-InteractiveStdin) {
    Read-Host '[waired] Install complete. Press Enter to close this window' | Out-Null
}
