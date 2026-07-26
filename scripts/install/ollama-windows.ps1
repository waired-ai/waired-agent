#Requires -Version 5.1
<#
.SYNOPSIS
    Installs Ollama for Windows in a layout the waired-agent can discover.

.DESCRIPTION
    Downloads the official ollama-windows-amd64.zip from GitHub releases and
    extracts it to %ProgramFiles%\Ollama\, so the binary lands at
    %ProgramFiles%\Ollama\ollama.exe. That path is the first candidate
    searched by internal/download/ollama_path_windows.go, which is necessary
    because the waired-agent runs as LocalSystem when registered as a Windows
    Service and LocalSystem cannot read other users' %LOCALAPPDATA%.

    Why ZIP and not OllamaSetup.exe:
        The official Ollama installer (Inno Setup, OllamaSetup.exe) is
        per-user only by design - PrivilegesRequired=lowest means /ALLUSERS
        is silently ignored, the binary always lands under %LOCALAPPDATA%,
        and the tray app's Run-key auto-start is user-scoped. LocalSystem
        cannot read other users' %LOCALAPPDATA%, so a Service-mode
        waired-agent would fail to locate ollama.exe. The ZIP release lets
        us put files exactly where path discovery expects them.

        Note: waired-agent itself spawns and supervises ollama.exe via
        internal/runtime.OllamaAdapter, so there is no need to register
        ollama as a Windows Service or rely on auto-start.

    AMD GPU support (-GpuMode auto):
        The base ollama-windows-amd64.zip bundles CUDA + Vulkan + CPU
        runtimes but NOT ROCm -- ROCm is shipped as a separate
        ~350 MiB overlay ZIP (ollama-windows-amd64-rocm.zip). When this
        script detects an AMD GPU it picks one of two paths:

        - **ROCm path** (Radeon RX 6800+, RX 7000 series, Radeon PRO
          W6/W7, V620): download the ROCm overlay ZIP and extract it
          on top of the base install. Best performance for supported
          AMD discrete cards on Windows.
        - **Vulkan path** (everything else AMD -- iGPU / APU like Strix
          Halo, RX 5000-and-below, unsupported discrete): set the
          machine-scope env var OLLAMA_VULKAN=1 so Ollama's
          experimental Vulkan backend kicks in.

        Note about Strix Halo / Ryzen AI MAX (gfx1151) specifically:
        AMD ROCm 6.4.1+ on Linux and 6.4.4+ on Windows DO support this
        SoC's Radeon 8060S iGPU. But Ollama's Windows ROCm overlay
        ships ROCm v6.1 and is only compiled for RX 6800+/RX 7000/
        Radeon PRO W6/W7 targets, so this script falls back to Vulkan
        on Strix Halo even though the hardware itself is ROCm-capable.
        Users who want the ROCm path on Strix Halo on Windows today
        must use the community `likelovewant/ollama-for-amd` fork
        (gfx1151 included) rather than the official Ollama release.

        Both decisions can be forced via -GpuMode (rocm / vulkan /
        cuda-only / cpu-only) when auto-detection picks the wrong
        path.

    MAINTENANCE NOTE (review on each major Ollama bump):
        The Test-AMDRocmSupported list and the rocm-vs-vulkan branch
        in Resolve-GpuMode track Ollama's *official Windows* AMD
        support stance as of 2026-05-16 (Ollama 0.30.7, ROCm v6.1
        overlay, RX 6800+/7000/Radeon PRO discrete only). When
        Ollama upstream changes its AMDGPU_TARGETS, when AMD ships
        Adrenalin-bundled Ollama integration to more SKUs (started
        2026-01 for RX 7700+/Ryzen AI 300+/400+/MAX), or when the
        Vulkan backend leaves experimental, revisit this script --
        new SKUs may move from the vulkan branch to the rocm branch
        and the OLLAMA_VULKAN gate may stop being required. See
        docs/todo.md "Ollama Windows AMD support tracking".

    This script is intended to be:
        * Run manually today (Phase W-1.5 era).
        * Embedded verbatim (or invoked via Start-Process) by a future
          waired-agent Windows installer / first-run bootstrap.

    The script is idempotent. Re-running with an existing install is a no-op
    unless -Force is passed. The GPU-mode resolution (PATH / OLLAMA_VULKAN /
    ROCm overlay) runs on every invocation so re-running with -GpuMode rocm
    on a previously vulkan-installed host correctly adds the overlay.

    Install atomicity and the completion receipt:
        The base archive is extracted and signature-verified in
        "<InstallDir>.new" and only then swapped into place, so a failed
        download or a failed signature check cannot leave a partially
        installed Ollama behind. The .waired-managed.json marker is
        written LAST, after PATH, models dir, GPU env and the post-install
        check have all succeeded -- it is a completion receipt, and
        `waired init` repairs an install under %ProgramFiles%\Ollama that
        does not carry one. Re-running against such an install is cheap:
        the base bits are already there, so nothing is downloaded again.

.PARAMETER ZipUrl
    URL of ollama-windows-amd64.zip. Defaults to the bundled-pinned
    v0.31.1 (kept in sync with OllamaPinnedVersion in
    internal/runtime/ollama_version.go so all platforms install the same
    Ollama; bump both together -- tracked by Renovate, #290). Override to
    a different version with e.g.
    https://github.com/ollama/ollama/releases/download/v0.31.1/ollama-windows-amd64.zip

.PARAMETER RocmZipUrl
    URL of ollama-windows-amd64-rocm.zip (the AMD ROCm overlay). Defaults
    to the same bundled-pinned v0.31.1. Downloaded only when -GpuMode
    resolves to 'rocm'.

.PARAMETER InstallDir
    Target install directory. Defaults to %ProgramFiles%\Ollama, which is the
    first candidate searched by internal/download/ollama_path_windows.go.

.PARAMETER Force
    Reinstall (overwrite) even if InstallDir\ollama.exe already exists.
    GPU-mode side-effects (PATH, OLLAMA_VULKAN, models dir) run regardless.

.PARAMETER ModelsDir
    If set, creates the directory and exports OLLAMA_MODELS as a Machine
    environment variable so the LocalSystem-spawned ollama subprocess stores
    model blobs there. Otherwise blobs land under %USERPROFILE%\.ollama (or
    %SystemProfile%\.ollama under LocalSystem) which often shares the system
    drive with the OS.

.PARAMETER NoPath
    Skip prepending InstallDir to the Machine PATH. waired-agent itself does
    not require ollama on PATH (it uses absolute paths), but interactive
    users typically do. Pass -NoPath to keep PATH untouched.

.PARAMETER GpuMode
    GPU acceleration mode selector:
        - 'auto'       (default): inspect Win32_VideoController, choose
                       rocm/vulkan/cuda-only based on detected adapters.
        - 'rocm'       force-download the ROCm overlay even if the
                       detector did not pick AMD (e.g. fresh-image
                       setup before AMD driver is installed).
        - 'vulkan'     skip the ROCm overlay and set OLLAMA_VULKAN=1.
                       Useful for unsupported AMD discrete or Intel Arc.
        - 'cuda-only'  base ZIP only; do not set OLLAMA_VULKAN; do not
                       fetch ROCm overlay. Use for pure-Nvidia or
                       CPU-only hosts.
        - 'cpu-only'   alias for cuda-only with extra reassurance: still
                       installs the base ZIP which contains the CPU
                       runtimes.

.EXAMPLE
    PS> .\ollama-windows.ps1

.EXAMPLE
    PS> .\ollama-windows.ps1 -ModelsDir D:\ollama\models -Force

.EXAMPLE
    PS> .\ollama-windows.ps1 -GpuMode rocm
#>
[CmdletBinding()]
param(
    [string]$ZipUrl     = 'https://github.com/ollama/ollama/releases/download/v0.31.1/ollama-windows-amd64.zip',
    [string]$RocmZipUrl = 'https://github.com/ollama/ollama/releases/download/v0.31.1/ollama-windows-amd64-rocm.zip',
    [string]$InstallDir = (Join-Path $env:ProgramFiles 'Ollama'),
    [switch]$Force,
    [string]$ModelsDir,
    [switch]$NoPath,
    [ValidateSet('auto', 'rocm', 'vulkan', 'cuda-only', 'cpu-only')]
    [string]$GpuMode    = 'auto'
)

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'

# Windows PowerShell 5.1 and PowerShell 7 ship separate, incompatible copies
# of the in-box modules. A 5.1 child launched from a pwsh 7 session inherits
# pwsh 7's PSModulePath; autoloading Microsoft.PowerShell.Security then dies
# on a types-file collision ("AuditToString" is already present) and
# Get-AuthenticodeSignature can never load. With $ErrorActionPreference
# 'Stop' that turns Verify-Signature below into a terminating error and the
# whole install fails as `exit status 1` (#178). `waired init` runs under an
# elevated pwsh 7 on the supported path, so this is the default, not an edge
# case. Note that an explicit Import-Module of the same module fails
# identically -- the path itself has to be repaired.
#
# The Go caller strips PSMODULEPATH before spawning us
# (internal/platform/pwsh), but this script is also fetched and run
# standalone, so repair it here as well: keep only the WindowsPowerShell
# roots, then re-add 5.1's own $PSHOME\Modules and the registry values.
if ($PSVersionTable.PSEdition -eq 'Desktop') {
    $wantedModulePaths = @()
    $modulePathSources = @(
        $env:PSModulePath,
        [Environment]::GetEnvironmentVariable('PSModulePath', 'Machine'),
        [Environment]::GetEnvironmentVariable('PSModulePath', 'User'),
        (Join-Path $PSHOME 'Modules')
    )
    foreach ($src in $modulePathSources) {
        foreach ($p in (($src -split ';') | Where-Object { $_ })) {
            # '...\PowerShell\...' is a PowerShell 7 root; 5.1's own roots
            # all say '...\WindowsPowerShell\...', which this does not match.
            if ($p -match '(?i)[\\/]PowerShell[\\/]') { continue }
            if ($wantedModulePaths -notcontains $p) { $wantedModulePaths += $p }
        }
    }
    if ($wantedModulePaths.Count -gt 0) {
        $env:PSModulePath = ($wantedModulePaths -join ';')
    }
}

function Assert-Admin {
    $id   = [System.Security.Principal.WindowsIdentity]::GetCurrent()
    $prin = New-Object System.Security.Principal.WindowsPrincipal($id)
    if (-not $prin.IsInRole([System.Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'This script must run as Administrator. Writing under %ProgramFiles% requires elevation.'
    }
}

function Get-OllamaExePath {
    # Mirrors internal/download/ollama_path_windows.go discovery order.
    $candidates = @(
        (Join-Path $env:ProgramFiles 'Ollama\ollama.exe'),
        (Join-Path $env:LOCALAPPDATA 'Programs\Ollama\ollama.exe')
    )
    foreach ($c in $candidates) {
        if (Test-Path -LiteralPath $c) { return $c }
    }
    return $null
}

# Get-DetectedGPUs queries Win32_VideoController and returns one
# pscustomobject per adapter with Name + VendorID (PCI vendor in hex).
# Used by Resolve-GpuMode to pick rocm vs vulkan vs cuda-only.
function Get-DetectedGPUs {
    $adapters = @()
    try {
        $cim = Get-CimInstance Win32_VideoController -ErrorAction Stop
    } catch {
        Write-Warning ("Get-CimInstance Win32_VideoController failed: {0}. Falling back to no detection (GpuMode auto -> cuda-only)." -f $_.Exception.Message)
        return @()
    }
    foreach ($a in $cim) {
        $vendor = ''
        if ($a.PNPDeviceID -match 'VEN_([0-9A-F]{4})') {
            $vendor = $matches[1].ToUpper()
        }
        $adapters += [pscustomobject]@{
            Name        = $a.Name
            PNPDeviceID = $a.PNPDeviceID
            VendorID    = $vendor
        }
    }
    return $adapters
}

# Test-AMDRocmSupported returns $true for AMD GPUs that **Ollama's
# bundled Windows ROCm overlay v6.1** supports (per docs.ollama.com/
# gpu). NOT a statement about AMD ROCm hardware support in general --
# Strix Halo / gfx1151 IS ROCm-capable upstream (ROCm 6.4.1 on Linux,
# 6.4.4 on Windows) but is not in Ollama's Windows bundle today, so
# it returns $false here and the caller falls back to Vulkan.
#
# The list is intentionally a heuristic on the device Name string
# because the PCI-ID space would be a much larger lookup table.
#
# !!! MAINTENANCE: This list mirrors Ollama's Windows ROCm overlay
# !!! supported SKUs as of 2026-05-16 (Ollama 0.30.7). On every
# !!! major Ollama upstream release that adds/removes AMD targets in
# !!! AMDGPU_TARGETS (see ollama/scripts/build_windows.sh upstream),
# !!! revisit and adjust the patterns below. See docs/todo.md
# !!! "Ollama Windows AMD support tracking" for the review checklist.
# !!! The waired-agent runtime mirrors this same list in Go
# !!! (amdROCmSupported in internal/runtime/ollama_backend.go) so the
# !!! agent's backend routing matches the installed overlay -- update
# !!! both together.
#
# Returns $true for (per Ollama docs):
#   Radeon RX 7900 XTX/XT/GRE, 7800 XT, 7700 XT, 7600 XT, 7600
#   Radeon RX 6950 XT, 6900 XTX/XT, 6800 XT, 6800
#   Radeon PRO W7900/W7800/W7700/W7600/W7500
#   Radeon PRO W6900X/W6800X Duo/W6800X/W6800
#   Radeon PRO V620
#
# Returns $false for (Vulkan fallback):
#   Ryzen AI APU iGPUs (Strix Halo Radeon 8060S, 780M, ...) -- ROCm-
#     capable upstream but missing from Ollama Windows bundle.
#   RX 5000 series and older -- pre-RDNA2, never in Ollama Windows.
#   RX 6700/6600/6500/6400 and below -- RDNA2 but below Ollama's cut.
function Test-AMDRocmSupported {
    param([string]$Name)
    if (-not $Name) { return $false }
    $patterns = @(
        'Radeon\s+RX\s+7\d{3}',             # 7000 series (all of them are supported per Ollama)
        'Radeon\s+RX\s+6[89]\d{2}',         # 6800/6900/6950
        'Radeon\s+(\(TM\)\s+)?PRO\s+W[67]\d{3}',  # PRO W6xxx / W7xxx
        'Radeon\s+(\(TM\)\s+)?PRO\s+V620'
    )
    foreach ($p in $patterns) {
        if ($Name -match $p) { return $true }
    }
    return $false
}

# Resolve-GpuMode converts the -GpuMode parameter into the final
# concrete mode by running the auto-detector when needed. Returns one
# of: 'rocm', 'vulkan', 'cuda-only'. The detection rationale is
# reported via Write-Host for the operator's benefit.
function Resolve-GpuMode {
    param([string]$Requested)
    if ($Requested -eq 'cpu-only') {
        Write-Host 'GpuMode = cuda-only (cpu-only alias; base ZIP includes CPU runtimes)'
        return 'cuda-only'
    }
    if ($Requested -ne 'auto') {
        Write-Host "GpuMode = $Requested (explicit)"
        return $Requested
    }
    $gpus = Get-DetectedGPUs
    if ($gpus.Count -eq 0) {
        Write-Host 'GpuMode = cuda-only (no GPU adapters detected; CPU-only host)'
        return 'cuda-only'
    }
    foreach ($g in $gpus) {
        Write-Host "  detected adapter: $($g.Name) [VEN_$($g.VendorID)]"
    }
    $hasNvidia = $false
    $hasRocmAmd = $false
    $hasOtherAmd = $false
    foreach ($g in $gpus) {
        switch ($g.VendorID) {
            '10DE' { $hasNvidia = $true }
            '1002' {
                if (Test-AMDRocmSupported -Name $g.Name) { $hasRocmAmd = $true }
                else { $hasOtherAmd = $true }
            }
        }
    }
    if ($hasRocmAmd) {
        Write-Host 'GpuMode = rocm (auto: ROCm v6.1-supported AMD adapter detected)'
        return 'rocm'
    }
    if ($hasOtherAmd) {
        Write-Host 'GpuMode = vulkan (auto: AMD adapter detected but not in Ollama Windows ROCm overlay supported list; using Vulkan path)'
        Write-Host '  (hardware may be ROCm-capable upstream, e.g. Strix Halo gfx1151, but Ollama Windows ships ROCm v6.1 only for RX 6800+/7000/Radeon PRO. See likelovewant/ollama-for-amd fork for community builds.)'
        return 'vulkan'
    }
    if ($hasNvidia) {
        Write-Host 'GpuMode = cuda-only (auto: only Nvidia GPU detected; CUDA bundled in base ZIP)'
        return 'cuda-only'
    }
    Write-Host 'GpuMode = cuda-only (auto: no Nvidia/AMD adapter found; base ZIP only)'
    return 'cuda-only'
}

# Invoke-DownloadWithProgress streams $Url to $OutFile while printing
# progress. The Ollama archive is ~1.4 GB, and the old silent
# `Invoke-WebRequest -OutFile` left the console dead for minutes (waired#747).
# This gives the byte-level feedback the Linux/macOS path already gets from
# `waired runtimes install ollama` (cmd/waired/runtimes_install_render.go's
# drawDownloadLine: percent, transferred/total, rate). PS 5.1-safe: a raw
# HttpWebRequest + manual read loop -- NOT Invoke-WebRequest, whose 5.1 progress
# bar re-renders per read and cripples large-file throughput (the reason
# $ProgressPreference is 'SilentlyContinue' above).
#
# Rendering matches drawDownloadLine's two modes: on an interactive console
# ONE in-place line is rewritten via `r (the fresh-line-per-3% variant this
# replaces scrolled a wall of progress rows past the user); when output is
# redirected (CI logs / transcripts capture Write-Host, not [Console]::Write)
# it falls back to a fresh line every >=10% or ~5s.
function Invoke-DownloadWithProgress {
    param(
        [Parameter(Mandatory)][string]$Url,
        [Parameter(Mandatory)][string]$OutFile
    )
    $interactive = $true
    try { $interactive = -not [Console]::IsOutputRedirected } catch { $interactive = $false }
    # Windows PowerShell 5.1 on older .NET does not negotiate TLS 1.2 by
    # default; opt in for the raw request. Best-effort so this never throws.
    try {
        [Net.ServicePointManager]::SecurityProtocol =
            [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    } catch { }

    $req = [System.Net.HttpWebRequest]::Create($Url)
    $req.UserAgent        = 'waired-installer'
    $req.AllowAutoRedirect = $true   # GitHub release assets 302 to a CDN host
    $req.Timeout          = 60000    # connect timeout (ms)
    $req.ReadWriteTimeout = 120000   # per-read stall timeout (ms)

    $resp = $null; $rs = $null; $fs = $null
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    try {
        $resp    = $req.GetResponse()
        $total   = [int64]$resp.ContentLength   # -1 when the server omits it
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
                # One in-place line, rewritten at most ~4x/s. Pad to clear
                # residue from a previously longer render.
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
                # Redirected output: fresh lines, sparse (>=10% / ~5s), so CI
                # logs and transcripts stay readable without scroll spam.
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
        # End the in-place line before the summary so it is not overwritten.
        [Console]::Write("`r" + (' ' * 72) + "`r")
    }
    Write-Host ("  done: {0:N1} MB in {1:N0}s" -f `
        ((Get-Item -LiteralPath $OutFile).Length / 1MB), $sw.Elapsed.TotalSeconds)
}

function Stage-ZipDownload {
    param(
        [string]$Url,
        [int]$MinSizeBytes
    )
    $tmpDir = Join-Path $env:TEMP ("ollama-stage-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmpDir -Force | Out-Null
    $zip = Join-Path $tmpDir ([IO.Path]::GetFileName(([Uri]$Url).AbsolutePath))
    Write-Host "Downloading $Url"
    Write-Host "          -> $zip"
    Invoke-DownloadWithProgress -Url $Url -OutFile $zip
    $size = (Get-Item $zip).Length
    if ($size -lt $MinSizeBytes) {
        Remove-Item -LiteralPath $tmpDir -Recurse -Force -ErrorAction SilentlyContinue
        throw ("Downloaded archive is suspiciously small ({0} bytes, expected >= {1}); refusing to extract." -f $size, $MinSizeBytes)
    }
    Write-Host ("  archive size: {0:N1} MB" -f ($size / 1MB))
    return $zip
}

function Clean-InstallDir {
    param([string]$Target)
    if (Test-Path -LiteralPath $Target) {
        # Best-effort clean of previous extraction so stale .dll / lib/
        # files do not bleed across versions. Keep the directory itself
        # in case Defender / antivirus has a handle.
        Write-Host "Cleaning previous contents under $Target"
        Get-ChildItem -LiteralPath $Target -Force -ErrorAction SilentlyContinue | ForEach-Object {
            Remove-Item -LiteralPath $_.FullName -Recurse -Force -ErrorAction SilentlyContinue
        }
    } else {
        New-Item -ItemType Directory -Path $Target -Force | Out-Null
    }
}

function Expand-Overlay {
    param(
        [string]$ZipPath,
        [string]$Target,
        [string]$Label
    )
    Write-Host "Expanding $Label into $Target"
    Expand-Archive -LiteralPath $ZipPath -DestinationPath $Target -Force
}

# Promote-StagedInstall replaces $Target's contents with an already-verified
# staging tree. $Staged sits beside $Target on the same volume, so each
# per-entry Move-Item is a rename rather than a copy. The target directory
# itself is kept (not renamed) for the same reason Clean-InstallDir keeps it:
# Defender / antivirus commonly holds a handle on it, which fails a directory
# rename but not a move into it.
function Promote-StagedInstall {
    param(
        [string]$Staged,
        [string]$Target
    )
    Clean-InstallDir -Target $Target
    Write-Host "Installing into $Target"
    try {
        Get-ChildItem -LiteralPath $Staged -Force | ForEach-Object {
            Move-Item -LiteralPath $_.FullName -Destination (Join-Path $Target $_.Name) -Force
        }
    } catch {
        # A failure part-way through the swap would leave a tree that still
        # has ollama.exe in it, which the next run would read as a complete
        # base install and never re-extract. Remove it instead.
        Remove-Item -LiteralPath $Target -Recurse -Force -ErrorAction SilentlyContinue
        throw
    }
}

function Verify-Signature {
    param([string]$Exe)

    # Get-AuthenticodeSignature lives in Microsoft.PowerShell.Security. The
    # PSModulePath repair at the top of this script is what normally keeps it
    # loadable (#178); this catch is the belt to that braces, so a host where
    # the cmdlet is unavailable for any other reason still gets *a*
    # signature check instead of a hard failure.
    $sig = $null
    try {
        $sig = Get-AuthenticodeSignature -FilePath $Exe
    } catch {
        Write-Warning "Get-AuthenticodeSignature unavailable ($($_.Exception.Message))"
    }
    if ($sig) {
        if ($sig.Status -ne 'Valid') {
            throw "ollama.exe Authenticode status is '$($sig.Status)' (expected 'Valid')."
        }
        Write-Host "Signed by: $($sig.SignerCertificate.Subject)"
        return
    }

    # Fallback: read the embedded signing certificate and validate its chain.
    # Weaker than the cmdlet -- it proves the file carries a chain-valid
    # signing certificate, not that the file's hash still matches the
    # signature -- so it is announced as such rather than passing silently.
    $cert = $null
    try {
        $cert = [Security.Cryptography.X509Certificates.X509Certificate2]::CreateFromSignedFile($Exe)
    } catch {
        throw "ollama.exe is not signed, or its signature could not be read ($($_.Exception.Message))."
    }
    $chain = New-Object Security.Cryptography.X509Certificates.X509Chain
    # Revocation is left unchecked: this path is already the degraded one,
    # and a flaky OCSP/CRL endpoint must not fail an otherwise good install.
    $chain.ChainPolicy.RevocationMode = [Security.Cryptography.X509Certificates.X509RevocationMode]::NoCheck
    if (-not $chain.Build($cert)) {
        $why = (($chain.ChainStatus | ForEach-Object { $_.Status }) -join ', ')
        throw "ollama.exe signing certificate does not chain to a trusted root ($why)."
    }
    Write-Warning 'Signature checked via the certificate chain only (Get-AuthenticodeSignature was unavailable).'
    Write-Host "Signed by: $($cert.Subject)"
}

function Set-MachineModelsDir {
    param([string]$Path)
    if (-not (Test-Path -LiteralPath $Path)) {
        New-Item -ItemType Directory -Path $Path -Force | Out-Null
    }
    [Environment]::SetEnvironmentVariable('OLLAMA_MODELS', $Path, 'Machine')
    Write-Host "OLLAMA_MODELS=$Path (Machine scope)"
}

function Set-MachineVulkanFlag {
    [Environment]::SetEnvironmentVariable('OLLAMA_VULKAN', '1', 'Machine')
    Write-Host 'OLLAMA_VULKAN=1 (Machine scope) -- Ollama Vulkan backend enabled at next start'
    # Ollama 0.30.x DROPS integrated GPUs by default ("dropping integrated
    # GPU; to enable, set OLLAMA_IGPU_ENABLE=1") and silently falls back to
    # CPU. The Vulkan path here is exactly the iGPU/APU case (Strix Halo
    # Radeon 8060S, Intel iGPU, ...), so un-gate integrated GPUs too.
    # Harmless for the unsupported-discrete cases that also take this path.
    [Environment]::SetEnvironmentVariable('OLLAMA_IGPU_ENABLE', '1', 'Machine')
    Write-Host 'OLLAMA_IGPU_ENABLE=1 (Machine scope) -- integrated GPU (Strix Halo / Intel iGPU) un-gated'
}

function Add-ToMachinePath {
    param([string]$Dir)
    $cur = [Environment]::GetEnvironmentVariable('PATH', 'Machine')
    $entries = $cur -split ';' | Where-Object { $_ -ne '' }
    if ($entries -contains $Dir) {
        Write-Host "PATH already contains $Dir"
        return
    }
    $new = ($entries + $Dir) -join ';'
    [Environment]::SetEnvironmentVariable('PATH', $new, 'Machine')
    Write-Host "Prepended $Dir to Machine PATH (new shells will pick it up)"
}

# Write-WairedManagedMarker drops the marker file `waired init` uses to
# recognise this Ollama as waired's own install (internal/setup DetectOllama
# -> WairedManaged), so init never asks the bundled-vs-reuse question about
# an Ollama waired itself put here.
#
# It is also the install's COMPLETION RECEIPT: the caller writes it only
# after PATH, models dir, GPU env and Test-Install have all succeeded, and
# cmd/waired/init_engine.go treats bits under %ProgramFiles%\Ollama with no
# marker as an incomplete install to repair (#190). Still best-effort: a
# write failure costs one cheap repair pass on the next run (the base bits
# are already there, so nothing is re-downloaded), which is a far better
# outcome than failing an otherwise complete install.
function Write-WairedManagedMarker {
    param([string]$InstallDir)
    $marker = Join-Path $InstallDir '.waired-managed.json'
    try {
        Set-Content -LiteralPath $marker -Encoding Ascii -Value '{"managed_by":"waired","installer":"ollama-windows.ps1"}'
        Write-Host "Marked as waired-managed: $marker"
    } catch {
        Write-Warning "could not write $marker ($($_.Exception.Message)); this install will be treated as incomplete and repaired on the next run"
    }
}

function Test-Install {
    param(
        [string]$InstallDir,
        [string]$GpuMode,
        [switch]$NoPath
    )

    $exe = Join-Path $InstallDir 'ollama.exe'
    if (-not (Test-Path -LiteralPath $exe)) {
        throw "Post-install check: $exe missing."
    }
    Write-Host "Installed at: $exe"
    Write-Host "GPU mode:     $GpuMode"

    # PATH and the GPU env vars are part of a COMPLETE install, and the
    # marker written straight after this call is what records that
    # completeness -- so verify them here rather than trusting that the
    # writers above ran. Without the GPU vars recent Ollama drops the iGPU
    # and quietly runs on CPU, which is exactly the half-configured state
    # #190 is about.
    if (-not $NoPath) {
        $machinePath = [Environment]::GetEnvironmentVariable('PATH', 'Machine')
        if ((($machinePath -split ';') | Where-Object { $_ -eq $InstallDir }).Count -eq 0) {
            throw "Post-install check: $InstallDir is not on the machine PATH."
        }
        Write-Host "Machine PATH: $InstallDir"
    }

    if ($GpuMode -eq 'vulkan') {
        foreach ($name in @('OLLAMA_VULKAN', 'OLLAMA_IGPU_ENABLE')) {
            if ([Environment]::GetEnvironmentVariable($name, 'Machine') -ne '1') {
                throw "Post-install check: $name is not set at Machine scope (GPU mode is 'vulkan')."
            }
        }
        Write-Host 'GPU env:      OLLAMA_VULKAN=1, OLLAMA_IGPU_ENABLE=1 (Machine scope)'
    }

    if ($GpuMode -eq 'rocm') {
        $rocmDir = Join-Path $InstallDir 'lib\ollama\rocm'
        if (Test-Path -LiteralPath $rocmDir) {
            Write-Host "ROCm overlay: $rocmDir"
        } else {
            Write-Warning "GPU mode is 'rocm' but $rocmDir was not found after extraction."
        }
    }

    # Run --version through cmd.exe to dodge a PowerShell-specific quirk
    # where short-lived programs that don't redirect stderr trigger
    # 'StandardErrorEncoding is only supported when standard error is
    # redirected.'
    $verRaw = & cmd.exe /c "`"$exe`" --version 2>&1"
    Write-Host "ollama --version: $verRaw"

    # Confirm waired-agent discovery matches.
    $discoveryFirst = Join-Path $env:ProgramFiles 'Ollama\ollama.exe'
    if ($exe -eq $discoveryFirst) {
        Write-Host "Discovery: this is the first candidate searched by waired-agent."
    } else {
        Write-Warning "Discovery: $exe is NOT the first candidate ($discoveryFirst). waired-agent will still find it only if it is the per-user fallback path."
    }
}

# --- main ---

Assert-Admin

$resolvedMode = Resolve-GpuMode -Requested $GpuMode

# Decide on OUR target directory, not on "an Ollama exists somewhere":
# Get-OllamaExePath also matches the per-user OllamaSetup.exe layout under
# %LOCALAPPDATA%, and counting that as installed left $InstallDir empty
# while every step below still ran against it -- adding a non-existent
# directory to PATH, failing to write the marker, and finally throwing in
# Test-Install.
$targetExe = Join-Path $InstallDir 'ollama.exe'
$existing  = Get-OllamaExePath
$needBaseInstall = (-not (Test-Path -LiteralPath $targetExe)) -or $Force
$needRocmInstall = $resolvedMode -eq 'rocm' -and (
    $needBaseInstall -or
    -not (Test-Path -LiteralPath (Join-Path $InstallDir 'lib\ollama\rocm'))
)

if ($needBaseInstall) {
    if ($existing -and ($existing -ne $targetExe)) {
        Write-Host "Note: another Ollama is present at $existing; installing waired's own copy into $InstallDir"
    }
    $stagedDir = "$InstallDir.new"
    $baseZip   = Stage-ZipDownload -Url $ZipUrl -MinSizeBytes (50MB)
    $stageRoot = Split-Path -Parent $baseZip
    try {
        # Extract and verify BESIDE the target, and swap only once the bits
        # are known good. Verifying after Clean-InstallDir + Expand-Overlay
        # had already run is what stranded a half-installed Ollama whenever
        # the signature check failed (#190): binaries on disk, no PATH
        # entry, no GPU env and no marker -- and every later run then
        # skipped the install, so repeated clean reinstalls reproduced the
        # identical failure.
        Remove-Item -LiteralPath $stagedDir -Recurse -Force -ErrorAction SilentlyContinue
        Expand-Overlay -ZipPath $baseZip -Target $stagedDir -Label 'base archive'
        $stagedExe = Join-Path $stagedDir 'ollama.exe'
        if (-not (Test-Path -LiteralPath $stagedExe)) {
            throw "Extraction completed but ollama.exe was not found at $stagedExe."
        }
        Verify-Signature -Exe $stagedExe
        # Free the archive before the swap so peak disk use does not grow
        # relative to extracting in place.
        Remove-Item -LiteralPath $baseZip -Force -ErrorAction SilentlyContinue
        Promote-StagedInstall -Staged $stagedDir -Target $InstallDir
    } finally {
        Remove-Item -LiteralPath $stagedDir -Recurse -Force -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath $stageRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
} else {
    Write-Host "Base install already present: $targetExe (pass -Force to reinstall)"
}

if ($needRocmInstall) {
    $rocmZip = Stage-ZipDownload -Url $RocmZipUrl -MinSizeBytes (100MB)
    try {
        Expand-Overlay -ZipPath $rocmZip -Target $InstallDir -Label 'ROCm overlay'
    } finally {
        Remove-Item -LiteralPath (Split-Path -Parent $rocmZip) -Recurse -Force -ErrorAction SilentlyContinue
    }
} elseif ($resolvedMode -eq 'rocm') {
    Write-Host "ROCm overlay already present under $InstallDir\lib\ollama\rocm"
}

if (-not $NoPath) {
    Add-ToMachinePath -Dir $InstallDir
}

if ($ModelsDir) {
    Set-MachineModelsDir -Path $ModelsDir
}

if ($resolvedMode -eq 'vulkan') {
    Set-MachineVulkanFlag
}

# Order matters: the marker is the completion receipt, so it goes last, only
# once the post-install check has confirmed the binary, the PATH entry and
# the GPU env vars. A marker written before this point would make a
# half-configured install look complete forever (#190).
Test-Install -InstallDir $InstallDir -GpuMode $resolvedMode -NoPath:$NoPath
Write-WairedManagedMarker -InstallDir $InstallDir
Write-Host 'Done.'
