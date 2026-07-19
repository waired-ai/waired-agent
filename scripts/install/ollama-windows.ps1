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

# Invoke-DownloadWithProgress streams $Url to $OutFile while printing periodic
# progress. The Ollama archive is ~1.4 GB, and the old silent
# `Invoke-WebRequest -OutFile` left the console dead for minutes (waired#747).
# This gives the byte-level feedback the Linux/macOS path already gets from
# `waired runtimes install ollama` (cmd/waired/runtimes_install_render.go's
# drawDownloadLine: percent, transferred/total, rate). PS 5.1-safe: a raw
# HttpWebRequest + manual read loop -- NOT Invoke-WebRequest, whose 5.1 progress
# bar re-renders per read and cripples large-file throughput (the reason
# $ProgressPreference is 'SilentlyContinue' above). Prints a fresh line (not an
# in-place \r bar) every >=3% or ~2s so the elevated-UAC transcript and non-TTY
# CI logs capture it cleanly.
function Invoke-DownloadWithProgress {
    param(
        [Parameter(Mandatory)][string]$Url,
        [Parameter(Mandatory)][string]$OutFile
    )
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
            # Throttle: advance printed percent by >=3, or >=2s since last line
            # (the time gate also covers unknown-length downloads).
            if ((($pct -ge 0) -and ($pct -ge $lastPct + 3)) -or (($elapsed - $lastTick) -ge 2)) {
                $rate = if ($elapsed -gt 0) { ($done / 1MB) / $elapsed } else { 0 }
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

function Verify-Signature {
    param([string]$Exe)
    $sig = Get-AuthenticodeSignature -FilePath $Exe
    if ($sig.Status -ne 'Valid') {
        throw "ollama.exe Authenticode status is '$($sig.Status)' (expected 'Valid')."
    }
    Write-Host "Signed by: $($sig.SignerCertificate.Subject)"
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

function Test-Install {
    param(
        [string]$InstallDir,
        [string]$GpuMode
    )

    $exe = Join-Path $InstallDir 'ollama.exe'
    if (-not (Test-Path -LiteralPath $exe)) {
        throw "Post-install check: $exe missing."
    }
    Write-Host "Installed at: $exe"
    Write-Host "GPU mode:     $GpuMode"

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

$existing = Get-OllamaExePath
$needBaseInstall = (-not $existing) -or $Force
$needRocmInstall = $resolvedMode -eq 'rocm' -and (
    $needBaseInstall -or
    -not (Test-Path -LiteralPath (Join-Path $InstallDir 'lib\ollama\rocm'))
)

if ($needBaseInstall) {
    $baseZip = Stage-ZipDownload -Url $ZipUrl -MinSizeBytes (50MB)
    try {
        Clean-InstallDir -Target $InstallDir
        Expand-Overlay -ZipPath $baseZip -Target $InstallDir -Label 'base archive'
        $exe = Join-Path $InstallDir 'ollama.exe'
        if (-not (Test-Path -LiteralPath $exe)) {
            throw "Extraction completed but ollama.exe was not found at $exe."
        }
        Verify-Signature -Exe $exe
    } finally {
        Remove-Item -LiteralPath (Split-Path -Parent $baseZip) -Recurse -Force -ErrorAction SilentlyContinue
    }
} else {
    Write-Host "Base install already present: $existing (pass -Force to reinstall)"
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

Test-Install -InstallDir $InstallDir -GpuMode $resolvedMode
Write-Host 'Done.'
