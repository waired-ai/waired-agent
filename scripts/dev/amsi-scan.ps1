#!/usr/bin/env pwsh
<#
.SYNOPSIS
  Scan the installer PowerShell scripts through the live Defender AMSI engine —
  the same verdict path `iex` consults — to catch a loader-shaped literal that
  would get `iwr ... | iex` blocked on stock Windows (#552 / #553).

.DESCRIPTION
  install.ps1 and ollama-windows.ps1 ship to users via `iwr | iex` / fetch+run.
  When the body is handed to Invoke-Expression, PowerShell passes the WHOLE
  script text to AMSI; a contiguous download-decode-execute cradle in the body
  can get the entire script blocked ("This script contains malicious content").
  #552 was exactly that. installtest.yml never exercises the AMSI path (it runs
  a call-operator on the on-disk file, elevated, with WAIRED_NO_OLLAMA=1), so
  this is the dedicated AMSI gate.

  The scan calls AmsiScanString on the file *content* with the app name
  "PowerShell" (Defender's PowerShell-script signatures are only in scope under
  that app name) — identical to what `iex` triggers, without executing the
  installer. A path/extension Defender exclusion on the checkout dir does NOT
  suppress this: the content is scanned as an in-memory string, not as a file
  sitting at an excluded path, so this works even inside the #547 golden whose
  Provision-Golden.ps1 bakes ProgramFiles\Waired / $RunnerDir / .exe/.dll/.zip
  exclusions.

  POSITIVE CONTROL: a live AMSI provider is required for a *clean* verdict to
  MEAN anything. GH-hosted windows-latest frequently has Defender off, which
  makes AmsiScanString return clean for everything (false-green). Before
  trusting any target verdict we scan Microsoft's documented AMSI test sample,
  which any live provider MUST flag. If it comes back clean we have no live
  provider and act per -OnNoProvider.

.PARAMETER Path
  Script(s) to scan. Default: the two installer scripts that ship to users.

.PARAMETER OnNoProvider
  What to do when the positive control does NOT fire (no live AMSI provider):
  'skip' (default) exits 0 with a warning — correct for best-effort legs like
  windows-latest; 'fail' exits non-zero — correct for a Defender-live box (the
  #547 golden) where a missing provider is itself a misconfiguration.

.PARAMETER Strict
  Treat a target detection as a hard failure (non-zero exit). Default: a target
  detection is reported as a warning but does NOT fail the run, because AMSI
  verdicts are non-deterministic (engine + signature version + cloud-delivered
  protection) and a definitions update alone can flip a verdict (#552: the very
  cradle that blocked a user scanned clean hours later).

.PARAMETER ClearExclusionsForScan
  Belt-and-braces for the Defender-live golden: transiently remove every
  Defender path exclusion for the duration of the scan and restore it after
  (needs elevation + the Defender module). The string-scan above is already
  exclusion-independent, so this is defensive only; it is a best-effort no-op
  when the Defender cmdlets are unavailable (e.g. windows-latest with Defender
  off).

.EXAMPLE
  pwsh -File scripts/dev/amsi-scan.ps1
  Scan the two installer scripts; skip (exit 0) if no live AMSI provider.

.EXAMPLE
  pwsh -File scripts/dev/amsi-scan.ps1 -Strict -OnNoProvider fail -ClearExclusionsForScan
  Defender-live canary (Gate B): a missing provider or a target detection fails.

.NOTES
  Manual companion + caveats: packaging/install/README.md (Verification →
  Windows — AMSI / Defender pre-publish scan). Tracking: #553.
#>
[CmdletBinding()]
param(
    [string[]] $Path,
    [ValidateSet('skip', 'fail')] [string] $OnNoProvider = 'skip',
    [switch] $Strict,
    [switch] $ClearExclusionsForScan
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# Exit codes (kept stable so CI / callers can branch on them).
$EXIT_OK = 0          # provider live + every target clean (or detection & not -Strict)
$EXIT_SETUP = 1       # unexpected setup/interop failure
$EXIT_DETECTED = 2    # a target tripped AMSI and -Strict was set
$EXIT_NO_PROVIDER = 3 # positive control did not fire and -OnNoProvider fail

# AMSI verdict >= AMSI_RESULT_DETECTED means blocked. AMSI_RESULT_CLEAN = 0,
# AMSI_RESULT_NOT_DETECTED = 1, AMSI_RESULT_DETECTED = 32768.
$AMSI_RESULT_DETECTED = 32768

function Write-Ann {
    # GitHub Actions workflow-command annotation (plain text off-CI).
    param([ValidateSet('error', 'warning', 'notice')] [string] $Level, [string] $Message)
    Write-Host "::${Level}::${Message}"
}

# --- Default targets: the scripts that reach the AMSI path in the user's hands.
if (-not $Path -or $Path.Count -eq 0) {
    $repoRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)  # scripts/dev -> repo root
    $Path = @(
        (Join-Path $repoRoot 'packaging/install/install.ps1'),
        (Join-Path $repoRoot 'scripts/install/ollama-windows.ps1')
    )
}

# --- AMSI interop (same three imports the README manual procedure documents).
if (-not ('WairedAmsi' -as [type])) {
    Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
public static class WairedAmsi {
  [DllImport("amsi.dll", CharSet = CharSet.Unicode)] public static extern int AmsiInitialize(string appName, out IntPtr context);
  [DllImport("amsi.dll")] public static extern int AmsiOpenSession(IntPtr context, out IntPtr session);
  [DllImport("amsi.dll", CharSet = CharSet.Unicode)] public static extern int AmsiScanString(IntPtr context, string content, string contentName, IntPtr session, out int result);
  [DllImport("amsi.dll")] public static extern void AmsiCloseSession(IntPtr context, IntPtr session);
  [DllImport("amsi.dll")] public static extern void AmsiUninitialize(IntPtr context);
}
'@
}

$script:amsiCtx = [IntPtr]::Zero
$script:amsiSession = [IntPtr]::Zero

function Initialize-Amsi {
    $ctx = [IntPtr]::Zero
    # App name "PowerShell" puts Defender's PowerShell-script signatures in
    # scope — a custom app name slips past them and everything looks clean.
    $hr = [WairedAmsi]::AmsiInitialize('PowerShell', [ref]$ctx)
    if ($hr -ne 0 -or $ctx -eq [IntPtr]::Zero) { throw "AmsiInitialize failed (hr=0x$($hr.ToString('x8')))" }
    $session = [IntPtr]::Zero
    $hr = [WairedAmsi]::AmsiOpenSession($ctx, [ref]$session)
    if ($hr -ne 0) { [WairedAmsi]::AmsiUninitialize($ctx); throw "AmsiOpenSession failed (hr=0x$($hr.ToString('x8')))" }
    $script:amsiCtx = $ctx
    $script:amsiSession = $session
}

function Close-Amsi {
    if ($script:amsiSession -ne [IntPtr]::Zero) { [WairedAmsi]::AmsiCloseSession($script:amsiCtx, $script:amsiSession); $script:amsiSession = [IntPtr]::Zero }
    if ($script:amsiCtx -ne [IntPtr]::Zero) { [WairedAmsi]::AmsiUninitialize($script:amsiCtx); $script:amsiCtx = [IntPtr]::Zero }
}

function Get-AmsiResult {
    # Return the AMSI_RESULT for a string, or 0 ("no verdict") when the provider
    # could not scan it. A non-S_OK HRESULT from AmsiScanString means no verdict
    # was produced — almost always "no live AMSI provider" (0x80070015
    # ERROR_NOT_READY on a box whose Defender AMSI provider is absent, e.g.
    # GH-hosted windows runners). Treat that as 0 rather than throwing: the
    # positive control turns a 0 here into the -OnNoProvider decision, so a
    # missing provider self-skips (Gate A) or fails cleanly (Gate B) instead of
    # looking like a scanner crash. Also compile the string via
    # [ScriptBlock]::Create (PowerShell's own AMSI integration) as a second
    # signal when a provider IS live: a real block throws there even if the
    # string scan under our content name did not. We report the stronger of the two.
    param([string] $Content, [string] $Name)
    $result = 0
    $hr = [WairedAmsi]::AmsiScanString($script:amsiCtx, $Content, $Name, $script:amsiSession, [ref]$result)
    if ($hr -ne 0) {
        Write-Verbose "AmsiScanString('$Name') hr=0x$($hr.ToString('x8')) — no verdict (treating as no live provider)."
        return 0
    }
    if ($result -lt $AMSI_RESULT_DETECTED) {
        try { [void][ScriptBlock]::Create($Content) }
        catch {
            if ($_.Exception.Message -match 'malicious content|antivirus|AMSI') { $result = $AMSI_RESULT_DETECTED }
        }
    }
    return $result
}

# --- Positive control: Microsoft's documented AMSI test sample. Assembled from
#     fragments at runtime so this scanner file does not itself carry the
#     contiguous trigger literal (a live real-time provider would flag it on
#     checkout/write, and gh rejected posting the literal in a comment — #553).
function Get-AmsiTestSample {
    return 'AMSI Test' + ' Sample: ' + '7e72c3ce-' + '861b-4339-' + '8740-' + '0ac1484c1386'
}

# --- Optional transient un-exclusion (Gate B belt-and-braces; see -help).
function Invoke-WithoutDefenderExclusions {
    param([scriptblock] $Body)
    if (-not $ClearExclusionsForScan) { return & $Body }
    $saved = @()
    $canManage = $false
    try { $saved = @((Get-MpPreference -ErrorAction Stop).ExclusionPath); $canManage = $true }
    catch { Write-Ann warning "ClearExclusionsForScan: Defender cmdlets unavailable ($($_.Exception.Message)); scanning with exclusions in place."; return & $Body }
    try {
        foreach ($p in $saved) { if ($p) { try { Remove-MpPreference -ExclusionPath $p -ErrorAction Stop } catch {} } }
        return & $Body
    }
    finally {
        if ($canManage) { foreach ($p in $saved) { if ($p) { try { Add-MpPreference -ExclusionPath $p -ErrorAction Stop } catch {} } } }
    }
}

# --- Run.
$exit = $EXIT_OK
try {
    Initialize-Amsi

    Invoke-WithoutDefenderExclusions {
        # 1. Positive control — is there a live provider at all?
        $ctrl = Get-AmsiResult -Content (Get-AmsiTestSample) -Name 'amsi-positive-control'
        if ($ctrl -lt $AMSI_RESULT_DETECTED) {
            $msg = "AMSI positive control did NOT fire (result=$ctrl): no live AMSI provider on this runner. A clean verdict here is meaningless."
            if ($OnNoProvider -eq 'fail') {
                Write-Ann error "$msg (-OnNoProvider fail)"
                $script:exit = $EXIT_NO_PROVIDER
            }
            else {
                Write-Ann warning "$msg Skipping (-OnNoProvider skip)."
                $script:exit = $EXIT_OK
            }
            return
        }
        Write-Host "positive control: DETECTED (result=$ctrl) — live AMSI provider confirmed."

        # 2. Scan each target.
        $detected = @()
        foreach ($file in $Path) {
            if (-not (Test-Path -LiteralPath $file)) {
                Write-Ann error "target not found: $file"
                $script:exit = $EXIT_SETUP
                continue
            }
            $abs = (Resolve-Path -LiteralPath $file).Path
            $src = Get-Content -Raw -LiteralPath $abs
            $name = Split-Path -Leaf $abs
            $r = Get-AmsiResult -Content $src -Name $name
            if ($r -ge $AMSI_RESULT_DETECTED) {
                Write-Host "  [BLOCKED] $name (result=$r)"
                $detected += $name
            }
            else {
                Write-Host "  [clean]   $name (result=$r)"
            }
        }

        if ($detected.Count -gt 0) {
            $list = $detected -join ', '
            if ($Strict) {
                Write-Ann error "AMSI flagged: $list. This would get 'iwr | iex' blocked on stock Windows Defender (see #552). Use AMSITrigger/ThreatCheck to bisect the offending bytes; route loader-shaped literals through a temp .ps1 + -File."
                if ($script:exit -eq $EXIT_OK) { $script:exit = $EXIT_DETECTED }
            }
            else {
                Write-Ann warning "AMSI flagged: $list. Verdicts are non-deterministic; investigate with AMSITrigger/ThreatCheck. (soft — pass -Strict to fail)"
            }
        }
        elseif ($script:exit -eq $EXIT_OK) {
            Write-Host "OK: all targets clean under a live AMSI provider."
        }
    }
}
catch {
    Write-Ann error "amsi-scan failed: $($_.Exception.Message)"
    $exit = $EXIT_SETUP
}
finally {
    Close-Amsi
}

exit $exit
