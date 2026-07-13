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
#>
[CmdletBinding()]
param(
    [int]$Tier = 1,
    [switch]$WithInference,
    # -WithIntegration: after enroll, run the coding-agent routing sentinel
    # (#496). Implies inference but PINS the tiny 0.5B as the bundled model (so
    # the deploy pulls ~0.4 GB), then runs the Go harness that drives each leg at
    # the gateway surface and asserts served-locally via the event ring.
    [switch]$WithIntegration
)

# -WithIntegration rides the inference engine.
if ($WithIntegration) { $WithInference = $true }

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

# --- inference assert (Windows analog of assert_inference) -------------------
# Prove the Ollama-install -> bundled-model-pull -> benchmark tail of the
# first-run journey ran (Tier-2 -WithInference): install.ps1 installed Ollama
# (no -SkipOllama), and `waired init --inference-enabled=true` started the agent
# and (via #519's waitForBundledModel) blocked until the agent pulled the
# bundled model into the waired-owned engine on :9475, then ran the benchmark.
#
# #564: the bundled engine is waired-owned on :9475 with its own model store; the
# agent pulls there, NOT into the upstream Ollama default :11434. So readiness is
# asserted through the agent's mgmt API (/waired/v1/inference/status), the same
# source init's own foreground wait polls — never a bare `ollama list` (which
# queries :11434 and is always empty here, the original false negative).
function Assert-Inference {
    param([string]$InitLog)

    # 1) ollama.exe discoverable (mirror install.ps1's Test-OllamaInstalled order)
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
    else { ItBad "ollama engine not installed (install.ps1 should have, without -SkipOllama)" }

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

    # 4) benchmark figure in the init transcript (offerBenchmark throughput).
    #    Match the throughput OUTPUT only (tok/s | tokens/s | throughput) — NOT a
    #    bare "benchmark", which also appears in init's "run `waired runtimes
    #    benchmark` later" tip and would pass even when no benchmark actually ran
    #    (the old false positive surfaced once #564's start-agent fix lets a real
    #    benchmark run).
    if (Test-Path -LiteralPath $InitLog) {
        $txt = Get-Content -LiteralPath $InitLog -Raw
        if ($txt -match '(?i)tok/s|tokens/s|throughput') {
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
Set-Content -LiteralPath (Join-Path $Stage 'VERSION') -Value "0.0.0-$ver" -Encoding ASCII -NoNewline

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
    $env:WAIRED_NO_TRAY          = '1'
    # WAIRED_NO_EMOJI is intentionally NOT set for the install step so
    # install.ps1's rich (UTF-8) banner path runs here -- exercising the
    # Base64 art + Glyph/Utf8FromB64 runtime construction. A regression that
    # reintroduces literal non-ASCII source bytes (the iwr|iex mojibake) or
    # breaks glyph construction then fails this leg. Source-byte purity is
    # also guarded by scripts/install/encoding_test.go. It is reset to '1'
    # before the Tier-2 'waired init' so the binary's enroll output stays
    # ASCII, matching the macOS/Linux legs.
    $env:WAIRED_DEV_CONTROL_URL  = $ControlUrl

    # -WithInference (#514) installs Ollama (the -Dev URL above lets
    # Install-OllamaIfRequested run) so the Tier-2 model pull + benchmark have an
    # engine; the default path skips it (installer + enroll only). Pass the
    # switches inline per branch — array splat (@args) binds elements as
    # POSITIONAL args, not named switches, so install.ps1 would misread -Dev as
    # the control URL.
    $installPs1 = Join-Path $Root 'packaging\install\install.ps1'
    if ($WithInference) {
        ItStep "running install.ps1 (-Dev -SkipInit -NonInteractive; Ollama enabled for inference)"
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

        ItStep "enrolling via OIDC grant (google-sa-login, host-minted token)"
        # Stop the installer-started service so init's enroll writes identity
        # without daemon contention. init then starts the agent itself (default
        # --start-agent=true) — mirroring a real install — so #519's foreground
        # model wait runs; the Start-Service below is a redundant safety net.
        Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue

        $aud = (Invoke-RestMethod -Uri "$ControlUrl/v1/login/oidc-grant/audience" -TimeoutSec 15).audience
        if (-not $aud) { ItDie "could not resolve the OIDC audience from $ControlUrl/v1/login/oidc-grant/audience" }
        ItLog "minting SA id_token (sa=$ImpersonateSa)"
        $tok = (& gcloud auth print-identity-token --impersonate-service-account="$ImpersonateSa" --audiences="$aud" --include-email).Trim()
        if (-not $tok) { ItDie "failed to mint an SA id_token (is the CI principal in oidc_grant_token_creators on $ImpersonateSa?)" }

        $runId  = if ($env:GITHUB_RUN_ID) { $env:GITHUB_RUN_ID } else { Get-Date -Format yyyyMMddHHmmss }
        $device = "win-ci-$runId"
        $waired = Join-Path $InstallDir 'waired.exe'
        $inferFlag = if ($WithInference) { '--inference-enabled=true' } else { '--inference-enabled=false' }
        $initLog = Join-Path $Work 'init.log'
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

        # Safety net: init already started the agent (--start-agent default);
        # this is a no-op unless that best-effort start was skipped.
        Start-Service -Name $ServiceName -ErrorAction SilentlyContinue

        ItStep "Tier 2 asserts"
        if (Test-Path -LiteralPath (Join-Path $StateDir 'identity.json')) { ItOk "identity.json written under $StateDir" }
        else { ItBad "identity.json missing under $StateDir" }

        $enrolled = $false
        for ($i = 0; $i -lt 25; $i++) {
            try {
                $st = Invoke-RestMethod -Uri $MgmtStatus -TimeoutSec 5
                if ($st.device_id -match '^dev_') { $enrolled = $true; break }
            } catch { }
            Start-Sleep 1
        }
        if ($enrolled) { ItOk "daemon read the enrolled state and reports an identity" }
        else { ItBad "daemon did not report enrolled" }

        if ($WithInference) {
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

# --- teardown ---------------------------------------------------------------
# Bound the best-effort logout (a deregister network call) so it can't stall the
# billed runner, and force-remove the mirror job — its HttpListener thread is
# blocked in a synchronous GetContext(), so a graceful Stop-Job would hang.
$lj = Start-Job { param($exe) & $exe logout 2>$null } -ArgumentList (Join-Path $InstallDir 'waired.exe')
Wait-Job $lj -Timeout 20 | Out-Null
Remove-Job $lj        -Force -ErrorAction SilentlyContinue | Out-Null
Remove-Job $mirrorJob -Force -ErrorAction SilentlyContinue | Out-Null

Write-Host ""
ItStep "Tier $Tier summary: $script:Pass passed, $script:Fail failed"
if ($script:Fail -gt 0) { exit 1 }
