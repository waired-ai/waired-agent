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

.EXAMPLE
    iwr -useb $BaseUrl/latest/download/install.ps1 | iex

.EXAMPLE
    # Latest main build (edge channel)
    $env:WAIRED_VERSION = 'edge'
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
    # Skip the Ollama install. When -Control / -Dev resolved a Control
    # Plane URL the installer normally fetches scripts/install/ollama-
    # windows.ps1 from the public mirror and runs it inside Phase 2.
    # Pass -SkipOllama on headless / low-disk hosts; the operator can
    # finish later by hand. No-op when no $ControlUrl was resolved (the
    # installer doesn't auto-install Ollama in that case either).
    # Equivalent env var: WAIRED_NO_OLLAMA=1 (resolved into $SkipOllama
    # below) -- same skip semantics as install.sh's --skip-ollama /
    # WAIRED_NO_OLLAMA, so the two installers stay aligned.
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
    # -Check reports whether a newer waired is available and exits;
    # -Update applies it; -Yes assumes "yes" to the update prompt
    # (required to update on a non-interactive / no-TTY host). See #292.
    [switch]$Check,
    [switch]$Update,
    [switch]$Yes,
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
    # URL of the ollama-windows.ps1 helper to fetch + run. Independent of
    # WAIRED_INSTALL_BASE_URL (which hosts the *waired* binaries): the Ollama
    # installer is an external dependency pulled from the official public
    # channel, mirroring install.sh's WAIRED_OLLAMA_LINUX_URL /
    # WAIRED_OLLAMA_DARWIN_URL. Empty -> the public-mirror default resolved
    # below. Decoupling it keeps installer tests that point
    # WAIRED_INSTALL_BASE_URL at a loopback mirror from redirecting (and
    # 404-ing) the Ollama-helper fetch (#561).
    [string]$OllamaWindowsUrl = $env:WAIRED_OLLAMA_WINDOWS_URL,
    # Force `waired init --inference-enabled <true|false>`. Empty = no
    # override (the prompt or hardware-based default decides).
    [string]$InferenceEnabled = '',
    # Force `waired init --share-with-mesh <true|false>`. Empty = no
    # override.
    [string]$ShareWithMesh = '',
    # Internal: non-empty when re-invoked elevated by Phase 1 after the
    # download has already happened. Skips re-download and goes straight
    # to the privileged install steps. Not documented in -Help -- callers
    # never set this directly.
    [string]$StagedZipPath,
    # Internal: path the elevated Phase-2 child writes its Start-Transcript
    # log to. The un-elevated parent picks a path under its own %TEMP% (so the
    # log stays readable without elevation) and forwards it; empty -> the child
    # defaults it. Not documented in -Help. (waired#748)
    [string]$LogPath,
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

# Ollama installer helper URL. Independent of WAIRED_INSTALL_BASE_URL -- the
# Ollama engine is an external dependency fetched from the official public
# channel (the helper lives in the public waired-ai/waired-agent release,
# the same URL the docs' manual `iwr ... | iex` one-liner uses), mirroring
# install.sh's WAIRED_OLLAMA_LINUX_URL / WAIRED_OLLAMA_DARWIN_URL. Resolved
# here (not at the use site) so the elevated Phase-2 child inherits a concrete
# value, and so installer tests that point WAIRED_INSTALL_BASE_URL at a
# loopback mirror no longer drag this fetch onto the mirror (#561).
if (-not $OllamaWindowsUrl) {
    $OllamaWindowsUrl = 'https://github.com/waired-ai/waired-agent/releases/latest/download/ollama-windows.ps1'
}

# WAIRED_NO_OLLAMA is the env-var form of -SkipOllama (mirrors install.sh,
# where --skip-ollama and WAIRED_NO_OLLAMA are equivalent). Fold it into
# the switch here so every downstream check (Install-OllamaIfRequested,
# the elevation re-invoke that forwards -SkipOllama) sees one resolved
# value regardless of which form the operator used. The env block is also
# inherited by the elevated child, so the resolution holds across phases.
if ($env:WAIRED_NO_OLLAMA) { $SkipOllama = $true }

# WAIRED_NO_CLAUDE_PROXY is the env-var form of -SkipClaudeProxy (mirrors the
# Linux installer's WAIRED_NO_CLAUDE_PROXY / --skip-proxy). Folded into the
# switch so every downstream check + the elevation re-invoke see one value.
if ($env:WAIRED_NO_CLAUDE_PROXY) { $SkipClaudeProxy = $true }

# Built-in dogfood Control Plane URL surfaced via -Dev. Script-level only;
# never compiled into the waired binary (spec section 10.4 -- binary hash stays
# identical across environments).
$DevControlUrl = if ($env:WAIRED_DEV_CONTROL_URL) { $env:WAIRED_DEV_CONTROL_URL } `
                 else { 'https://app.dev.waired.net' }
$ControlUrl    = ''   # resolved by Resolve-ControlUrl after param parsing.
$InitRan       = $false  # set by Invoke-WairedInit; read by Show-NextSteps.
# True only inside the spawned elevated Phase-2 console (set in Phase 2). Gates
# the transcript-path + pause-on-exit behaviour so that window never vanishes
# before its output can be read (waired#748).
$ElevatedConsole = $false

$InstallDir  = Join-Path $env:ProgramFiles 'Waired'
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

function Common-Log  { param([string]$Msg) Write-Host "[waired] $Msg" -ForegroundColor Cyan }
function Common-Warn { param([string]$Msg) Write-Host "[waired] $Msg" -ForegroundColor Yellow }

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
        if ($script:LogPath) { Write-Host "[waired] Full install log: $($script:LogPath)" -ForegroundColor Red }
        Stop-TranscriptQuietly
        if (Test-InteractiveStdin) {
            Read-Host '[waired] Install FAILED. Press Enter to close this window' | Out-Null
        }
    }
    exit 1
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
            'skip-ollama'       { $script:SkipOllama = $true }
            'skip-init'         { $script:SkipInit = $true }
            'skip-claude-proxy' { $script:SkipClaudeProxy = $true }
            'skip-proxy'        { $script:SkipClaudeProxy = $true }
            'non-interactive'   { $script:NonInteractive = $true }
            'dry-run'           { $script:DryRun = $true }
            'yes'               { $script:Yes = $true }
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
  -SkipOllama       Skip the Ollama install + bundled-model pre-pull
                    (same as WAIRED_NO_OLLAMA=1).
  -SkipInit         Skip the post-install `waired init` invocation; finish
                    with the manual-Next-steps block instead.
  -SkipClaudeProxy  Don't configure Claude Code integration after enrolment
                    (default: on -- writes managed settings pointing
                    ANTHROPIC_BASE_URL at local inference, no credential). Same
                    as WAIRED_NO_CLAUDE_PROXY=1.
  -NonInteractive   Forward `--non-interactive` to `waired init`
                    (skip the install-time inference role prompts).
  -Check            Report whether a newer waired is available, then exit.
                    Read-only: no download and no UAC prompt.
  -Update           Update an existing install to the latest release for
                    the active channel (WAIRED_VERSION): stops the
                    service, swaps the binaries in place, restarts. The
                    SCM registration and the state/identity under
                    %ProgramData%\waired are preserved; a reused Ollama is
                    not touched. Re-running install.ps1 on a host that
                    already has waired offers this automatically.
  -Yes              Assume "yes" to the update prompt (required to update
                    on a non-interactive / no-TTY host).
  -Help             Print this help.

Parameters:
  -OllamaGpuMode <mode>      auto | rocm | vulkan | cuda-only | cpu-only
                             (default: auto)
  -OllamaModelsDir <path>    Forward to ollama-windows.ps1 -ModelsDir.
  -OllamaWindowsUrl <url>    URL of the ollama-windows.ps1 helper to install
                             (default: the public waired-ai/waired-agent latest
                             release). Independent of WAIRED_INSTALL_BASE_URL --
                             the Ollama engine comes from its official channel.
  -InferenceEnabled <bool>   true | false to force `waired init
                             --inference-enabled`. Empty = prompt.
  -ShareWithMesh <bool>      true | false to force `waired init
                             --share-with-mesh`. Empty = prompt.

Environment variables:
  WAIRED_VERSION           Pin a specific release tag (e.g. v1.2.3), or 'edge'
                           for the latest main build (same as -Edge). Default: latest.
  WAIRED_NO_TRAY           If set, skip waired-tray.exe.
  WAIRED_NO_OLLAMA         If set, skip the Ollama install (same as -SkipOllama).
  WAIRED_NO_CLAUDE_PROXY   If set, skip configuring Claude Code managed settings (same as -SkipClaudeProxy).
  WAIRED_STATE_DIR         Override on-disk state location. Default: %ProgramData%\waired.
  WAIRED_CONTROL_URL       Control Plane URL used when -Dev / -Control are
                           not given (lower-priority fallback for per-org
                           installer wrappers).
  WAIRED_DEV_CONTROL_URL   Override the URL -Dev resolves to.
                           Default: https://app.dev.waired.net.
  WAIRED_OLLAMA_MODELS_DIR -OllamaModelsDir fallback.
  WAIRED_OLLAMA_WINDOWS_URL -OllamaWindowsUrl fallback: the ollama-windows.ps1
                           helper URL. Independent of WAIRED_INSTALL_BASE_URL
                           (parallel to install.sh's WAIRED_OLLAMA_LINUX_URL /
                           WAIRED_OLLAMA_DARWIN_URL). Default: the public
                           waired-ai/waired-agent latest release.
  WAIRED_INSTALL_BASE_URL  Override the mirror base URL (tests / staging).
                           Hosts the waired binaries only -- NOT the Ollama
                           helper (see WAIRED_OLLAMA_WINDOWS_URL).

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
# total, no double-fetch). Two cases for re-invocation source:
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
function Invoke-SelfElevate {
    param([string]$ZipPath)

    Common-Log "Privileged step ahead -- requesting UAC..."

    # WAIRED_NO_TRAY / WAIRED_STATE_DIR / WAIRED_CONTROL_URL are read
    # from $env in the elevated child too -- Start-Process inherits the
    # parent's environment block. Only switches / explicit values bound
    # to non-env params need explicit forwarding.
    $passthroughArgs = @('-StagedZipPath', $ZipPath, '-LogPath', $LogPath)
    if ($DryRun)         { $passthroughArgs += '-DryRun' }
    if ($Update)         { $passthroughArgs += '-Update' }
    if ($Yes)            { $passthroughArgs += '-Yes' }
    if ($Dev)            { $passthroughArgs += '-Dev' }
    if ($Control)        { $passthroughArgs += @('-Control', $Control) }
    if ($SkipOllama)     { $passthroughArgs += '-SkipOllama' }
    if ($SkipInit)       { $passthroughArgs += '-SkipInit' }
    if ($SkipClaudeProxy){ $passthroughArgs += '-SkipClaudeProxy' }
    if ($NonInteractive) { $passthroughArgs += '-NonInteractive' }
    if ($OllamaGpuMode -and $OllamaGpuMode -ne 'auto') { $passthroughArgs += @('-OllamaGpuMode', $OllamaGpuMode) }
    if ($OllamaModelsDir)  { $passthroughArgs += @('-OllamaModelsDir',  $OllamaModelsDir) }
    if ($OllamaWindowsUrl) { $passthroughArgs += @('-OllamaWindowsUrl', $OllamaWindowsUrl) }
    if ($InferenceEnabled) { $passthroughArgs += @('-InferenceEnabled', $InferenceEnabled) }
    if ($ShareWithMesh)    { $passthroughArgs += @('-ShareWithMesh',    $ShareWithMesh) }

    $psArgs = @('-NoProfile', '-ExecutionPolicy', 'Bypass')
    $tempScript = $null
    if ($PSCommandPath) {
        $psArgs += @('-File', $PSCommandPath) + $passthroughArgs
    } else {
        $url = if ($Version -eq 'latest') {
            "$BaseUrl/latest/download/install.ps1"
        } else {
            "$BaseUrl/download/$Version/install.ps1"
        }
        # No on-disk path (sourced via iwr|iex): stage the script body to a
        # temp .ps1 and re-launch it with -File, which binds the named
        # passthrough params just like case (a). Writing to a file -- rather
        # than ScriptBlock-Create on the fetched bytes -- keeps install.ps1
        # out of Defender's in-memory download-and-execute AMSI heuristic
        # (#552) and reads the body back as text, so the Windows PowerShell
        # 5.1 octet-stream byte[] pitfall cannot occur. -Verb RunAs auto-quotes
        # each -ArgumentList element, so a $env:TEMP with spaces is fine.
        $tempScript = Join-Path $env:TEMP "waired-install-elevate-$([Guid]::NewGuid().ToString('N')).ps1"
        Invoke-WebRequest -Uri $url -OutFile $tempScript -UseBasicParsing
        $psArgs += @('-File', $tempScript) + $passthroughArgs
    }

    try {
        $proc = Start-Process -FilePath 'powershell.exe' `
            -ArgumentList $psArgs -Verb RunAs -PassThru -Wait
        if ($proc.ExitCode -ne 0) {
            Common-Die "elevated installer exited code $($proc.ExitCode). Full install log: $LogPath"
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
# Asset download + verification
# -------------------------------------------------------------------

function Resolve-ReleaseBase {
    if ($Version -eq 'latest') {
        return "$BaseUrl/latest/download"
    }
    return "$BaseUrl/download/$Version"
}

function Get-AssetWithChecksum {
    param([string]$WorkDir)

    $releaseBase = Resolve-ReleaseBase
    $zipPath = Join-Path $WorkDir $ZipName
    $shaPath = Join-Path $WorkDir $ShaName

    Common-Log "Downloading $ZipName from $releaseBase"
    Common-Run "Invoke-WebRequest $releaseBase/$ZipName -> $zipPath" {
        Invoke-WebRequest -Uri "$releaseBase/$ZipName" -OutFile $zipPath -UseBasicParsing
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
# installer's [Icons]: a "Waired Tray" launcher + a "Waired (CLI)" help shortcut.
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
                $lnk = $ws.CreateShortcut((Join-Path $group 'Waired Tray.lnk'))
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
    $installArgs = @('install')
    if ($StateDir) { $installArgs += "-state-dir=$StateDir" }
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

# Test-OllamaInstalled mirrors internal/download/ollama_path_windows.go's
# discovery order so the installer can skip re-installing Ollama on
# hosts where waired-agent (LocalSystem) can already find it.
function Test-OllamaInstalled {
    $candidates = @(
        (Join-Path $env:ProgramFiles 'Ollama\ollama.exe'),
        (Join-Path $env:LOCALAPPDATA 'Programs\Ollama\ollama.exe')
    )
    foreach ($c in $candidates) {
        if (Test-Path -LiteralPath $c) { return $c }
    }
    $cmd = Get-Command ollama.exe -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }
    return $null
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

# Install-OllamaIfRequested fetches the ollama-windows.ps1 helper from
# $OllamaWindowsUrl (the official public channel by default, independent of
# WAIRED_INSTALL_BASE_URL -- see its resolution near the top) and runs it
# inside Phase 2. Only runs when -Control / -Dev resolved a CP
# URL -- without a CP we don't know whether the operator wants this host
# to run inference at all, and silently dropping ~1.5 GB onto disk
# would surprise them. Idempotent (the helper script handles existing
# installs); we still detect them up front and ask before reinstall to
# avoid an unwanted ~10-min re-download.
function Install-OllamaIfRequested {
    if ($SkipOllama) {
        Common-Log "-SkipOllama set; not touching Ollama."
        return
    }
    if (-not $ControlUrl) {
        Common-Log "No Control Plane URL resolved -- skipping Ollama install (re-run with -Dev / -Control <URL> to enable)."
        return
    }

    $existing = Test-OllamaInstalled
    if ($existing) {
        if (-not (Test-InteractiveStdin)) {
            Common-Log "Ollama already installed at $existing; -NonInteractive / non-TTY -> not reinstalling."
            return
        }
        $reply = Read-Host "[waired] Ollama already installed at $existing. Reinstall / upgrade now? [y/N]"
        if ($reply -notmatch '^(y|yes)$') {
            Common-Log "Keeping existing Ollama install."
            return
        }
    }

    $ollamaScriptUrl = $OllamaWindowsUrl
    Common-Log "Fetching $ollamaScriptUrl"
    # Stage the helper to a temp .ps1 and run it from disk (the call operator
    # on the path binds its param() block by name via the hashtable splat
    # below). Downloading to a file rather than ScriptBlock-Create-ing the
    # fetched bytes keeps install.ps1 out of Defender's in-memory
    # download-and-execute AMSI heuristic (#552); reading the file back as
    # text also sidesteps the Windows PowerShell 5.1 octet-stream byte[] pitfall.
    $ollamaScript = $null
    try {
        if (-not $DryRun) {
            $ollamaScript = Join-Path $env:TEMP "waired-ollama-$([Guid]::NewGuid().ToString('N')).ps1"
            Invoke-WebRequest -Uri $ollamaScriptUrl -OutFile $ollamaScript -UseBasicParsing
        }
    } catch {
        Common-Warn "could not fetch ollama-windows.ps1 ($($_.Exception.Message)); skipping Ollama install. Re-run by hand later: download $ollamaScriptUrl and run it from a saved file."
        return
    }

    # Splat a HASHTABLE so these bind to ollama-windows.ps1's param() block
    # BY NAME. An array splat (@('-GpuMode', $mode)) binds POSITIONALLY:
    # '-GpuMode' lands in $ZipUrl and the helper tries to download a URL
    # literally named '-GpuMode' ("remote name could not be resolved").
    $ollamaArgs = @{ GpuMode = $OllamaGpuMode }
    if ($OllamaModelsDir) { $ollamaArgs['ModelsDir'] = $OllamaModelsDir }
    Common-Log ("Installing Ollama (mode={0}{1})..." -f $OllamaGpuMode,
        ($(if ($OllamaModelsDir) { "; models=$OllamaModelsDir" } else { '' })))

    $ollamaDesc = "ollama-windows.ps1 -GpuMode $OllamaGpuMode" + $(if ($OllamaModelsDir) { " -ModelsDir $OllamaModelsDir" } else { '' })
    try {
        Common-Run $ollamaDesc {
            if ($DryRun) { return }
            try {
                # Call operator on the staged path + hashtable splat binds the
                # helper's param() block by name. Invoke-Expression would
                # discard the param() bindings.
                & $ollamaScript @ollamaArgs
            } catch {
                Common-Warn "Ollama install failed: $($_.Exception.Message); the agent will retry pulling the bundled model at boot."
            }
        }
    } finally {
        if ($ollamaScript) {
            Remove-Item -LiteralPath $ollamaScript -Force -ErrorAction SilentlyContinue
        }
    }
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

    $exe = Join-Path $InstallDir 'waired.exe'
    if (-not (Test-Path -LiteralPath $exe)) {
        Common-Warn "waired.exe not found at $exe; cannot run `waired init`."
        return
    }

    $stateForInit = if ($StateDir) { $StateDir } else { $AgentStateDir }
    $initArgs = @('init', '--state-dir', $stateForInit)
    # waired init self-defaults the Control Plane URL (machine env var /
    # baked production default), so --control is only passed when we have an
    # explicit one. This is why init no longer needs a URL to run.
    if ($ControlUrl) { $initArgs += @('--control', $ControlUrl) }
    if (-not (Test-InteractiveStdin)) { $initArgs += '--non-interactive' }
    if ($InferenceEnabled) { $initArgs += @('--inference-enabled', $InferenceEnabled) }
    if ($ShareWithMesh)    { $initArgs += @('--share-with-mesh',   $ShareWithMesh) }

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
    Write-Host "State / identity:  $cpHint"
    Write-Host "PATH:              $InstallDir (added to PATH; open a NEW shell to run 'waired' directly)"
    Write-Host 'Diagnostics:       waired doctor   (logs: Get-WinEvent -ProviderName waired-agent -LogName Application)'
    Write-Host "Uninstall:         & `"$InstallDir\waired-agent.exe`" uninstall"
    Write-Host 'More:              waired init --help'
    Write-Host 'Quickstart:        https://github.com/waired-ai/waired/blob/main/docs/quickstarts/README.md'
    Write-Host ''
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

# Ensure-AgentRunning -- best-effort start of the registered service after a
# fresh install, regardless of whether init ran. The SCM service is
# registered StartType=Automatic by `waired-agent install`, and the daemon
# boots identity-less safely (#177), so starting it now lets a non-admin
# user finish setup via the tray even when sign-in was skipped. Never
# aborts the install: a start failure is a warning.
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

# Enable-ClaudeProxy configures Claude Code routing via the elevated CLI (#488):
# `waired claude enable` writes the system-wide Claude Code managed settings
# (ANTHROPIC_BASE_URL -> the local gateway, NO credential, so the subscription
# and auto-mode are preserved) and sweeps up any retired MITM proxy artifacts.
# No certificate, hosts redirect, or :443 bind is involved. `waired init` already
# does this when run elevated; this is the explicit fallback. Best-effort: a
# failure warns but never aborts. Callers gate on a successful enrolment
# ($initRan); -SkipClaudeProxy / WAIRED_NO_CLAUDE_PROXY opt out entirely.
function Enable-ClaudeProxy {
    if ($SkipClaudeProxy) {
        Common-Log "-SkipClaudeProxy set; leaving Claude Code routed directly to api.anthropic.com."
        return
    }
    $exe = Join-Path $InstallDir 'waired.exe'
    if (-not (Test-Path -LiteralPath $exe)) {
        Common-Warn "waired.exe not found at $exe; skipping Claude integration setup."
        return
    }
    $stateForProxy = if ($StateDir) { $StateDir } else { $AgentStateDir }
    Common-Log "Configuring Claude Code integration (writes managed settings: ANTHROPIC_BASE_URL -> local inference, no credential, subscription preserved). Opt out with -SkipClaudeProxy."
    $proxyArgs = @('claude', 'enable', '--state-dir', $stateForProxy)
    Common-Run "& $exe $($proxyArgs -join ' ')" {
        & $exe @proxyArgs
        if ($LASTEXITCODE -ne 0) {
            Common-Warn "waired claude enable exited with code $LASTEXITCODE; enable later with: & `"$exe`" claude enable"
        }
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
if ($env:WAIRED_ARGTEST) {
    Write-Host ("ARGTEST Dev={0} Control={1} ControlUrl={2} Version={3} SkipOllama={4} SkipInit={5} SkipClaudeProxy={6} NonInteractive={7} DryRun={8} Update={9} Check={10} Yes={11}" -f `
        [bool]$Dev, $Control, $ControlUrl, $Version, [bool]$SkipOllama, [bool]$SkipInit, `
        [bool]$SkipClaudeProxy, [bool]$NonInteractive, [bool]$DryRun, [bool]$Update, [bool]$Check, [bool]$Yes)
    return
}

Detect-Platform

# Welcome banner -- Phase 1 only ($StagedZipPath set => elevated Phase 2
# child, which would otherwise print it a second time).
if (-not $StagedZipPath) { Show-Banner }

# -Check / -Update, or a bare re-run that detects an existing install,
# routes through the update flow instead of a fresh install (mirror of
# install.sh main()'s dispatch). The elevated child carries -Update, so
# $StagedZipPath being set means "already in Phase 2" -- exclude it from
# the bare-re-run auto-detect so the child doesn't re-enter Phase 1.
$installedVersion = Get-InstalledVersion
$updateRequested  = $Check -or $Update -or ($installedVersion -and -not $StagedZipPath)

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
    $workDir = Join-Path $env:TEMP "waired-install-$([Guid]::NewGuid().ToString('N'))"
    New-Item -ItemType Directory -Path $workDir -Force | Out-Null
    $stagedZip = $null
    try {
        $stagedZip = Get-AssetWithChecksum -WorkDir $workDir
        if (Test-Admin) {
            # Already elevated -> skip the self-re-exec and just run
            # Phase 2 inline so we don't pop a no-op UAC dialog. Log to a
            # transcript too so a record exists even here (waired#748); no
            # pause -- this is the user's own console, it does not vanish.
            try { Start-Transcript -Path $LogPath -Force -ErrorAction SilentlyContinue | Out-Null } catch { }
            try {
                Stop-ExistingService
                Extract-Zip -ZipPath $stagedZip
                Remove-TrayIfRequested
                Invoke-AgentInstall
                Add-InstallDirToPath
                Install-OllamaIfRequested
                # Invoke-WairedInit as a bare statement (not assigned) so waired
                # init's stdout reaches the console; it records the outcome in
                # $script:InitRan instead of returning it.
                Invoke-WairedInit
                $initRan = $script:InitRan
                Ensure-AgentRunning
                if ($initRan) { Enable-ClaudeProxy }
                New-StartMenuShortcuts
                Start-TrayAsOriginalUser
                Show-NextSteps -InitRan:$initRan
            } finally {
                Stop-TranscriptQuietly
            }
        } else {
            Invoke-SelfElevate -ZipPath $stagedZip
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

# This is the spawned elevated console: it closes the instant the script
# returns, taking every message with it. Record a transcript and pause on exit
# so its output (Show-NextSteps, or any failure) survives (waired#748).
# $ElevatedConsole also makes Common-Die pause, covering steps that exit 1
# directly. Both the pause and the CI legs are safe: Test-InteractiveStdin is
# false under -NonInteractive / redirected stdin, and elevated CI runners take
# the already-admin inline path above, never this spawned one.
$script:ElevatedConsole = $true
try { Start-Transcript -Path $LogPath -Force -ErrorAction SilentlyContinue | Out-Null } catch { }

if ($Update) {
    # Elevated swap-only path (the parent already gated + downloaded).
    Invoke-WairedUpdateSwap -StagedZip $StagedZipPath
    Stop-TranscriptQuietly
    if (Test-InteractiveStdin) { Read-Host '[waired] Update complete. Press Enter to close this window' | Out-Null }
    return
}

try {
    Common-Log "elevated phase: installing from $StagedZipPath"
    Stop-ExistingService
    Extract-Zip -ZipPath $StagedZipPath
    Remove-TrayIfRequested
    Invoke-AgentInstall
    Add-InstallDirToPath
    Install-OllamaIfRequested
    # Invoke-WairedInit as a bare statement (not assigned) so waired init's stdout
    # reaches the console; it records the outcome in $script:InitRan.
    Invoke-WairedInit
    $initRan = $script:InitRan
    Ensure-AgentRunning
    if ($initRan) { Enable-ClaudeProxy }
    New-StartMenuShortcuts
    Start-TrayAsOriginalUser
    Show-NextSteps -InitRan:$initRan
} catch {
    # A terminating error that was NOT a Common-Die (those exit + pause on their
    # own). Route it through Common-Die for the same log-path + pause + exit 1.
    Common-Die "install failed: $($_.Exception.Message)"
}

Stop-TranscriptQuietly
if (Test-InteractiveStdin) {
    Read-Host '[waired] Install complete. Press Enter to close this window' | Out-Null
}
