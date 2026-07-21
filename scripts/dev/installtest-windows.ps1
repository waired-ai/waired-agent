#Requires -Version 5.1
<#
.SYNOPSIS
    Run the working-tree Windows installer end-to-end on THIS host and assert
    the result — the Windows analog of installtest-run.sh's Linux path (#497).

.DESCRIPTION
    Tier 1: build waired.exe + waired-agent.exe from the worktree, pack the
      release zip with the real packer (packaging/windows/make-zip.ps1), serve
      it from a loopback HTTP mirror laid out the way install.ps1 expects, then
      run install.ps1. On a GitHub-hosted windows runner the process is already
      elevated, so install.ps1 takes its "already admin" path: real download +
      SHA-256 verify, then an inline SCM install (no UAC, no child process).
      Asserts: the waired-agent service is registered, Running, Automatic; the
      %ProgramData%\waired state dir exists; the binaries are in place; and
      %ProgramFiles%\Waired is on the machine PATH (#482 regression guard).
    Tier 2 (-Tier 2): + hands-free enroll against the real app.dev.waired.net
      via the #339 SA-OIDC grant — gcloud (WIF) mints the SA id_token, then
      `waired init --google-sa-login --oidc-id-token <tok>`. Asserts identity
      lands under %ProgramData%\waired and the daemon reports it on the mgmt API.

    Designed to run directly on a disposable runner (no nesting). Mirrors the
    enroll knobs of lib/installtest-enroll.sh: IT_ENROLL_MODE (only `oidc`
    supported here), IT_IMPERSONATE_SA, IT_CONTROL_URL.

.PARAMETER Tier
    1 = install + service asserts; 2 = + hands-free enroll. Default 1.

.PARAMETER WithInference
    Pairs with -Tier 2 (#514): install Ollama (no -SkipOllama) and enroll with
    --inference-enabled=true. init starts the agent and, via #519's foreground
    wait, blocks until the agent has pulled the bundled model into the
    waired-owned engine on :9475, then runs the end-of-init benchmark. Asserts:
    Ollama present, the bundled model reaches `ready` in the waired-owned store
    (queried through the agent mgmt API at :9476 /waired/v1/inference/status, NOT
    a bare `ollama list` which targets the upstream :11434 store the bundled
    engine does not use — see #564), inference enabled in the persisted config,
    and a benchmark figure in the init transcript (the Windows analog of
    lib/installtest-enroll.sh's assert_inference).

.PARAMETER Contract
    waired#760: behavioral-contract asserts (`waired status` exit 0 incl.
    standard-user and filtered-token contexts, `waired claude enable` →
    managed-settings, tray surfaced) + teardown via uninstall.ps1 -Clean with
    leftover asserts. Each assert is tied to an open issue (#749/#751/#754/
    #755) and soft-fails (WARN) until the fix merges and flips its
    $ContractBlocking entry. Requires -Tier 2. This is what the per-PR CI
    (installtest.yml) runs.

.PARAMETER ExeVariant
    waired#760/#759: after the ps1-path -Clean uninstall, ISCC-compile the
    Inno installer from the same staged binaries, install it /VERYSILENT,
    re-run Tier-1-level asserts (no second enroll), then uninstall. Implies
    -Contract. Needs Inno Setup 6 (iscc) on the machine.
#>
[CmdletBinding()]
param(
    [int]$Tier = 1,
    [switch]$WithInference,
    # -WithIntegration: after enroll, run the coding-agent routing sentinel
    # (#496). Implies inference but PINS the tiny 0.5B as the bundled model (so
    # the deploy pulls ~0.4 GB), then runs the Go harness that drives each leg at
    # the gateway surface and asserts served-locally via the event ring.
    [switch]$WithIntegration,
    # -Contract (waired#760): behavioral-contract asserts + non-elevated
    # contexts + uninstall.ps1 -Clean teardown asserts. Each assert is tied to
    # an open issue (#749/#751/#754/#755) and SOFT-fails (WARN) until the fix
    # merges and flips its $ContractBlocking entry below. Requires -Tier 2
    # (the asserts run against an enrolled device).
    [switch]$Contract,
    # -ExeVariant (waired#760/#759): after the ps1-path -Clean uninstall,
    # ISCC-compile the Inno installer from the same staged binaries, install it
    # silently, re-run Tier-1-level asserts (no second enroll), uninstall.
    # Implies -Contract (it needs the -Clean uninstall between the two installs).
    [switch]$ExeVariant,
    # -DaemonEngine (waired#835 §9/§11): drive the DAEMON-path first-run so the
    # resident `waired init` executor installs the engine on an engine-less host
    # -- the path the standalone --google-sa-login enrol never reaches (that flag
    # forces the standalone path). Keeps install.ps1's engine-absent state,
    # completes the daemon login out-of-band via the OIDC grant, and asserts the
    # engine landed via the executor (not install.ps1). Its own mode; Tier 2.
    [switch]$DaemonEngine
)

# -WithIntegration rides the inference engine.
if ($WithIntegration) { $WithInference = $true }
# -ExeVariant needs the ps1 path torn down first (fresh-install, not upgrade).
if ($ExeVariant) { $Contract = $true }
if ($Contract -and $Tier -lt 2) {
    Write-Host "[installtest] -Contract requires -Tier 2 (asserts need an enrolled device)" -ForegroundColor Red
    exit 1
}
if ($DaemonEngine -and ($WithInference -or $WithIntegration)) {
    Write-Host "[installtest] -DaemonEngine is its own mode; not with -WithInference/-WithIntegration" -ForegroundColor Red
    exit 1
}
if ($DaemonEngine -and $Tier -lt 2) {
    Write-Host "[installtest] -DaemonEngine requires -Tier 2 (it enrolls to reach the executor)" -ForegroundColor Red
    exit 1
}

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'

# --- config / constants (mirror install.ps1) --------------------------------
$Root         = (& git rev-parse --show-toplevel).Trim()
$InstallDir   = Join-Path $env:ProgramFiles 'Waired'
$ServiceName  = 'waired-agent'
$StateDir     = Join-Path $env:ProgramData 'waired'
$ZipName      = 'waired-windows-amd64.zip'
$Port         = if ($env:IT_REPO_PORT) { [int]$env:IT_REPO_PORT } else { 8099 }
$ControlUrl   = if ($env:IT_CONTROL_URL) { $env:IT_CONTROL_URL } else { 'https://app.dev.waired.net' }
$EnrollMode   = if ($env:IT_ENROLL_MODE) { $env:IT_ENROLL_MODE } else { 'oidc' }
$ImpersonateSa= $env:IT_IMPERSONATE_SA
$MgmtStatus   = 'http://127.0.0.1:9476/waired/v1/status'

$Work         = Join-Path ([System.IO.Path]::GetTempPath()) 'waired-installtest-win'
$Stage        = Join-Path $Work 'stage'
$Mirror       = Join-Path $Work 'mirror'

# --- logging / assert counters ----------------------------------------------
$script:Pass = 0
$script:Fail = 0
function ItStep { param([string]$m) Write-Host "[installtest] ==> $m" -ForegroundColor Green }
function ItLog  { param([string]$m) Write-Host "[installtest] $m" -ForegroundColor Cyan }
function ItOk   { param([string]$m) Write-Host "[installtest]  ok  $m" -ForegroundColor Green; $script:Pass++ }
function ItBad  { param([string]$m) Write-Host "[installtest] FAIL $m" -ForegroundColor Red; $script:Fail++ }
function ItDie  { param([string]$m) Write-Host "[installtest] $m" -ForegroundColor Red; exit 1 }

# --- contract asserts (waired#760): soft-fail while the underlying issue is
# open. When a fix merges, its PR flips the ONE matching line below to $true
# and the assert becomes blocking from then on.
$script:ContractBlocking = @{
    '749' = $true    # waired#749: `waired claude enable` writes managed-settings on Windows (FIXED)
    '751' = $true    # waired#751: `waired status` exits 0 in non-elevated contexts (FIXED)
    '754' = $true    # waired#754: uninstall.ps1 -Clean leaves zero per-user artifacts (FIXED)
    '755' = $true    # waired#755: the install path surfaces the tray (Start Menu group / autostart) (FIXED)
    '838' = $true    # waired#838: management writes travel over the local named pipe, not TCP (FIXED)
}
$script:Warn = 0
$script:WarnLines = @()
function ItSoft {
    param([string]$Issue, [bool]$Ok, [string]$m)
    if ($Ok) { ItOk "$m (waired#$Issue)"; return }
    if ($script:ContractBlocking[$Issue]) {
        ItBad "$m (waired#$Issue fix merged -- blocking)"
    } else {
        Write-Host "[installtest] WARN $m (waired#$Issue open -- soft)" -ForegroundColor Yellow
        $script:Warn++
        $script:WarnLines += "waired#${Issue}: $m"
    }
}

# --- daemon-path executor engine-install assert (waired#835 §9/§11) ----------
# Windows analog of lib/installtest-daemon-engine.sh's assert_daemon_engine.
# Regression bar: an engine-less daemon-path first-run ends up WITH an engine
# (pre-N3 it stayed engine-less and engine_install was red forever). install.ps1
# ran engine-absent, so only the resident executor could have installed one.
function Assert-DaemonEngine {
    param([string]$InitLog, [string]$Flag)

    if (Select-String -Path $InitLog -Pattern 'signing in via the daemon' -Quiet -ErrorAction SilentlyContinue) {
        ItOk "init took the daemon path (setup-executor-capable first-run)"
    } else { ItBad "init did NOT take the daemon path (executor engine install not exercised)" }

    $flagText = if (Test-Path -LiteralPath $Flag) { Get-Content -LiteralPath $Flag -Raw } else { '' }
    if ($flagText -match '(?m)^completed=1') { ItOk "daemon login completed out-of-band via the OIDC grant" }
    else { ItBad "out-of-band OIDC completion did not report success" }
    if ($flagText -match '(?m)^executor_attached=1') { ItOk "setup executor lease was live during setup (executor_attached)" }
    else { ItBad "never observed executor_attached -- executor engine-install path not reached" }
    if ($flagText -match '(?m)^install_claimed=ollama') { ItOk "executor claimed the ollama install (install_claimed=ollama)" }
    else { ItLog "did not catch install_claimed=ollama in the 2s poll -- non-fatal" }

    # The regression bar: an engine is present (mirror Assert-Inference's lookup).
    $ollama = $null
    foreach ($p in @(
            (Join-Path $env:ProgramFiles 'Ollama\ollama.exe'),
            (Join-Path $env:LOCALAPPDATA 'Programs\Ollama\ollama.exe'))) {
        if (Test-Path -LiteralPath $p) { $ollama = $p; break }
    }
    if (-not $ollama) { $cmd = Get-Command ollama.exe -ErrorAction SilentlyContinue; if ($cmd) { $ollama = $cmd.Source } }
    if ($ollama) { ItOk "ollama engine installed by the daemon-path executor ($ollama)" }
    else { ItBad "no engine after a daemon-path first-run (executor install did not land -- pre-N3 behaviour)" }

    $state = ''
    try { $state = (Invoke-RestMethod -Uri 'http://127.0.0.1:9476/waired/v1/inference/status' -TimeoutSec 5).subsystem_state } catch { }
    if ($state -and $state -ne 'no_engine') { ItOk "inference subsystem left no_engine (state=$state)" }
    else { ItBad "inference subsystem still reports '$state' (engine not installed)" }

    $claim = ''
    try { $claim = (Invoke-RestMethod -Uri 'http://127.0.0.1:9476/waired/v1/setup/state' -TimeoutSec 5).install_claimed } catch { }
    if (-not $claim) { ItOk "no stuck executor install claim after init (install_claimed cleared)" }
    else { ItBad "executor install claim still set after init (install_claimed=$claim; stuck)" }
}

# --- inference assert (Windows analog of assert_inference) -------------------
# Prove the Ollama-install -> bundled-model-pull -> benchmark tail of the
# first-run journey ran (Tier-2 -WithInference): `waired init
# --inference-enabled=true` installed the Ollama engine itself (init owns the
# engine install now; install.ps1 no longer pre-installs it), started the
# agent, and (via #519's waitForBundledModel) blocked until the agent pulled
# the bundled model into the waired-owned engine on :9475, then ran the
# benchmark.
#
# #564: the bundled engine is waired-owned on :9475 with its own model store; the
# agent pulls there, NOT into the upstream Ollama default :11434. So readiness is
# asserted through the agent's mgmt API (/waired/v1/inference/status), the same
# source init's own foreground wait polls — never a bare `ollama list` (which
# queries :11434 and is always empty here, the original false negative).
function Assert-Inference {
    param([string]$InitLog)

    # 1) ollama.exe discoverable (mirror internal/download's Windows order)
    $ollama = $null
    foreach ($p in @(
            (Join-Path $env:ProgramFiles 'Ollama\ollama.exe'),
            (Join-Path $env:LOCALAPPDATA 'Programs\Ollama\ollama.exe'))) {
        if (Test-Path -LiteralPath $p) { $ollama = $p; break }
    }
    if (-not $ollama) {
        $cmd = Get-Command ollama.exe -ErrorAction SilentlyContinue
        if ($cmd) { $ollama = $cmd.Source }
    }
    if ($ollama) { ItOk "ollama engine installed ($ollama)" }
    else { ItBad "ollama engine not installed (waired init --inference-enabled=true should have installed it)" }

    # 1b) the waired-managed marker: init's install must drop it so a later
    #     `waired init` recognises the engine as waired's own and never asks
    #     the bundled-vs-reuse question about it.
    $marker = Join-Path $env:ProgramFiles 'Ollama\.waired-managed.json'
    if (Test-Path -LiteralPath $marker) { ItOk "waired-managed marker present ($marker)" }
    else { ItBad "waired-managed marker missing ($marker)" }

    # 2) bundled model READY in the waired-owned store (:9475), via the agent
    #    mgmt API. init (#519) foreground-waits for the pull, so it is normally
    #    ready the moment init returns; poll briefly to absorb any residual async
    #    tail (e.g. the harness's post-init service restart re-checking the model).
    $inferStatusUrl = 'http://127.0.0.1:9476/waired/v1/inference/status'
    $modelReady = $false; $subState = ''; $modelsReady = @()
    $deadline = (Get-Date).AddMinutes(5)
    while ((Get-Date) -lt $deadline) {
        try {
            $st = Invoke-RestMethod -Uri $inferStatusUrl -TimeoutSec 10
            $subState    = [string]$st.subsystem_state
            $modelsReady = @($st.models.ready)
            if (($subState -eq 'ready') -or ($modelsReady -match '(?i)qwen|coder')) { $modelReady = $true; break }
            if ($subState -in @('pull_failed','disabled','stopped')) { break }
        } catch { }
        Start-Sleep -Seconds 10
    }
    if ($modelReady) {
        $name = if ($modelsReady) { @($modelsReady)[0] } else { '(ready)' }
        ItOk "bundled model ready in waired store :9475 ($name; subsystem_state=$subState)"
    } else {
        ItBad "bundled model not ready via mgmt API (subsystem_state=$subState; models.ready=$($modelsReady -join ','))"
        # Diagnostics: query the waired-owned store directly (NOT the default :11434).
        if ($ollama) {
            $env:OLLAMA_HOST = '127.0.0.1:9475'
            try { ((& $ollama list 2>&1 | Out-String) -split "`n") | ForEach-Object { Write-Host "    :9475 $_" } } catch { }
            Remove-Item Env:\OLLAMA_HOST -ErrorAction SilentlyContinue
        }
    }

    # 3) inference enabled in the persisted config under %ProgramData%\waired
    $cfgEnabled = $false
    Get-ChildItem -LiteralPath $StateDir -Filter *.json -ErrorAction SilentlyContinue | ForEach-Object {
        $raw = Get-Content -LiteralPath $_.FullName -Raw -ErrorAction SilentlyContinue
        if ($raw -match '"enabled"\s*:\s*true') { $cfgEnabled = $true }
    }
    if ($cfgEnabled) { ItOk "inference enabled in persisted agent config" }
    else { ItBad "inference not enabled in persisted config" }

    # 4) benchmark ran in the init transcript (offerBenchmark). Accept a
    #    throughput number (tok/s | tokens/s | throughput) OR the "Local
    #    inference works" smoke line: a host too slow to measure a stable rate
    #    exhausts the boot benchmark's budget and reports MeasuredTokps=0
    #    ("…interactive performance looks good"), yet a real generation still
    #    ran. Both print ONLY after a benchmark ran — never the "run `waired
    #    runtimes benchmark` later" tip (the #564 false positive).
    if (Test-Path -LiteralPath $InitLog) {
        $txt = Get-Content -LiteralPath $InitLog -Raw
        if ($txt -match '(?i)tok/s|tokens/s|throughput|Local inference works') {
            $m = [regex]::Match($txt, '(?i)[0-9]+(\.[0-9]+)?\s*(tok|tokens)/s')
            $tps = if ($m.Success) { " ($($m.Value))" } else { '' }
            ItOk "benchmark ran during init$tps"
        } else {
            ItBad "no benchmark output captured in init transcript ($InitLog)"
        }
    } else {
        ItBad "no init transcript captured ($InitLog)"
    }
}

# --- management write pipe assert (waired#838/#80) --------------------------
# Windows analog of lib/installtest-enroll.sh's assert_mgmt_socket: mutating
# management requests must travel over the local named pipe and must NOT be
# accepted on the loopback TCP port, while reads stay on TCP.
#
# Load-bearing because writeGuard fails OPEN: if the pipe never comes up,
# writes silently fall back to the old TCP behaviour and nothing else goes
# red. (On Linux this same assert is what caught a missing systemd
# RuntimeDirectory.) The pipe DACL is SDDL "SY+BA+IU" — IU excludes network
# logons, so the pipe is unreachable over SMB.
#
# Written through ItSoft '838' so it shares the contract-assert plumbing. The
# entry is BLOCKING ($ContractBlocking['838'] = $true): it was staged as a WARN
# for one observation run (the pipe path cannot be exercised off a real Windows
# host) and flipped once that run came back clean on all five legs.
function Assert-MgmtPipe {
    $pipe = 'waired-mgmt'

    # There is no filesystem node for a pipe, so connectability IS the
    # existence proof.
    $connected = $false
    $client = $null
    try {
        $client = New-Object System.IO.Pipes.NamedPipeClientStream(
            '.', $pipe, [System.IO.Pipes.PipeDirection]::InOut)
        $client.Connect(3000)
        $connected = $client.IsConnected
    } catch {
        ItLog "named-pipe connect threw: $($_.Exception.Message)"
    } finally {
        if ($client) { $client.Dispose() }
    }
    if (-not $connected) {
        # Diagnostic: what waired-ish pipes exist at all?
        try {
            $names = [System.IO.Directory]::GetFiles('\\.\pipe\') |
                     Where-Object { $_ -match 'waired' }
            ItLog "pipes matching 'waired': $(if ($names) { $names -join ', ' } else { '(none)' })"
        } catch { ItLog "could not enumerate \\.\pipe\: $($_.Exception.Message)" }
    }
    ItSoft '838' $connected "management write pipe \\.\pipe\$pipe is connectable"

    # The exit code alone proves nothing: runPhaseTransition treats an
    # unreachable daemon as the documented offline fallback (persist the
    # desired phase, return 0). Assert on stdout — "pause ok." is printed
    # only after a real daemon round-trip.
    # EAP is relaxed around the native calls (redirected native stderr becomes
    # a terminating NativeCommandError under EAP=Stop in PS 5.1 — the same
    # trap the Tier-2 init call documents).
    $waired  = Join-Path $InstallDir 'waired.exe'
    $prevEap = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $pauseOut  = (& $waired pause  2>&1 | Out-String)
        $resumeOut = (& $waired resume 2>&1 | Out-String)
        # The CLI pretty-prints the daemon's JSON reply; flatten it so each
        # assert below stays one readable log line.
        $pauseLine  = ($pauseOut  -replace '\s+', ' ').Trim()
        $resumeLine = ($resumeOut -replace '\s+', ' ').Trim()
    } finally {
        $ErrorActionPreference = $prevEap
    }
    $pauseOk = ($pauseOut -match 'pause ok\.') -and ($pauseOut -notmatch 'not running')
    ItSoft '838' $pauseOk "waired pause reached the daemon over the pipe -- $pauseLine"
    $resumeOk = ($resumeOut -match 'resume ok\.') -and ($resumeOut -notmatch 'not running')
    ItSoft '838' $resumeOk "waired resume reached the daemon over the pipe -- $resumeLine"

    # Negative: the same mutating verb must be refused on the TCP port.
    # PS 5.1 has no -SkipHttpErrorCheck, so a non-2xx surfaces as a terminating
    # WebException whose Response carries the status.
    $tcpCode = $null
    try {
        $r = Invoke-WebRequest -UseBasicParsing -Method POST `
                -ContentType 'application/json' `
                -Uri 'http://127.0.0.1:9476/waired/v1/pause' -TimeoutSec 5
        $tcpCode = [int]$r.StatusCode
    } catch {
        if ($_.Exception.Response) { $tcpCode = [int]$_.Exception.Response.StatusCode }
    }
    $tcpRefused = ($null -ne $tcpCode) -and ($tcpCode -lt 200 -or $tcpCode -ge 300)
    ItSoft '838' $tcpRefused "TCP :9476 refuses mutating writes (HTTP $tcpCode)"

    # Reads deliberately stay on TCP.
    $readOk = $false
    try { $null = Invoke-RestMethod -Uri $MgmtStatus -TimeoutSec 5; $readOk = $true } catch { }
    ItSoft '838' $readOk "TCP :9476 still serves reads"

    # Leave the daemon active whichever leg above failed.
    $prevEap = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try { & $waired resume *> $null } finally { $ErrorActionPreference = $prevEap }
}

# --- loopback HTTP mirror (no external deps) --------------------------------
# Serves $Mirror over http://127.0.0.1:$Port/ in a background job, so
# install.ps1's real Invoke-WebRequest download + SHA path is exercised.
function Start-Mirror {
    param([string]$RootDir, [int]$ListenPort)
    $job = Start-Job -ScriptBlock {
        param($RootDir, $ListenPort)
        $listener = [System.Net.HttpListener]::new()
        $listener.Prefixes.Add("http://127.0.0.1:$ListenPort/")
        $listener.Start()
        while ($listener.IsListening) {
            try { $ctx = $listener.GetContext() } catch { break }
            $rel  = [Uri]::UnescapeDataString($ctx.Request.Url.AbsolutePath.TrimStart('/'))
            $path = Join-Path $RootDir $rel
            if (Test-Path -LiteralPath $path -PathType Leaf) {
                $bytes = [System.IO.File]::ReadAllBytes($path)
                $ctx.Response.ContentType   = 'application/octet-stream'
                $ctx.Response.ContentLength64 = $bytes.Length
                $ctx.Response.OutputStream.Write($bytes, 0, $bytes.Length)
            } else {
                $ctx.Response.StatusCode = 404
            }
            $ctx.Response.Close()
        }
    } -ArgumentList $RootDir, $ListenPort
    return $job
}

# --- non-elevated execution helpers (waired#760 / #751) ----------------------
# Both run a command in a LESS-privileged context inside this same guest and
# capture exit code + output via an on-disk .cmd wrapper writing output + an
# exit-code marker file (the launchers detach, so a direct exit code is not
# available). Artifacts live under C:\Users\Public so the restricted contexts
# can read/execute/write there (the elevated user's %TEMP% is not).
$PubWork  = 'C:\Users\Public\waired-it'
$TestUser = 'waired-it'

# Write the wrapper + return its paths. The %ERRORLEVEL% echo keeps a space
# before '>' — `echo 0> file` would parse `0>` as a HANDLE redirect (stdin)
# and write "ECHO is off." instead of the code; the trailing space is trimmed
# on read. It also sits on its own line so cmd expands it at run time.
function Write-ItCmdWrapper {
    param([string]$Exe, [string]$ArgLine, [string]$Tag)
    New-Item -ItemType Directory -Path $PubWork -Force | Out-Null
    $paths = @{
        Cmd = Join-Path $PubWork "$Tag.cmd"
        Out = Join-Path $PubWork "$Tag.out"
        Rc  = Join-Path $PubWork "$Tag.rc"
    }
    Remove-Item -LiteralPath $paths.Out, $paths.Rc -Force -ErrorAction SilentlyContinue
    @(
        '@echo off'
        "`"$Exe`" $ArgLine > `"$($paths.Out)`" 2>&1"
        "echo %ERRORLEVEL% > `"$($paths.Rc)`""
    ) | Set-Content -LiteralPath $paths.Cmd -Encoding ASCII
    return $paths
}

# Poll for the wrapper's rc marker and parse it defensively (never throw —
# these run inside soft-assert flows).
function Wait-ItCmdWrapper {
    param([hashtable]$Paths, [int]$TimeoutSec)
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline -and -not (Test-Path -LiteralPath $Paths.Rc)) { Start-Sleep -Milliseconds 250 }
    if (-not (Test-Path -LiteralPath $Paths.Rc)) { return @{ Exit = -1; Out = "(timeout: wrapped command never completed within ${TimeoutSec}s)" } }
    Start-Sleep -Milliseconds 200   # let cmd flush + close the redirects
    $raw  = [string](Get-Content -LiteralPath $Paths.Rc -First 1)
    $code = 0
    if (-not [int]::TryParse($raw.Trim(), [ref]$code)) {
        return @{ Exit = -1; Out = "(unparseable exit-code marker: '$raw')" }
    }
    return @{ Exit = $code; Out = (Get-Content -LiteralPath $Paths.Out -Raw -ErrorAction SilentlyContinue) }
}

# Plain Users members lack SeBatchLogonRight, so a password-stored scheduled
# task for them never launches (Status stays Ready, Last Result 267011 =
# SCHED_S_TASK_HAS_NOT_RUN — observed on the first CI runs). secedit is the
# standard way to grant it non-interactively on the disposable guest.
function Grant-ItBatchLogonRight {
    param([string]$User)
    $sid = (New-Object System.Security.Principal.NTAccount($User)).Translate([System.Security.Principal.SecurityIdentifier]).Value
    $cur = Join-Path $Work 'rights-cur.inf'
    $inf = Join-Path $Work 'rights-new.inf'
    $db  = Join-Path $Work 'rights.sdb'
    & secedit /export /cfg $cur /areas USER_RIGHTS | Out-Null
    $line = Get-Content -LiteralPath $cur -ErrorAction SilentlyContinue | Where-Object { $_ -match '^SeBatchLogonRight' } | Select-Object -First 1
    $val  = if ($line) { (($line -split '=', 2)[1]).Trim() } else { '' }
    if ($val -match [regex]::Escape($sid)) { return }
    $val = if ($val) { "$val,*$sid" } else { "*$sid" }
    @(
        '[Unicode]'
        'Unicode=yes'
        '[Version]'
        'signature="$CHICAGO$"'
        'Revision=1'
        '[Privilege Rights]'
        "SeBatchLogonRight = $val"
    ) | Set-Content -LiteralPath $inf -Encoding Unicode
    & secedit /configure /db $db /cfg $inf /areas USER_RIGHTS | Out-Null
}

# Fresh standard (non-admin) user, run via a one-shot scheduled task (batch
# logon). Start-Process -Credential (CreateProcessWithLogonW) fails with
# 0xC0000142 here: the second user's process cannot initialize against the
# runner session's window station/desktop. A Task Scheduler batch logon has
# no window-station dependency, so the wrapped command runs and reports its
# REAL exit code. The plaintext /RP on the command line is fine: throwaway
# password, throwaway user, disposable guest.
function Invoke-AsStandardUser {
    param([string]$Exe, [string]$ArgLine, [string]$Tag)
    if (-not $script:TestUserPw) {
        # Random password satisfying default complexity; the guest is ephemeral.
        $script:TestUserPw = "Wt1!$([Guid]::NewGuid().ToString('N').Substring(0,12))"
        $sec = ConvertTo-SecureString $script:TestUserPw -AsPlainText -Force
        if (-not (Get-LocalUser -Name $TestUser -ErrorAction SilentlyContinue)) {
            New-LocalUser -Name $TestUser -Password $sec -PasswordNeverExpires -AccountNeverExpires | Out-Null
            Add-LocalGroupMember -Group 'Users' -Member $TestUser
        } else {
            Set-LocalUser -Name $TestUser -Password $sec
        }
        Grant-ItBatchLogonRight -User $TestUser
    }
    # Grant BEFORE writing the wrapper: (OI)(CI) inheritance only applies to
    # children created afterwards, and the batch logon is not INTERACTIVE so
    # Public-folder defaults may not cover it.
    New-Item -ItemType Directory -Path $PubWork -Force | Out-Null
    & icacls $PubWork /grant "${TestUser}:(OI)(CI)M" | Out-Null
    $paths = Write-ItCmdWrapper -Exe $Exe -ArgLine $ArgLine -Tag $Tag
    $task = "waired-it-$Tag"
    # $PubWork deliberately contains no spaces, so /TR needs no inner quotes —
    # schtasks mangles nested quoting notoriously; keep the action bare.
    $create = (& schtasks /Create /F /TN $task /TR "cmd /c $($paths.Cmd)" /SC ONCE /ST 23:59 /RU $TestUser /RP $script:TestUserPw 2>&1 | Out-String).Trim()
    if ($LASTEXITCODE -ne 0) { return @{ Exit = -1; Out = "(schtasks /Create failed: $create)" } }
    $run = (& schtasks /Run /TN $task 2>&1 | Out-String).Trim()
    $r = Wait-ItCmdWrapper -Paths $paths -TimeoutSec 60
    if ($r.Exit -eq -1) {
        # Surface why the task never produced the marker (logon-right denial,
        # action mangling, ...) instead of a bare timeout.
        $query = (& schtasks /Query /TN $task /V /FO LIST 2>&1 | Out-String) -split "`r?`n" |
                 Where-Object { $_ -match 'Last Result|Status:' } | ForEach-Object { $_.Trim() }
        $r.Out = "$($r.Out) [run: $run] [$($query -join '; ')]"
    }
    & schtasks /Delete /TN $task /F 2>&1 | Out-Null
    return $r
}

# Filtered/basic token of the CURRENT user via `runas /trustlevel:0x20000` — a
# SAFER-restricted token, the same class as a UAC-filtered admin (#751's exact
# context). runas detaches immediately (its exit code only reflects launch),
# hence the wrapper + marker poll.
function Invoke-AsBasicToken {
    param([string]$Exe, [string]$ArgLine, [string]$Tag)
    $paths = Write-ItCmdWrapper -Exe $Exe -ArgLine $ArgLine -Tag $Tag
    & runas /trustlevel:0x20000 "cmd /c `"$($paths.Cmd)`"" | Out-Null
    return (Wait-ItCmdWrapper -Paths $paths -TimeoutSec 45)
}

# ============================================================================
# Build + pack + serve
# ============================================================================
ItStep "building waired.exe + waired-agent.exe from worktree"
$ver = (& git -C $Root rev-parse --short HEAD).Trim()
$ldf = "-s -w -X github.com/waired-ai/waired-agent/internal/buildinfo.Version=$ver -X github.com/waired-ai/waired-agent/internal/buildinfo.BuildSHA=$ver"
Remove-Item -LiteralPath $Work -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Path $Stage -Force | Out-Null
Set-Location -LiteralPath $Root
$env:GOOS = 'windows'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
& go build -trimpath -ldflags="$ldf" -o (Join-Path $Stage 'waired.exe')       ./cmd/waired
if ($LASTEXITCODE -ne 0) { ItDie "go build waired failed" }
& go build -trimpath -ldflags="$ldf" -o (Join-Path $Stage 'waired-agent.exe') ./cmd/waired-agent
if ($LASTEXITCODE -ne 0) { ItDie "go build waired-agent failed" }
# waired-tray.exe ships in the real release zip (Makefile dist-windows-installer)
# and is an Inno [Files] input — build it too so the harness zip matches the
# release layout and the #755 tray-surface assert isn't vacuous. -H=windowsgui
# mirrors the Makefile (no console window if anything ever launches it).
& go build -trimpath -ldflags="$ldf -H=windowsgui" -o (Join-Path $Stage 'waired-tray.exe') ./cmd/waired-tray
if ($LASTEXITCODE -ne 0) { ItDie "go build waired-tray failed" }
Set-Content -LiteralPath (Join-Path $Stage 'VERSION') -Value "0.0.0-$ver" -Encoding ASCII -NoNewline
# LICENSE + THIRD_PARTY_LICENSES are release-zip contents and Inno [Files]
# inputs (#4). The release build stages them via `make dist-windows-installer`
# (go-licenses); the harness copies the real repo LICENSE and writes a
# THIRD_PARTY_LICENSES placeholder, so the zip layout and the .iss compile are
# exercised end-to-end without a go-licenses run on the Windows leg.
Copy-Item -LiteralPath (Join-Path $Root 'LICENSE') -Destination (Join-Path $Stage 'LICENSE') -Force
Set-Content -LiteralPath (Join-Path $Stage 'THIRD_PARTY_LICENSES') -Value "installtest placeholder - real third-party notices are generated at release time (make third-party-licenses)." -Encoding ASCII -NoNewline

ItStep "packing $ZipName (real packer) + laying out the loopback mirror"
$relDir = Join-Path $Mirror 'latest\download'      # Version=latest -> $BaseUrl/latest/download
New-Item -ItemType Directory -Path $relDir -Force | Out-Null
$zipOut = Join-Path $relDir $ZipName
& (Join-Path $Root 'packaging\windows\make-zip.ps1') -SourceDir $Stage -OutZip $zipOut
if (-not (Test-Path -LiteralPath $zipOut)) { ItDie "make-zip.ps1 did not produce $zipOut" }

ItStep "serving mirror on http://127.0.0.1:$Port"
$mirrorJob = Start-Mirror -RootDir $Mirror -ListenPort $Port
$ready = $false
for ($i = 0; $i -lt 20; $i++) {
    try { Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$Port/latest/download/$ZipName.sha256" -TimeoutSec 3 | Out-Null; $ready = $true; break }
    catch { Start-Sleep -Milliseconds 500 }
}
if (-not $ready) { Receive-Job $mirrorJob 2>&1 | Out-Host; ItDie "mirror did not come up on :$Port" }

# ============================================================================
# Tier 1: install + assert
# ============================================================================
try {
    $env:WAIRED_INSTALL_BASE_URL = "http://127.0.0.1:$Port"
    $env:WAIRED_VERSION          = 'latest'
    # WAIRED_NO_TRAY is deliberately NOT set (waired#760): the zip now ships
    # waired-tray.exe like a real release, so the #755 tray-surface contract
    # assert below observes what a real web install leaves behind. install.ps1
    # never LAUNCHES the tray, so this adds no GUI process to the CI session.
    # WAIRED_NO_EMOJI is intentionally NOT set for the install step so
    # install.ps1's rich (UTF-8) banner path runs here -- exercising the
    # Base64 art + Glyph/Utf8FromB64 runtime construction. A regression that
    # reintroduces literal non-ASCII source bytes (the iwr|iex mojibake) or
    # breaks glyph construction then fails this leg. Source-byte purity is
    # also guarded by scripts/install/encoding_test.go. It is reset to '1'
    # before the Tier-2 'waired init' so the binary's enroll output stays
    # ASCII, matching the macOS/Linux legs.
    $env:WAIRED_DEV_CONTROL_URL  = $ControlUrl

    # Ollama: install.ps1 no longer installs the engine at all — `waired init`
    # owns the decision + install (the Tier-2 -WithInference init below, run
    # elevated with --inference-enabled=true, installs it via the embedded
    # ollama-windows.ps1). -SkipOllama now just resolves to WAIRED_NO_OLLAMA
    # for the init child, so the default installer+enroll leg still opts out
    # explicitly below. Pass the switches inline per branch — array splat
    # (@args) binds elements as POSITIONAL args, not named switches, so install.ps1
    # would misread -Dev as the control URL.
    $installPs1 = Join-Path $Root 'packaging\install\install.ps1'

    # install.ps1 arg-parsing contract (waired#746). install.ps1's WAIRED_ARGTEST
    # seam returns right after arg-normalization + Resolve-ControlUrl, before any
    # download / UAC, so these run cheaply in a child process and install
    # NOTHING. Assert the install.sh-style --dev / --control spellings resolve,
    # and that a stray / mistyped arg fails loudly instead of silently
    # mis-binding to -Control (the pre-fix bug ran `waired init --control --dev`).
    ItStep "install.ps1 arg-parsing asserts (waired#746)"
    function Invoke-Argtest([string[]]$a) {
        # Invoke install.ps1 the way Phase 1 actually runs -- IN-SESSION
        # (`& install.ps1 <args>` / iwr|iex), NOT -File. That is the parse mode
        # where a bare `--dev` is a positional value (the #746 bug); -File would
        # instead bind `--dev` natively to -Dev and never exercise the fix. Run
        # in a child process so a Common-Die (exit 1) can't tear down this test.
        $env:WAIRED_ARGTEST = '1'
        try {
            $cmd = "& '$installPs1' " + ($a -join ' ')
            $o = & powershell.exe -NoProfile -ExecutionPolicy Bypass -Command $cmd 2>&1 | Out-String
        } finally { Remove-Item Env:WAIRED_ARGTEST -ErrorAction SilentlyContinue }
        [pscustomobject]@{ Exit = $LASTEXITCODE; Out = $o }
    }
    $r = Invoke-Argtest @('--dev')
    if ($r.Exit -eq 0 -and $r.Out -match 'ControlUrl=https?://\S') { ItOk "--dev resolves a Control URL (install.sh parity)" }
    else { ItBad "--dev parity broken (exit $($r.Exit)): $($r.Out.Trim())" }
    $r = Invoke-Argtest @('--control','https://cp.example.test')
    if ($r.Exit -eq 0 -and $r.Out -match 'ControlUrl=https://cp\.example\.test') { ItOk "--control <url> resolves the URL (parity)" }
    else { ItBad "--control <url> parity broken (exit $($r.Exit)): $($r.Out.Trim())" }
    $r = Invoke-Argtest @('--control','dev.waired.net')
    if ($r.Exit -eq 0 -and $r.Out -match 'ControlUrl=dev\.waired\.net') { ItOk "scheme-less --control host is accepted (install.sh parity; waired init normalises it)" }
    else { ItBad "scheme-less --control host rejected (exit $($r.Exit)): $($r.Out.Trim())" }
    $r = Invoke-Argtest @('--control','--dev')
    if ($r.Exit -ne 0 -and $r.Out -match 'stray flag') { ItOk "--control --dev (a flag as the value) dies loudly" }
    else { ItBad "--control --dev did not fail loudly (exit $($r.Exit)): $($r.Out.Trim())" }
    $r = Invoke-Argtest @('--frobnicate')
    if ($r.Exit -ne 0 -and $r.Out -match 'unknown argument') { ItOk "stray --frobnicate rejected loudly" }
    else { ItBad "stray arg not rejected (exit $($r.Exit)): $($r.Out.Trim())" }
    $r = Invoke-Argtest @('https://cp.example.test')
    if ($r.Exit -ne 0) { ItOk "bare positional URL rejected (no silent -Control mis-bind)" }
    else { ItBad "bare positional URL accepted (exit $($r.Exit)): $($r.Out.Trim())" }
    # Clean install (-Clean / --clean / WAIRED_CLEAN) wiring. The ARGTEST
    # seam returns before Confirm-CleanInstall / Invoke-CleanWipe, so these
    # assert flag resolution only -- no wipe, no UAC.
    $r = Invoke-Argtest @('--clean')
    if ($r.Exit -eq 0 -and $r.Out -match 'Clean=True') { ItOk "--clean resolves to -Clean (install.sh parity)" }
    else { ItBad "--clean parity broken (exit $($r.Exit)): $($r.Out.Trim())" }
    $r = Invoke-Argtest @('--clean','--check')
    if ($r.Exit -ne 0 -and $r.Out -match 'cannot be combined') { ItOk "--clean + --check rejected loudly" }
    else { ItBad "--clean + --check not rejected (exit $($r.Exit)): $($r.Out.Trim())" }
    $env:WAIRED_CLEAN = '1'
    try { $r = Invoke-Argtest @() } finally { Remove-Item Env:WAIRED_CLEAN -ErrorAction SilentlyContinue }
    if ($r.Exit -eq 0 -and $r.Out -match 'Clean=True') { ItOk "WAIRED_CLEAN env resolves to -Clean (piped iwr|iex form)" }
    else { ItBad "WAIRED_CLEAN env not resolved (exit $($r.Exit)): $($r.Out.Trim())" }

    if ($WithInference) {
        ItStep "running install.ps1 (-Dev -SkipInit -NonInteractive; engine installed later by the Tier-2 init)"
        & $installPs1 -Dev -SkipInit -NonInteractive
    } else {
        $env:WAIRED_NO_OLLAMA = '1'
        ItStep "running install.ps1 (-Dev -SkipOllama -SkipInit -NonInteractive)"
        & $installPs1 -Dev -SkipOllama -SkipInit -NonInteractive
    }
    if ($LASTEXITCODE -ne 0) { ItDie "install.ps1 exited $LASTEXITCODE" }

    ItStep "Tier 1 asserts"
    $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if ($svc) { ItOk "service '$ServiceName' registered" } else { ItBad "service '$ServiceName' not registered" }
    # The service may take a beat to reach Running after install starts it.
    for ($i = 0; $i -lt 15 -and $svc -and $svc.Status -ne 'Running'; $i++) { Start-Sleep 1; $svc.Refresh() }
    if ($svc -and $svc.Status -eq 'Running') { ItOk "service Running" } else { ItBad "service not Running (status=$($svc.Status))" }
    $startType = (Get-CimInstance Win32_Service -Filter "Name='$ServiceName'" -ErrorAction SilentlyContinue).StartMode
    if ($startType -match 'Auto') { ItOk "service start mode = $startType" } else { ItBad "service start mode = $startType (want Auto)" }

    if (Test-Path -LiteralPath (Join-Path $InstallDir 'waired.exe'))       { ItOk "waired.exe installed" }       else { ItBad "waired.exe missing in $InstallDir" }
    if (Test-Path -LiteralPath (Join-Path $InstallDir 'waired-agent.exe')) { ItOk "waired-agent.exe installed" } else { ItBad "waired-agent.exe missing in $InstallDir" }
    if (Test-Path -LiteralPath (Join-Path $InstallDir 'waired-tray.exe'))  { ItOk "waired-tray.exe installed (zip ships it, WAIRED_NO_TRAY unset)" } else { ItBad "waired-tray.exe missing in $InstallDir" }
    if (Test-Path -LiteralPath $StateDir) { ItOk "state dir present ($StateDir)" } else { ItBad "state dir missing ($StateDir)" }

    $machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine') -split ';'
    if ($machinePath -contains $InstallDir) { ItOk "InstallDir on machine PATH (#482)" } else { ItBad "InstallDir NOT on machine PATH (#482 regression)" }
}
catch {
    ItBad "Tier 1 threw: $($_.Exception.Message)"
}

# ============================================================================
# Tier 2: hands-free enroll + assert
# ============================================================================
if ($Tier -ge 2) {
    try {
        if ($EnrollMode -ne 'oidc') { ItDie "installtest-windows.ps1 supports IT_ENROLL_MODE=oidc only (got '$EnrollMode')" }
        if (-not $ImpersonateSa)    { ItDie "IT_ENROLL_MODE=oidc needs IT_IMPERSONATE_SA (the #339 test SA)" }

        ItStep "enrolling via OIDC grant (host-minted token)"
        # Stop the installer-started service so init's enroll writes identity
        # without daemon contention. init then starts the agent itself (default
        # --start-agent=true) — mirroring a real install — so #519's foreground
        # model wait runs; the Start-Service below is a redundant safety net.
        # Daemon-path mode is the exception: it leaves the service RUNNING —
        # that (unenrolled but reachable) is what makes init take the daemon
        # path and reach the setup executor engine install (waired#835 §11).
        if (-not $DaemonEngine) { Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue }

        $aud = (Invoke-RestMethod -Uri "$ControlUrl/v1/login/oidc-grant/audience" -TimeoutSec 15).audience
        if (-not $aud) { ItDie "could not resolve the OIDC audience from $ControlUrl/v1/login/oidc-grant/audience" }
        ItLog "minting SA id_token (sa=$ImpersonateSa)"
        $tok = (& gcloud auth print-identity-token --impersonate-service-account="$ImpersonateSa" --audiences="$aud" --include-email).Trim()
        if (-not $tok) { ItDie "failed to mint an SA id_token (is the CI principal in oidc_grant_token_creators on $ImpersonateSa?)" }

        $runId  = if ($env:GITHUB_RUN_ID) { $env:GITHUB_RUN_ID } else { Get-Date -Format yyyyMMddHHmmss }
        $device = "win-ci-$runId"
        $waired = Join-Path $InstallDir 'waired.exe'
        $initLog = Join-Path $Work 'init.log'
        if ($DaemonEngine) {
            # Daemon-path enrol: complete the login out-of-band so the resident
            # executor installs the engine (waired#835 §9/§11). No
            # --google-sa-login (that forces the standalone path); the running
            # service makes init take the daemon path. A background job rejoins
            # the in-flight session (POST /login/start is single-flight →
            # init's session), completes it via the OIDC grant (the CP flips any
            # waiting session), then watches the executor lease.
            $daemonFlag = Join-Path $Work 'daemon-engine.flag'
            $watcher = Start-Job -ScriptBlock {
                param($controlUrl, $tok, $initLog, $flag)
                $ErrorActionPreference = 'SilentlyContinue'
                Set-Content -LiteralPath $flag -Value '' -NoNewline
                # (1) Scrape the login session id from init's transcript (a READ:
                # POST /login/start is refused on TCP by the #838 writeGuard). The
                # session id is the login URL's last path segment (lastPathSegment).
                $sess = $null
                for ($i = 0; $i -lt 60 -and -not $sess; $i++) {
                    $txt = ''
                    try { $txt = Get-Content -LiteralPath $initLog -Raw -ErrorAction SilentlyContinue } catch { }
                    if ($txt -and $txt -match 'https?://\S+') {
                        $seg = (($Matches[0] -split '/')[-1] -split '[?#]')[0]
                        if ($seg) { $sess = $seg }
                    }
                    if (-not $sess) { Start-Sleep 1 }
                }
                if (-not $sess) { Add-Content -LiteralPath $flag -Value 'no-session'; return }
                Add-Content -LiteralPath $flag -Value "session=$sess"
                # (2) Complete out-of-band at the CP (no writeGuard there).
                try {
                    Invoke-RestMethod -Uri "$controlUrl/v1/login/oidc-grant" -Method Post -ContentType 'application/json' `
                        -Body (@{ login_session_id = $sess; id_token = $tok } | ConvertTo-Json -Compress) -TimeoutSec 20 | Out-Null
                    Add-Content -LiteralPath $flag -Value 'completed=1'
                } catch { Add-Content -LiteralPath $flag -Value 'complete-failed'; return }
                $seenExec = $false; $seenClaim = $false
                for ($i = 0; $i -lt 150; $i++) {
                    try {
                        $stt = Invoke-RestMethod -Uri 'http://127.0.0.1:9476/waired/v1/setup/state' -TimeoutSec 5
                        if (-not $seenExec  -and $stt.executor_attached)        { Add-Content -LiteralPath $flag -Value 'executor_attached=1'; $seenExec  = $true }
                        if (-not $seenClaim -and $stt.install_claimed -eq 'ollama') { Add-Content -LiteralPath $flag -Value 'install_claimed=ollama'; $seenClaim = $true }
                    } catch { }
                    Start-Sleep 2
                }
            } -ArgumentList $ControlUrl, $tok, $initLog, $daemonFlag

            # inference on + tiny model so an engine-less host installs one;
            # --non-interactive so the resident executor runs
            # ensureDaemonPathEngine. NO --google-sa-login → daemon path.
            $initArgs = @(
                'init'
                '--control', $ControlUrl
                '--device-name', $device
                '--inference-enabled=true'
                '--inference-bundled-model-id=qwen2.5-coder-0.5b-instruct'
                '--non-interactive'
                '--skip-integration'
                '--state-dir', $StateDir
            )
            $env:WAIRED_NO_EMOJI = '1'
            $prevEap = $ErrorActionPreference
            $ErrorActionPreference = 'Continue'
            & $waired @initArgs 2>&1 | Tee-Object -FilePath $initLog
            $initExit = $LASTEXITCODE
            $ErrorActionPreference = $prevEap
            Stop-Job $watcher -ErrorAction SilentlyContinue
            Receive-Job $watcher -ErrorAction SilentlyContinue | Out-Null
            Remove-Job $watcher -Force -ErrorAction SilentlyContinue
            if ($initExit -ne 0) { ItLog "daemon-path init exited $initExit -- asserts will surface what landed" }
        } else {
        $inferFlag = if ($WithInference) { '--inference-enabled=true' } else { '--inference-enabled=false' }
        # Build the whole init arg vector as ONE flat array and splat it once (matches
        # packaging/install/install.ps1's $initArgs idiom and the bash legs' initargs=(...)).
        # Do NOT build a separate $pinArgs via `if {@('x')} else {@()}` and splat it inline:
        # PowerShell unwraps a single-element array returned from an `if` into a *scalar
        # string*, and `@string` then splats character-by-character, feeding `waired init`
        # a lone leading "-" (cobra: unknown command "-"). See #613.
        $initArgs = @(
            'init'
            '--control', $ControlUrl
            '--google-sa-login'
            '--oidc-id-token', $tok
            '--device-name', $device
            '--non-interactive'
            $inferFlag
            '--skip-integration'
            '--state-dir', $StateDir
        )
        # Routing sentinel pins the tiny 0.5B as the bundled model (deploy pulls ~0.4 GB).
        if ($WithIntegration) { $initArgs += '--inference-bundled-model-id=qwen2.5-coder-0.5b-instruct' }
        # With -WithInference, init starts the agent and foreground-waits (#519)
        # while the agent pulls the bundled model into the :9475 engine, then runs
        # the end-of-init benchmark; tee for Assert-Inference. We let init own the
        # agent start (no --start-agent=false) so this exercises the real
        # ready-on-install path — #564.
        # Relax EAP around the native call: with 2>&1 + EAP=Stop, init's stderr
        # progress (model pull %, benchmark) can surface as a terminating
        # NativeCommandError. Tee-Object is a cmdlet, so $LASTEXITCODE reflects
        # waired.exe; we capture it before restoring EAP.
        # Keep the binary's enroll output ASCII (the install step above ran
        # with emoji enabled to exercise the banner; the other OS legs always
        # set this). CI stdout is non-TTY so waired falls back to ASCII anyway
        # -- this just makes the intent explicit and stable.
        $env:WAIRED_NO_EMOJI = '1'
        $prevEap = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        & $waired @initArgs 2>&1 | Tee-Object -FilePath $initLog
        $initExit = $LASTEXITCODE
        $ErrorActionPreference = $prevEap
        if ($initExit -ne 0) { ItBad "waired init (oidc) exited $initExit" }
        }

        # Safety net: init already started the agent (--start-agent default);
        # this is a no-op unless that best-effort start was skipped. Harmless in
        # daemon-path mode too (the service was never stopped).
        Start-Service -Name $ServiceName -ErrorAction SilentlyContinue

        ItStep "Tier 2 asserts"
        if (Test-Path -LiteralPath (Join-Path $StateDir 'identity.json')) { ItOk "identity.json written under $StateDir" }
        else { ItBad "identity.json missing under $StateDir" }

        # Tightened poll (waired#760): the old 25 x (TimeoutSec 5 + 1s) shape
        # burned up to ~2.5 min on a slow daemon. The mgmt API is loopback, so
        # a 1s per-request timeout is plenty; poll densely (250ms) at first —
        # init normally leaves the daemon already enrolled, so the common case
        # lands in the first second — then back off to 1s up to a 45s ceiling.
        $enrolled = $false
        $attempt  = 0
        $deadline = (Get-Date).AddSeconds(45)
        while ((Get-Date) -lt $deadline) {
            $attempt++
            try {
                $st = Invoke-RestMethod -Uri $MgmtStatus -TimeoutSec 1
                if ($st.device_id -match '^dev_') { $enrolled = $true; break }
            } catch { }
            Start-Sleep -Milliseconds $(if ($attempt -le 10) { 250 } else { 1000 })
        }
        if ($enrolled) { ItOk "daemon read the enrolled state and reports an identity" }
        else { ItBad "daemon did not report enrolled" }

        # Cheap and fast, so it runs before the minutes-long inference asserts.
        ItStep "management write pipe asserts (waired#838)"
        Assert-MgmtPipe

        if ($DaemonEngine) {
            ItStep "daemon-path executor engine-install asserts (waired#835 §9/§11)"
            Assert-DaemonEngine -InitLog $initLog -Flag $daemonFlag
        }
        elseif ($WithInference) {
            ItStep "inference asserts (-WithInference)"
            Assert-Inference -InitLog $initLog
        }

        if ($WithIntegration) {
            ItStep "coding-agent routing sentinel (-WithIntegration)"
            if (Get-Command go -ErrorAction SilentlyContinue) {
                # The Go harness drives each coding-agent leg at the real gateway
                # surface and asserts via the event ring that the completion was
                # served locally (no fail-open). It pulls + retries the tiny model
                # itself, tolerating a still-warming engine.
                $env:WAIRED_MGMT_URL   = 'http://127.0.0.1:9476'
                $env:WAIRED_TINY_ALIAS = 'waired/tiny'
                $env:WAIRED_STATE_DIR  = $StateDir
                Push-Location -LiteralPath $Root
                & go test -tags integration -count=1 -v ./internal/e2e/integration/...
                $goExit = $LASTEXITCODE
                Pop-Location
                if ($goExit -eq 0) { ItOk "coding-agent routing sentinel: every leg served locally (no fail-open)" }
                else { ItBad "coding-agent routing sentinel failed (go test exit $goExit)" }
            } else {
                ItBad "go toolchain not on PATH (needed to run the routing harness)"
            }
        }
    }
    catch {
        ItBad "Tier 2 threw: $($_.Exception.Message)"
    }
}

# ============================================================================
# Contract asserts (-Contract, waired#760) — behavioral user-visible contract,
# each tied to an open issue and soft-failing until its fix merges (ItSoft).
# Run after Tier 2 (enrolled daemon) and BEFORE any teardown.
# ============================================================================
if ($Contract) {
    try {
        $waired = Join-Path $InstallDir 'waired.exe'

        ItStep "contract asserts (waired#749/#751/#755) -- soft until each fix merges"

        # Relax EAP around the native calls below: they redirect stderr
        # (*>), and under EAP=Stop PS 5.1 turns redirected native stderr
        # into a terminating NativeCommandError (same trap as the Tier-2
        # init call). These commands are EXPECTED to fail while the issues
        # are open — their exit codes are the assert inputs.
        $prevEapContract = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'

        # (#751) `waired status` exits 0 in all three contexts the sv-evox2
        # dogfood hit. As of the #751 fix, when the per-user dir is empty
        # status falls back to the SYSTEM dir: elevated/admin reads it and
        # renders; a standard/basic-token user (whom the SYSTEM DACL denies)
        # gets an informational "enrolled system-wide, needs elevation" notice
        # -- both exit 0. Elevated first (baseline), then the two non-elevated
        # contexts.
        & $waired status *> (Join-Path $Work 'status-elevated.log')
        ItSoft '751' ($LASTEXITCODE -eq 0) "waired status exits 0 (elevated); got $LASTEXITCODE"

        $isSystem = ([Security.Principal.WindowsIdentity]::GetCurrent().User.Value -eq 'S-1-5-18')
        if ($isSystem) {
            ItLog "running as SYSTEM -- skipping non-elevated context asserts (CreateProcessWithLogonW unavailable)"
        } else {
            $r = Invoke-AsStandardUser -Exe $waired -ArgLine 'status' -Tag 'status-stduser'
            $first = (($r.Out -split "`r?`n") | Where-Object { $_ } | Select-Object -First 2) -join ' / '
            ItLog "standard-user status (exit $($r.Exit)): $first"
            ItSoft '751' ($r.Exit -eq 0) "waired status exits 0 as a standard user; got $($r.Exit)"

            $r = Invoke-AsBasicToken -Exe $waired -ArgLine 'status' -Tag 'status-basictoken'
            $first = (($r.Out -split "`r?`n") | Where-Object { $_ } | Select-Object -First 2) -join ' / '
            ItLog "basic-token status (exit $($r.Exit)): $first"
            ItSoft '751' ($r.Exit -eq 0) "waired status exits 0 under a filtered/basic token (runas /trustlevel:0x20000); got $($r.Exit)"
        }

        # (#749) `waired claude enable` must land managed-settings at the real
        # Windows path. As of the #749 fix an *elevated* `waired init` also
        # auto-enables (the eligibility gate now keys on an OS-aware elevation
        # predicate, not euid==0 which is -1 on Windows — cmd/waired/main.go +
        # internal/platform/elevation); this asserts the `enable` command path.
        & $waired claude enable --state-dir $StateDir *> (Join-Path $Work 'claude-enable.log')
        $claudeEnableExit = $LASTEXITCODE
        $ms = Join-Path $env:ProgramFiles 'ClaudeCode\managed-settings.json'
        $msOk = (Test-Path -LiteralPath $ms) -and
                ((Get-Content -LiteralPath $ms -Raw -ErrorAction SilentlyContinue) -match 'ANTHROPIC_BASE_URL')
        ItSoft '749' $msOk "waired claude enable (exit $claudeEnableExit) writes $ms with ANTHROPIC_BASE_URL"

        # (#755) the install path must surface the tray: an autostart
        # registration (HKCU Run value 'waired-tray') or a Start Menu group.
        # Surface-only assert — CI never launches the GUI process.
        $runVal = Get-ItemProperty -Path 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run' `
                    -Name 'waired-tray' -ErrorAction SilentlyContinue
        $smGroups = @(
            (Join-Path $env:ProgramData 'Microsoft\Windows\Start Menu\Programs\Waired'),
            (Join-Path $env:AppData     'Microsoft\Windows\Start Menu\Programs\Waired')
        ) | Where-Object { Test-Path -LiteralPath $_ }
        ItSoft '755' ([bool]$runVal -or [bool]$smGroups) "install surfaced the tray (HKCU Run 'waired-tray' or a Start Menu 'Waired' group)"

        $ErrorActionPreference = $prevEapContract
    }
    catch {
        $ErrorActionPreference = $prevEapContract
        ItBad "contract asserts threw: $($_.Exception.Message)"
    }
}

# --- teardown ---------------------------------------------------------------
# Bound the best-effort logout so it can't stall the runner. --revoke, not a
# plain logout: a revoked device frees its slot under the per-account device
# cap (#659); a plain logout leaves it counted (reauth_required).
$lj = Start-Job { param($exe, $sd) & $exe logout --revoke --yes --state-dir $sd 2>$null } `
      -ArgumentList (Join-Path $InstallDir 'waired.exe'), $StateDir
Wait-Job $lj -Timeout 10 | Out-Null
Remove-Job $lj -Force -ErrorAction SilentlyContinue | Out-Null

# With -Contract the teardown IS a test subject (waired#760): run the real
# uninstall.ps1 -Clean and assert what it leaves behind. Without -Contract,
# keep the historical behavior (no uninstall — the runner is disposable).
if ($Contract) {
    try {
        ItStep "teardown = uninstall.ps1 -Clean + asserts (waired#754 soft)"
        # Seed the GPU-backend machine env vars Set-MachineVulkanFlag writes on a
        # Vulkan/iGPU host. CI runners have no such GPU so the install leg never
        # sets them, which would make the clear-after-uninstall asserts below
        # vacuous -- seed them here so -Clean's Remove-Ollama is actually exercised.
        [Environment]::SetEnvironmentVariable('OLLAMA_VULKAN', '1', 'Machine')
        [Environment]::SetEnvironmentVariable('OLLAMA_IGPU_ENABLE', '1', 'Machine')
        & (Join-Path $Root 'packaging\install\uninstall.ps1') -Clean -Yes
        if ($LASTEXITCODE -ne 0) { ItBad "uninstall.ps1 -Clean exited $LASTEXITCODE" }

        # Hard asserts: uninstall's long-standing documented contract.
        if (-not (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue)) { ItOk "service unregistered" } else { ItBad "service still registered after uninstall" }
        if (-not (Test-Path -LiteralPath $InstallDir)) { ItOk "InstallDir removed" } else { ItBad "InstallDir remains after uninstall" }
        if (-not (Test-Path -LiteralPath $StateDir))   { ItOk "state dir wiped (-Clean)" } else { ItBad "state dir remains after -Clean" }
        if (([Environment]::GetEnvironmentVariable('Path', 'Machine') -split ';') -notcontains $InstallDir) { ItOk "machine PATH entry removed" } else { ItBad "machine PATH entry remains" }
        # (#45) -Clean clears the GPU-backend machine env vars Set-MachineVulkanFlag
        # wrote (seeded above), not just OLLAMA_MODELS.
        if (-not [Environment]::GetEnvironmentVariable('OLLAMA_VULKAN', 'Machine'))      { ItOk "OLLAMA_VULKAN cleared (-Clean)" }      else { ItBad "OLLAMA_VULKAN remains after -Clean" }
        if (-not [Environment]::GetEnvironmentVariable('OLLAMA_IGPU_ENABLE', 'Machine')) { ItOk "OLLAMA_IGPU_ENABLE cleared (-Clean)" } else { ItBad "OLLAMA_IGPU_ENABLE remains after -Clean" }

        # (#754) zero per-user / cross-surface artifacts. uninstall.ps1 -Clean now
        # runs `waired claude disable` + `waired unlink` for the invoking user (the
        # un-elevated parent phase) and deletes %APPDATA%\waired, so this sweep must
        # come up empty.
        $left = @()
        if (Test-Path -LiteralPath (Join-Path $env:AppData 'waired')) { $left += '%AppData%\waired' }
        if (Test-Path -LiteralPath "C:\Users\$TestUser\AppData\Roaming\waired") { $left += "test-user %AppData%\waired" }
        if (Get-ItemProperty -Path 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run' -Name 'waired-tray' -ErrorAction SilentlyContinue) { $left += "HKCU Run 'waired-tray'" }
        if (Test-Path -LiteralPath (Join-Path $env:ProgramFiles 'ClaudeCode\managed-settings.json')) { $left += 'ClaudeCode managed-settings.json' }
        $claudeSettings = Join-Path $env:USERPROFILE '.claude\settings.json'
        if ((Get-Content -LiteralPath $claudeSettings -Raw -ErrorAction SilentlyContinue) -match 'waired') { $left += '~/.claude/settings.json waired entry' }
        if (Get-ChildItem -LiteralPath (Join-Path $env:USERPROFILE '.claude\skills') -Filter '*waired*' -ErrorAction SilentlyContinue) { $left += '~/.claude/skills waired skill' }
        foreach ($g in @(
                (Join-Path $env:ProgramData 'Microsoft\Windows\Start Menu\Programs\Waired'),
                (Join-Path $env:AppData     'Microsoft\Windows\Start Menu\Programs\Waired'))) {
            if (Test-Path -LiteralPath $g) { $left += $g }
        }
        ItSoft '754' ($left.Count -eq 0) "uninstall.ps1 -Clean left artifacts: $(if ($left) { $left -join '; ' } else { '(none)' })"
    }
    catch {
        ItBad "uninstall teardown threw: $($_.Exception.Message)"
    }
}

# ============================================================================
# .exe-install variant (-ExeVariant, waired#760/#759): ISCC-compile the Inno
# installer from the SAME staged binaries, silent-install onto the now-clean
# machine (fresh-install path, not upgrade), Tier-1-level asserts, uninstall.
# Assert level tracks #759's phases: tier 1 now; no second enroll (the OIDC
# enroll already ran once, on the ps1 path).
# ============================================================================
if ($ExeVariant) {
    try {
        ItStep "ExeVariant: compiling the Inno installer (ISCC)"
        # Stage the .iss [Files] inputs exactly like reusable-build-artifacts.yml:
        # dist/windows-amd64/{waired,waired-agent,waired-tray}.exe + VERSION,
        # compiled with /DAppVersion (SourceDir=..\.., OutputDir=dist).
        $distDir = Join-Path $Root 'dist\windows-amd64'
        Remove-Item -LiteralPath $distDir -Recurse -Force -ErrorAction SilentlyContinue
        New-Item -ItemType Directory -Path $distDir -Force | Out-Null
        Copy-Item -Path (Join-Path $Stage '*') -Destination $distDir
        $iscc = 'iscc'
        if (-not (Get-Command iscc -ErrorAction SilentlyContinue)) {
            $iscc = Join-Path ${env:ProgramFiles(x86)} 'Inno Setup 6\ISCC.exe'
        }
        & $iscc "/DAppVersion=0.0.0-$ver" (Join-Path $Root 'packaging\windows\waired-setup.iss') | Select-Object -Last 3 | Out-Host
        if ($LASTEXITCODE -ne 0) { ItDie "ISCC exited $LASTEXITCODE" }
        $setup = Join-Path $Root "dist\WairedSetup-0.0.0-$ver-x64.exe"
        if (Test-Path -LiteralPath $setup) { ItOk "Inno installer compiled ($(Split-Path -Leaf $setup))" }
        else { ItDie "ISCC produced no installer at $setup" }

        ItStep "ExeVariant: silent install (/VERYSILENT)"
        # /MERGETASKS=!claudeproxy: uncheck the default-on claudeproxy task so
        # the [Run] `waired claude enable` step does not write machine-wide
        # managed-settings during this test install (the GUI installer is the
        # sole decider of routing in its own flow — there is no `waired init`
        # here). skipifsilent already suppresses the tray launch.
        $p = Start-Process -FilePath $setup -ArgumentList '/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART', '/MERGETASKS=!claudeproxy', "/LOG=$Work\innosetup.log" -Wait -PassThru
        if ($p.ExitCode -ne 0) { ItDie "WairedSetup exited $($p.ExitCode) (see $Work\innosetup.log)" }

        # A fresh Inno install registers the service but does NOT start it (a
        # real user gets it via `waired init` or the delayed-auto start after
        # reboot) — start it explicitly, then assert like Tier 1.
        Start-Service -Name $ServiceName -ErrorAction SilentlyContinue

        ItStep "ExeVariant: Tier-1-level asserts"
        $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
        if ($svc) { ItOk "service registered by the .exe installer" } else { ItBad "service not registered by the .exe installer" }
        for ($i = 0; $i -lt 15 -and $svc -and $svc.Status -ne 'Running'; $i++) { Start-Sleep 1; $svc.Refresh() }
        if ($svc -and $svc.Status -eq 'Running') { ItOk "service Running" } else { ItBad "service not Running (status=$($svc.Status))" }
        $startType = (Get-CimInstance Win32_Service -Filter "Name='$ServiceName'" -ErrorAction SilentlyContinue).StartMode
        if ($startType -match 'Auto') { ItOk "service start mode = $startType" } else { ItBad "service start mode = $startType (want Auto)" }
        foreach ($exe in 'waired.exe', 'waired-agent.exe', 'waired-tray.exe') {
            if (Test-Path -LiteralPath (Join-Path $InstallDir $exe)) { ItOk "$exe installed" } else { ItBad "$exe missing in $InstallDir" }
        }
        if (Test-Path -LiteralPath $StateDir) { ItOk "state dir present ($StateDir)" } else { ItBad "state dir missing ($StateDir)" }
        # NOTE: no machine-PATH assert here — waired-setup.iss intentionally
        # adds no PATH entry (that is install.ps1 behavior, #482).
        $smGroup = Join-Path $env:ProgramData 'Microsoft\Windows\Start Menu\Programs\Waired'
        if (Test-Path -LiteralPath $smGroup) { ItOk "Start Menu group created by the .exe installer" } else { ItBad "Start Menu group missing ($smGroup)" }

        ItStep "ExeVariant: uninstall (unins000.exe /VERYSILENT)"
        # Bounded by POLLING, not -Wait: the Inno uninstaller re-spawns itself
        # as a %TEMP% _iu*.tmp copy (the original exe exits early), and
        # PS 5.1's Start-Process -Wait waits on the whole descendant tree —
        # which is exactly what hung the first CI run for 28 min on the
        # (since fixed) unsuppressed wipe-state MsgBox in waired-setup.iss.
        # Completion signal = the service is unregistered.
        $unins = Join-Path $InstallDir 'unins000.exe'
        if (Test-Path -LiteralPath $unins) {
            Start-Process -FilePath $unins -ArgumentList '/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART' | Out-Null
            $deadline = (Get-Date).AddSeconds(120)
            while ((Get-Date) -lt $deadline -and (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue)) {
                Start-Sleep -Milliseconds 500
            }
            if (-not (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue)) { ItOk "Inno uninstall completed (service unregistered)" }
            else {
                Get-Process -Name '_iu*' -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
                ItBad "Inno uninstall did not complete within 120s (uninstaller killed)"
            }
        } else {
            ItBad "unins000.exe missing in $InstallDir"
        }
        # Silent uninstalls keep the state dir by design (waired-setup.iss);
        # sweep the residue — the guest is disposable.
        Remove-Item -LiteralPath $StateDir, $InstallDir -Recurse -Force -ErrorAction SilentlyContinue
        if (-not (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue)) { ItOk "service gone after Inno uninstall" } else { ItBad "service survived the Inno uninstall" }
    }
    catch {
        ItBad "ExeVariant threw: $($_.Exception.Message)"
    }
}

# --- final cleanup ------------------------------------------------------------
# The mirror job's HttpListener thread is blocked in a synchronous
# GetContext(), so a graceful Stop-Job would hang — force-remove.
Remove-Job $mirrorJob -Force -ErrorAction SilentlyContinue | Out-Null
# Contract-assert scratch: test user + profile + C:\Users\Public\waired-it.
# Best-effort — the guest is disposable; done AFTER the #754 asserts, which
# inspect the test user's %AppData%.
if ($Contract) {
    Remove-Item -LiteralPath $PubWork -Recurse -Force -ErrorAction SilentlyContinue
    if (Get-LocalUser -Name $TestUser -ErrorAction SilentlyContinue) {
        Get-CimInstance Win32_UserProfile -ErrorAction SilentlyContinue |
            Where-Object { $_.LocalPath -like "*\$TestUser" } |
            Remove-CimInstance -ErrorAction SilentlyContinue
        Remove-LocalUser -Name $TestUser -ErrorAction SilentlyContinue
    }
}

Write-Host ""
ItStep "Tier $Tier summary: $script:Pass passed, $script:Fail failed, $script:Warn warn (open-issue soft asserts)"
if ($script:Warn -gt 0) {
    $script:WarnLines | ForEach-Object { Write-Host "[installtest]   WARN $_" -ForegroundColor Yellow }
}
if ($script:Fail -gt 0) { exit 1 }
