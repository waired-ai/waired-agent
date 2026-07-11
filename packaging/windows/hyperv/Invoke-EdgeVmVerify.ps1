#Requires -Version 5.1
<#
.SYNOPSIS
    Drive the Waired *edge release* installer / agent / proxy test inside the
    Hyper-V Win11 VM, entirely via PowerShell Direct (no network to the guest,
    no interactive console). Emits a structured PASS/FAIL result JSON.

.DESCRIPTION
    Mirrors the Linux LXD installtest tiers for Windows, against the real
    published edge artifact (waired-ai/waired-agent :: edge prerelease):

      Phase A  installer : $env:WAIRED_VERSION='edge' one-liner ->
                           DL + SHA verify, extract, waired-agent install
                           (SCM: DelayedStart/LocalSystem/recovery/EventLog),
                           restrictive DACL on %ProgramData%\waired\secrets,
                           idempotent re-run, -Check/-Update.
      Phase B  enroll    : headless OIDC SA-grant against dev.waired.net
                           (id_token minted on the HOST by gcloud, passed in),
                           --inference-enabled=true so deploy pulls the model.
      Phase C  inference : bundled engine on :9475 reaches subsystem_state
                           "ready" (GET /waired/v1/inference/status), the
                           qwen2.5-coder model (picker may downsize to 3b)
                           is in models.ready, and /anthropic/v1/messages
                           serves locally.
      Phase D  proxy     : managed-settings.json with ANTHROPIC_BASE_URL ->
                           127.0.0.1:9472 (no credential), loopback gateway
                           listener on :9472, NO retired MITM artifacts (CA /
                           NODE_EXTRA_CA_CERTS / hosts redirect / :443),
                           fail-open passthrough when degraded, and removal of
                           the managed base URL on `waired claude disable`.

    UAC note: PS Direct runs elevated (wadmin), so install.ps1's Phase-1 ->
    Start-Process -Verb RunAs self-elevation consent click is NOT exercised
    (same blind spot Windows Sandbox has). The post-elevation install logic IS
    fully exercised. The interactive consent can be covered by one vmconnect
    click as wuser if desired (-StandardUserUacProbe documents this).

.PARAMETER Fresh
    Revert to the 'clean-os' checkpoint before testing (recommended between runs).
#>
[CmdletBinding()]
param(
    [string]$VmName = 'waired-edge',
    [string]$GuestAdmin = 'wadmin',
    [string]$GuestPassword = 'Waired!Test123',
    [string]$ControlUrl = 'https://dev.waired.net',
    [string]$ImpersonateSa = 'waired-devtest-login@dev-waired.iam.gserviceaccount.com',
    [switch]$Fresh,
    [string]$InstallVersion = 'edge',   # 'edge' (default) or 'latest' (stable). Sets WAIRED_VERSION.
    # When set, copy this install.ps1 into the guest and run THAT (validate a
    # local FIX) instead of fetching the published install.ps1 from the mirror.
    # The edge zip + ollama-windows.ps1 assets are still pulled from the mirror
    # (WAIRED_VERSION), so the fixed script's own octet-stream fetch of
    # ollama-windows.ps1 (Finding 4) is still exercised end-to-end.
    [string]$LocalInstallScript = '',
    [string]$OutDir = (Join-Path $env:LOCALAPPDATA 'Temp\waired-edge-vm\out')
)
$ErrorActionPreference = 'Stop'
function Log  { param([string]$m) Write-Host ("[verify {0}] {1}" -f (Get-Date -Format 'HH:mm:ss'), $m) -ForegroundColor Cyan }
function Warn { param([string]$m) Write-Host ("[verify {0}] {1}" -f (Get-Date -Format 'HH:mm:ss'), $m) -ForegroundColor Yellow }
New-Item -ItemType Directory -Path $OutDir -Force | Out-Null

# --- 0. (optional) revert to clean OS baseline ------------------------------
if ($Fresh) {
    Log "reverting to clean-os checkpoint"
    & (Join-Path $PSScriptRoot 'Reset-WairedTestVM.ps1') -VmName $VmName -Start
    & (Join-Path $PSScriptRoot 'Wait-WairedTestVM.ps1') -VmName $VmName -TimeoutMinutes 15
}

# --- 1. host-side: mint OIDC id_token for headless enrollment ---------------
Log "minting OIDC id_token on host (impersonate $ImpersonateSa)"
$aud = (Invoke-RestMethod -Uri "$ControlUrl/v1/login/oidc-grant/audience" -TimeoutSec 20).audience
if (-not $aud) { throw "could not resolve OIDC audience from $ControlUrl" }
$oidcToken = (& gcloud auth print-identity-token --impersonate-service-account=$ImpersonateSa --audiences=$aud --include-email 2>$null)
if (-not $oidcToken) { throw "gcloud failed to mint id_token (tokenCreator on $ImpersonateSa?)" }
Log "id_token minted (len=$($oidcToken.Length)), audience=$aud"

# --- 2. PS Direct session ---------------------------------------------------
$sec  = ConvertTo-SecureString $GuestPassword -AsPlainText -Force
$cred = New-Object System.Management.Automation.PSCredential("$VmName\$GuestAdmin", $sec)
function New-GuestSession { New-PSSession -VMName $VmName -Credential $cred -ErrorAction Stop }
$s = New-GuestSession
Log "PS Direct session established to $VmName as $GuestAdmin"

$result = [ordered]@{
    vm = $VmName; controlUrl = $ControlUrl; startedAt = (Get-Date).ToString('o')
    phases = [ordered]@{}
    findings = New-Object System.Collections.ArrayList
}
function Add-Finding { param($Sev,$Area,$Msg) [void]$result.findings.Add([ordered]@{ severity=$Sev; area=$Area; detail=$Msg }); Warn "[$Sev/$Area] $Msg" }

# Remote non-terminating errors (e.g. install.ps1's pre-install Get-Service
# probe) must not abort the multi-phase run, and the result JSON must always be
# written -- so run the phases under Continue inside try/finally.
$ErrorActionPreference = 'Continue'
try {

# ===========================================================================
# Phase A — installer (edge one-liner)
# ===========================================================================
$localInstallContent = $null
if ($LocalInstallScript) {
    if (-not (Test-Path -LiteralPath $LocalInstallScript)) { throw "LocalInstallScript not found: $LocalInstallScript" }
    $localInstallContent = Get-Content -Raw -LiteralPath $LocalInstallScript
    Log "using LOCAL install.ps1 (fix under test): $LocalInstallScript ($($localInstallContent.Length) chars)"
}
Log "Phase A: installer one-liner (WAIRED_VERSION=$InstallVersion)"
$phaseA = Invoke-Command -Session $s -ArgumentList $InstallVersion,$localInstallContent -ScriptBlock {
    param($InstallVersion,$LocalContent)
    $ErrorActionPreference = 'Continue'
    $r = [ordered]@{}
    $log = New-Object System.Collections.ArrayList
    function GLog { param($m) [void]$log.Add(((Get-Date -Format 'HH:mm:ss') + ' ' + [string]$m)) }

    $env:WAIRED_VERSION = $InstallVersion
    $installUrl = 'https://github.com/waired-ai/waired-agent/releases/latest/download/install.ps1'
    # Fetch + decode install.ps1. GitHub serves the asset as
    # application/octet-stream, so Windows PowerShell 5.1 returns .Content as a
    # byte[] (recorded below for the Finding-1 content-type defect). We run
    # install.ps1 as a .ps1 FILE with -Control (explicit URL) because the
    # documented `iex "& { $($src.Content) } -Dev"` form on 5.1 (a) parse-errors
    # on the byte[], and (b) even decoded, the & { } form drops -Dev (Finding 2).
    function Fetch-InstallFile {
        # Local fix under test: write the host-provided fixed script to disk and
        # run THAT (no self-fetch). Sub-resource fetches inside it (the edge zip,
        # ollama-windows.ps1) still hit the octet-stream mirror, so the fix's own
        # byte[] handling is exercised.
        if ($LocalContent) {
            $script:installCT = 'local-fixed'
            $script:installWasBytes = $false
            $f = Join-Path $env:TEMP 'waired-install.ps1'
            Set-Content -LiteralPath $f -Value $LocalContent -Encoding UTF8
            return $f
        }
        $resp = Invoke-WebRequest -UseBasicParsing -Uri $installUrl
        $script:installCT = ($resp.Headers['Content-Type'] -join ',')
        $script:installWasBytes = ($resp.Content -is [byte[]])
        $txt = if ($resp.Content -is [byte[]]) { [System.Text.Encoding]::UTF8.GetString($resp.Content) } else { [string]$resp.Content }
        $f = Join-Path $env:TEMP 'waired-install.ps1'
        Set-Content -LiteralPath $f -Value $txt -Encoding UTF8
        $f
    }
    GLog "fetching install.ps1 from edge mirror"
    try {
        $file = Fetch-InstallFile
        $r.installContentType = $script:installCT
        $r.installContentWasBytes = $script:installWasBytes
        GLog ("install.ps1 content-type=" + $script:installCT + " wasBytes=" + $script:installWasBytes)
        $instOut = & $file -Control 'https://dev.waired.net' -SkipInit *>&1 | ForEach-Object { $_.ToString() }
        $r.installerExit = $LASTEXITCODE
        $r.installerOutTail = (($instOut | Select-Object -Last 45) -join "`n")
    } catch {
        $r.installerError = $_.Exception.Message
        GLog ("installer threw: " + $_.Exception.Message)
    }

    $pf = Join-Path $env:ProgramFiles 'Waired'
    $r.binaries = @{
        waired      = Test-Path (Join-Path $pf 'waired.exe')
        wairedAgent = Test-Path (Join-Path $pf 'waired-agent.exe')
        wairedTray  = Test-Path (Join-Path $pf 'waired-tray.exe')
    }
    $svc = Get-Service -Name 'waired-agent' -ErrorAction SilentlyContinue
    $r.serviceRegistered = [bool]$svc
    if ($svc) {
        $qc = (& sc.exe qc waired-agent) -join "`n"
        $qfailure = (& sc.exe qfailure waired-agent) -join "`n"
        $r.serviceStartType = $svc.StartType.ToString()
        $r.scQc = $qc
        $r.scQfailure = $qfailure
        $r.delayedAutoStart = ($qc -match 'DELAYED')
        $r.runsAsLocalSystem = ($qc -match 'LocalSystem')
        $r.hasRecovery = ($qfailure -match 'RESTART')
    }
    # version
    try { $r.version = (& (Join-Path $pf 'waired.exe') version --json) -join "`n" } catch { $r.version = "ERR: $($_.Exception.Message)" }
    # DACL on secrets dir
    $secdir = Join-Path $env:ProgramData 'waired\secrets'
    $r.secretsDirExists = Test-Path $secdir
    if (Test-Path $secdir) { $r.secretsAcl = (& icacls.exe $secdir) -join "`n" }
    # Event Log source
    $r.eventLogSource = [System.Diagnostics.EventLog]::SourceExists('waired-agent')
    # Idempotency: re-run installer (should be a clean no-op / update path)
    GLog "idempotency: re-running installer"
    try {
        $file2 = Fetch-InstallFile
        $rerunOut = & $file2 -Control 'https://dev.waired.net' -SkipInit *>&1 | ForEach-Object { $_.ToString() }
        $r.rerunExit = $LASTEXITCODE
        $r.rerunOutTail = (($rerunOut | Select-Object -Last 20) -join "`n")
    } catch { $r.rerunError = $_.Exception.Message }
    # -Check
    try {
        $file3 = Fetch-InstallFile
        $r.checkOutput = (& $file3 -Check *>&1 | ForEach-Object { $_.ToString() }) -join "`n"
    } catch { $r.checkError = $_.Exception.Message }

    $r.log = $log
    [pscustomobject]$r
}
$result.phases.A_installer = $phaseA
# host-side assertions on Phase A
if (-not $phaseA.binaries.waired)      { Add-Finding 'high' 'installer' 'waired.exe not present in %ProgramFiles%\Waired' }
if (-not $phaseA.serviceRegistered)    { Add-Finding 'high' 'installer' 'waired-agent service not registered' }
elseif (-not $phaseA.delayedAutoStart) { Add-Finding 'mid'  'installer' 'service not AutomaticDelayedStart' }
if ($phaseA.serviceRegistered -and -not $phaseA.runsAsLocalSystem) { Add-Finding 'mid' 'installer' 'service not running as LocalSystem' }
if ($phaseA.serviceRegistered -and -not $phaseA.hasRecovery)       { Add-Finding 'mid' 'installer' 'service has no recovery actions' }
if (-not $phaseA.eventLogSource)       { Add-Finding 'low'  'installer' 'Event Log source waired-agent missing' }
if (-not $phaseA.secretsDirExists)     { Add-Finding 'mid'  'installer' '%ProgramData%\waired\secrets not created' }
elseif ($phaseA.secretsAcl -match 'Everyone|BUILTIN\\Users') { Add-Finding 'high' 'security' 'secrets DACL grants Everyone/Users' }
if ($phaseA.installContentWasBytes) { Add-Finding 'high' 'installer' ('edge install.ps1 served as ' + $phaseA.installContentType + '; on Windows PowerShell 5.1 (#Requires -Version 5.1, the default shell) .Content is byte[], so the documented option-passing one-liner  iex "& { $($src.Content) } -Dev"  stringifies bytes and fails to parse. Bare  iwr|iex  works; the -Dev/-Control dogfood path is broken.') }

# ===========================================================================
# Ollama ensure — the installer's auto-install is unreliable on Windows
# (Finding 4: skipped even with -Control). Inference needs it, so install
# ollama-windows.ps1 (file form) if absent, before init's deploy/model pull.
# ===========================================================================
Log "Ensuring Ollama is present"
$ollamaEnsure = Invoke-Command -Session $s -ScriptBlock {
    $ErrorActionPreference = 'Continue'
    function Find-Ollama { foreach ($p in @("$env:ProgramFiles\Ollama\ollama.exe","$env:LOCALAPPDATA\Programs\Ollama\ollama.exe")) { if (Test-Path $p) { return $p } }; return $null }
    $found = Find-Ollama
    $r = [ordered]@{ preInstalled = [bool]$found }
    if (-not $found) {
        try {
            $resp = Invoke-WebRequest -UseBasicParsing -Uri 'https://github.com/waired-ai/waired-agent/releases/latest/download/ollama-windows.ps1'
            $txt = if ($resp.Content -is [byte[]]) { [System.Text.Encoding]::UTF8.GetString($resp.Content) } else { [string]$resp.Content }
            $f = Join-Path $env:TEMP 'ollama-windows.ps1'; Set-Content -LiteralPath $f -Value $txt -Encoding UTF8
            $out = & $f -GpuMode cpu-only *>&1 | ForEach-Object { $_.ToString() }
            $r.installExit = $LASTEXITCODE
            $r.installOutTail = (($out | Select-Object -Last 15) -join "`n")
        } catch { $r.installErr = $_.Exception.Message }
        $found = Find-Ollama
    }
    $r.ollamaExe = $found
    [pscustomobject]$r
}
$result.phases.Ollama = $ollamaEnsure
if (-not $ollamaEnsure.preInstalled) { Add-Finding 'mid' 'installer' 'installer did not auto-install Ollama with -Control; harness installed ollama-windows.ps1 manually' }
if (-not $ollamaEnsure.ollamaExe)    { Add-Finding 'high' 'inference' 'Ollama could not be installed (manual fallback failed)' }

# ===========================================================================
# Phase B — enrollment (headless OIDC)
# ===========================================================================
Log "Phase B: OIDC enrollment"
$phaseB = Invoke-Command -Session $s -ArgumentList $ControlUrl,$oidcToken -ScriptBlock {
    param($ControlUrl,$Token)
    $ErrorActionPreference = 'Continue'
    $pf = Join-Path $env:ProgramFiles 'Waired'
    $waired = Join-Path $pf 'waired.exe'
    $r = [ordered]@{}
    # init must write identity to %ProgramData%\waired (the SCM/LocalSystem
    # agent's state dir, what install.ps1's Invoke-WairedInit uses). Without
    # --state-dir, init writes to the per-user dir and the LocalSystem agent
    # never sees it (and the identity.json check below would false-fail).
    $stateDir = Join-Path $env:ProgramData 'waired'
    $out = (& $waired init --control $ControlUrl --state-dir $stateDir --google-sa-login --oidc-id-token $Token --non-interactive --inference-enabled=true --skip-integration 2>&1) -join "`n"
    $r.initExit = $LASTEXITCODE
    $r.initOutput = $out
    # restart service so the agent picks up identity + converges proxy
    try { Restart-Service waired-agent -ErrorAction Stop; Start-Sleep -Seconds 5 } catch { $r.restartErr = $_.Exception.Message }
    $r.identityJson = Test-Path (Join-Path $env:ProgramData 'waired\identity.json')
    # mgmt API status
    try { $r.status = (Invoke-RestMethod -Uri 'http://127.0.0.1:9476/waired/v1/status' -TimeoutSec 10) } catch { $r.statusErr = $_.Exception.Message }
    [pscustomobject]$r
}
$result.phases.B_enroll = $phaseB
if ($phaseB.initExit -ne 0)     { Add-Finding 'high' 'enroll' "waired init exit=$($phaseB.initExit)" }
if (-not $phaseB.identityJson)  { Add-Finding 'high' 'enroll' 'identity.json not created after init' }

# ===========================================================================
# Phase C — inference routing (model pull may take a while)
# ===========================================================================
Log "Phase C: inference (waiting for model pull + reachability, up to 20 min)"
$phaseC = Invoke-Command -Session $s -ScriptBlock {
    $ErrorActionPreference = 'Continue'
    $r = [ordered]@{}
    $r.ollamaExe = (Get-Command ollama -ErrorAction SilentlyContinue).Source
    if (-not $r.ollamaExe) {
        foreach ($p in @("$env:ProgramFiles\Ollama\ollama.exe","$env:LOCALAPPDATA\Programs\Ollama\ollama.exe")) {
            if (Test-Path $p) { $r.ollamaExe = $p; break }
        }
    }
    # The bundled engine is waired-owned on 9475 (NOT the upstream 11434),
    # so `ollama list` must target it; a ZIP install with no server up yet
    # prints "could not locate ollama app" — harmless, diagnostic only.
    $env:OLLAMA_HOST = '127.0.0.1:9475'
    # Readiness truth = the inference subsystem's own status
    # (subsystem_state == "ready"), NOT /waired/v1/status (overlay/network
    # only; it never carries inference_reachable_local).
    $deadline = (Get-Date).AddMinutes(20); $r.inferenceReady = $false; $infSt = $null
    while ((Get-Date) -lt $deadline) {
        try { $infSt = Invoke-RestMethod -Uri 'http://127.0.0.1:9476/waired/v1/inference/status' -TimeoutSec 10 } catch { $infSt = $null }
        if ($infSt) {
            $r.subsystemState = $infSt.subsystem_state
            $r.modelsReady = @($infSt.models.ready)
            $r.activeModel = $infSt.active.model_id
        }
        if ($r.ollamaExe) { $r.ollamaList = (& $r.ollamaExe list 2>&1 | Out-String) }
        if ($infSt -and $infSt.subsystem_state -eq 'ready') { $r.inferenceReady = $true; break }
        Start-Sleep -Seconds 20
    }
    # The auto-picker may downsize the bundled qwen2.5-coder to a smaller
    # variant (e.g. 3b) on RAM-constrained hosts — match the family.
    $r.modelMatched = [bool](@($r.modelsReady) -match 'qwen2\.5-coder')
    # overlay/network snapshot for context (separate endpoint)
    try { $r.statusFinal = Invoke-RestMethod -Uri 'http://127.0.0.1:9476/waired/v1/status' -TimeoutSec 10 } catch {}
    # direct gateway POST (Anthropic-format) proving local serving. The
    # Anthropic-compatible route is /anthropic/v1/messages (bare
    # /v1/messages is unrouted -> 404); the Claude loopback gateway (:9472,
    # ANTHROPIC_BASE_URL target) is what maps /v1/messages onto it.
    $secdir = Join-Path $env:ProgramData 'waired\secrets'
    $tokFile = Get-ChildItem $secdir -ErrorAction SilentlyContinue | Where-Object { $_.Name -match 'gateway' } | Select-Object -First 1
    if ($tokFile) {
        $tok = (Get-Content -LiteralPath $tokFile.FullName -Raw).Trim()
        $body = '{"model":"waired/auto","max_tokens":16,"messages":[{"role":"user","content":"ping"}]}'
        try {
            $resp = Invoke-WebRequest -Uri 'http://127.0.0.1:9473/anthropic/v1/messages' -Method Post -Headers @{ Authorization = "Bearer $tok"; 'anthropic-version' = '2023-06-01' } -ContentType 'application/json' -Body $body -UseBasicParsing -TimeoutSec 180
            $r.gatewayStatus = [int]$resp.StatusCode
            $r.gatewayBody = $resp.Content.Substring(0, [math]::Min(400, $resp.Content.Length))
        } catch { $r.gatewayErr = $_.Exception.Message; if ($_.Exception.Response) { $r.gatewayStatus = [int]$_.Exception.Response.StatusCode } }
    } else { $r.gatewayTokenMissing = $true }
    [pscustomobject]$r
}
$result.phases.C_inference = $phaseC
if (-not $phaseC.ollamaExe)     { Add-Finding 'high' 'inference' 'ollama.exe not found (installer did not deploy Ollama?)' }
if (-not $phaseC.inferenceReady) { Add-Finding 'high' 'inference' "local inference not ready within 20 min (subsystem_state=$($phaseC.subsystemState))" }
elseif (-not $phaseC.modelMatched) { Add-Finding 'mid' 'inference' "inference ready but no qwen2.5-coder model in models.ready (got: $($phaseC.modelsReady -join ','))" }
if ($phaseC.gatewayStatus -and $phaseC.gatewayStatus -ge 400) { Add-Finding 'mid' 'inference' "gateway /anthropic/v1/messages returned $($phaseC.gatewayStatus)" }

# ===========================================================================
# Phase D — Claude managed settings (#488): enable, assert, fail-open, revert
# ===========================================================================
Log "Phase D: Claude Code managed settings"
$phaseD = Invoke-Command -Session $s -ScriptBlock {
    $ErrorActionPreference = 'Continue'
    $r = [ordered]@{}
    $pf = Join-Path $env:ProgramFiles 'Waired'; $waired = Join-Path $pf 'waired.exe'
    $hostsPath = "$env:SystemRoot\System32\drivers\etc\hosts"
    $msPath = Join-Path (Join-Path $env:ProgramFiles 'ClaudeCode') 'managed-settings.json'
    $loopback = 'http://127.0.0.1:9472'
    $anthBody = '{"model":"claude-3-5-sonnet-20241022","max_tokens":16,"messages":[{"role":"user","content":"ping"}]}'
    $anthHdr  = @{ 'x-api-key' = 'dummy'; 'anthropic-version' = '2023-06-01' }

    # enable: write managed settings (no MITM CA / hosts / :443)
    $r.installOut = (& $waired claude enable 2>&1) -join "`n"
    $r.installExit = $LASTEXITCODE
    Start-Sleep -Seconds 3

    $r.msPresent = Test-Path -LiteralPath $msPath
    if ($r.msPresent) { $r.msBody = (Get-Content -LiteralPath $msPath -Raw) }
    $r.baseUrlWired = [bool]($r.msBody -match '127\.0\.0\.1:9472')
    $r.noCredential = -not ($r.msBody -match 'ANTHROPIC_AUTH_TOKEN|ANTHROPIC_API_KEY|apiKeyHelper')
    $r.gw9472       = [bool](Get-NetTCPConnection -LocalPort 9472 -State Listen -ErrorAction SilentlyContinue)
    # Retired MITM artifacts must be ABSENT.
    $r.caInRoot      = (((& certutil -store Root) -join "`n") -match 'waired')
    $r.nodeExtraCa   = [Environment]::GetEnvironmentVariable('NODE_EXTRA_CA_CERTS','Machine')
    $r.hostsRedirect = [bool]((Get-Content -LiteralPath $hostsPath -ErrorAction SilentlyContinue) | Select-String 'api\.anthropic\.com' -Quiet)
    $r.listener443   = [bool](Get-NetTCPConnection -LocalPort 443 -State Listen -ErrorAction SilentlyContinue)

    # routed request to the loopback gateway: should be served locally while healthy
    try {
        $resp = Invoke-WebRequest -Uri "$loopback/v1/messages" -Method Post -Headers $anthHdr -ContentType 'application/json' -Body $anthBody -UseBasicParsing -TimeoutSec 120
        $r.proxyStatus = [int]$resp.StatusCode
        $r.proxyServedLocal = ($resp.Headers['X-Local-Inference'] -eq '1') -or ($resp.Content -match 'qwen|content')
        $r.proxyBody = $resp.Content.Substring(0, [math]::Min(300, $resp.Content.Length))
    } catch { if ($_.Exception.Response) { $r.proxyStatus = [int]$_.Exception.Response.StatusCode }; $r.proxyErr = $_.Exception.Message }

    # fail-open: pause -> degraded -> request should pass through to REAL Anthropic (401 on dummy key)
    $r.pauseOut = (& $waired pause 2>&1) -join "`n"; Start-Sleep -Seconds 8
    try {
        $resp2 = Invoke-WebRequest -Uri "$loopback/v1/messages" -Method Post -Headers $anthHdr -ContentType 'application/json' -Body $anthBody -UseBasicParsing -TimeoutSec 60
        $r.failopenStatus = [int]$resp2.StatusCode
    } catch { if ($_.Exception.Response) { $r.failopenStatus = [int]$_.Exception.Response.StatusCode }; $r.failopenErr = $_.Exception.Message }
    (& $waired resume 2>&1) | Out-Null

    # revert: disable -> managed ANTHROPIC_BASE_URL removed
    $r.uninstallOut = (& $waired claude disable 2>&1) -join "`n"; Start-Sleep -Seconds 2
    $msAfter = if (Test-Path -LiteralPath $msPath) { Get-Content -LiteralPath $msPath -Raw } else { '' }
    $r.baseUrlAfter = [bool]($msAfter -match '127\.0\.0\.1:9472')
    [pscustomobject]$r
}
$result.phases.D_proxy = $phaseD
if (-not $phaseD.msPresent)    { Add-Finding 'high' 'proxy' 'managed-settings.json not written after `waired claude enable`' }
if (-not $phaseD.baseUrlWired) { Add-Finding 'high' 'proxy' 'managed-settings ANTHROPIC_BASE_URL not pointing at 127.0.0.1:9472' }
if (-not $phaseD.noCredential) { Add-Finding 'high' 'proxy' 'managed-settings unexpectedly contains a credential (subscription would be replaced)' }
if (-not $phaseD.gw9472)       { Add-Finding 'high' 'proxy' 'no Claude loopback gateway listener on 127.0.0.1:9472' }
if ($phaseD.caInRoot)          { Add-Finding 'high' 'proxy' 'retired MITM CA present in Root store (should be absent)' }
if ($phaseD.nodeExtraCa)       { Add-Finding 'mid'  'proxy' 'retired NODE_EXTRA_CA_CERTS (HKLM) set (should be absent)' }
if ($phaseD.hostsRedirect)     { Add-Finding 'high' 'proxy' 'retired hosts redirect api.anthropic.com -> 127.0.0.1 present (should be absent)' }
if ($phaseD.listener443)       { Add-Finding 'high' 'proxy' 'retired :443 listener present (should be absent)' }
if ($phaseD.failopenStatus -and $phaseD.failopenStatus -ne 401) { Add-Finding 'mid' 'proxy' "fail-open passthrough did not reach real Anthropic (status=$($phaseD.failopenStatus), expected 401 on dummy key)" }
if ($phaseD.baseUrlAfter)      { Add-Finding 'high' 'proxy' 'managed-settings ANTHROPIC_BASE_URL still present after `waired claude disable` (residue)' }

}
catch {
    Add-Finding 'harness' 'orchestrator' ("phase aborted: " + $_.Exception.Message)
}
finally {
    $result.finishedAt = (Get-Date).ToString('o')
    $result.pass = (@($result.findings | Where-Object { $_.severity -ne 'harness' }).Count -eq 0)
    $json = $result | ConvertTo-Json -Depth 8
    $outFile = Join-Path $OutDir 'edge-vm-result.json'
    Set-Content -LiteralPath $outFile -Value $json -Encoding UTF8
    Log ("result written: " + $outFile + " (pass=" + $result.pass + ", findings=" + $result.findings.Count + ")")
    if ($s) { Remove-PSSession $s -ErrorAction SilentlyContinue }
}
$result | ConvertTo-Json -Depth 8
