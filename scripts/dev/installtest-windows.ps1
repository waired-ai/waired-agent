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
      hands-free — gcloud (WIF) mints the SA id_token (#339), exchanges it for
      a reusable auth key at the CP's dev issuer (waired#976), then
      `waired init --auth-key <key>` enrols THROUGH the running service (#175).
      Asserts identity
      lands under %ProgramData%\waired and the daemon reports it on the mgmt API.

    Designed to run directly on a disposable runner (no nesting). Mirrors the
    enroll knobs of lib/installtest-enroll.sh: IT_ENROLL_MODE (only `authkey`
    supported here), IT_IMPERSONATE_SA, IT_CONTROL_URL.

.PARAMETER Tier
    1 = install + service asserts; 2 = + hands-free enroll. Default 1.

.PARAMETER WithInference
    Pairs with -Tier 2 (#514): enroll with --inference-enabled=true, so init
    installs the bundled engine itself (since #138 install.ps1 puts no engine on
    the host; -SkipOllama is how you tell init not to). init starts the agent
    and, via #519's foreground wait, blocks until the agent has pulled the
    bundled model into the waired-owned engine on :9475, then runs the
    end-of-init benchmark. Asserts: the engine is waired's own under the state
    dir AND is what serves, at the pin (#494); the bundled model reaches `ready`
    in the waired-owned store (queried through the agent mgmt API at :9476
    /waired/v1/inference/status, NOT a bare `ollama list` which targets the
    upstream :11434 store the bundled engine does not use — see #564); inference
    enabled in the persisted config; and a benchmark figure in the init
    transcript (the Windows analog of lib/installtest-enroll.sh's
    assert_inference).

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
    # (#496). Implies inference but PINS the withheld 350M as the bundled model
    # (so the deploy pulls ~0.7 GB), then runs the Go harness that drives each leg at
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
    # -- the path an auth-key enrol never reaches, because the key settles the
    # login inside the create call and leaves the executor lease nothing to
    # observe. Keeps install.ps1's engine-absent state,
    # completes the daemon login out-of-band via the OIDC grant, and asserts the
    # engine landed via the executor (not install.ps1). Its own mode; Tier 2.
    #
    # Two inits, on purpose (waired-agent#551): the first still has
    # WAIRED_NO_OLLAMA set and must exit 0 having installed nothing -- the only
    # end-to-end coverage of the executor's opt-out arm anywhere in CI -- and
    # the second, after the variable is cleared, is where the engine install
    # being asserted actually happens.
    [switch]$DaemonEngine,
    # -EngineOnly (waired-agent#590): install the AI software and answer the
    # model picker with "don't download a model now", then assert that state is
    # a FINISHED install -- exit 0, an engine on disk, and a standing choice the
    # daemon keeps across a restart. Its own mode; Tier 2. The Windows twin of
    # installtest-run.sh's --engine-only.
    #
    # Its single init is INTERACTIVE, which is what keeps it out of every other
    # mode: they all pass --non-interactive, and runInitModelPicker returns on
    # that flag before it asks anything.
    [switch]$EngineOnly
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
if ($EngineOnly -and ($WithInference -or $WithIntegration -or $DaemonEngine)) {
    Write-Host "[installtest] -EngineOnly is its own mode; not with -WithInference/-WithIntegration/-DaemonEngine" -ForegroundColor Red
    exit 1
}
if ($EngineOnly -and $Tier -lt 2) {
    Write-Host "[installtest] -EngineOnly requires -Tier 2 (it enrolls before it asks about models)" -ForegroundColor Red
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
$EnrollMode   = if ($env:IT_ENROLL_MODE) { $env:IT_ENROLL_MODE } else { 'authkey' }
$ImpersonateSa= $env:IT_IMPERSONATE_SA
$MgmtStatus   = 'http://127.0.0.1:9476/waired/v1/status'

$Work         = Join-Path ([System.IO.Path]::GetTempPath()) 'waired-installtest-win'
$Stage        = Join-Path $Work 'stage'
$Mirror       = Join-Path $Work 'mirror'

# Failure lines `waired init` prints when an engine install did not succeed.
# A leg whose transcript contains one of these FAILED, whatever else it can
# still find on disk -- #178 printed the exact reason into CI logs for five
# straight days while the leg said `ok  ollama engine installed`.
#
# Mirror of lib/installtest-enroll.sh's IT_INSTALL_FAILURE_RE. Kept as one
# alternation per harness so scripts/ci/harness-failure-strings-guard.sh can
# check the three copies agree and that every branch still exists in the
# product source (cmd/waired/init_engine.go, cmd/waired/setup_install.go) --
# a grep for wording the product stopped printing is green forever.
$InstallFailureRe = 'Engine install failed:|vLLM install failed:'

# Mirrors of lib/installtest-enroll.sh's engine-opt-out pair
# (waired-agent#551) -- see the comment there for why the second one needs the
# guard more than the first. Same guard checks these three copies agree.
$EngineOptOutRe = 'Engine install skipped (WAIRED_NO_OLLAMA)'
$InstallFailureBoxRe = 'The AI engine could not be installed on this device'

# Lines `waired init` prints when the benchmark did not run because the MODEL
# was not ready -- not because anything is broken (#382). Mirror of
# lib/installtest-enroll.sh's IT_BENCH_NOT_READY_RE; see the comment there for
# which Go file prints each branch. Same guard checks the three copies agree.
$BenchNotReadyRe = 'Model not ready in time|Model download failed|Model still downloading'

# Mirrors of lib/installtest-enroll.sh's step-4 default / models-pull pair
# (waired-agent#590) -- see the comments there. The guard checks the first two
# agree across the three harnesses and still exist in the product.
#
# $PullQueuedRe / $PullReachedRe are NOT guarded and NOT shaped like their .sh
# counterparts: the .sh side reads `queued pull:|cannot download` as one ERE
# alternation, and PowerShell matches these with -SimpleMatch (a .NET regex
# would misread the shared literals), so Windows keeps them as two literals
# and ORs the two reads. Neither string is literally searchable in the product
# either -- `queued pull:` has a %s after it and `cannot download` is a
# wrapped error -- and a guard entry that can never pass is worse than none.
#
# One space around each `=`, not aligned columns: the guard reads these
# declarations with `sed -n "s/^\$<name> = '\(...\)'"`, so padding makes the
# value invisible to it and the check reports "no alternation found".
$UnfitSkipRe = 'Non-interactive: skipping local AI'
$PullDeclineRe = 'Not downloading. Re-run with --yes --force to download it anyway.'
$PullQueuedRe = 'queued pull:'
$PullReachedRe = 'cannot download'
# Mirror of lib/installtest-enroll.sh's IT_STATUS_FIELDS_RE (waired-agent#573)
# -- the inference-status field names Get-ModelReadyState reads. Same guard
# checks these three copies agree and that the product still publishes them.
$StatusFieldsRe = 'no_model_selected|host_speed|probe_model_id|turn_floor_seconds'
# Mirror of lib/installtest-enroll.sh's IT_DAEMON_EVIDENCE_RE
# (waired-agent#579) -- the daemon-log lines the not-ready dump greps for. See
# the comment there for why the host-speed group belongs in a dump that used to
# be pull-side only, why 'api/pull' is appended at the use site rather than
# living in the alternation, and what the last two branches are for
# (waired-agent#642).
$DaemonEvidenceRe = 'boot pre-pull|bundled model|host speed|host cutoff|below the recommended spec|measuring whether this host|engine log truncated at cap|no engine logs found'

# Mirror of lib/installtest-enroll.sh's IT_NO_MODEL_RE (waired-agent#586/#590)
# -- see the comment there, including why only the ASCII head of the product's
# line is matched. Guarded: the three harnesses must agree and the product must
# still print it. One space around the `=`, for the reason above.
$NoModelRe = 'No model selected'

# --- logging / assert counters ----------------------------------------------
# All three declared together, above the helpers: ItDie prints the tally, so
# every counter it reads must exist before any function can be called.
$script:Pass = 0
$script:Fail = 0
$script:Skip = 0
function ItStep { param([string]$m) Write-Host "[installtest] ==> $m" -ForegroundColor Green }
function ItLog  { param([string]$m) Write-Host "[installtest] $m" -ForegroundColor Cyan }
function ItOk   { param([string]$m) Write-Host "[installtest]  ok  $m" -ForegroundColor Green; $script:Pass++ }
function ItBad  { param([string]$m) Write-Host "[installtest] FAIL $m" -ForegroundColor Red; $script:Fail++ }
# A die counts as a failure and prints the tally before exiting. Without that
# it printed one line and exited straight past the summary, so a leg that died
# mid-run and a leg that failed an assert produced the same red job with none
# of the same evidence (#505). FAIL-prefixed so a die lands in the same grep as
# every other failure. The assert-count floor is deliberately NOT run here: its
# question is already answered by the die's own reason.
function ItDie  {
    param([string]$m)
    Write-Host "[installtest] FAIL $m" -ForegroundColor Red
    $script:Fail++
    Write-Host ""
    Write-Host ("[installtest] ==> Tier {0} summary (died before finishing): {1} passed, {2} failed, {3} skipped" -f $Tier, $script:Pass, $script:Fail, $script:Skip) -ForegroundColor Green
    exit 1
}
# ItSkip -- an assert deliberately not run, WITH its reason. Counted and
# printed in the summary. The SYSTEM branch below used to take this path
# through ItLog, which moves no counter at all: the two asserts it covers
# simply stopped existing and the leg still reported success (#215).
$script:SkipLines = @()
function ItSkip { param([string]$m)
    Write-Host "[installtest] SKIP $m" -ForegroundColor Yellow
    $script:Skip++; $script:SkipLines += $m
}

# --- contract asserts (waired#760): soft-fail while the underlying issue is
# open. When a fix merges, its PR flips the ONE matching line below to $true
# and the assert becomes blocking from then on.
$script:ContractBlocking = @{
    '749' = $true    # waired#749: `waired claude enable` writes managed-settings on Windows (FIXED)
    '751' = $true    # waired#751: `waired status` exits 0 in non-elevated contexts (FIXED)
    '754' = $true    # waired#754: uninstall.ps1 -Clean leaves zero per-user artifacts (FIXED)
    '755' = $true    # waired#755: the install path surfaces the tray (Start Menu group / autostart) (FIXED)
    '838' = $true    # waired#838: management writes travel over the local named pipe, not TCP (FIXED)
    '313' = $true    # waired-agent#313: `waired init` on an enrolled device resumes instead of failing (FIXED)
    '315' = $true    # waired#315: SCM recovery actions also fire on a non-crash failure exit (FIXED)
    '579' = $true    # waired-agent#579: the host-speed measurement reaches a verdict inside init's window (FIXED)
    '660' = $true    # waired-agent#660: uninstall verifies its own deletes instead of reporting success over them (FIXED)
    '630' = $true    # waired-agent#630: uninstall.ps1 existence-gates its steps the way uninstall.sh does (FIXED)
}
$script:Warn = 0
$script:WarnLines = @()
function ItSoft {
    # Repo names which tracker the issue lives in: the contract asserts
    # started as monorepo-only, and an agent-repo number rendered as
    # "waired#313" points at an unrelated issue.
    param([string]$Issue, [bool]$Ok, [string]$m, [string]$Repo = 'waired')
    $ref = "$Repo#$Issue"
    if ($Ok) { ItOk "$m ($ref)"; return }
    if ($script:ContractBlocking[$Issue]) {
        ItBad "$m ($ref fix merged -- blocking)"
    } else {
        Write-Host "[installtest] WARN $m ($ref open -- soft)" -ForegroundColor Yellow
        $script:Warn++
        $script:WarnLines += "${ref}: $m"
    }
}

# --- bundled-engine assert (#493) --------------------------------------------
# The engine is the one waired installed, at the one path the daemon will
# serve from.
#
# This replaces Assert-EngineComplete, which asserted the two side effects the
# old %ProgramFiles%\Ollama layout needed to be complete: a .waired-managed.json
# marker (the completion receipt #190 added, and the only way to tell waired's
# install from the user's own at a shared path) and a machine PATH entry. Both
# went with the layout. Under the state dir the path IS the identity, and the
# agent spawns the engine by absolute path, so there is nothing to put on PATH.
#
# What is asserted instead is stronger: an engine ANYWHERE ELSE is now a
# failure, not a pass. That is the #139 bar — a pre-installed Ollama could turn
# this leg green while the daemon served through software waired never
# installed.
function Assert-BundledEngine {
    param([string]$Context)

    $bin = Join-Path $StateDir 'runtimes\ollama\bin\ollama.exe'
    if (Test-Path -LiteralPath $bin) {
        ItOk "bundled ollama installed under the state dir ($Context): $bin"
    } else {
        ItBad "no bundled ollama at $bin ($Context) -- the install did not land where the daemon looks"
        Get-ChildItem -LiteralPath (Join-Path $StateDir 'runtimes') -Recurse -Depth 2 -ErrorAction SilentlyContinue |
            Select-Object -First 20 | ForEach-Object { Write-Host "    $($_.FullName)" }
        return
    }

    # The runners came out of the archive with it. The Windows release keeps
    # them in lib\ollama\ beside ollama.exe, so an extract that placed the
    # binary but scattered the rest would still look installed here and fail at
    # the first inference instead.
    $server = Join-Path $StateDir 'runtimes\ollama\bin\lib\ollama\llama-server.exe'
    if (Test-Path -LiteralPath $server) {
        ItOk "the engine's runner is beside it ($Context): lib\ollama\llama-server.exe"
    } else {
        ItBad "llama-server.exe missing under $bin's lib\ollama -- the archive did not extract intact"
    }

    # Nothing installed into %ProgramFiles%\Ollama, and nothing left on the
    # machine PATH. On a disposable runner both can only be this run's doing.
    $legacyDir = Join-Path $env:ProgramFiles 'Ollama'
    if (-not (Test-Path -LiteralPath $legacyDir)) {
        ItOk "no %ProgramFiles%\Ollama ($Context) -- waired installs nothing there since #493"
    } else {
        ItBad "%ProgramFiles%\Ollama exists on a fresh runner ($Context) -- something still installs there"
    }
    $machinePath = [Environment]::GetEnvironmentVariable('PATH', 'Machine')
    if ((($machinePath -split ';') | Where-Object { $_ -eq $legacyDir }).Count -eq 0) {
        ItOk "machine PATH untouched ($Context) -- the agent spawns the engine by absolute path"
    } else {
        ItBad "machine PATH still carries $legacyDir ($Context)"
    }
}

# --- serving-engine assert (#494) --------------------------------------------
# The engine ANSWERING REQUESTS is waired's own, at the pinned version. Twin of
# lib/installtest-enroll.sh's assert_serving_ollama; that function carries the
# reasoning for all three asserts. Mirror any change there and in
# installtest-macos.sh (assert_serving_ollama_macos).
#
# Assert-BundledEngine above stats a file. This is a different claim, and the
# gap between the two is the whole of #139: a host can hold waired's binary at
# the right path and still be served by something else.
#
# The Windows-only part is how the listener is resolved -- Get-NetTCPConnection
# for the owning pid, Win32_Process for its image path (there is no /proc, and
# Get-Process .Path throws on a process owned by SYSTEM). The runner is
# permanently Administrator, so the CIM query sees the LocalSystem-owned
# engine.
function Assert-ServingEngine {
    param([string]$Context)

    $bin = Join-Path $StateDir 'runtimes\ollama\bin\ollama.exe'

    # No engine on disk means nothing can be serving -- see the Linux twin
    # (lib/installtest-enroll.sh) for why this comes before the poll. This is
    # the leg it was found on: the executor never attached (#505), no engine
    # was ever installed, and the poll spent 180 s to report "installed but
    # not answering" one line under an assert saying it was not installed.
    if (-not (Test-Path -LiteralPath $bin)) {
        ItBad "nothing can be serving on :9475 ($Context): no engine at $bin"
        return
    }

    # The engine is normally up already -- the -WithInference leg is past
    # init's foreground model wait (#519). The daemon-engine leg can arrive mid
    # cold start, so give the port a bounded window rather than racing it.
    #
    # 180 s, deliberately LONGER than the agent's own first-readiness budget
    # (OllamaConfig.StartupReadyTimeout, 150 s by default): a harness window
    # shorter than the product's tolerance reds on a slow cold start the
    # product is still happy with -- and Windows is where cold starts are
    # slowest, with Defender scanning a freshly extracted 1.9 GB tree.
    $live = ''
    $deadline = (Get-Date).AddSeconds(180)
    while ((Get-Date) -lt $deadline) {
        try { $live = [string](Invoke-RestMethod -Uri 'http://127.0.0.1:9475/api/version' -TimeoutSec 5).version } catch { }
        if ($live) { break }
        Start-Sleep -Seconds 3
    }
    if (-not $live) {
        ItBad "nothing is serving on :9475 after 180 s ($Context) -- the engine is installed but not answering"
        $englog = Join-Path $StateDir 'runtimes\ollama\logs\engine.log'
        if (Test-Path -LiteralPath $englog) {
            Get-Content -LiteralPath $englog -Tail 40 -ErrorAction SilentlyContinue |
                ForEach-Object { Write-Host "    engine.log| $_" }
        }
        return
    }

    $ollamaStatus = $null
    try { $ollamaStatus = (Invoke-RestMethod -Uri 'http://127.0.0.1:9476/waired/v1/inference/status' -TimeoutSec 5).runtimes.ollama } catch { }
    $pinned = [string]$ollamaStatus.pinned_version
    $mode   = [string]$ollamaStatus.mode

    # 1. the listener IS the state-dir binary. $ownerPid, never $pid: that name
    #    is a PowerShell automatic variable holding OUR process id, and
    #    assigning it here would silently compare the harness against itself.
    $conn = $null
    try { $conn = Get-NetTCPConnection -LocalPort 9475 -State Listen -ErrorAction Stop | Select-Object -First 1 } catch { }
    if (-not $conn) {
        # /api/version answered above, so something IS listening -- an empty
        # result is a lookup failure, not an absent engine, and must not be
        # reported as the wrong binary.
        ItBad "could not identify the process listening on :9475 ($Context) -- Get-NetTCPConnection found no listening process"
    } else {
        $ownerPid = [int]$conn.OwningProcess
        $exe = ''
        try { $exe = [string](Get-CimInstance Win32_Process -Filter "ProcessId=$ownerPid" -ErrorAction Stop).ExecutablePath } catch { }
        if ($exe -eq $bin) {
            ItOk "the process serving :9475 is the state-dir binary ($Context; pid $ownerPid)"
        } else {
            $shown = if ($exe) { $exe } else { 'unreadable' }
            ItBad "the process serving :9475 is not waired's engine ($Context): pid=$ownerPid exe=$shown, expected $bin"
        }
    }

    # 2. it reports the pin. An empty pinned_version is its own failure -- two
    #    empty strings compare equal, which is the assert-that-cannot-fail
    #    shape #178/#215 already cost this repo five days of green CI.
    if (-not $pinned) {
        ItBad "the daemon published no pinned_version ($Context) -- the version comparison would be vacuous"
    } elseif ($live -eq $pinned) {
        ItOk "the serving engine is the pinned release ($Context; /api/version = $live)"
    } else {
        ItBad "the serving engine is not the pinned release ($Context): /api/version = $live, pinned $pinned"
    }

    # 3. waired spawned it, rather than adopting a survivor of a previous run.
    if ($mode -eq 'spawned') {
        ItOk "waired spawned the serving engine ($Context; mode=spawned)"
    } elseif (-not $mode) {
        ItBad "the daemon published no engine mode ($Context) -- cannot tell a spawned engine from an adopted one"
    } else {
        ItBad "waired did not spawn the serving engine ($Context; mode=$mode) -- it adopted a process it does not supervise"
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

    # THE REGRESSION BAR: install.ps1 ran engine-absent, so only the resident
    # executor could have put an engine here -- and since #493 "here" is one
    # path, not "anywhere internal/download can see" (#139).
    Assert-BundledEngine -Context 'daemon-path executor'
    # ...and that binary is the one serving, at the pin (#494). The assert
    # above proves the executor put something on disk; this proves the host is
    # not being served by something else, which is the half #139 was about.
    Assert-ServingEngine -Context 'daemon-path executor'

    $state = ''
    try { $state = (Invoke-RestMethod -Uri 'http://127.0.0.1:9476/waired/v1/inference/status' -TimeoutSec 5).subsystem_state } catch { }
    if ($state -and $state -ne 'no_engine') { ItOk "inference subsystem left no_engine (state=$state)" }
    else { ItBad "inference subsystem still reports '$state' (engine not installed)" }

    $setupState = $null
    try { $setupState = Invoke-RestMethod -Uri 'http://127.0.0.1:9476/waired/v1/setup/state' -TimeoutSec 5 } catch { }

    # engine_installed -- what the SETUP WIZARD reads (#195/#179). The checks
    # above look at the filesystem and at the inference subsystem; neither is
    # the value the daemon reports to the UI, and the two have disagreed
    # (#179: an engine on disk but not on PATH, so the wizard kept offering to
    # install it). desired_engine is read first because SetupState computes
    # engine_installed only when one is set -- see
    # lib/installtest-daemon-engine.sh's item 7.
    if ($null -eq $setupState) {
        ItBad "could not read /setup/state (daemon unreachable) -- engine_installed unverifiable"
    } elseif (-not $setupState.desired_engine) {
        ItLog "no desired_engine at the end of the leg, so engine_installed is false by definition -- not a #179 signal: $($setupState | ConvertTo-Json -Compress)"
    } elseif ($setupState.engine_installed) {
        ItOk "daemon reports engine_installed=true for desired_engine=$($setupState.desired_engine) (setup wizard sees the engine)"
    } else {
        ItBad "engine is on the host but the daemon reports engine_installed=false for desired_engine=$($setupState.desired_engine) (#179 class)"
    }

    $claim = if ($setupState) { $setupState.install_claimed } else { '' }
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
# Get-ModelReadyState -- mirror of it_model_ready_state in
# lib/installtest-enroll.sh; see the comment there for why the verdict is keyed
# on `active` rather than on models.ready being non-empty (waired-agent#573).
# Returns exactly one of 'ready <id>', 'none', 'probe <id>', 'pending'.
#
# Still matches NO model id literal, which is what the #322 note below asked
# for: the id comes from the daemon, so a pinned-fixture change needs no
# harness edit. What changed is WHICH field the daemon is asked for -- the one
# that says which model it committed to serving, instead of the one that lists
# everything on disk (where the #496 cutoff probe lands like any other pull).
#
# scripts/dev/installtest-model-ready-asserts.sh drives this and its two .sh
# twins through the same scenarios per PR: these run only in the dispatch-only
# inference leg, so a copy that had stopped being able to fail would sit green
# for a long time.
function Get-ModelReadyState {
    param($Status)
    if (-not $Status) { return 'pending' }
    $id    = [string]$Status.active.model_id
    $ready = @($Status.models.ready)
    if ($id -and ($ready -contains $id)) { return "ready $id" }
    # subsystem_state 'ready' needs an active selection whose model is ready
    # (cmd/waired-agent/inference.go, subsystemState), so the probe cannot
    # satisfy it. Kept as a second accepting arm rather than dropped.
    if ([string]$Status.subsystem_state -eq 'ready') {
        $shown = if ($id) { $id } else { '(ready)' }
        return "ready $shown"
    }
    if ($Status.no_model_selected -eq $true) { return 'none' }
    # Only when NOTHING was selected: an active pick whose 20-45 GB has not
    # landed yet is the ordinary state for most of this poll.
    $probe = [string]$Status.host_speed.probe_model_id
    if ((-not $id) -and $probe -and ($ready.Count -eq 1) -and ($ready[0] -eq $probe)) {
        return "probe $probe"
    }
    return 'pending'
}

# Write-EvidenceDump: the Windows twin of lib/installtest-enroll.sh's
# _it_evidence_dump. See that function for why the pull group is separate and
# untruncated, why both groups are counted, and why free space is here
# (waired-agent#642).
function Write-EvidenceDump {
    param([string]$Bundle)

    if (-not (Test-Path -LiteralPath $Bundle)) {
        Write-Host "    agent| (no log bundle at $Bundle)"
        return
    }
    $evidence = @(Select-String -LiteralPath $Bundle -Pattern $DaemonEvidenceRe)
    Write-Host "    agent| daemon evidence: $($evidence.Count) line(s) matched, showing the last 40"
    if ($evidence.Count -gt 0) {
        $evidence | Select-Object -Last 40 | ForEach-Object { Write-Host "    agent| $($_.Line)" }
    } else {
        Write-Host "    agent| (no pre-pull or host-speed lines in the daemon log)"
    }
    $pulls = @(Select-String -LiteralPath $Bundle -Pattern 'api/pull')
    Write-Host "    agent| engine pull requests: $($pulls.Count) line(s) matched, showing all"
    if ($pulls.Count -gt 0) {
        $pulls | ForEach-Object { Write-Host "    agent| $($_.Line)" }
    } else {
        Write-Host "    agent| (no api/pull lines in the daemon log)"
    }
    try {
        $drive = (Get-Item -LiteralPath $StateDir).PSDrive
        Write-Host "    agent| state-dir free space: $([math]::Round($drive.Free / 1GB, 1)) GB free on $($drive.Name):"
    } catch {
        Write-Host "    agent| state-dir free space: (unreadable)"
    }
}

function Assert-Inference {
    param([string]$InitLog)

    # 0) PRIMARY: init's own transcript. A leg whose transcript says the engine
    #    install failed FAILED, whatever it can still find on disk. #178 is the
    #    reason this is first and the reason it outranks (1): the ollama.exe
    #    lookup below found a binary the half-finished install had already
    #    unpacked before Get-AuthenticodeSignature blew up, so this function
    #    printed `ok  ollama engine installed` while the exact failure sat in
    #    the same CI log, for five straight days.
    #    See $InstallFailureRe's declaration for the string provenance.
    if (Test-Path -LiteralPath $InitLog) {
        $hits = @(Select-String -Path $InitLog -Pattern $InstallFailureRe -ErrorAction SilentlyContinue)
        if ($hits.Count) {
            ItBad "init transcript reports an engine install failure ($InitLog)"
            $hits | Select-Object -First 5 | ForEach-Object { Write-Host "    $($_.LineNumber): $($_.Line.Trim())" }
        } else {
            ItOk "init transcript reports no engine install failure"
        }
    } else {
        ItBad "no init transcript to check for install failures ($InitLog)"
    }

    # 1) The engine is waired's own, under the state dir.
    #    SECONDARY, and worded as presence rather than success -- see (0).
    $ollama = Join-Path $StateDir 'runtimes\ollama\bin\ollama.exe'
    Assert-BundledEngine -Context 'waired init'
    # ...and it is what actually serves, at the pin (#494). "Installed" and
    # "serving" are two claims; see Assert-ServingEngine for why.
    Assert-ServingEngine -Context 'waired init'

    # 2) bundled model READY in the waired-owned store (:9475), via the agent
    #    mgmt API. init (#519) foreground-waits for the pull, so it is normally
    #    ready the moment init returns; poll briefly to absorb any residual async
    #    tail (e.g. the harness's post-init service restart re-checking the model).
    $inferStatusUrl = 'http://127.0.0.1:9476/waired/v1/inference/status'
    $modelReady = $false; $subState = ''; $modelsReady = @(); $st = $null
    $verdict = 'pending'
    $deadline = (Get-Date).AddMinutes(5)
    while ((Get-Date) -lt $deadline) {
        try {
            $st = Invoke-RestMethod -Uri $inferStatusUrl -TimeoutSec 10
            $subState    = [string]$st.subsystem_state
            $modelsReady = @($st.models.ready)
            $verdict     = Get-ModelReadyState $st
            if ($verdict -like 'ready *') { $modelReady = $true; break }
            # The operator's standing "no model now" choice (waired-agent#586)
            # is terminal: nothing is coming, so waiting out the budget only
            # delays the red.
            if ($verdict -eq 'none') { break }
            # engine_failed is terminal too (waired-agent#29): the engine
            # crashed and automatic recovery either is mid-flight (which shows
            # as "starting") or has given up. Either way, polling for "ready"
            # will not fix it — this list had drifted from the Linux one in
            # lib/installtest-enroll.sh, so a crashed engine burned the whole
            # 5-minute budget here.
            if ($subState -in @('pull_failed','disabled','stopped','engine_failed')) { break }
        } catch { }
        Start-Sleep -Seconds 10
    }
    if ($modelReady) {
        ItOk "bundled model ready in waired store :9475 ($($verdict -replace '^ready ',''); the daemon's active selection, subsystem_state=$subState)"
    } else {
        if ($verdict -like 'probe *') {
            # The #573 red. Reported by name because "not ready" sends the
            # reader to the download, and on this host the download SUCCEEDED:
            # it was the measurement's own 1 GB probe, and selection is what
            # declined. See waired-agent#579 for the defect this surfaces.
            ItBad "this host got a probe, not a pick: the only model in the waired store is the host-cutoff probe ($($verdict -replace '^probe ','')), and the daemon committed to no selection (#573)"
        } elseif ($verdict -eq 'none') {
            ItBad "no model was selected on this host (mgmt API no_model_selected=true) -- ``waired init --inference-enabled=true`` should have picked one"
        } else {
            ItBad "bundled model not ready via mgmt API (subsystem_state=$subState; models.ready=$($modelsReady -join ','))"
        }
        # Diagnostics: query the waired-owned store directly (NOT the default :11434).
        if ($ollama) {
            $env:OLLAMA_HOST = '127.0.0.1:9475'
            try { ((& $ollama list 2>&1 | Out-String) -split "`n") | ForEach-Object { Write-Host "    :9475 $_" } } catch { }
            Remove-Item Env:\OLLAMA_HOST -ErrorAction SilentlyContinue
        }
    }

    # #496/#579: the one-time host-speed measurement -- see the Linux twin in
    # lib/installtest-enroll.sh for why this leg asserts it.
    # POLLED, not read once -- see the Linux twin in lib/installtest-enroll.sh.
    # The measurement is asynchronous by design (it must not block init), so a
    # single read asserts on scheduling rather than on the daemon. Returns as
    # soon as a figure appears, so a healthy leg pays nothing.
    $hsDeadline = (Get-Date).AddSeconds(180)
    while ($st -and (Get-Date) -lt $hsDeadline) {
        if (([double]$st.host_speed.turn_seconds -gt 0) -or
            ([double]$st.host_speed.turn_floor_seconds -gt 0)) { break }
        Start-Sleep -Seconds 5
        try { $st = Invoke-RestMethod -Uri 'http://127.0.0.1:9476/waired/v1/inference/status' -TimeoutSec 5 } catch { }
    }

    if (-not $st) {
        ItLog "no inference status payload -- skipping the host-speed assert"
    } else {
        $turn    = [double]$st.host_speed.turn_seconds
        $budget  = [double]$st.host_speed.budget_seconds
        $samples = [int]$st.host_speed.samples
        # A host far below the cutoff publishes a BOUND and no turn -- see the
        # Linux twin in lib/installtest-enroll.sh (waired-agent#579 Stage 3).
        $floor   = [double]$st.host_speed.turn_floor_seconds
        $method  = [string]$st.host_speed.method
        $figure  = if ($turn -gt 0) { $turn } else { $floor }
        # Still ItSoft in shape, and BLOCKING in effect: $ContractBlocking['579']
        # is now $true, which is exactly what that mechanism is for. It was soft
        # while #579 was open because the absent case was a real defect every PR
        # would have gone red for; Stage 3 closed it from both ends, and this
        # assert takes either figure -- a measured turn, or the prefill-only
        # bound a host too slow to measure at full depth publishes instead.
        ItSoft '579' ($figure -gt 0) "host speed measured inside init (${method}: turn ${turn}s, floor ${floor}s, against a ${budget}s budget; $samples samples)" 'waired-agent'
    }

    # 3) local inference is on -- asked of the DAEMON, not of the config under
    #    %ProgramData%\waired. The file carries the install-time DEFAULT; the
    #    runtime answer is planInitialInference's, folding that default with
    #    the persisted desired-inference toggle and any --inference-enabled
    #    flag. See the darwin twin in installtest-macos.sh (waired-agent#552).
    $desired = ''
    try { $desired = (Invoke-RestMethod -Uri 'http://127.0.0.1:9476/waired/v1/inference/status' -TimeoutSec 5).desired_state } catch { }
    if ($desired -eq 'enabled') { ItOk "local inference is on (mgmt API desired_state=enabled)" }
    elseif ([string]::IsNullOrEmpty($desired)) { ItBad "the daemon published no desired_state -- cannot tell an enabled host from a disabled one" }
    else {
        # turned_inference_off names WHICH thing turned it off: it is the
        # host-speed cutoff's own claim, and it stops being made the moment
        # anything else moves the toggle (HostSpeedStatus). Without it a red
        # here needs a second run to tell a cutoff from an operator.
        $byCutoff = [bool]$st.host_speed.turned_inference_off
        ItBad "local inference is off (mgmt API desired_state=$desired; the host-speed cutoff claims this: turned_inference_off=$byCutoff)"
    }

    # 4) benchmark ran in the init transcript (offerBenchmark): require a
    #    THROUGHPUT NUMBER (tok/s | tokens/s). Mirrors installtest-enroll.sh
    #    and installtest-macos.sh (cross-OS parity). The bare "Local inference
    #    works" line used to be accepted for a host too slow to measure a
    #    rate — but a benchmark whose warm-up got an engine 500 printed that
    #    same line, so the assert passed while the engine was dead
    #    (waired-agent#29). A current daemon 503s a failed run and the CLI
    #    then prints no success line at all.
    #
    #    Three arms, not two (#382): a benchmark that RAN and produced nothing
    #    is an engine problem, a benchmark that NEVER RAN because the model was
    #    not ready in time is a download one. Both stay red -- the distinction
    #    is what the red says, not whether it is red.
    if (Test-Path -LiteralPath $InitLog) {
        $txt = Get-Content -LiteralPath $InitLog -Raw
        $m = [regex]::Match($txt, '(?i)[0-9]+(\.[0-9]+)?\s*(tok|tokens)/s')
        $nr = [regex]::Match($txt, $BenchNotReadyRe)
        if ($m.Success) {
            ItOk "benchmark ran during init ($($m.Value))"
        } elseif ($nr.Success) {
            ItBad "the model was not ready inside init's benchmark window, so nothing was measured -- the download, not the engine (`"$($nr.Value)`"; $InitLog)"
            # Pull-side evidence only; the engine-side grep stays on the arm
            # below, because printing it here is what made every one of these
            # failures read as an engine problem.
            ($txt -split "`n" | Select-String -Pattern 'download|model|pull' |
                Select-Object -Last 20 | ForEach-Object { "    init| $_" }) | Write-Host
            try {
                $st = Invoke-RestMethod -Uri 'http://127.0.0.1:9476/waired/v1/inference/status' -TimeoutSec 10
                Write-Host "    status| $($st | ConvertTo-Json -Depth 6 -Compress)"
            } catch { Write-Host "    status| (unreachable)" }
            # The daemon's own account of a model that never arrived (#540).
            # Mirrors lib/installtest-enroll.sh's it_prepull_evidence and the
            # macOS block: `waired logs` is the one surface that reads the
            # service log and the bundled engine log on every OS, which is how
            # this leg gets an engine log at all -- it had none, so a Windows
            # occurrence could not be diagnosed without a re-run.
            #
            # The pattern carries the #496 measurement too (#579): "the
            # download was slow" and "the measurement was in front of the
            # download" are different failures, and a pull-side-only grep
            # cannot tell them apart.
            $bundle = Join-Path $env:TEMP 'it-logs.txt'
            & (Join-Path $InstallDir 'waired.exe') logs --since 30m --state-dir $StateDir -o $bundle *> $null
            Write-EvidenceDump -Bundle $bundle
        } else {
            ItBad "no benchmark THROUGHPUT figure in init transcript ($InitLog)"
            ($txt -split "`n" | Select-String -Pattern 'benchmark|inference|engine' |
                Select-Object -Last 20 | ForEach-Object { "    init| $_" }) | Write-Host
        }
    } else {
        ItBad "no init transcript captured ($InitLog)"
    }
}

# --- service recovery-policy assert (waired#315) -----------------------------
# `waired-agent install` configures three restarts with backoff, but the SCM
# only runs recovery actions for a service that dies WITHOUT reporting
# SERVICE_STOPPED. Our svc.Handler reports Stopped and exits 1 on a fatal
# error, so until SetRecoveryActionsOnNonCrashFailures(true) was added, none of
# those restarts ever fired for the failure mode most likely in the field.
#
# sc.exe qfailureflag is the only place that bit is observable, and nothing
# else in the suite would go red if the call were dropped again: the service
# installs, starts, and serves exactly the same either way. Blocking from the
# start ($ContractBlocking['315'] = $true) because the fix ships in the same
# PR as this assert.
# --- the #568 measurement seam, and the two probes that need it --------------
#
# Wait-Enrolled: poll the daemon until it reports an identity. The Windows
# twin of lib/installtest-enroll.sh's _it_wait_enrolled, factored out of the
# Tier-2 readback below so the probes that restart the service can reuse it —
# a restart drops the enrolled session for a few seconds, and on linux the
# assert right after a restart was the first casualty of not waiting (#605).
function Wait-Enrolled {
    param([int]$TimeoutSec = 45)
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    $attempt  = 0
    while ((Get-Date) -lt $deadline) {
        $attempt++
        try {
            $st = Invoke-RestMethod -Uri $MgmtStatus -TimeoutSec 1
            if ($st.device_id -match '^dev_') { return $true }
        } catch { }
        Start-Sleep -Milliseconds $(if ($attempt -le 10) { 250 } else { 1000 })
    }
    return $false
}

# Set-HostBelowSpec / Restore-HostMemory: make this host read as below the
# recommended spec for the probes that need it, and put it back.
#
# Linux arranges this with WAIRED_RAM_AVAILABLE_GB in the systemd
# EnvironmentFile. Windows services have no equivalent that a restart is
# guaranteed to pick up: the machine environment block is cached by the SCM
# at boot. The persisted measurement is the OTHER end of the same read --
# hostMemoryGB() takes the env seam first and this record otherwise -- so
# patching the record arranges the same fact through the path production
# actually uses, with no service-manager subtleties. installtest-macos.sh
# does the same, for the same reason.
#
# The daemon reuses a record whose agent_version matches its own build
# (waired-agent#568), so the patched figure survives the restart instead of
# being re-measured. Only the number is rewritten; agent_version is left
# exactly as the daemon wrote it, which is what makes that true.
#
# .NET's ReadAllText/WriteAllText rather than Get-Content/Set-Content: under
# Windows PowerShell 5.1 `Set-Content -Encoding utf8` writes a BOM, and a BOM
# makes the record unparseable, which ReadHostMemory treats as "never
# measured" -- the daemon would then re-measure and the seam would silently
# not take.
function Get-HostMemoryPath { Join-Path $StateDir 'runtime\host-memory.json' }

# Get-EnginePresentNote: the likeliest reason the arm under test was not
# reached, when it is this one, as a clause to append to a failure message.
# Empty otherwise, so the caller appends it unconditionally. The Windows twin
# of lib/installtest-enroll.sh's _it_engine_present_note (waired-agent#640).
function Get-EnginePresentNote {
    $bin = Join-Path $StateDir 'runtimes\ollama\bin\ollama.exe'
    if (Test-Path -LiteralPath $bin) {
        return " -- an engine is already installed at $bin, so the daemon no longer wanted one (waired-agent#640)"
    }
    return ''
}

function Set-HostBelowSpec {
    param([string]$Waired, [string]$Who)
    & $Waired inference on --state-dir $StateDir 2>&1 | Out-Null
    # The third thing these probes need, and the one this cannot arrange: an
    # engine-less host. See lib/installtest-enroll.sh's _it_force_below_spec
    # for the whole story (waired-agent#640) -- the state is inherited from
    # whatever ran before, so the only useful thing to do is say when it does
    # not hold.
    $engine = Join-Path $StateDir 'runtimes\ollama\bin\ollama.exe'
    if (Test-Path -LiteralPath $engine) {
        ItLog "WARN an engine is already installed at $engine before the $Who probe -- the daemon no longer wants one, so the arm under test will not be reached (waired-agent#640)"
    }
    $rec = Get-HostMemoryPath
    $bak = Join-Path $Work 'host-memory.json.bak'
    if (Test-Path -LiteralPath $rec) {
        Copy-Item -LiteralPath $rec -Destination $bak -Force -ErrorAction SilentlyContinue
        $txt = [System.IO.File]::ReadAllText($rec)
        $txt = [regex]::Replace($txt, '"available_gb"\s*:\s*\d+', '"available_gb": 1')
        [System.IO.File]::WriteAllText($rec, $txt)
        if ($txt -notmatch '"available_gb"\s*:\s*1\b') {
            ItLog "WARN the #568 measurement seam did not take on $rec; the $Who probe's own asserts are what will say so"
        }
    } else {
        ItLog "WARN no host-memory record at $rec -- the $Who probe cannot force a below-spec verdict"
    }
    Restart-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
    if (-not (Wait-Enrolled)) { ItLog "WARN daemon did not report enrolled after the $Who seam restart" }
}

# Leave the host as we found it: the real measurement back, the daemon
# restarted on it, and inference off -- the state every assert after these
# probes was written against.
function Restore-HostMemory {
    param([string]$Waired, [string]$Who)
    $rec = Get-HostMemoryPath
    $bak = Join-Path $Work 'host-memory.json.bak'
    if (Test-Path -LiteralPath $bak) {
        Copy-Item -LiteralPath $bak -Destination $rec -Force -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath $bak -Force -ErrorAction SilentlyContinue
    } else {
        # Nothing to restore: removing it makes the daemon measure again on
        # its next clean boot, which is the state we would have left anyway.
        Remove-Item -LiteralPath $rec -Force -ErrorAction SilentlyContinue
    }
    Restart-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
    if (-not (Wait-Enrolled)) { ItLog "WARN daemon did not report enrolled after the $Who cleanup restart" }
    & $Waired inference off --state-dir $StateDir 2>&1 | Out-Null
}

# Assert-ReinitDefaultUnfit: the Windows twin of lib/installtest-enroll.sh's
# assert_reinit_default_unfit (waired-agent#590). On a host below the
# recommended spec, a non-interactive init with NO inference flag must end
# with local AI off, exit 0, and the skip note -- a choice, not a fault (the
# #551 exit discipline; distinct from the #569/#576 exit-3 contract).
#
# Exactly four asserts, always -- the tier-2 floor counts on it.
function Assert-ReinitDefaultUnfit {
    param([string]$Waired, [string]$Device, [string]$ControlUrl)

    Set-HostBelowSpec -Waired $Waired -Who '#590 default'

    $log = Join-Path $Work 'reinit-default-unfit.log'
    $env:WAIRED_NO_EMOJI = '1'
    $prevEap = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    & $Waired init --control $ControlUrl --device-name $Device `
        --non-interactive --skip-integration --state-dir $StateDir 2>&1 |
        Tee-Object -FilePath $log
    $rc = $LASTEXITCODE
    $ErrorActionPreference = $prevEap

    if ($rc -eq 0) { ItOk "flagless init on a below-spec host exits 0 (a choice, not a fault -- waired-agent#590)" }
    else { ItBad "flagless init exited $rc on a below-spec host -- the non-interactive default is skip-and-continue, never a failure -- see $log" }

    # -SimpleMatch on every one of these: -Pattern is a .NET regex, and the
    # shared literals are written for grep's BRE. See the EngineOptOutRe
    # comment for the whole story.
    $skipped = Select-String -Path $log -Pattern $UnfitSkipRe -SimpleMatch -Quiet -ErrorAction SilentlyContinue
    if ($skipped) { ItOk "the step-4 non-interactive default said what it did" }
    else {
        ItBad ("init never printed the skip note -- the step-4 default arm was not reached, so the asserts around it prove nothing" + (Get-EnginePresentNote) + " -- see $log")
        Get-Content -LiteralPath $log -Tail 20 -ErrorAction SilentlyContinue | ForEach-Object { ItLog "    init| $_" }
    }
    $calledItFailed = Select-String -Path $log -Pattern $InstallFailureBoxRe -SimpleMatch -Quiet -ErrorAction SilentlyContinue
    if ($calledItFailed) { ItBad "init reported the below-spec default as a failed install -- see $log" }
    else { ItOk "the default is not reported as a failed install" }

    $desired = ''
    try { $desired = (Invoke-RestMethod -Uri 'http://127.0.0.1:9476/waired/v1/inference/status' -TimeoutSec 5).desired_state } catch { }
    if ($desired -eq 'disabled') { ItOk "the default landed as the persisted toggle (mgmt API desired_state=disabled)" }
    else { ItBad "mgmt API desired_state=$desired after the flagless below-spec init, want disabled" }

    Restore-HostMemory -Waired $Waired -Who '#590 default'
}

# Assert-ModelsPullConfirm: the Windows twin of lib/installtest-enroll.sh's
# assert_models_pull_confirm (waired-agent#590). See that function for the
# contract and for why an engine-less host makes the honoured row free -- the
# daemon refuses the handed-on pull at #307's admission check instead of
# fetching weights.
#
# Exactly five asserts, always -- the tier-2 floor counts on it.
function Assert-ModelsPullConfirm {
    param([string]$Waired)

    Set-HostBelowSpec -Waired $Waired -Who '#590 pull twin'

    # From the catalog, not a literal: the bundled set is retired and
    # replaced on its own schedule (#577). Under the seam every family is
    # fits=false, so any of them will do.
    $model = ''
    try {
        $cat = Invoke-RestMethod -Uri 'http://127.0.0.1:9476/waired/v1/inference/catalog' -TimeoutSec 5
        if ($cat.families -and $cat.families.Count -gt 0) { $model = [string]$cat.families[0].model_id }
    } catch { }
    if (-not $model) {
        ItBad "no model_id in the catalog response -- the pull gate reads the same endpoint, so nothing below would be testing it"
        Restore-HostMemory -Waired $Waired -Who '#590 pull twin'
        # Still five: a leg that reports four has a block that stopped
        # executing, and the floor is what says so.
        ItBad "skipped: --yes on a model that does not fit was not exercised"
        ItBad "skipped: the decline is not a failed command"
        ItBad "skipped: --yes alone did not reach the pull layer"
        ItBad "skipped: --yes --force honoured the choice"
        return
    }

    $env:WAIRED_NO_EMOJI = '1'
    $prevEap = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'

    $log = Join-Path $Work 'models-pull-yes.log'
    & $Waired models pull $model --yes --wait=false 2>&1 | Tee-Object -FilePath $log
    $rc = $LASTEXITCODE

    if ($rc -eq 0) { ItOk "declining an over-memory pull is not a failed command (exit 0)" }
    else { ItBad "``models pull --yes`` exited $rc on a model that does not fit -- a decline is a choice, not a fault -- see $log" }
    $declined = Select-String -Path $log -Pattern $PullDeclineRe -SimpleMatch -Quiet -ErrorAction SilentlyContinue
    if ($declined) { ItOk "--yes alone declines an over-memory pull and says how to override" }
    else { ItBad "``models pull --yes`` never printed the decline line -- --yes must not auto-confirm a default-No question -- see $log" }
    $queued = Select-String -Path $log -Pattern $PullQueuedRe -SimpleMatch -Quiet -ErrorAction SilentlyContinue
    if ($queued) { ItBad "``models pull --yes`` queued the download anyway -- the gate did not stop it -- see $log" }
    else { ItOk "--yes alone dispatched nothing to the daemon" }

    $log = Join-Path $Work 'models-pull-force.log'
    & $Waired models pull $model --yes --force --wait=false 2>&1 | Tee-Object -FilePath $log
    $ErrorActionPreference = $prevEap

    $declined = Select-String -Path $log -Pattern $PullDeclineRe -SimpleMatch -Quiet -ErrorAction SilentlyContinue
    if ($declined) { ItBad "``models pull --yes --force`` still declined -- the scripted consent is the pair, and it was not honoured -- see $log" }
    else { ItOk "--yes --force is not stopped by the over-memory gate" }
    # Two literals, so two -SimpleMatch reads rather than the .sh side's one
    # alternation: -Pattern would make it a regex and the shared strings are
    # written for grep.
    $reached = (Select-String -Path $log -Pattern $PullQueuedRe -SimpleMatch -Quiet -ErrorAction SilentlyContinue) -or
               (Select-String -Path $log -Pattern $PullReachedRe -SimpleMatch -Quiet -ErrorAction SilentlyContinue)
    if ($reached) { ItOk "--yes --force handed the pull to the daemon" }
    else {
        ItBad "``models pull --yes --force`` neither queued nor reached the daemon's pull layer -- see $log"
        Get-Content -LiteralPath $log -Tail 10 -ErrorAction SilentlyContinue | ForEach-Object { ItLog "    pull| $_" }
    }

    Restore-HostMemory -Waired $Waired -Who '#590 pull twin'
}

# Test-NoModelSelected: is the daemon publishing the standing "run without a
# model" choice? Twin of lib/installtest-enroll.sh's _it_no_model_selected; see
# that one for why it reads the mgmt API's own field rather than inferring from
# an empty model list.
function Test-NoModelSelected {
    try {
        $st = Invoke-RestMethod -Uri 'http://127.0.0.1:9476/waired/v1/inference/status' -TimeoutSec 5
        return [bool]$st.no_model_selected
    } catch { return $false }
}

# Assert-EngineOnlyInstall: the Windows twin of lib/installtest-enroll.sh's
# assert_engine_only_install (waired-agent#590). See that function for the
# contract -- "the AI software is installed and no model was chosen" is a
# FINISHED install, and the restart is what makes the answer a standing choice
# rather than a transient one.
#
# THE ONE INTERACTIVE INIT ON THIS HARNESS. Every other init here passes
# --non-interactive, which is exactly what makes them unable to reach the
# picker. One line of stdin is enough because --inference-enabled=true silences
# the two questions in front of it.
#
# Piping a string into a native command is how that line is delivered. It is
# the one piece of this twin with no Linux precedent to copy -- PowerShell has
# no `<` redirect and its pipeline carries objects, not bytes -- so it was
# measured before it was written, against a stdin-reading stub built for
# GOOS=windows and run from both Windows PowerShell 5.1 (5.1.26100) and pwsh
# 7.6.3, the shell this leg actually runs under: the line arrives, $LASTEXITCODE
# survives Tee-Object, and a spaced install path changes nothing. The same run
# with no stdin at all reached EOF instead -- which is what a leg that silently
# answered nothing would look like, and is the reason the no-model grep below
# is the load-bearing assert rather than a nicety.
#
# Exactly six asserts, always -- the floor counts on it.
function Assert-EngineOnlyInstall {
    param([string]$Waired, [string]$Device, [string]$ControlUrl)

    ItLog "installing an engine and answering the model picker with 0 (waired-agent#590)"

    # WINDOWS-ONLY ARRANGEMENT, and the one thing this twin needs that neither
    # sibling does. install.ps1 is invoked with `&` -- in THIS process -- so its
    # Set-OllamaEnvForInit leaves WAIRED_NO_OLLAMA=1 in our environment, and
    # every `waired init` the harness spawns inherits it. Today only the
    # -DaemonEngine branch clears it (see the #551 block below), because that is
    # the only other leg that needs an engine to actually install. On Linux the
    # question never arises: install.sh sets the variable in its own process and
    # the probe's init runs in a fresh one.
    #
    # Cleared for this probe and put back after it, rather than globally: the
    # opt-out state is what the asserts before this one were written against.
    $prevNoOllama = $env:WAIRED_NO_OLLAMA
    Remove-Item Env:WAIRED_NO_OLLAMA -ErrorAction SilentlyContinue

    # The daemon has to WANT an engine: the two #590 probes above leave the
    # toggle off, and the #551 probe before them turned it off too.
    & $Waired inference on --state-dir $StateDir 2>&1 | Out-Null

    $log = Join-Path $Work 'engine-only.log'
    $env:WAIRED_NO_EMOJI = '1'
    $prevEap = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    '0' | & $Waired init --control $ControlUrl --device-name $Device `
        --inference-enabled=true --skip-integration --state-dir $StateDir 2>&1 |
        Tee-Object -FilePath $log
    $rc = $LASTEXITCODE
    $ErrorActionPreference = $prevEap

    if ($rc -eq 0) { ItOk "an install that ends with no model chosen exits 0 (waired-agent#590)" }
    else { ItBad "init exited $rc after the operator chose not to download a model -- that is a finished install, not a failure -- see $log" }

    # Anti-vacuity, and the load-bearing one: without it every assert here
    # would pass on a host where the picker never ran and the daemon's own
    # auto-selection quietly applied instead (which is what #607 was).
    $answered = Select-String -Path $log -Pattern $NoModelRe -SimpleMatch -Quiet -ErrorAction SilentlyContinue
    if ($answered) { ItOk "the picker asked and recorded the no-model answer" }
    else {
        ItBad "init never printed the no-model line -- the picker did not run, so the asserts around it prove nothing -- see $log"
        Get-Content -LiteralPath $log -Tail 25 -ErrorAction SilentlyContinue | ForEach-Object { ItLog "    init| $_" }
    }
    $calledItFailed = Select-String -Path $log -Pattern $InstallFailureBoxRe -SimpleMatch -Quiet -ErrorAction SilentlyContinue
    if ($calledItFailed) { ItBad "init reported an engine-only install as a failed install -- see $log" }
    else { ItOk "an engine-only install is not reported as a failed install" }

    $bin = Join-Path $StateDir 'runtimes\ollama\bin\ollama.exe'
    if (Test-Path -LiteralPath $bin) { ItOk "the engine is installed ($bin) -- this host runs AI, it just has no model yet" }
    else { ItBad "no engine at $bin -- the point of this state is that the software IS installed -- see $log" }

    if (Test-NoModelSelected) { ItOk "the daemon publishes the standing no-model choice (mgmt API no_model_selected=true)" }
    else { ItBad "mgmt API does not report no_model_selected after the operator chose not to download a model" }

    # The restart is the whole point of the sixth assert: an answer that does
    # not survive one is not a standing choice, and the #379 boot pre-pull is
    # what would otherwise fetch a model nobody asked for.
    Restart-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
    if (-not (Wait-Enrolled)) { ItLog "WARN daemon did not report enrolled after the #590 engine-only restart" }
    if (Test-NoModelSelected) { ItOk "the choice survives a restart -- the boot pre-pull stands down (waired-agent#379)" }
    else { ItBad "no_model_selected is gone after a restart -- the boot pre-pull is about to download a model nobody asked for" }

    # Leave the host as we found it for anything after this one.
    & $Waired inference off --state-dir $StateDir 2>&1 | Out-Null
    if ($prevNoOllama) { $env:WAIRED_NO_OLLAMA = $prevNoOllama }
}

function Assert-ServiceRecoveryFlag {
    $out = & sc.exe qfailureflag $ServiceName 2>&1 | Out-String
    ItLog "sc qfailureflag: $($out.Trim())"
    # Localised Windows translates the label, so key off the value, not the
    # phrase: the line reads "<label>: TRUE" / ": 1" depending on the build.
    $set = $out -match '(?im):\s*(TRUE|1)\s*$'
    ItSoft '315' $set "SCM restarts the agent after a non-crash failure exit (qfailureflag set)"
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
#
# -Env sets variables INSIDE the wrapper. The restricted contexts below are
# reached through a scheduled task (a fresh logon) or runas (a new token), and
# neither inherits this process's environment — so `set` in the wrapper is the
# only way to hand an env knob to the wrapped command.
function Write-ItCmdWrapper {
    param([string]$Exe, [string]$ArgLine, [string]$Tag, [hashtable]$Env)
    New-Item -ItemType Directory -Path $PubWork -Force | Out-Null
    $paths = @{
        Cmd = Join-Path $PubWork "$Tag.cmd"
        Out = Join-Path $PubWork "$Tag.out"
        Rc  = Join-Path $PubWork "$Tag.rc"
    }
    Remove-Item -LiteralPath $paths.Out, $paths.Rc -Force -ErrorAction SilentlyContinue
    $lines = @('@echo off')
    if ($Env) { foreach ($k in $Env.Keys) { $lines += "set $k=$($Env[$k])" } }
    $lines += @(
        "`"$Exe`" $ArgLine > `"$($paths.Out)`" 2>&1"
        "echo %ERRORLEVEL% > `"$($paths.Rc)`""
    )
    $lines | Set-Content -LiteralPath $paths.Cmd -Encoding ASCII
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
    param([string]$Exe, [string]$ArgLine, [string]$Tag, [hashtable]$Env)
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
    $paths = Write-ItCmdWrapper -Exe $Exe -ArgLine $ArgLine -Tag $Tag -Env $Env
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
    param([string]$Exe, [string]$ArgLine, [string]$Tag, [hashtable]$Env)
    $paths = Write-ItCmdWrapper -Exe $Exe -ArgLine $ArgLine -Tag $Tag -Env $Env
    & runas /trustlevel:0x20000 "cmd /c `"$($paths.Cmd)`"" | Out-Null
    return (Wait-ItCmdWrapper -Paths $paths -TimeoutSec 45)
}

# ============================================================================
# Build + pack + serve
# ============================================================================
ItStep "building waired.exe + waired-agent.exe from worktree"
$ver = (& git -C $Root rev-parse --short HEAD).Trim()
# Version and BuildSHA are DIFFERENT strings, as they are in a real build.
# Stamping the bare SHA into both is the shape of #631, and it is what this
# harness used to do — so it could never have caught it. $semver is the same
# dev version already used for the VERSION file and the Inno AppVersion.
$semver = "0.0.0-$ver"
$ldf = "-s -w -X github.com/waired-ai/waired-agent/internal/buildinfo.Version=$semver -X github.com/waired-ai/waired-agent/internal/buildinfo.BuildSHA=$ver"
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
Set-Content -LiteralPath (Join-Path $Stage 'VERSION') -Value $semver -Encoding ASCII -NoNewline
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
    function Invoke-ArgtestRaw([string]$cmd) {
        # Invoke install.ps1 the way Phase 1 actually runs -- IN-SESSION
        # (`& install.ps1 <args>` / iwr|iex), NOT -File. That is the parse mode
        # where a bare `--dev` is a positional value (the #746 bug); -File would
        # instead bind `--dev` natively to -Dev and never exercise the fix. Run
        # in a child process so a Common-Die (exit 1) can't tear down this test.
        $env:WAIRED_ARGTEST = '1'
        try {
            $o = & powershell.exe -NoProfile -ExecutionPolicy Bypass -Command $cmd 2>&1 | Out-String
        } finally { Remove-Item Env:WAIRED_ARGTEST -ErrorAction SilentlyContinue }
        [pscustomobject]@{ Exit = $LASTEXITCODE; Out = $o }
    }
    function Invoke-Argtest([string[]]$a) {
        Invoke-ArgtestRaw ("& '$installPs1' " + ($a -join ' '))
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

    # --- -LogLevel (#164) ----------------------------------------------------
    # This CI runner is already Administrator, so the Tier-1 install below takes
    # install.ps1's inline Phase-2 path and NEVER crosses Invoke-SelfElevate --
    # which is exactly why #164 (the flag being dropped across UAC) survived
    # unnoticed. These cases assert the in-process fold; the crossing itself is
    # asserted by the -StateFile block below.
    ItStep "install.ps1 -LogLevel asserts (#164)"
    $r = Invoke-Argtest @('-LogLevel','debug')
    if ($r.Exit -eq 0 -and $r.Out -match 'LogLevel=debug EnvLogLevel=debug') { ItOk "-LogLevel debug is published to the env every child of this process inherits" }
    else { ItBad "-LogLevel not published to the env (exit $($r.Exit)): $($r.Out.Trim())" }
    $r = Invoke-Argtest @('--log-level','DEBUG')
    if ($r.Exit -eq 0 -and $r.Out -match 'LogLevel=debug EnvLogLevel=debug') { ItOk "--log-level DEBUG folds + normalises (install.sh parity)" }
    else { ItBad "--log-level parity broken (exit $($r.Exit)): $($r.Out.Trim())" }
    # Validation must fire in Phase 1. ARGTEST returns before any download or
    # UAC, so reaching this die at all proves the check is no longer buried in
    # Invoke-AgentInstall (i.e. after the UAC click, inside a window that
    # closes on exit).
    $r = Invoke-Argtest @('--log-level','bogus')
    if ($r.Exit -ne 0 -and $r.Out -match 'must be one of') { ItOk "a bad --log-level dies before any privileged step" }
    else { ItBad "bad --log-level not rejected early (exit $($r.Exit)): $($r.Out.Trim())" }

    # --- the UAC hand-off: -StateFile and the elevated argv (#192, #177) ------
    # The previous regression test for this ran two install.ps1 invocations in
    # ONE process and asserted the second still saw $env:WAIRED_LOG_LEVEL. That
    # is a tautology: it observes an environment it never lost. The real child
    # is created by the AppInfo service under -Verb RunAs, which builds a FRESH
    # environment block -- so every WAIRED_* value Phase 1 resolved was dropped
    # (#192), and -LogLevel was never actually fixed on that path.
    #
    # This runner is always Administrator, so it cannot raise UAC. It does not
    # need to: scrubbing WAIRED_* from a child process reproduces exactly what
    # CreateEnvironmentBlock does to the elevated one, and everything that has
    # to survive now travels in -StateFile.
    ItStep "install.ps1 UAC hand-off asserts (#192, #177)"
    $stateFile   = Join-Path $env:TEMP "waired-it-state-$([Guid]::NewGuid().ToString('N')).json"
    $stateLog    = Join-Path $env:TEMP "waired-it-state-$([Guid]::NewGuid().ToString('N')).log"
    $probeDir    = Join-Path $env:TEMP "waired it space $([Guid]::NewGuid().ToString('N'))"
    $probeScript = Join-Path $probeDir 'install.ps1'
    New-Item -ItemType Directory -Path $probeDir -Force | Out-Null
    Copy-Item -LiteralPath $installPs1 -Destination $probeScript -Force

    # Phase 1: resolve a configuration that uses every mechanism at once -- a
    # parameter (-LogLevel), a parameter whose default is an env read
    # (-Control), and two env-only knobs with no parameter form at all
    # (WAIRED_NO_TRAY, WAIRED_STATE_DIR) -- and capture the state document
    # Invoke-SelfElevate would hand the child.
    $env:WAIRED_ARGTEST_STATEFILE = $stateFile
    $env:WAIRED_NO_TRAY           = '1'
    $env:WAIRED_STATE_DIR         = 'C:\WairedStateProbe'
    try {
        $r = Invoke-ArgtestRaw ("& '$probeScript' -LogLevel debug -Control https://cp.example.test -LogPath '$stateLog' -NonInteractive")
    } finally {
        Remove-Item Env:WAIRED_ARGTEST_STATEFILE -ErrorAction SilentlyContinue
        Remove-Item Env:WAIRED_NO_TRAY           -ErrorAction SilentlyContinue
        Remove-Item Env:WAIRED_STATE_DIR         -ErrorAction SilentlyContinue
    }
    $elevateArgs = if ($r.Out -match 'ElevateArgs=\[([^\]]*)\]') { $Matches[1] } else { '' }
    if ($r.Exit -eq 0 -and (Test-Path -LiteralPath $stateFile)) { ItOk "Phase 1 writes the resolved state document" }
    else { ItBad "Phase 1 wrote no state document (exit $($r.Exit)): $($r.Out.Trim())" }

    # The crossing. Nothing but -StateFile reaches this process.
    $r = Invoke-ArgtestRaw ("Get-ChildItem Env:WAIRED_* -ErrorAction SilentlyContinue | Remove-Item -ErrorAction SilentlyContinue; " +
                            "`$env:WAIRED_ARGTEST='1'; & '$probeScript' -StateFile '$stateFile'")
    if ($r.Exit -eq 0 -and $r.Out -match 'LogLevel=debug EnvLogLevel=debug') { ItOk "-LogLevel survives an environment the child did not inherit (#164 on the self-elevating path)" }
    else { ItBad "-LogLevel lost across the boundary (exit $($r.Exit)): $($r.Out.Trim())" }
    if ($r.Out -match 'NoTray=True') { ItOk "WAIRED_NO_TRAY crosses (it has no parameter form, so there was no workaround)" }
    else { ItBad "WAIRED_NO_TRAY lost across the boundary: $($r.Out.Trim())" }
    if ($r.Out -match 'StateDir=C:\\WairedStateProbe') { ItOk "WAIRED_STATE_DIR crosses" }
    else { ItBad "WAIRED_STATE_DIR lost across the boundary: $($r.Out.Trim())" }
    if ($r.Out -match 'ControlUrl=https://cp\.example\.test') { ItOk "the resolved Control URL crosses (it decided which CP the device enrols against)" }
    else { ItBad "the Control URL lost across the boundary: $($r.Out.Trim())" }
    if ($r.Out -match 'InstallDir=\S+ Files\\Waired') { ItOk "the resolved install dir crosses whole, spaces and all" }
    else { ItBad "the install dir did not survive: $($r.Out.Trim())" }

    # The argv itself (#177). 'C:\Program Files\Waired' used to split across two
    # parameters, and the child died in Normalize-ExtraArgs before any
    # diagnostics existed. Nothing configuration-bearing belongs in it any more.
    if ($elevateArgs -match '-File "[^"]+ [^"]+"') { ItOk "the elevated argv quotes a script path containing spaces" }
    else { ItBad "the elevated argv leaves a spaced script path unquoted: [$elevateArgs]" }
    if ($elevateArgs -match '-StateFile' -and $elevateArgs -notmatch '-InstallDir' -and $elevateArgs -notmatch '-Control' -and $elevateArgs -notmatch '-LogLevel') { ItOk "the elevated argv carries no configuration, only -StagedZipPath / -StateFile" }
    else { ItBad "configuration is still riding the elevated argv: [$elevateArgs]" }

    # Execute that exact command line. Start-Process passes a single-string
    # -ArgumentList through verbatim, so this is the real construction, not a
    # re-quoted copy of it -- run from a directory whose name contains spaces.
    if (-not $elevateArgs) {
        ItBad "no elevated argv was printed, so it could not be executed"
    } else {
        $probeOut = Join-Path $probeDir 'child.out'
        $env:WAIRED_ARGTEST = '1'
        try {
            $p = Start-Process -FilePath 'powershell.exe' -ArgumentList $elevateArgs `
                    -NoNewWindow -PassThru -Wait -RedirectStandardOutput $probeOut
            $childOut = if (Test-Path -LiteralPath $probeOut) { (Get-Content -LiteralPath $probeOut -Raw) } else { '' }
        } finally { Remove-Item Env:WAIRED_ARGTEST -ErrorAction SilentlyContinue }
        if ($p.ExitCode -eq 0 -and $childOut -match 'LogLevel=debug') { ItOk "the constructed argv binds in a real child process (#177)" }
        else { ItBad "the constructed argv did not bind (exit $($p.ExitCode)): $($childOut.Trim())" }
    }

    # A child that dies before its transcript exists still has to leave a trace
    # the un-elevated parent can print -- the whole point of #177's third item.
    # Two paths, because they are caught by different machinery: an uncaught
    # terminating error (the trap) and Common-Die (which calls exit, so no trap
    # can see it -- that is the path every ordinary Phase-2 failure takes).
    $corruptState = Join-Path $probeDir 'corrupt.json'
    Set-Content -LiteralPath $corruptState -Value '{ not json'
    $r = Invoke-ArgtestRaw ("& '$probeScript' -StateFile '$corruptState'")
    if ($r.Exit -ne 0 -and (Test-Path -LiteralPath "$corruptState.status")) { ItOk "an early elevated failure leaves a .status marker for the parent to read" }
    else { ItBad "no .status marker after an early failure (exit $($r.Exit)): $($r.Out.Trim())" }
    # No ARGTEST here on purpose: that seam returns before the Phase-2 guards,
    # so it would never reach a Common-Die. -NonInteractive rides the state file
    # so the failure cannot sit on a Read-Host.
    Remove-Item -LiteralPath "$stateFile.status" -Force -ErrorAction SilentlyContinue
    $o = & powershell.exe -NoProfile -ExecutionPolicy Bypass -File $probeScript `
            -StateFile $stateFile -StagedZipPath (Join-Path $probeDir 'missing.zip') 2>&1 | Out-String
    if ($LASTEXITCODE -ne 0 -and (Test-Path -LiteralPath "$stateFile.status")) { ItOk "a Common-Die inside the elevated phase leaves a .status marker too" }
    else { ItBad "no .status marker after a Common-Die (exit $LASTEXITCODE): $($o.Trim())" }

    Remove-Item -LiteralPath $stateFile -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $stateLog  -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $probeDir  -Recurse -Force -ErrorAction SilentlyContinue

    # --- Test-Admin against a REAL restricted token (#195) --------------------
    # Which arm install.ps1 takes -- inline Phase 2 vs Invoke-SelfElevate -- is
    # decided by one predicate, Test-Admin. Both existing layers stub it:
    # installtest-pwsh.ps1 aliases it to $env:IT_ADMIN (so it covers the BRANCH
    # on every PR, on any host), and this runner is permanently Administrator,
    # so the True arm is all it has ever produced. That leaves the predicate
    # itself untested against a token that is actually restricted -- CLAUDE.md
    # §Test discipline: "a `var xFn = realFn` seam needs a table test on
    # realFn, or the real one is never called by any test".
    #
    # The two restricted contexts are the two real ones a user arrives in: a
    # plain standard user, and a UAC-FILTERED administrator (the default for an
    # admin who has not clicked through — the #751 context). Both must answer
    # False, and this elevated session must answer True; asserting only one side
    # would pass against a Test-Admin folded to a constant.
    #
    # ARGTEST returns before any download, SCM or UAC work, so these install
    # nothing. install.ps1 is copied under C:\Users\Public because the runner's
    # workspace is not reliably readable by a second local user -- into a
    # SPACED subdirectory, because the argv assert below is #177's and the
    # token that has to survive quoting is the one with a space in it.
    # ($PubWork itself stays unspaced: Invoke-AsStandardUser's schtasks /TR
    # takes the wrapper path bare.) The ACL is granted explicitly rather than
    # inherited: Invoke-AsStandardUser's own (OI)(CI) grant only reaches
    # children created after it runs, and this directory exists before.
    ItStep "install.ps1 Test-Admin under real restricted tokens (#195)"
    $argDir = Join-Path $PubWork 'arg test'
    New-Item -ItemType Directory -Path $argDir -Force | Out-Null
    # *S-1-5-32-545 = BUILTIN\Users by SID, so this does not depend on the
    # runner image's display language.
    & icacls $argDir /grant '*S-1-5-32-545:(OI)(CI)RX' | Out-Null
    $pubInstall = Join-Path $argDir 'install.ps1'
    Copy-Item -LiteralPath $installPs1 -Destination $pubInstall -Force
    $argtestLine = "-NoProfile -ExecutionPolicy Bypass -File `"$pubInstall`" -NonInteractive"

    $r = Invoke-ArgtestRaw ("& '$pubInstall' -NonInteractive")
    if ($r.Out -match 'ARGTEST .*Admin=True') { ItOk "Test-Admin reports True in this elevated session (the arm CI has always taken)" }
    else { ItBad "elevated session did not report Admin=True (exit $($r.Exit)): $($r.Out.Trim())" }

    foreach ($ctx in @(
            @{ Tag = 'argtest-stduser';    Label = 'a standard user';                Run = { param($e) Invoke-AsStandardUser -Exe 'powershell.exe' -ArgLine $argtestLine -Tag 'argtest-stduser' -Env $e } },
            @{ Tag = 'argtest-basictoken'; Label = 'a UAC-filtered admin token';     Run = { param($e) Invoke-AsBasicToken  -Exe 'powershell.exe' -ArgLine $argtestLine -Tag 'argtest-basictoken' -Env $e } })) {
        $r = & $ctx.Run @{ WAIRED_ARGTEST = '1' }
        $out = [string]$r.Out
        if ($r.Exit -ne 0) {
            ItBad "install.ps1 -WAIRED_ARGTEST failed under $($ctx.Label) (exit $($r.Exit)): $($out.Trim())"
            continue
        }
        # The predicate. False here is what routes the install through
        # Invoke-SelfElevate; True would silently run the privileged step list
        # in a process that cannot write %ProgramFiles%.
        if ($out -match 'ARGTEST .*Admin=False') { ItOk "Test-Admin reports False under $($ctx.Label) (install would raise UAC)" }
        else { ItBad "Test-Admin did not report False under $($ctx.Label): $($out.Trim())" }

        # The argv THAT token builds. The quoting assert above ran as an
        # administrator, whose %TEMP% differs from a restricted user's -- and
        # #177 was a path that split on a space, so the path has to come from
        # the same context that would hand it to Start-Process.
        $ea = if ($out -match 'ElevateArgs=\[([^\]]*)\]') { $Matches[1] } else { '' }
        if ($ea -match '-File "[^"]+ [^"]+"') { ItOk "the elevated argv built under $($ctx.Label) quotes its spaced script path (#177)" }
        else { ItBad "the elevated argv built under $($ctx.Label) leaves a spaced path unquoted: [$ea]" }
        if ($ea -match '-StateFile' -and $ea -notmatch '-InstallDir' -and $ea -notmatch '-Control' -and $ea -notmatch '-LogLevel') {
            ItOk "the elevated argv built under $($ctx.Label) carries no configuration (#192)"
        } else { ItBad "configuration rides the elevated argv built under $($ctx.Label): [$ea]" }
    }
    Remove-Item -LiteralPath $argDir -Recurse -Force -ErrorAction SilentlyContinue

    # --- ConvertTo-NativeArg, and the two copies of it ------------------------
    # install.ps1 and uninstall.ps1 are downloaded and run independently, so
    # each carries its own copy of the quoter. Two copies drift; assert they do
    # not, and pin the behaviour itself so neither can regress quietly.
    ItStep "install.ps1 / uninstall.ps1 argument-quoting asserts (#177)"
    $uninstallPs1 = Join-Path $Root 'packaging\install\uninstall.ps1'
    function Get-Ps1Function {
        param([string]$Path, [string]$Name)
        $lines = Get-Content -LiteralPath $Path
        $start = ($lines | Select-String -Pattern "^function\s+$([regex]::Escape($Name))\b" | Select-Object -First 1).LineNumber
        if (-not $start) { return $null }
        for ($i = $start - 1; $i -lt $lines.Count; $i++) { if ($lines[$i] -match '^\}') { break } }
        return (($lines[($start - 1)..$i]) -join "`n")
    }
    $qInstall   = Get-Ps1Function -Path $installPs1   -Name 'ConvertTo-NativeArg'
    $qUninstall = Get-Ps1Function -Path $uninstallPs1 -Name 'ConvertTo-NativeArg'
    if ($qInstall -and $qUninstall -and $qInstall -ceq $qUninstall) { ItOk "both installers carry an identical ConvertTo-NativeArg (no drift)" }
    else { ItBad "ConvertTo-NativeArg differs between install.ps1 and uninstall.ps1 (or is missing from one)" }
    if ($qInstall) {
        Invoke-Expression $qInstall
        # Cases that actually occur: the default install dir, a %TEMP%-derived
        # path under a username with a space, and a directory value an operator
        # may well type with a trailing separator.
        $cases = @(
            @{ In = 'simple';                    Out = 'simple' },
            @{ In = '-DryRun';                   Out = '-DryRun' },
            @{ In = 'C:\Program Files\Waired';   Out = '"C:\Program Files\Waired"' },
            @{ In = 'D:\Waired\';                Out = 'D:\Waired\' },
            @{ In = 'C:\a b\c\';                 Out = '"C:\a b\c\\"' },
            @{ In = '';                          Out = '""' }
        )
        $bad = @()
        foreach ($c in $cases) {
            $got = ConvertTo-NativeArg $c.In
            if ($got -cne $c.Out) { $bad += "[$($c.In)] -> [$got], want [$($c.Out)]" }
        }
        if ($bad.Count -eq 0) { ItOk "ConvertTo-NativeArg follows the CommandLineToArgvW rules" }
        else { ItBad ("ConvertTo-NativeArg wrong: " + ($bad -join '; ')) }
    }

    # --- Get-ExitCodeReason, and its two copies ------------------------------
    # Same shape and same reason as the quoter above: both installers decode
    # the elevated child's exit code, so both carry the table and the two must
    # not drift. This leg cannot be end-to-end here -- the runner is already
    # Administrator, so Invoke-SelfElevate never executes on this host -- which
    # is precisely why the decoder is a pure function: it can be lifted out of
    # the source and driven directly. installtest-pwsh.ps1 covers the wiring.
    ItStep "install.ps1 / uninstall.ps1 exit-code decode asserts (#314)"
    $dInstall   = Get-Ps1Function -Path $installPs1   -Name 'Get-ExitCodeReason'
    $dUninstall = Get-Ps1Function -Path $uninstallPs1 -Name 'Get-ExitCodeReason'
    if ($dInstall -and $dUninstall -and $dInstall -ceq $dUninstall) { ItOk "both installers carry an identical Get-ExitCodeReason (no drift)" }
    else { ItBad "Get-ExitCodeReason differs between install.ps1 and uninstall.ps1 (or is missing from one)" }
    if ($dInstall) {
        Invoke-Expression $dInstall
        # Product contract, not a snapshot: 0xC000013A is the code a closed
        # elevated console produces and the one the whole of #314 is about, so
        # it must always decode to something an operator can act on. An
        # unrecognised code must decode to '' -- that is what makes the caller
        # fall back to printing the raw value instead of inventing a cause.
        $bad = @()
        if ((Get-ExitCodeReason -Code -1073741510) -notmatch 'closed') {
            $bad += '0xC000013A does not mention the window being closed'
        }
        foreach ($known in @(-1073741502, -1073741819, -1073741515, -1073740791)) {
            if (-not (Get-ExitCodeReason -Code $known)) { $bad += "no reason for $known" }
        }
        foreach ($unknown in @(0, 1, 5, 1223)) {
            if ((Get-ExitCodeReason -Code $unknown) -ne '') { $bad += "$unknown should decode to '' (caller prints the raw code)" }
        }
        # The hex the caller pairs with the reason. Int32 formats its
        # two's-complement bit pattern, which is the whole reason the decoder
        # matches signed literals and never casts to [uint32] (that throws).
        if (('{0:X8}' -f -1073741510) -cne 'C000013A') { $bad += "'{0:X8}' does not render -1073741510 as C000013A" }
        if ($bad.Count -eq 0) { ItOk "Get-ExitCodeReason decodes the known NTSTATUS codes and only those" }
        else { ItBad ("Get-ExitCodeReason wrong: " + ($bad -join '; ')) }
    }

    # --- Test-InstallComplete / Format-LockHolders ---------------------------
    # The two predicates #660 turns on, lifted out and driven directly for the
    # same reason as the decoder above: the wrecked half-state they exist for
    # cannot be staged on the runner that is mid-install.
    ItStep "install.ps1 / uninstall.ps1 partial-install asserts (#660)"
    $cInstall = Get-Ps1Function -Path $installPs1 -Name 'Test-InstallComplete'
    if ($cInstall) {
        Invoke-Expression $cInstall
        # Product contract (waired-ai/waired-agent#660): a binary alone is a
        # broken install to repair, so only the binary-AND-service combination
        # may route a bare re-run to the update path.
        $bad = @()
        if (Test-InstallComplete -Version '0.0.2-edge+abc' -ServiceRegistered $false) {
            $bad += 'a leftover binary with no registered service reads as installed'
        }
        if (Test-InstallComplete -Version 'unknown' -ServiceRegistered $false) {
            $bad += "a version-less leftover binary reads as installed"
        }
        if (Test-InstallComplete -Version '' -ServiceRegistered $true) {
            $bad += 'a registered service with no binary reads as installed'
        }
        if (-not (Test-InstallComplete -Version '0.0.2-edge+abc' -ServiceRegistered $true)) {
            $bad += 'a complete install does not read as installed'
        }
        if (-not (Test-InstallComplete -Version 'unknown' -ServiceRegistered $true)) {
            $bad += "a complete install too old to report a version does not read as installed"
        }
        if ($bad.Count -eq 0) { ItOk "Test-InstallComplete requires the service, not just the binary" }
        else { ItBad ("Test-InstallComplete wrong: " + ($bad -join '; ')) }
    } else {
        ItBad "install.ps1 has no Test-InstallComplete (#660)"
    }

    $hUninstall = Get-Ps1Function -Path $uninstallPs1 -Name 'Format-LockHolders'
    if ($hUninstall) {
        Invoke-Expression $hUninstall
        $bad = @()
        $one = Format-LockHolders @([pscustomobject]@{ Name = 'waired'; Id = 4321 })
        if ($one -cne 'waired (PID 4321)') { $bad += "one holder -> [$one]" }
        $two = Format-LockHolders @(
            [pscustomobject]@{ Name = 'waired'; Id = 1 },
            [pscustomobject]@{ Name = 'waired-tray'; Id = 2 })
        if ($two -cne 'waired (PID 1), waired-tray (PID 2)') { $bad += "two holders -> [$two]" }
        # The case that matters most: Get-Process cannot read .Path for another
        # user's process, so the list can come back empty on exactly the host
        # where something IS holding the file. The message must still be a
        # sentence.
        foreach ($empty in @(@(), $null)) {
            $none = Format-LockHolders $empty
            if (-not $none -or $none -match 'PID') { $bad += "empty holder list -> [$none]" }
        }
        if ($bad.Count -eq 0) { ItOk "Format-LockHolders names the holders, and says something when it cannot" }
        else { ItBad ("Format-LockHolders wrong: " + ($bad -join '; ')) }
    } else {
        ItBad "uninstall.ps1 has no Format-LockHolders (#660)"
    }

    # --- the two pre-answered setup questions --------------------------------
    # `waired init`'s --inference-enabled / --share-with-mesh are Go bool flags:
    # the space form leaves the value as a positional arg, which cobra.NoArgs
    # rejects, so install.ps1 used to kill enrolment for BOTH true and false.
    # Assert the single-token `=` spelling, and that no bare value survives.
    ItStep "install.ps1 init-answer asserts"
    $r = Invoke-Argtest @('-InferenceEnabled','false','-ShareWithMesh','TRUE')
    $initArgs = if ($r.Out -match 'InitArgs=\[([^\]]*)\]') { $Matches[1] } else { '' }
    if ($r.Exit -eq 0 -and $initArgs -match '--inference-enabled=false' -and $initArgs -match '--share-with-mesh=true') { ItOk "init answers use the = form and normalise case" }
    else { ItBad "init answers not in = form (exit $($r.Exit)) InitArgs=[$initArgs]" }
    if ($initArgs -notmatch '(^|\s)(true|false)(\s|$)') { ItOk "no bare true/false left as a positional arg (cobra.NoArgs would reject it)" }
    else { ItBad "a bare bool value survived into the init argv: [$initArgs]" }
    $r = Invoke-Argtest @('-InferenceEnabled','yes')
    if ($r.Exit -ne 0 -and $r.Out -match 'must be true or false') { ItOk "a non-bool -InferenceEnabled dies before any privileged step" }
    else { ItBad "bad -InferenceEnabled not rejected early (exit $($r.Exit)): $($r.Out.Trim())" }
    # install.sh spellings, for the same parity reason --dev / --control have them.
    $r = Invoke-Argtest @('--inference-enabled','false','--share-with-mesh=TRUE')
    $initArgs = if ($r.Out -match 'InitArgs=\[([^\]]*)\]') { $Matches[1] } else { '' }
    if ($r.Exit -eq 0 -and $initArgs -match '--inference-enabled=false' -and $initArgs -match '--share-with-mesh=true') { ItOk "--inference-enabled / --share-with-mesh fold from the install.sh spelling" }
    else { ItBad "install.sh spelling of the init answers not folded (exit $($r.Exit)) InitArgs=[$initArgs]" }

    # --- -Yes implies init --non-interactive (#166) ---------------------------
    # install.sh --help has always said --yes covers "init non-interactive";
    # Windows never applied it there, so the same documented flag drove init
    # differently per OS.
    ItStep "install.ps1 -Yes / non-interactive contract asserts (#166)"
    $r = Invoke-Argtest @('-Yes')
    $initArgs = if ($r.Out -match 'InitArgs=\[([^\]]*)\]') { $Matches[1] } else { '' }
    if ($r.Exit -eq 0 -and $initArgs -match '--non-interactive') { ItOk "-Yes forwards --non-interactive to waired init (install.sh parity)" }
    else { ItBad "-Yes did not forward --non-interactive (exit $($r.Exit)) InitArgs=[$initArgs]" }

    if ($WithInference) {
        ItStep "running install.ps1 (-Dev -SkipInit -NonInteractive; engine installed later by the Tier-2 init)"
        & $installPs1 -Dev -SkipInit -NonInteractive -LogLevel debug
    } else {
        # Set here AND left in the environment by install.ps1's own
        # Set-OllamaEnvForInit (it runs in this process, `&`). On the lean
        # legs that is exactly what we want -- they init with
        # --inference-enabled=false, so nothing reaches the engine decision
        # anyway. On -DaemonEngine it is deliberately kept through the FIRST
        # init, which is waired-agent#551's end-to-end case, and cleared
        # immediately after it so the re-init installs the engine that leg
        # exists to prove. Do not clear it here (see the #551 block below).
        $env:WAIRED_NO_OLLAMA = '1'
        ItStep "running install.ps1 (-Dev -SkipOllama -SkipInit -NonInteractive -LogLevel debug)"
        & $installPs1 -Dev -SkipOllama -SkipInit -NonInteractive -LogLevel debug
    }
    if ($LASTEXITCODE -ne 0) { ItDie "install.ps1 exited $LASTEXITCODE" }
    # install.ps1 runs in THIS session (`&`, not a child process), so its
    # Resolve-LogLevel left WAIRED_LOG_LEVEL in our environment. Clear it so
    # Tier 2 runs with a stock environment; the level under test is the one
    # baked into the service ExecStart, asserted below.
    Remove-Item Env:WAIRED_LOG_LEVEL -ErrorAction SilentlyContinue

    ItStep "Tier 1 asserts"
    $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if ($svc) { ItOk "service '$ServiceName' registered" } else { ItBad "service '$ServiceName' not registered" }
    # The service may take a beat to reach Running after install starts it.
    for ($i = 0; $i -lt 15 -and $svc -and $svc.Status -ne 'Running'; $i++) { Start-Sleep 1; $svc.Refresh() }
    if ($svc -and $svc.Status -eq 'Running') { ItOk "service Running" } else { ItBad "service not Running (status=$($svc.Status))" }
    $svcCim = Get-CimInstance Win32_Service -Filter "Name='$ServiceName'" -ErrorAction SilentlyContinue
    $startType = $svcCim.StartMode
    if ($startType -match 'Auto') { ItOk "service start mode = $startType" } else { ItBad "service start mode = $startType (want Auto)" }
    # The Windows service has no EnvironmentFile, so -LogLevel can only reach
    # the daemon by being baked into the SCM ExecStart (#164).
    if ($svcCim.PathName -match '--log-level\s+debug') { ItOk "-LogLevel debug reached the service command line" }
    else { ItBad "-LogLevel debug missing from the service command line: $($svcCim.PathName)" }

    if (Test-Path -LiteralPath (Join-Path $InstallDir 'waired.exe'))       { ItOk "waired.exe installed" }       else { ItBad "waired.exe missing in $InstallDir" }
    if (Test-Path -LiteralPath (Join-Path $InstallDir 'waired-agent.exe')) { ItOk "waired-agent.exe installed" } else { ItBad "waired-agent.exe missing in $InstallDir" }
    if (Test-Path -LiteralPath (Join-Path $InstallDir 'waired-tray.exe'))  { ItOk "waired-tray.exe installed (zip ships it, WAIRED_NO_TRAY unset)" } else { ItBad "waired-tray.exe missing in $InstallDir" }
    if (Test-Path -LiteralPath $StateDir) { ItOk "state dir present ($StateDir)" } else { ItBad "state dir missing ($StateDir)" }

    # #42: the installer must persist the resolved Control Plane URL. This
    # Tier-1 run used -SkipInit, i.e. exactly the case where `waired init` never
    # bakes the URL into identity.json -- without agent.env a later bare
    # `waired init` falls back to the baked production CP. Windows analog of
    # installtest-run.sh's /etc/waired/agent.env assert.
    #
    # (?m)^ also fails on a UTF-8 BOM, which is the encoding trap here: the Go
    # reader (cmd/waired/control_url_shared.go) scans raw lines, so a BOM'd
    # first key would never match.
    $envFile = Join-Path $StateDir 'agent.env'
    $envText = if (Test-Path -LiteralPath $envFile) { Get-Content -LiteralPath $envFile -Raw } else { '' }
    if ($envText -match "(?m)^WAIRED_CONTROL_URL=$([regex]::Escape($ControlUrl))\s*$") {
        ItOk "control URL persisted to agent.env (#42; parity with /etc/waired/agent.env)"
    } else {
        ItBad "agent.env missing or wrong at ${envFile}: want WAIRED_CONTROL_URL=$ControlUrl, got [$($envText -replace '\r?\n', '\\n')]"
    }

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
        if ($EnrollMode -ne 'authkey') { ItDie "installtest-windows.ps1 supports IT_ENROLL_MODE=authkey only (got '$EnrollMode')" }
        if (-not $ImpersonateSa)       { ItDie "IT_ENROLL_MODE=authkey needs IT_IMPERSONATE_SA (the #339 test SA)" }

        ItStep "enrolling with an auth key (host-minted SA token -> CP dev issuer)"
        # The service stays RUNNING. Since #175 an auth key is redeemed by the
        # DAEMON, so init must reach it — stopping the service would now fail
        # the run with "the background service is installed but isn't
        # responding" instead of silently enrolling locally, which is the whole
        # point of the change. Daemon-path (-DaemonEngine) mode already relied
        # on the service being up for the setup executor (waired#835 §11).

        $aud = (Invoke-RestMethod -Uri "$ControlUrl/v1/login/oidc-grant/audience" -TimeoutSec 15).audience
        if (-not $aud) { ItDie "could not resolve the OIDC audience from $ControlUrl/v1/login/oidc-grant/audience" }
        ItLog "minting SA id_token (sa=$ImpersonateSa)"
        $tok = (& gcloud auth print-identity-token --impersonate-service-account="$ImpersonateSa" --audiences="$aud" --include-email).Trim()
        if (-not $tok) { ItDie "failed to mint an SA id_token (is the CI principal in oidc_grant_token_creators on $ImpersonateSa?)" }

        # Exchange the token for a reusable auth key. -DaemonEngine keeps using
        # the raw token: that leg completes an ordinary login session
        # out-of-band so the executor lease has an in-flight window to work in,
        # which an auth key would collapse.
        $authKey = (Invoke-RestMethod -Uri "$ControlUrl/test/auth-key" -Method Post `
            -ContentType 'application/json' -TimeoutSec 30 `
            -Body (@{ id_token = $tok; reusable = $true; description = 'installtest windows' } | ConvertTo-Json -Compress)).auth_key
        if (-not $authKey) { ItDie "could not mint an auth key at $ControlUrl/test/auth-key (is the CP new enough - waired#976?)" }

        $runId  = if ($env:GITHUB_RUN_ID) { $env:GITHUB_RUN_ID } else { Get-Date -Format yyyyMMddHHmmss }
        $device = "win-ci-$runId"
        $waired = Join-Path $InstallDir 'waired.exe'
        $initLog = Join-Path $Work 'init.log'
        if ($DaemonEngine) {
            # Daemon-path enrol: complete the login out-of-band so the resident
            # executor installs the engine (waired#835 §9/§11). No credential
            # flag (an auth key would collapse the window); the running
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
                # 240 x 2 s = 8 min. It used to be 150 and the job was
                # stopped the moment the first init returned; the executor's
                # engine install now happens in the SECOND init (the #313
                # re-init below), a minute or two further along, so the
                # window this loop has to cover is the whole of both plus
                # the Tier-2 asserts between them.
                $seenExec = $false; $seenClaim = $false
                for ($i = 0; $i -lt 240; $i++) {
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
            # ensureDaemonPathEngine. No credential flag -> daemon path.
            $initArgs = @(
                'init'
                '--control', $ControlUrl
                '--device-name', $device
                '--inference-enabled=true'
                '--inference-bundled-model-id=granite4-350m'
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
            # The watcher is deliberately NOT stopped here (it was, until
            # #551): the engine install it has to observe now happens in the
            # re-init further down. Torn down after that block.

            # --- waired-agent#551: the engine opt-out is not a failed init ---
            # This init ran with WAIRED_NO_OLLAMA still set -- install.ps1 is
            # invoked with `&`, in THIS process, so its Set-OllamaEnvForInit
            # left the variable in our environment (same leak the
            # WAIRED_LOG_LEVEL cleanup above exists for). That was an
            # accident, and it is the only place in CI that reaches the
            # executor's opt-out arm at all, so it is kept ON PURPOSE for
            # this one init and asserted, then cleared.
            #
            # Before #551 all three of these were wrong on this leg: init
            # exited 3, and the four asserts downstream failed because the
            # executor had been told not to install the engine the leg
            # exists to prove it installs.
            ItStep "engine opt-out asserts (waired-agent#551)"
            if ($initExit -eq 0) {
                ItOk "daemon-path init with engine installs turned off exits 0 (waired-agent#551)"
            } else {
                ItBad "daemon-path init exited $initExit with engine installs turned off; an opt-out the operator configured is not a failed init -- see $initLog"
            }
            # Anti-vacuity: proves the executor REACHED the opt-out arm. Without
            # it, a daemon that answered `disabled` would satisfy the exit-0 and
            # no-engine asserts while never running the code under test.
            # -SimpleMatch on both, and it is load-bearing: Select-String's
            # -Pattern is a .NET REGEX, so the parentheses in the shared
            # literal would be a capture group and the pattern would look for
            # "skipped WAIRED_NO_OLLAMA" with no brackets -- text that never
            # appears. grep's BRE treats them literally, which is why only the
            # Windows copy was wrong. Escaping instead is not an option: one
            # literal is shared by all three harnesses (see the declarations
            # above), and `\(` in BRE means the opposite of what it means here.
            $skipped = Select-String -Path $initLog -Pattern $EngineOptOutRe -SimpleMatch -Quiet -ErrorAction SilentlyContinue
            if ($skipped) {
                ItOk "the executor reached the opt-out arm and said so (Engine install skipped)"
            } else {
                ItBad "init never reported the engine install as skipped -- the opt-out arm was not reached, so the asserts around it prove nothing. See $initLog"
                Get-Content -LiteralPath $initLog -Tail 20 -ErrorAction SilentlyContinue |
                    ForEach-Object { ItLog "    init| $_" }
            }
            $calledItFailed = Select-String -Path $initLog -Pattern $InstallFailureBoxRe -SimpleMatch -Quiet -ErrorAction SilentlyContinue
            if ($calledItFailed) {
                ItBad "init called the operator's own opt-out a failed install -- see $initLog"
            } else {
                ItOk "init does not report the opt-out as a failed install"
            }
            $optOutBin = Join-Path $StateDir 'runtimes\ollama\bin\ollama.exe'
            if (Test-Path -LiteralPath $optOutBin) {
                ItBad "an engine is installed at $optOutBin despite WAIRED_NO_OLLAMA -- the opt-out was not honoured"
            } else {
                ItOk "no engine was installed while the opt-out was set (and the install below is the executor's)"
            }

            # Now clear it, so the rest of the leg does what it exists to do:
            # the #313 re-init below runs opt-out-free against a daemon that
            # still wants an engine (this init turned inference on through the
            # mgmt route), so the resident executor installs it.
            Remove-Item Env:WAIRED_NO_OLLAMA -ErrorAction SilentlyContinue
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
            '--auth-key', $authKey
            '--device-name', $device
            '--non-interactive'
            $inferFlag
            '--skip-integration'
            '--state-dir', $StateDir
        )
        # Routing sentinel pins the withheld 350M as the bundled model (deploy pulls ~0.7 GB).
        if ($WithIntegration) { $initArgs += '--inference-bundled-model-id=granite4-350m' }
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
        # Three outcomes, not two (#310): 0 signed in, 3 signed in but this
        # host has no local AI, anything else failed. Only
        # lib/installtest-enroll.sh learned that; this leg kept reading 3 as an
        # outright failure, which would fail a host that enrolled perfectly on
        # every non-inference tier (#505).
        switch ($initExit) {
            0 { }
            3 {
                # A tier that asked for local inference and did not get it IS a
                # failure: that is the thing that tier exists to verify.
                #
                # The other arm is ItOk, not ItLog: it IS an assertion -- that
                # init honours #310's exit-code contract on a host that never
                # asked for an engine. Counting it also keeps the assert-count
                # floor stable, since ItLog moves no counter and would leave
                # the -Contract leg one short of its 80 the day init starts
                # exiting 3 there.
                if ($WithInference) {
                    ItBad "waired init (authkey) enrolled but local AI is not running, and this tier asked for it -- see $initLog"
                } else {
                    ItOk "waired init (authkey) enrolled; local AI is not running here (expected: this tier did not ask for it)"
                }
            }
            default { ItBad "waired init (authkey) exited $initExit -- see $initLog" }
        }
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
        $enrolled = Wait-Enrolled
        if ($enrolled) { ItOk "daemon read the enrolled state and reports an identity" }
        else { ItBad "daemon did not report enrolled" }

        # waired-agent#313: re-init on an enrolled device. The point of the
        # leg is what it does NOT pass -- no --state-dir, the way an operator
        # types it, and the way NAVI prescribes it to resume a stuck setup.
        # That is exactly what used to fail on every Windows box: the CLI
        # resolved %AppData%\waired, found no identity, asked for a plain
        # login, and reported the daemon's idempotent no-op as
        # "daemon did not return a login session id".
        #
        # The auth key is deliberately still passed: an already-signed-in
        # device must not spend it (the `tailscale up` rule), and must say so.
        #
        # $inferFlag is passed for the same reason the Linux twin passes it
        # (lib/installtest-enroll.sh, assert_reinit_resumes): this leg must
        # leave the host's local-AI posture exactly as it found it, in EITHER
        # direction. Leaving the toggle unset hands the decision to
        # install-flow step 6, and GitHub-hosted runners are genuinely below
        # the recommended spec -- 268.9 s per coding question against a 45 s
        # budget on this very leg (run 31330389679). Step 6 then turns local
        # AI off, correctly, and Assert-Inference's "local inference is on"
        # fails for a reason that has nothing to do with #313. It began only
        # when init learned to READ the prefill bound (waired-agent#579
        # Stage 3c); the behaviour is the fix, not the bug.
        #
        # A bare 'true' would be wrong in the other direction, for the reason
        # recorded on the Linux twin: it installs an engine, which is a
        # postcondition the lean legs depend on.
        if ($authKey) {
            ItStep "re-init on an enrolled device (waired-agent#313)"
            $reinitLog  = Join-Path $Work 'reinit.log'
            $reinitArgs = @(
                'init'
                '--control', $ControlUrl
                '--auth-key', $authKey
                '--device-name', $device
                '--non-interactive'
                # Recomputed rather than reusing $inferFlag: that one is
                # assigned inside the non-DaemonEngine branch above, so on the
                # -DaemonEngine leg it would splat as $null and hand
                # `waired init` an empty argument (the #613 shape).
                $(if ($WithInference) { '--inference-enabled=true' } else { '--inference-enabled=false' })
                '--skip-integration'
            )
            $env:WAIRED_NO_EMOJI = '1'
            $prevEap = $ErrorActionPreference
            $ErrorActionPreference = 'Continue'
            & $waired @reinitArgs 2>&1 | Tee-Object -FilePath $reinitLog
            $reinitExit = $LASTEXITCODE
            $ErrorActionPreference = $prevEap

            ItSoft '313' ($reinitExit -eq 0) "re-init on an enrolled device exits 0 (no --state-dir)" -Repo 'waired-agent'
            $resumed = Select-String -Path $reinitLog -Pattern 'resuming setup' -Quiet -ErrorAction SilentlyContinue
            ItSoft '313' ([bool]$resumed) "re-init resumes setup instead of starting a sign-in" -Repo 'waired-agent'
            $keyNoted = Select-String -Path $reinitLog -Pattern 'auth key was not used' -Quiet -ErrorAction SilentlyContinue
            ItSoft '313' ([bool]$keyNoted) "re-init says the auth key went unused" -Repo 'waired-agent'
            if (-not $resumed) { Get-Content -LiteralPath $reinitLog -Tail 20 | ForEach-Object { ItLog "    reinit| $_" } }
        }

        # The step-4 twin's other half and the models-pull twin
        # (waired-agent#590). Lean leg only, for the same reason the #551
        # opt-out probe is: this host still has no engine, so every arm they
        # test is reachable -- and for the pull twin, an engine-less host is
        # what keeps the honoured row from downloading anything.
        if (-not $DaemonEngine -and -not $WithInference) {
            ItStep "below-spec default asserts (waired-agent#590)"
            Assert-ReinitDefaultUnfit -Waired $waired -Device $device -ControlUrl $ControlUrl
            ItStep "models-pull confirmation asserts (waired-agent#590)"
            Assert-ModelsPullConfirm -Waired $waired
        }

        # Watcher teardown. It used to happen the moment the enrolling init
        # returned; on -DaemonEngine the executor's engine install is the
        # re-init just above, so the job has to outlive it or
        # executor_attached / install_claimed can never be observed (#551).
        if ($DaemonEngine -and $watcher) {
            Stop-Job $watcher -ErrorAction SilentlyContinue
            Receive-Job $watcher -ErrorAction SilentlyContinue | Out-Null
            Remove-Job $watcher -Force -ErrorAction SilentlyContinue
        }

        # Cheap and fast, so they run before the minutes-long inference asserts.
        ItStep "service recovery-policy assert (waired#315)"
        Assert-ServiceRecoveryFlag

        ItStep "management write pipe asserts (waired#838)"
        Assert-MgmtPipe

        # LAST of the engine-less probes, because it is the one that ends this
        # host's engine-less life: it installs one (waired-agent#590).
        if ($EngineOnly) {
            ItStep "engine installed, no model chosen (waired-agent#590)"
            Assert-EngineOnlyInstall -Waired $waired -Device $device -ControlUrl $ControlUrl
        }

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
                & go test -tags integration -count=1 -v -timeout 15m ./internal/e2e/integration/...
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
            ItSkip "non-elevated context asserts: running as SYSTEM, where CreateProcessWithLogonW is unavailable"
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
        # (#630) A dry run is a preview, so it must not list removals that
        # cannot happen. Run FIRST, before the OLLAMA_* seeds below plant the
        # artifacts the teardown needs: at this point the runner has no Ollama
        # of any kind, which is the host the issue was reported on.
        ItStep "uninstall.ps1 -DryRun previews only what exists (#630)"
        # *>&1, not 2>&1 -- see the locked-binary leg below: the uninstaller
        # reports through Write-Host, which is the information stream, so
        # capturing only errors captures nothing and the three "does not
        # announce" asserts below would pass against an empty string.
        $dryOut = (& (Join-Path $Root 'packaging\install\uninstall.ps1') -Clean -Yes -DryRun *>&1 | Out-String)
        $dryRc  = $LASTEXITCODE
        Write-Host $dryOut   # captured, so echo it or CI sees nothing
        ItSoft '630' ($dryRc -eq 0) "uninstall.ps1 -DryRun exits 0 (got $dryRc)" 'waired-agent'
        ItSoft '630' ($dryOut -match 'Ollama not present') `
            'uninstall.ps1 -DryRun says Ollama is not present instead of announcing its removal' 'waired-agent'
        ItSoft '630' ($dryOut -notmatch 'Removing Ollama') `
            'uninstall.ps1 -DryRun does not announce removing an Ollama that is not installed' 'waired-agent'
        ItSoft '630' ($dryOut -notmatch 'clear OLLAMA_') `
            'uninstall.ps1 -DryRun does not announce clearing env vars that are not set' 'waired-agent'
        # A dry run that removed something would make every assert after it
        # meaningless, and the teardown below is what proves the real run works.
        if (Test-Path -LiteralPath $InstallDir) { ItOk "-DryRun changed nothing" }
        else { ItBad "-DryRun removed $InstallDir" }

        # (#660) The false-success chain, staged with a planted victim in the
        # same spirit as the seeds below. Hold waired.exe open with no sharing
        # so Windows refuses to delete it -- the same refusal an orphaned
        # `waired init` produced on the reported host -- and assert the
        # uninstaller says so and exits non-zero, instead of printing "Waired
        # fully removed" and exiting 0 over a 13 MB binary still on disk.
        #
        # Runs before the real teardown below, and leaves the host in exactly
        # the wrecked half-state #660 is about: no service, binary present.
        ItStep "uninstall.ps1 with a locked binary fails loudly (#660)"
        $lockedExe = Join-Path $InstallDir 'waired.exe'
        $lock = $null
        try { $lock = [System.IO.File]::Open($lockedExe, 'Open', 'Read', 'None') } catch { }
        if (-not $lock) {
            ItBad "could not lock $lockedExe, so the #660 leg proves nothing"
        } else {
            try {
                # *>&1, not 2>&1: the uninstaller reports through Write-Host,
                # which is the information stream. Capturing only errors caught
                # none of its output, and the "does not say fully removed"
                # assert below would then have passed against an empty string.
                $lockedOut = (& (Join-Path $Root 'packaging\install\uninstall.ps1') -Clean -Yes *>&1 | Out-String)
                $lockedRc  = $LASTEXITCODE
                Write-Host $lockedOut   # captured, so echo it or CI sees nothing
                ItSoft '660' ($lockedRc -ne 0) `
                    "uninstall.ps1 exits non-zero when it could not delete the binary (got $lockedRc)" 'waired-agent'
                ItSoft '660' ($lockedOut -notmatch 'fully removed') `
                    'uninstall.ps1 does not claim "fully removed" over a binary it left behind' 'waired-agent'
                ItSoft '660' ($lockedOut -match [regex]::Escape($InstallDir) + '.*could not be removed') `
                    'uninstall.ps1 names the path it could not remove' 'waired-agent'
                # Naming the holding process is best-effort by design: the lock
                # above is a file handle held by this harness, not a running
                # image under InstallDir, so Get-LockHolders correctly finds
                # nothing and the message falls back to its "could not identify"
                # wording. What must always hold is that the reason is stated.
                ItSoft '660' ($lockedOut -match 'still in use by') `
                    'uninstall.ps1 says why the removal failed' 'waired-agent'
                if (Test-Path -LiteralPath $lockedExe) { ItOk "the locked binary is still there, as the assert above claims" }
                else { ItBad "the locked binary vanished; the lock did not hold and the asserts above are vacuous" }
            }
            finally {
                $lock.Dispose()
            }
        }

        ItStep "teardown = uninstall.ps1 -Clean + asserts (waired#754 soft)"
        # Seed the GPU-backend machine env vars Set-MachineVulkanFlag writes on a
        # Vulkan/iGPU host. CI runners have no such GPU so the install leg never
        # sets them, which would make the clear-after-uninstall asserts below
        # vacuous -- seed them here so -Clean's Remove-Ollama is actually exercised.
        [Environment]::SetEnvironmentVariable('OLLAMA_VULKAN', '1', 'Machine')
        [Environment]::SetEnvironmentVariable('OLLAMA_IGPU_ENABLE', '1', 'Machine')
        # (#191) Same reasoning for the download staging directory: the CI
        # install leg succeeds, so it never leaves one behind, and the sweep
        # assert below would be vacuous without a planted victim.
        $stageProbe = Join-Path $env:TEMP ('ollama-stage-' + [Guid]::NewGuid().ToString('N'))
        New-Item -ItemType Directory -Path $stageProbe -Force | Out-Null
        Set-Content -LiteralPath (Join-Path $stageProbe 'ollama-windows-amd64.zip') -Value 'stub'
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
        # (#191) -Clean reclaims the ~1.4 GB staging directories a killed
        # engine download used to leave behind forever (seeded above).
        if (-not (Test-Path -LiteralPath $stageProbe)) { ItOk "ollama staging directories swept (-Clean)" }
        else {
            ItBad "ollama-stage-* remains after -Clean ($stageProbe)"
            Remove-Item -LiteralPath $stageProbe -Recurse -Force -ErrorAction SilentlyContinue
        }

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
        & $iscc "/DAppVersion=$semver" (Join-Path $Root 'packaging\windows\waired-setup.iss') | Select-Object -Last 3 | Out-Host
        if ($LASTEXITCODE -ne 0) { ItDie "ISCC exited $LASTEXITCODE" }
        $setup = Join-Path $Root "dist\WairedSetup-$semver-x64.exe"
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
# Restricted-context scratch: test user + profile + C:\Users\Public\waired-it.
# Best-effort — the guest is disposable; done AFTER the #754 asserts, which
# inspect the test user's %AppData%.
#
# Not keyed on -Contract alone: the #195 Test-Admin asserts also reach
# Invoke-AsStandardUser, which creates the account lazily. $TestUserPw is set
# exactly when that happened, so it is the precise condition — otherwise a
# plain `-Tier 2` run on a dev box would leave a local account behind.
if ($Contract -or $script:TestUserPw) {
    Remove-Item -LiteralPath $PubWork -Recurse -Force -ErrorAction SilentlyContinue
    if (Get-LocalUser -Name $TestUser -ErrorAction SilentlyContinue) {
        Get-CimInstance Win32_UserProfile -ErrorAction SilentlyContinue |
            Where-Object { $_.LocalPath -like "*\$TestUser" } |
            Remove-CimInstance -ErrorAction SilentlyContinue
        Remove-LocalUser -Name $TestUser -ErrorAction SilentlyContinue
    }
}

Write-Host ""
ItStep "Tier $Tier summary: $script:Pass passed, $script:Fail failed, $script:Warn warn (open-issue soft asserts), $script:Skip skipped"
if ($script:Warn -gt 0) {
    $script:WarnLines | ForEach-Object { Write-Host "[installtest]   WARN $_" -ForegroundColor Yellow }
}
if ($script:Skip -gt 0) {
    $script:SkipLines | ForEach-Object { Write-Host "[installtest]   SKIP $_" -ForegroundColor Yellow }
}

# Assert-count floor (#215). Zero failures is not the same as having tested
# anything: a block that stops running -- an early return, a guard that
# silently opts out, a helper that stops being called -- subtracts asserts
# without ever printing FAIL, and the leg reports success. That is the shape
# behind "the leg said ok while the reason sat in the same log".
#
# The floor is PER CONFIGURATION, not per tier. That distinction was missing
# and it made every nightly Windows leg red (#505): 80 was measured from
# `-Tier 2 -Contract -ExeVariant`, which is what installtest.yml runs, and was
# then applied to every `$Tier -ge 2` run on the reasoning that "-Contract /
# -ExeVariant only ADD asserts, so a leaner invocation still clears it". The
# two halves of that contradict each other -- if 80 already counts what those
# switches add, a run without them executes fewer. installtest-inference.yml's
# three legs pass none of them, and in run 30998191050 they executed 67
# (-WithInference), 68 (-DaemonEngine) and 68 (-WithIntegration). All three
# reported "only N asserts ran at tier 2; at least 80 must" on top of whatever
# else was wrong with them.
#
# -Contract: 80, unchanged and still MEASURED from a green run of that exact
# configuration -- 78, then 80 once the #314 exit-code decode block added its
# 2 unconditional asserts (the install/uninstall drift check and the decode
# table), both in the same always-run Tier-1 section as the
# ConvertTo-NativeArg pair above them.
#
# Everything else at tier 2: 71, and as of waired-agent#551 this IS a measured
# green floor. It was 65 -- an unmeasured lower bound, because no Windows
# nightly leg had ever been green and there was no run to read a number from.
# The note here asked for the real figure from the first PR with a green
# `gh workflow run installtest-inference.yml -f os=windows`; run 31241725159
# is that run, and its three legs executed:
#
#   -WithInference    71   <- the minimum, and therefore the floor
#   -WithIntegration  72
#   -DaemonEngine     77   (+4 from #551's engine-opt-out block)
#
# The floor is the MINIMUM across the configurations that share it, not the
# largest: every one of them has to clear it. Re-measure the same way when a
# leg gains or loses an assert that always runs.
#
# Tier 1 deliberately has NO floor: CI only ever runs -Tier 2, so there is no
# green tier-1 run to take a number from, and a guessed floor is either
# useless (too low) or a spurious red (too high). Measure one and add it
# rather than estimating.
#
# waired-agent#590 adds a lean-only block, the way #551 did: the below-spec
# default probe (4) and the models-pull twin (5) run only where neither
# -DaemonEngine nor -WithInference is set, because both need a host with no
# engine. The configurations behind the 71 floor all set one of those, so 71
# is unaffected; -Contract is the per-PR configuration and does run them, so
# it goes 80 -> 89. Measured, not estimated: 80 was a green -Contract run and
# the two probes contribute exactly 9 unconditional asserts between them
# (their no-catalog arms report the same 5, on purpose).
#
# waired-agent#573's host-speed assert moves 71, and ONLY 71. While #579 was
# open it was an ItSoft that contributed 0 on a leg where no measurement was
# published; now that $ContractBlocking['579'] is $true it contributes 1
# either way, so the minimum for the configuration that runs it rises by one.
#
# Which configuration is that? Measured on run 31330389679, by counting the
# assert's own line across every Windows leg:
#
#   install+inference          runs it   71 passed + 1 failed = 72 executed
#   daemon-path engine install does not  77
#   engine installed, no model does not  75
#
# So -WithInference (the 71) goes to 72 and the other two are untouched. The
# one failure in that leg was Assert-Inference's "local inference is on",
# which the re-init argv fix in this same PR turns back into a pass — the
# executed count is 72 either way, because ItSoft counts on both arms.
#
# waired-agent#590 also adds -EngineOnly, and that one could NOT be derived
# from either number above. It is the first Windows configuration that is lean
# WITHOUT -Contract: the 71 behind -WithInference counts Assert-Inference's tail
# and none of the lean block, the 89 behind -Contract counts the lean block and
# a pile of contract asserts, and neither difference is separable by arithmetic.
# So it is MEASURED, the way this file has asked for since #505: run
# 31316424716's -EngineOnly leg executed 75 (75 passed, 0 failed, 0 warn,
# 0 skipped) and that is the floor.
#
#
# waired-agent#660 moves all three, by arithmetic on unconditional asserts --
# the same basis as the #314 note above (78 -> 80), not an estimate:
#
#   +2 everywhere: Test-InstallComplete and Format-LockHolders sit in the same
#      always-run Tier-1 block as the ConvertTo-NativeArg / Get-ExitCodeReason
#      pairs, and each reports exactly one assert. 89 -> 91, 75 -> 77, 72 -> 74.
#   +5 under -Contract only: the locked-binary teardown leg reports 4 ItSoft
#      (blocking, so one assert on either arm) plus the "the locked binary is
#      still there" check. 91 -> 96. Its could-not-lock arm reports 1 instead
#      of 5, but that arm is an ItBad and exits non-zero regardless, so the
#      floor is measured against the green path as usual.
#
# waired-agent#630 adds 5 more, -Contract only: the -DryRun preview leg's 4
# ItSoft plus its "changed nothing" check, all unconditional within the block.
# 96 -> 101.
#
# Raise these when you add an assert that always runs; lower one, in the same
# commit and with the reason, if a leg legitimately becomes conditional.
$executed = $script:Pass + $script:Fail
if ($Tier -ge 2) {
    $floor = if ($Contract) { 101 } elseif ($EngineOnly) { 77 } else { 74 }
    if ($executed -lt $floor) {
        Write-Host ("[installtest] FAIL only {0} asserts ran at tier {1}; at least {2} must (a block stopped executing -- see the assert-count floor in installtest-windows.ps1)" -f $executed, $Tier, $floor) -ForegroundColor Red
        exit 1
    }
}

if ($script:Fail -gt 0) { exit 1 }
