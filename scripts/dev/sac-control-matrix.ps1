#!/usr/bin/env pwsh
<#
.SYNOPSIS
  Build a set of unsigned programs that differ along one axis each, run them
  once on a Smart App Control host, and record which ones Windows refused.

.DESCRIPTION
  waired-ai/waired-agent#1191 hypothesis H3 says the classifier scores each
  file's content, which is why two files out of one archive land on opposite
  sides. The issue asks for "a control binary"; one binary cannot separate the
  axes, so this builds several and varies one thing at a time.

  Every binary is new: a GUID is compiled into each one, so no hash here has
  ever been seen by any service and none can carry reputation from a previous
  run. That is the property the whole method rests on.

  A PASS IS ONLY READABLE WHILE THE HOST IS STILL REFUSING SOMETHING. This is
  not a caveat, it is the main hazard, and it was measured the first time this
  script ran: every axis came back allowed, and so did a copy of a file the
  same host had refused four times between 2026-08-30 and 2026-09-02. The host
  had simply stopped refusing. Read on its own, that run said "size, entropy,
  listening and spawning do not matter" -- and it said nothing at all.

  So pass -RefusedControl. If that file runs, no axis in the pass means
  anything, and the run reports the flip instead: the moment a known-refused
  hash became allowed is exactly the refuse-to-allow latency
  waired-ai/waired-agent#1191 H1 is about, so a pass that cannot measure the
  axes still measures something worth keeping.

  Even with the control refusing, the verdict for a given hash moves over hours
  (docs/knowledges/20260829/1740-...). One pass measures this host at this
  minute. Run it on at least two days, and record the passes separately, before
  writing any axis down as a finding.

  Nor is any of this a claim about what Microsoft's classifier does. It
  measures what happened to these files. Microsoft's own account is that the
  service makes a safety prediction and, when it cannot, falls through to the
  signature check
  (<https://learn.microsoft.com/en-us/windows/apps/develop/smart-app-control/overview>).

.PARAMETER OutDir
  Where the binaries and the tables go. Default: a directory under TEMP.

.PARAMETER Axes
  Run only these axes (by name). Default: all of them.

.PARAMETER RefusedControl
  Path to a file this host is known to have refused. It is copied and launched
  alongside the axes; a copy has no cached extended attribute, so the verdict
  is taken fresh. If it is refused, the pass is readable. If it runs, the host
  has stopped refusing and the axis rows are reported as NOT MEASURABLE.

.PARAMETER BinDir
  Use `control-<axis>.exe` already built in this directory instead of building
  them here. A Smart App Control host is an observatory -- the point is that
  its judging conditions do not change -- so installing a toolchain on it is
  the kind of change worth avoiding. Cross-build elsewhere
  (`GOOS=windows GOARCH=amd64`) and point this at the result. The axes that are
  applied after the build (a self-signed signature, a mark-of-the-web stream)
  are still applied here.

.PARAMETER KeepBinaries
  Leave the built binaries in place. By default they are deleted after the
  verdicts are read -- an unsigned executable left lying around is exactly what
  everything else here is trying to keep out of the way.

.EXAMPLE
  powershell -NoProfile -File scripts\dev\sac-control-matrix.ps1 -OutDir C:\sac-matrix
#>
[CmdletBinding()]
param(
    [string]$OutDir,
    [string[]]$Axes,
    [string]$BinDir,
    [string]$RefusedControl,
    [switch]$KeepBinaries
)

$ErrorActionPreference = 'Continue'
$scriptDir = Split-Path -Parent $PSCommandPath
$verdictTool = Join-Path $scriptDir 'sac-verdict.ps1'

function Write-Note([string]$Text) { Write-Host "[sac-matrix] $Text" }

if (-not $BinDir -and -not (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Note 'go is not on PATH. Either install a toolchain, or cross-build the controls elsewhere and pass -BinDir.'
    exit 1
}
if (-not (Test-Path -LiteralPath $verdictTool)) {
    Write-Note "sac-verdict.ps1 not found next to this script ($verdictTool)"
    exit 1
}

if (-not $OutDir) { $OutDir = Join-Path $env:TEMP ('sac-matrix-' + (Get-Date).ToString('yyyyMMddHHmmss')) }
New-Item -ItemType Directory -Path $OutDir -Force | Out-Null

# The nonce goes in the source, so every axis in every run is a first sighting.
$runNonce = [guid]::NewGuid().ToString('N')
Write-Note "run nonce $runNonce -> $OutDir"

# --- the axes ---------------------------------------------------------------
#
# One thing different per row, and each one is something we could actually
# change about what we ship. Everything is stdlib, so a control never drags in
# a dependency whose own reputation would be part of what is measured.
$definitions = @(
    @{ Name = 'baseline';    Why = 'does nothing; the reference every other row is read against' }
    @{ Name = 'stripped';    Why = 'same source, -ldflags "-s -w" (what our release build uses)' }
    @{ Name = 'versioninfo'; Why = 'baseline plus a Windows version resource (company, product, description, a real version)' }
    @{ Name = 'large';       Why = 'baseline padded to roughly the size of waired-agent.exe, low entropy' }
    @{ Name = 'highentropy'; Why = 'same size as large, incompressible payload -- separates "big" from "looks packed"' }
    @{ Name = 'listener';    Why = 'binds a loopback port, as the daemon does' }
    @{ Name = 'spawner';     Why = 'starts a child process, as the setup path does' }
    @{ Name = 'selfsigned';  Why = 'baseline, signed with a self-signed certificate no root trusts' }
    @{ Name = 'motw';        Why = 'baseline with a mark-of-the-web stream, as a downloaded file has' }
)
# `powershell.exe -File script.ps1 -Axes a,b,c` binds the whole thing as ONE
# string: -File does not parse PowerShell syntax in its arguments. Splitting
# here makes the documented invocation work from cmd, bash and a PowerShell
# prompt alike -- sac-verdict.ps1 needs the same treatment for -Attempt.
if ($Axes) {
    $wanted = @()
    foreach ($a in $Axes) { $wanted += ($a -split ',' | ForEach-Object { $_.Trim() } | Where-Object { $_ }) }
    $unknown = @($wanted | Where-Object { $definitions.Name -notcontains $_ })
    if ($unknown.Count -gt 0) {
        # Loudly, rather than quietly measuring fewer axes than asked for.
        Write-Note ("unknown axis: {0}. Known: {1}" -f ($unknown -join ', '), ($definitions.Name -join ', '))
        exit 1
    }
    $definitions = @($definitions | Where-Object { $wanted -contains $_.Name })
}
if (-not $definitions -or $definitions.Count -eq 0) { Write-Note 'no axes selected'; exit 1 }

function New-ControlSource {
    param([string]$Axis, [string]$Dir)
    $body = switch ($Axis) {
        'listener' {
@"
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		_ = l.Close()
	}
"@
        }
        'spawner' {
@"
	_ = exec.Command("cmd.exe", "/c", "ver").Run()
"@
        }
        default { "" }
    }
    $imports = switch ($Axis) {
        'listener' { "`nimport `"net`"`n" }
        'spawner'  { "`nimport `"os/exec`"`n" }
        default    { "" }
    }
    # The size axes never combine with the behaviour axes, so the assignment
    # below can replace this rather than merge with it -- but say so, because a
    # future axis that did combine them would silently lose an import.

    # Padding has to end up in the FILE, which is why it is an embedded blob
    # and not an array literal: a zero-initialised Go array goes to BSS and
    # leaves the executable the same size, which would have made the size axis
    # measure nothing. go:embed also keeps the source small enough to compile
    # quickly at 30 MB, which a string literal of that length does not.
    $pad = ''
    $payloadBytes = $null
    if ($Axis -eq 'large') {
        # Low entropy: one repeated byte, so "big" and "looks packed" are two
        # separate rows rather than one confounded one.
        $payloadBytes = New-Object byte[] (30 * 1024 * 1024)
        for ($i = 0; $i -lt $payloadBytes.Length; $i++) { $payloadBytes[$i] = 0x41 }
    }
    if ($Axis -eq 'highentropy') {
        $payloadBytes = New-Object byte[] (30 * 1024 * 1024)
        (New-Object System.Random).NextBytes($payloadBytes)
    }
    if ($payloadBytes) {
        [System.IO.File]::WriteAllBytes((Join-Path $Dir 'payload.bin'), $payloadBytes)
        $imports = "`nimport _ `"embed`"`n"
        $pad = "`n//go:embed payload.bin`nvar padding []byte`n"
    }
    $src = @"
package main
$imports
// Built by scripts/dev/sac-control-matrix.ps1. Axis: $Axis.
// Nonce $runNonce -- present so this file's hash has never existed before.
$pad
func main() {
	println("waired sac control $Axis $runNonce")
$body
	println(len(padding))
}
"@
    # `_ = padding` is NOT enough: the linker drops an embedded blob whose only
    # use is a blank assignment, and the size axis then measures a 2 MB binary
    # under the name "large" (measured -- 2,048,512 bytes against 33,505,792
    # for the same source with a real use).
    if (-not $pad) { $src = $src -replace "(?m)^\s*println\(len\(padding\)\)\r?\n", "" }
    Set-Content -LiteralPath (Join-Path $Dir 'main.go') -Value $src -Encoding UTF8
    Set-Content -LiteralPath (Join-Path $Dir 'go.mod') -Value "module wairedsaccontrol$Axis`n`ngo 1.21`n" -Encoding UTF8
}

$built = @()
foreach ($def in $definitions) {
    $axis = $def.Name
    $exe = Join-Path $OutDir "control-$axis.exe"

    if ($BinDir) {
        $src = Join-Path $BinDir "control-$axis.exe"
        if (-not (Test-Path -LiteralPath $src)) {
            Write-Note "  ${axis}: not in -BinDir, skipping (expected control-$axis.exe)"
            continue
        }
        Copy-Item -LiteralPath $src -Destination $exe -Force
    }
    else {
    $dir = Join-Path $OutDir $axis
    New-Item -ItemType Directory -Path $dir -Force | Out-Null
    $srcAxis = if ($axis -in @('stripped', 'versioninfo', 'selfsigned', 'motw')) { 'baseline' } else { $axis }
    New-ControlSource -Axis $srcAxis -Dir $dir

    if ($axis -eq 'versioninfo') {
        # The same generator cmd/waired-tray's resource is made with, so this
        # row measures the resource our build could actually ship.
        $vi = @{
            FixedFileInfo  = @{ FileVersion = @{ Major = 1; Minor = 2; Patch = 3; Build = 4 }
                                ProductVersion = @{ Major = 1; Minor = 2; Patch = 3; Build = 4 }
                                FileFlagsMask = '3f'; FileFlags = '00'; FileOS = '040004'; FileType = '01'; FileSubType = '00' }
            StringFileInfo = @{ CompanyName = 'Waired'; FileDescription = 'Waired control binary'
                                InternalName = "control-$axis"; OriginalFilename = "control-$axis.exe"
                                ProductName = 'Waired'; LegalCopyright = '' }
            VarFileInfo    = @{ Translation = @{ LangID = '0409'; CharsetID = '04B0' } }
        }
        Set-Content -LiteralPath (Join-Path $dir 'versioninfo.json') -Value ($vi | ConvertTo-Json -Depth 6) -Encoding UTF8
        Push-Location $dir
        try {
            & go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo@v1.5.0 -64 -o resource_windows_amd64.syso versioninfo.json 2>&1 | Out-Null
        } finally { Pop-Location }
        if (-not (Test-Path -LiteralPath (Join-Path $dir 'resource_windows_amd64.syso'))) {
            Write-Note "  ${axis}: could not generate the version resource; SKIPPING this axis rather than reporting a baseline under its name"
            continue
        }
    }

    $ldflags = if ($axis -eq 'stripped') { '-s -w' } else { '' }
    Push-Location $dir
    try {
        if ($ldflags) { & go build -ldflags $ldflags -o $exe . 2>&1 | Out-Null }
        else          { & go build -o $exe . 2>&1 | Out-Null }
    } finally { Pop-Location }
    if (-not (Test-Path -LiteralPath $exe)) { Write-Note "  ${axis}: build failed, skipping"; continue }
    }

    if ($axis -eq 'selfsigned') {
        $cert = New-SelfSignedCertificate -Type CodeSigningCert -Subject 'CN=Waired SAC control (self-signed)' `
                    -CertStoreLocation Cert:\CurrentUser\My -ErrorAction SilentlyContinue
        if ($cert) {
            $r = Set-AuthenticodeSignature -FilePath $exe -Certificate $cert -ErrorAction SilentlyContinue
            Write-Note "  ${axis}: signature status $($r.Status)"
            Remove-Item -LiteralPath ("Cert:\CurrentUser\My\" + $cert.Thumbprint) -Force -ErrorAction SilentlyContinue
        } else {
            Write-Note "  ${axis}: could not mint a self-signed certificate; SKIPPING rather than reporting an unsigned file under this name"
            Remove-Item -LiteralPath $exe -Force -ErrorAction SilentlyContinue
            continue
        }
    }

    if ($axis -eq 'motw') {
        # What a browser writes on a downloaded file. ZoneId=3 is "Internet".
        Set-Content -LiteralPath $exe -Stream 'Zone.Identifier' -Encoding UTF8 -Value @'
[ZoneTransfer]
ZoneId=3
'@
    }

    $built += [pscustomobject]@{
        Axis   = $axis
        Why    = $def.Why
        Path   = $exe
        Sha256 = (Get-FileHash -LiteralPath $exe -Algorithm SHA256).Hash
        Bytes  = (Get-Item -LiteralPath $exe).Length
    }
    Write-Note ("  built {0,-12} {1,10:N0} bytes  sha256 {2}" -f $axis, (Get-Item -LiteralPath $exe).Length,
                (Get-FileHash -LiteralPath $exe -Algorithm SHA256).Hash.Substring(0, 16))
}

if ($built.Count -eq 0) { Write-Note 'nothing was built'; exit 1 }

# The known-refused reference, launched in the same window as the axes and
# from a COPY: copying carries the content (so the hash, which is what the
# verdict is keyed on) but not the extended attribute, so the answer is taken
# fresh rather than read out of this host's cache.
$refCopy = $null
if ($RefusedControl) {
    if (-not (Test-Path -LiteralPath $RefusedControl)) {
        Write-Note "-RefusedControl does not exist: $RefusedControl"
        exit 1
    }
    $refCopy = Join-Path $OutDir 'refused-control.exe'
    Copy-Item -LiteralPath $RefusedControl -Destination $refCopy -Force
    Write-Note ("refused control: {0} sha256 {1}" -f (Split-Path -Leaf $RefusedControl),
                (Get-FileHash -LiteralPath $refCopy -Algorithm SHA256).Hash.Substring(0, 16))
} else {
    Write-Note 'no -RefusedControl given. If nothing is refused in this pass you will not be able to tell "these axes are allowed" from "this host is allowing everything today".'
}

# One second of margin, so the window cannot start after the first load.
$t0 = (Get-Date).AddSeconds(-1)
Write-Note "running each control once (window opens $($t0.ToUniversalTime().ToString('o')))"
foreach ($b in $built) {
    try {
        $p = Start-Process -FilePath $b.Path -PassThru -WindowStyle Hidden -ErrorAction Stop
        if (-not $p.WaitForExit(10000)) { $p.Kill() }
        Write-Note "  ran $($b.Axis)"
    } catch {
        # A refusal IS the measurement -- CreateProcess is where Smart App
        # Control answers, so an exception here is a result, not an error.
        Write-Note "  $($b.Axis) refused at launch: $($_.Exception.Message -split "`n" | Select-Object -First 1)"
    }
}

$refRefused = $null
if ($refCopy) {
    try {
        $p = Start-Process -FilePath $refCopy -ArgumentList '--version' -PassThru -WindowStyle Hidden -ErrorAction Stop
        if (-not $p.WaitForExit(10000)) { $p.Kill() }
        $refRefused = $false
        Write-Note '  the refused control RAN -- this host is not refusing it any more'
    } catch {
        $refRefused = $true
        Write-Note '  the refused control was refused, as expected'
    }
}

# Give the log a moment; CodeIntegrity writes 3077 and 3118 asynchronously.
Start-Sleep -Seconds 5

Write-Note 'reading the verdicts through sac-verdict.ps1'
$verdictDir = Join-Path $OutDir 'verdicts'
& powershell.exe -NoProfile -ExecutionPolicy Bypass -File $verdictTool `
    -Attempt (($built.Path) -join ',') `
    -Since $t0.ToUniversalTime().ToString('o') `
    -Match 'control-' -NoRedact -Label 'matrix' -OutDir $verdictDir | Out-Host

$json = Get-ChildItem -LiteralPath $verdictDir -Filter '*matrix.json' -ErrorAction SilentlyContinue |
    Sort-Object LastWriteTime | Select-Object -Last 1
$verdicts = @()
if ($json) { $verdicts = (Get-Content -LiteralPath $json.FullName -Raw | ConvertFrom-Json).Verdicts }

$rows = foreach ($b in $built) {
    # 3077 reports the flat hash; for an unsigned file that is the file's own
    # SHA256, which is why these join. The signed row is the exception, so it
    # is matched on the file name as well.
    $hits = @($verdicts | Where-Object {
        $_.Sha256Flat -eq $b.Sha256 -or $_.FileKey -match ("(?i)control-" + [regex]::Escape($b.Axis) + "\.exe$")
    })
    $rep = ($hits | Where-Object { $_.Reputation } | Select-Object -First 1).Reputation
    [pscustomobject]@{
        Axis       = $b.Axis
        Bytes      = $b.Bytes
        Sha256     = $b.Sha256.Substring(0, 16)
        Refused    = ($hits.Count -gt 0)
        Events     = $hits.Count
        Trust      = $(if ($rep) { $rep.DefenderTrust } else { '' })
        CloudHttp  = $(if ($rep) { $rep.DefenderCloudHTTPCode } else { '' })
        Unfriendly = $(if ($rep) { $rep.IsUnfriendlyFile } else { '' })
        Why        = $b.Why
    }
}

$rows | Format-Table Axis, Bytes, Sha256, Refused, Events, Trust, CloudHttp, Unfriendly -AutoSize | Out-String -Width 200 | Write-Host

$md = @("# Smart App Control control matrix", '',
        "Run $((Get-Date).ToUniversalTime().ToString('o')), nonce ``$runNonce``.",
        "Every binary below is a first sighting. **One pass measures this host at this minute** -- see the header of the script.", '',
        $(if ($refRefused -eq $true) { '**Readable**: the known-refused control was still refused in this window.' }
          elseif ($refRefused -eq $false) { '**NOT MEASURABLE**: the known-refused control ran, so this host was refusing nothing in this window. The axis rows below say nothing; the flip does.' }
          else { '**Unverified**: no known-refused control was launched in this window, so an all-allowed table cannot be told from a permissive host.' }), '',
        '| axis | bytes | sha256 (16) | refused | events | trust | cloud HTTP | unfriendly | what differs |',
        '|---|---:|---|---|---:|---|---|---|---|')
foreach ($r in $rows) {
    $md += "| $($r.Axis) | $($r.Bytes) | $($r.Sha256) | $($r.Refused) | $($r.Events) | $($r.Trust) | $($r.CloudHttp) | $($r.Unfriendly) | $($r.Why) |"
}
$mdPath = Join-Path $OutDir 'matrix.md'
[System.IO.File]::WriteAllLines($mdPath, $md, (New-Object System.Text.UTF8Encoding($false)))
Write-Note "wrote $mdPath"

if (-not $KeepBinaries) {
    foreach ($b in $built) { Remove-Item -LiteralPath $b.Path -Force -ErrorAction SilentlyContinue }
    Write-Note 'removed the built binaries (-KeepBinaries to keep them)'
}

# --- is this pass readable at all? -----------------------------------------
$refusedCount = @($rows | Where-Object { $_.Refused }).Count
if ($refRefused -eq $true) {
    Write-Note "READABLE: the known-refused control was still refused, so $refusedCount refused / $($rows.Count - $refusedCount) allowed is a real split."
}
elseif ($refRefused -eq $false) {
    Write-Note 'NOT MEASURABLE: the known-refused control ran, so this host is not refusing anything right now and no axis row above can be read. What this pass DID measure is a flip -- record the control''s hash and this timestamp against its last recorded refusal (waired-agent#1191 H1).'
}
elseif ($refusedCount -eq 0) {
    Write-Note 'NOTHING was refused, and no -RefusedControl was given, so this pass cannot tell "these axes are allowed" from "this host is allowing everything today". Re-run with -RefusedControl.'
}
