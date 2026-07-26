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
    # Pin to a specific tag
    $env:WAIRED_VERSION = 'v1.2.3'
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
    # GPU mode forwarded to ollama-windows.ps1 -GpuMode. See that
    # script's docs for the full enum (auto / rocm / vulkan / cuda-only
    # / cpu-only).
    [string]$OllamaGpuMode    = 'auto',
    # Optional models directory forwarded to ollama-windows.ps1
    # -ModelsDir. Empty = ollama's built-in default.
    [string]$OllamaModelsDir  = $env:WAIRED_OLLAMA_MODELS_DIR,
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
            WAIRED_OLLAMA_MODELS_DIR = $env:WAIRED_OLLAMA_MODELS_DIR
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
            OllamaModelsDir    = [string]$OllamaModelsDir
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
# there -- except the two parameters whose defaults were already evaluated
# at BIND time (-LogLevel :$env:WAIRED_LOG_LEVEL and -OllamaModelsDir),
# which is why every param is assigned explicitly below.
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
    $script:OllamaModelsDir    = [string]$p.OllamaModelsDir
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
if (-not $LogPath) { $LogPath = Join-Path $env:TEMP 'waired-install.log' }

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

function Common-Die  {
    param([string]$Msg)
    Write-Host "[waired] $Msg" -ForegroundColor Red
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
                    %ProgramData%\waired are preserved; a reused Ollama is
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
  -OllamaModelsDir <path>    Models directory for the init-time engine install
                             (WAIRED_OLLAMA_MODELS_DIR).
  -InferenceEnabled <bool>   true | false to force `waired init
                             --inference-enabled`. Empty = prompt. Same as
                             install.sh's --inference-enabled.
  -ShareWithMesh <bool>      true | false to force `waired init
                             --share-with-mesh`. Empty = prompt. Same as
                             install.sh's --share-with-mesh.

Environment variables:
  WAIRED_VERSION           Pin a specific release tag (e.g. v1.2.3), or 'edge'
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
  WAIRED_CONTROL_URL       Control Plane URL used when -Dev / -Control are
                           not given (lower-priority fallback for per-org
                           installer wrappers).
  WAIRED_DEV_CONTROL_URL   Override the URL -Dev resolves to.
                           Default: https://app.dev.waired.net.
  WAIRED_OLLAMA_MODELS_DIR -OllamaModelsDir fallback.
  WAIRED_INSTALL_BASE_URL  Override the mirror base URL (tests / staging).
                           Hosts the waired binaries only. (The retired
                           WAIRED_OLLAMA_WINDOWS_URL is gone: the engine
                           installer is embedded in the waired binary and run
                           by `waired init` / `waired runtimes install ollama`.)

Diagnostics:
  Get-Service waired-agent
  Get-WinEvent -ProviderName waired-agent -LogName Application -MaxEvents 20

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
        $url = if ($Version -eq 'latest') {
            "$BaseUrl/latest/download/install.ps1"
        } else {
            "$BaseUrl/download/$Version/install.ps1"
        }
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
        $proc = Start-Process -FilePath 'powershell.exe' `
            -ArgumentList $psArgs -Verb RunAs -PassThru -Wait
        if ($proc.ExitCode -ne 0) {
            # A child that died before its transcript existed still leaves the
            # trap's marker behind (#177).
            $why = ''
            $marker = "$stateFile.status"
            if (Test-Path -LiteralPath $marker) {
                $why = " -- $(((Get-Content -LiteralPath $marker -Raw) -split "`r?`n")[0])"
            }
            Common-Die "elevated installer exited code $($proc.ExitCode)$why. Full install log: $LogPath"
        }
    } finally {
        # -Wait guarantees the elevated child finished reading the staged
        # script before we delete it. (PowerShell runs finally on exit, so
        # Common-Die above still cleans up.)
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
    if (-not $SkipOllama) {
        Write-Host "  * Install the Ollama AI engine (a few GB download)"
    }
    if (-not $SkipInit) {
        Write-Host "  * Sign you in (opens your web browser)"
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

function Resolve-ReleaseBase {
    if ($Version -eq 'latest') {
        return "$BaseUrl/latest/download"
    }
    return "$BaseUrl/download/$Version"
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

function Extract-Zip {
    param([string]$ZipPath)

    Common-Run "Expand-Archive $ZipPath -> $InstallDir" {
        if (-not (Test-Path -LiteralPath $InstallDir)) {
            New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
        }
        Expand-Archive -LiteralPath $ZipPath -DestinationPath $InstallDir -Force
    }
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
    if (-not (Test-InteractiveStdin)) { return }
    $tray = Join-Path $InstallDir 'waired-tray.exe'
    if (-not (Test-Path -LiteralPath $tray)) { return }
    Common-Run "launch waired-tray as the original user (via explorer.exe)" {
        try {
            Start-Process -FilePath (Join-Path $env:SystemRoot 'explorer.exe') `
                -ArgumentList $tray -ErrorAction Stop
        } catch {
            Common-Warn "could not auto-launch the tray ($($_.Exception.Message.Trim())); start `"$tray`" yourself or it runs at next logon"
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
    # The Windows service has no EnvironmentFile, so bake the log level into
    # the service ExecStart as --log-level (everything after `--` becomes an
    # agent flag; it wins over agent.json). Runtime changes: `waired config
    # log-level`.
    if ($LogLevel) { $installArgs += @('--', '--log-level', $LogLevel) }
    Common-Log "Running: $exe $($installArgs -join ' ')"
    Common-Run "& $exe $($installArgs -join ' ')" {
        & $exe @installArgs
        if ($LASTEXITCODE -ne 0) {
            Common-Die "waired-agent install exited with code $LASTEXITCODE"
        }
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
# `waired init` child inherits. The engine install itself moved INTO init
# (it asks "run local inference?" first, then installs via the embedded
# ollama-windows.ps1 when the answer calls for one) -- installing here,
# before init, made init re-detect waired's own install as a "foreign"
# Ollama and ask a confusing reuse question about it. The outcome line for
# Show-NextSteps is set here too (mirror of install.sh's $ollama_status).
function Set-OllamaEnvForInit {
    if ($SkipOllama) {
        $env:WAIRED_NO_OLLAMA = '1'
        $script:OllamaStatus = 'skipped (-SkipOllama / WAIRED_NO_OLLAMA; bundled engine later from an elevated prompt: waired runtimes install ollama -- or bring your own and pick "reuse" at sign-in)'
        return
    }
    if ($OllamaGpuMode -and $OllamaGpuMode -ne 'auto') { $env:WAIRED_OLLAMA_GPU_MODE = $OllamaGpuMode }
    if ($OllamaModelsDir) { $env:WAIRED_OLLAMA_MODELS_DIR = $OllamaModelsDir }
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
    $stateForInit = if ($StateDir) { $StateDir } else { $AgentStateDir }
    $initArgs = @('init', '--state-dir', $stateForInit)
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
    $script:InitRan = $false
    if ($SkipInit) {
        Common-Log "-SkipInit set; not running waired init."
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
        Common-Log "  - or open the tray app and pick `"Log in...`""
        Common-Log "  - or re-run the installer with -NonInteractive to attempt it anyway"
        return
    }

    $exe = Join-Path $InstallDir 'waired.exe'
    if (-not (Test-Path -LiteralPath $exe)) {
        Common-Warn "waired.exe not found at $exe; cannot run `waired init`."
        return
    }

    $stateForInit = if ($StateDir) { $StateDir } else { $AgentStateDir }
    $initArgs = Get-WairedInitArgs

    Common-Log "Running: $exe $($initArgs -join ' ')"
    if ($DryRun) {
        Common-Run "& $exe $($initArgs -join ' ')" { }
        $script:InitRan = $true
        return
    }
    & $exe @initArgs
    if ($LASTEXITCODE -ne 0) {
        Common-Warn "waired init exited with code $LASTEXITCODE -- enrolment did not complete."
        Common-Warn "Re-run manually: & `"$exe`" init --state-dir `"$stateForInit`""
        return
    }
    $script:InitRan = $true
}

function Show-NextSteps {
    param([bool]$InitRan = $false)
    $cpHint  = if ($StateDir) { $StateDir } else { $AgentStateDir }
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
    } else {
        Write-Host "$(Emo (Glyph 0x1F527) '*') The agent service is running - ready for sign-in."
        Write-Host "  Sign in:   & `"$InstallDir\waired.exe`" init"
        Write-Host '             (or right-click the waired-tray icon and pick "Log in...")'
        Write-Host "  Verify:    & `"$InstallDir\waired.exe`" status"
    }
    Write-Host ''
    Write-Host 'The agent service is enabled at boot and running now.'
    Write-Host ''
    if (-not $NoTray) {
        Write-Host 'Tray:  a "Waired" Start Menu shortcut was created; the tray auto-starts at each logon.'
        Write-Host "       Launch it from the Start Menu, or now: & `"$InstallDir\waired-tray.exe`""
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
    Write-Host 'Diagnostics:       waired doctor   (logs: Get-WinEvent -ProviderName waired-agent -LogName Application)'
    Write-Host "Uninstall:         & `"$InstallDir\waired-agent.exe`" uninstall"
    Write-Host 'More:              waired init --help'
    Write-Host 'Quickstart:        https://github.com/waired-ai/waired/blob/main/docs/quickstarts/README.md'
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
    Stop-ExistingService
    Extract-Zip -ZipPath $ZipPath
    Remove-TrayIfRequested
    Section 'Background service'
    Invoke-AgentInstall
    # Start the service now, BEFORE `waired init`: with the agent already
    # running, init attaches to it and takes the daemon-driven onboarding
    # path (browser sign-in + setup; waired#835 section 11.2) than the legacy
    # standalone enroll. Safe before sign-in -- the daemon boots
    # identity-less and idles until login (#177); macOS starts its
    # LaunchDaemon (RunAtLoad) before init for the same reason.
    Ensure-AgentRunning
    Add-InstallDirToPath
    Set-InstallDirRegistry
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

# ConvertTo-WairedVersion -- parse arbitrary versionish text into a
# [version]: drop a leading "v", keep the leading dotted-numeric run
# (so "0.6.3-rc1" -> 0.6.3), pad a bare major ("5" -> 5.0), and return
# $null when nothing parseable is present. Mirror of install.sh
# version_strip + the [version] cast.
function ConvertTo-WairedVersion {
    param([string]$Text)
    if (-not $Text) { return $null }
    $s = $Text.Trim()
    if ($s -match '^[vV]') { $s = $s.Substring(1) }
    $m = [regex]::Match($s, '^[0-9]+(\.[0-9]+)*')
    if (-not $m.Success) { return $null }
    # Zero-pad to a fixed 4 components so the [version] compare matches
    # install.sh version_lt (which zero-pads the shorter side). Without
    # this, [version]"1.2" sorts BELOW [version]"1.2.0": the unspecified
    # Build/Revision are -1, not 0, so "1.2" and "1.2.0" would compare
    # unequal. [version] accepts 2..4 components, so cap at 4 and treat
    # anything longer (not a real waired/Ollama version) as unparseable.
    $parts = $m.Value.TrimEnd('.').Split('.')
    if ($parts.Count -gt 4) { return $null }
    while ($parts.Count -lt 4) { $parts += '0' }
    try { return [version]($parts -join '.') } catch { return $null }
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
    return ($a -lt $b)
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

# Show-UpdateResult -- closing summary for the update path.
function Show-UpdateResult {
    param([string]$From, [string]$To)
    Write-Host ''
    Write-Host ("Waired updated: {0} -> {1}." -f $From, $To) -ForegroundColor Green
    if (-not $DryRun) {
        $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
        if ($svc) {
            Write-Host "Service:  $ServiceName is $($svc.Status)."
        } else {
            Write-Host "Service:  $ServiceName is not registered; run `"$InstallDir\waired-agent.exe`" install."
        }
    }
    Write-Host 'Ollama:   managed separately; not modified by update (update a reused engine yourself).'
    Write-Host "State:    $(if ($StateDir) { $StateDir } else { $AgentStateDir }) (identity/config preserved)."
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
    $hadService = Stop-ServiceForUpdate
    Extract-Zip -ZipPath $StagedZip
    Remove-TrayIfRequested
    if ($hadService) {
        Start-AgentService
    } else {
        Common-Warn "$ServiceName was not registered; running waired-agent install to register it."
        Invoke-AgentInstall
        Start-AgentService
    }
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
if ($env:WAIRED_ARGTEST) {
    Write-Host ("ARGTEST Dev={0} Control={1} ControlUrl={2} Version={3} SkipOllama={4} SkipInit={5} SkipClaudeProxy={6} NonInteractive={7} DryRun={8} Update={9} Check={10} Yes={11} Clean={12} LogLevel={13} EnvLogLevel={14} InferenceEnabled={15} ShareWithMesh={16} NoTray={17} StateDir={18} InstallDir={19} DevControlUrl={20}" -f `
        [bool]$Dev, $Control, $ControlUrl, $Version, [bool]$SkipOllama, [bool]$SkipInit, `
        [bool]$SkipClaudeProxy, [bool]$NonInteractive, [bool]$DryRun, [bool]$Update, [bool]$Check, [bool]$Yes, [bool]$Clean, `
        $LogLevel, $env:WAIRED_LOG_LEVEL, $InferenceEnabled, $ShareWithMesh, `
        [bool]$NoTray, $StateDir, $InstallDir, $DevControlUrl)
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
$installedVersion = Get-InstalledVersion
$updateRequested  = $Check -or $Update -or ($installedVersion -and -not $StagedZipPath -and -not $Clean)

# Channel preservation (Phase 1 only; the elevated Phase 2 just swaps the
# already-staged zip). When an update names no channel and none is pinned,
# stay on whatever channel this host already tracks -- an edge build
# (version contains "edge") updates to the latest edge instead of silently
# moving onto stable. -Stable / -Edge / WAIRED_VERSION already decided the
# channel above, so they short-circuit this. Mirrors install.sh main().
if (-not $StagedZipPath -and $updateRequested -and -not $Stable `
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
if (Test-InteractiveStdin) {
    Read-Host '[waired] Install complete. Press Enter to close this window' | Out-Null
}
