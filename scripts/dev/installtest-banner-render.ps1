#Requires -Version 5.1
<#
.SYNOPSIS
    CI guard (#571): render the installer banner on a REAL console under a
    simulated Japanese wire path and assert the glyphs survive with no "?"
    mojibake.

.DESCRIPTION
    #572 fixed the installer banner mojibake at the source (install.ps1 is now
    pure-ASCII on the wire; the non-ASCII glyphs are built at runtime via
    Glyph / Utf8FromB64). scripts/install/encoding_test.go guards the *source*
    bytes. What CI still never does is actually *render* the banner: every
    installtest step runs with redirected stdout, and Show-Banner only emits the
    rich UTF-8 banner on a real TTY (Console.OutputEncoding==65001 &&
    !IsOutputRedirected && WindowWidth>=60). So a regression that renders as "?"
    on a real Japanese console could slip past CI. This script closes that gap.

    Mechanism (runs on the en-US self-hosted Hyper-V golden -- NO locale change):
      * Serve the working-tree packaging/install/install.ps1 on a loopback
        HttpListener (same pattern as installtest-windows.ps1's Start-Mirror).
      * For each {Windows PowerShell 5.1, pwsh 7} x {realiwr, cp932} combination,
        spawn a CHILD process with its OWN real console (Win32 CREATE_NEW_CONSOLE,
        via Start-Process's default new window) so Show-Banner takes the rich
        path. Each child fetches the served script through the chosen wire path,
        runs it banner-only (the WAIRED_BANNER_SELFTEST seam in install.ps1),
        reads back its OWN console screen buffer (ReadConsoleOutputCharacterW --
        works in both PS editions, unlike GetBufferContents which is 5.1-only),
        and writes the captured code points to a result file.
      * The parent asserts each capture contains the sentinel banner glyphs
        (U+2588 etc.) and NO U+003F "?".

    Why the two wire legs differ (and which one gates):
      * cp932  = GATING. `chcp 932` sets only the console *output* code page; the
        real iwr|iex corruption is driven by the system ANSI code page (ACP),
        which is 1252 on the en-US golden. So we reproduce the Japanese client's
        wire coercion *ourselves* -- Encoding.GetEncoding(932).GetString(bytes) --
        which is byte-identical to what a ja-JP host's iwr|iex produces,
        independent of the runner's locale.
      * realiwr = smoke only. A genuine `iwr -useb <url>` on this en-US runner
        decodes through ACP 1252 and cannot reproduce the cp932 regression; it
        just exercises the real download + real-console render path.

    -SelfCheck additionally serves a MUTATED copy that reintroduces the pre-#572
    defect (a literal UTF-8 glyph in the banner output path) into the temp mirror
    (never the repo -- encoding_test.go keeps the repo file ASCII) and asserts the
    cp932 leg goes RED, proving the detector has teeth.

.PARAMETER SelfCheck
    Also run the negative (injected-mojibake) case and require it to be detected.
    CI runs with -SelfCheck.

.PARAMETER Port
    Loopback mirror port. Default 8097 (distinct from installtest-windows.ps1's
    8099); override via env IT_BANNER_PORT.

.PARAMETER Child
    Internal. Set by the parent when it re-invokes itself as a banner-rendering
    child; not for direct use.
#>
[CmdletBinding(DefaultParameterSetName = 'Parent')]
param(
    [Parameter(ParameterSetName = 'Parent')]
    [switch]$SelfCheck,

    [Parameter(ParameterSetName = 'Parent')]
    [int]$Port = $(if ($env:IT_BANNER_PORT) { [int]$env:IT_BANNER_PORT } else { 8097 }),

    # --- internal child-mode knobs ---
    [Parameter(ParameterSetName = 'Child', Mandatory = $true)]
    [switch]$Child,
    [Parameter(ParameterSetName = 'Child')][ValidateSet('winps', 'pwsh')][string]$Edition,
    [Parameter(ParameterSetName = 'Child')][ValidateSet('realiwr', 'cp932')][string]$Wire,
    [Parameter(ParameterSetName = 'Child')][string]$Url,
    [Parameter(ParameterSetName = 'Child')][string]$ResultFile
)

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'   # keep the iwr progress bar out of the screen buffer

# ---------------------------------------------------------------------------
# Sentinel glyphs the correctly-rendered rich banner must contain. Built at
# runtime from code points so THIS script stays pure-ASCII (it never embeds a
# non-ASCII byte and so can't itself mojibake). Assert set-membership + absence
# of "?" -- never column-exact equality: East-Asian-Ambiguous width can pad or
# duplicate cells in a DBCS console.
# ---------------------------------------------------------------------------
$script:Sentinels = [ordered]@{
    'U+2588 full-block' = [char]::ConvertFromUtf32(0x2588)  # in the WAIRED wordmark
    'U+2557 box-corner' = [char]::ConvertFromUtf32(0x2557)  # box drawing
    'U+255D box-corner' = [char]::ConvertFromUtf32(0x255D)  # box drawing
    'U+2504 tri-dash'   = [char]::ConvertFromUtf32(0x2504)  # the horizontal rules
    'U+00B7 middot'     = [char]::ConvertFromUtf32(0x00B7)  # tagline separators
    'U+2014 em-dash'    = [char]::ConvertFromUtf32(0x2014)  # "-- your own machine"
}

# The ASCII fallback figlet (Show-Banner's non-UTF-8 branch) is full of "\"; the
# rich banner contains none. A backslash in the capture therefore means the rich
# path was NOT taken (UTF-8 output wasn't active) -- a distinct failure from "?".
$script:FigletMarker = '\'

# =====================================================================
# CHILD MODE -- runs inside a fresh console; renders + captures the banner
# =====================================================================
if ($Child) {
    # C# console-buffer reader. ReadConsoleOutputCharacterW reads the character
    # plane only (color/VT attributes live in a separate plane), and works in
    # BOTH Windows PowerShell 5.1 and pwsh 7 -- unlike $Host.UI.RawUI.
    # GetBufferContents(), which throws PlatformNotSupported in pwsh 7.
    $conBufCs = @'
using System;
using System.Runtime.InteropServices;
public static class ConBuf {
    const int STD_OUTPUT_HANDLE = -11;
    [DllImport("kernel32.dll", SetLastError=true)]
    static extern IntPtr GetStdHandle(int n);
    [StructLayout(LayoutKind.Sequential)] public struct COORD { public short X, Y; }
    [StructLayout(LayoutKind.Sequential)] public struct SMALL_RECT { public short Left, Top, Right, Bottom; }
    [StructLayout(LayoutKind.Sequential)] public struct CSBI {
        public COORD dwSize; public COORD dwCursorPosition; public short wAttributes;
        public SMALL_RECT srWindow; public COORD dwMaximumWindowSize; }
    [DllImport("kernel32.dll", SetLastError=true)]
    static extern bool GetConsoleScreenBufferInfo(IntPtr h, out CSBI info);
    [DllImport("kernel32.dll", SetLastError=true, CharSet=CharSet.Unicode)]
    static extern bool ReadConsoleOutputCharacterW(IntPtr h, [Out] char[] buf, uint len, COORD at, out uint read);
    public static string[] ReadRows() {
        IntPtr h = GetStdHandle(STD_OUTPUT_HANDLE);
        CSBI ci;
        if (!GetConsoleScreenBufferInfo(h, out ci))
            throw new System.ComponentModel.Win32Exception(Marshal.GetLastWin32Error());
        int w = ci.dwSize.X;
        int rows = ci.dwCursorPosition.Y + 1;   // only the rows written so far
        var outp = new string[rows];
        var cbuf = new char[w];
        for (short y = 0; y < rows; y++) {
            uint got; COORD at; at.X = 0; at.Y = y;
            if (!ReadConsoleOutputCharacterW(h, cbuf, (uint)w, at, out got))
                throw new System.ComponentModel.Win32Exception(Marshal.GetLastWin32Error());
            string line = new string(cbuf, 0, (int)got);
            outp[y] = line.Replace("\0", "").TrimEnd();
        }
        return outp;
    }
}
'@
    try {
        # Put the console into DBCS (cp932) mode for realism -- a Japanese
        # console's starting state. install.ps1 forces OutputEncoding=UTF8 on
        # entry, so this is overridden for the *render*; but if that forcing ever
        # regresses, the banner would render through cp932 here and mojibake --
        # which is exactly a regression we want to catch.
        $null = & "$env:SystemRoot\System32\chcp.com" 932 2>&1

        $env:NO_COLOR              = '1'   # force Show-Banner's plain Write-Host path (no VT/SGR in the buffer)
        $env:WAIRED_BANNER_SELFTEST = '1'  # install.ps1 seam: render Show-Banner then return
        Remove-Item Env:\WAIRED_NO_EMOJI -ErrorAction SilentlyContinue  # we WANT the rich/UTF-8 path

        # --- fetch the served install.ps1 through the chosen wire path ---
        if ($Wire -eq 'cp932') {
            if ($PSVersionTable.PSEdition -eq 'Core') {
                # .NET Core ships no code pages by default; register cp932/936.
                [System.Text.Encoding]::RegisterProvider([System.Text.CodePagesEncodingProvider]::Instance)
            }
            $cp932 = [System.Text.Encoding]::GetEncoding(932)
            $bytes = (New-Object System.Net.WebClient).DownloadData($Url)
            $scriptText = $cp932.GetString($bytes)   # byte-identical to a ja-JP host's iwr|iex
        }
        else {
            $resp = Invoke-WebRequest -UseBasicParsing -Uri $Url
            $content = $resp.Content
            if ($content -is [byte[]]) {
                # WinPS 5.1 hands back byte[] for octet-stream; decode via the ACP,
                # the way a real iwr|iex would on this host (smoke leg).
                $scriptText = [System.Text.Encoding]::Default.GetString($content)
            }
            else {
                $scriptText = [string]$content
            }
        }

        # Render the banner to THIS real console. Executing the (possibly
        # corrupted) text as a script block isolates install.ps1's top-level
        # `return` (from the WAIRED_BANNER_SELFTEST seam) to the block, so control
        # comes back here for the capture. Parses/executes identically to iex.
        $sb = [ScriptBlock]::Create($scriptText)
        & $sb

        # Capture the rendered screen buffer.
        Add-Type -TypeDefinition $conBufCs -Language CSharp -ErrorAction Stop
        $rows = [ConBuf]::ReadRows()
        $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
        [System.IO.File]::WriteAllText($ResultFile, ($rows -join "`n"), $utf8NoBom)
        exit 0
    }
    catch {
        $msg = "$($_.Exception.GetType().FullName): $($_.Exception.Message)"
        try {
            $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
            [System.IO.File]::WriteAllText("$ResultFile.err", $msg, $utf8NoBom)
        } catch { }
        exit 3
    }
}

# =====================================================================
# PARENT MODE -- serve, spawn children, assert
# =====================================================================

# --- logging / counters (mirror installtest-windows.ps1) ---
$script:Pass = 0
$script:Fail = 0
function ItStep { param([string]$m) Write-Host "[banner-render] ==> $m" -ForegroundColor Green }
function ItOk   { param([string]$m) Write-Host "[banner-render]  ok  $m" -ForegroundColor Green; $script:Pass++ }
function ItBad  { param([string]$m) Write-Host "[banner-render] FAIL $m" -ForegroundColor Red;  $script:Fail++ }
function ItDie  { param([string]$m) Write-Host "[banner-render] $m" -ForegroundColor Red; exit 1 }

# --- loopback HTTP mirror (copied from installtest-windows.ps1; we intentionally
#     do NOT dot-source that script, which would run its whole build) ---
function Start-Mirror {
    param([string]$RootDir, [int]$ListenPort)
    Start-Job -ScriptBlock {
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
                $ctx.Response.ContentType    = 'application/octet-stream'
                $ctx.Response.ContentLength64 = $bytes.Length
                $ctx.Response.OutputStream.Write($bytes, 0, $bytes.Length)
            } else {
                $ctx.Response.StatusCode = 404
            }
            $ctx.Response.Close()
        }
    } -ArgumentList $RootDir, $ListenPort
}

# --- detector: classify a captured banner ---
function Test-BannerCapture {
    param([string]$Capture)
    $missing = @()
    foreach ($name in $script:Sentinels.Keys) {
        if (-not $Capture.Contains($script:Sentinels[$name])) { $missing += $name }
    }
    $hasQuestion = $Capture.Contains([char]0x003F)     # '?'
    $hasFiglet   = $Capture.Contains($script:FigletMarker)
    $ok = (-not $hasQuestion) -and (-not $hasFiglet) -and ($missing.Count -eq 0)
    $reason =
        if ($hasQuestion)          { "mojibake: '?' present in banner" }
        elseif ($hasFiglet)        { "ASCII figlet fallback rendered (UTF-8 output not active)" }
        elseif ($missing.Count)    { "missing glyphs: $($missing -join ', ')" }
        else                       { 'rich banner intact' }
    [pscustomobject]@{ Ok = $ok; Reason = $reason; HasQuestion = $hasQuestion; HasFiglet = $hasFiglet }
}

# --- run one child and return its capture (or $null on child error) ---
function Invoke-BannerChild {
    param([string]$Exe, [string]$Edition, [string]$Wire, [string]$Url)
    $rf = Join-Path $script:Work ("cap-$Edition-$Wire.txt")
    Remove-Item -LiteralPath $rf, "$rf.err" -ErrorAction SilentlyContinue
    $childArgs = @(
        '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $PSCommandPath,
        '-Child', '-Edition', $Edition, '-Wire', $Wire, '-Url', $Url, '-ResultFile', $rf
    )
    $p = Start-Process -FilePath $Exe -ArgumentList $childArgs -WindowStyle Hidden -Wait -PassThru
    if ($p.ExitCode -ne 0) {
        $errText = if (Test-Path -LiteralPath "$rf.err") { (Get-Content -LiteralPath "$rf.err" -Raw) } else { "(exit $($p.ExitCode), no .err)" }
        return [pscustomobject]@{ Capture = $null; Error = $errText.Trim() }
    }
    if (-not (Test-Path -LiteralPath $rf)) {
        return [pscustomobject]@{ Capture = $null; Error = 'child exited 0 but wrote no result file' }
    }
    [pscustomobject]@{ Capture = (Get-Content -LiteralPath $rf -Raw); Error = $null }
}

# --- resolve editions present on this host ---
function Resolve-Editions {
    $eds = @()
    $ps5 = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
    if (Test-Path -LiteralPath $ps5) { $eds += [pscustomobject]@{ Key = 'winps'; Exe = $ps5 } }
    else {
        $c = Get-Command powershell.exe -ErrorAction SilentlyContinue
        if ($c) { $eds += [pscustomobject]@{ Key = 'winps'; Exe = $c.Source } }
    }
    $pwsh = Get-Command pwsh.exe -ErrorAction SilentlyContinue
    if ($pwsh) { $eds += [pscustomobject]@{ Key = 'pwsh'; Exe = $pwsh.Source } }
    else { Write-Host "[banner-render] WARN pwsh.exe not found; skipping the pwsh 7 edition" -ForegroundColor Yellow }
    $eds
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------
if (-not $IsWindows -and $PSVersionTable.PSEdition -eq 'Core') {
    ItDie "this check is Windows-only (it exercises the Windows console + ANSI code page)."
}

$Root = (& git rev-parse --show-toplevel 2>$null)
if (-not $Root) { $Root = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path }
$Root = $Root.Trim()
$srcPs1 = Join-Path $Root 'packaging/install/install.ps1'
if (-not (Test-Path -LiteralPath $srcPs1)) { ItDie "install.ps1 not found at $srcPs1" }

$script:Work = Join-Path ([System.IO.Path]::GetTempPath()) 'waired-banner-render'
Remove-Item -LiteralPath $script:Work -Recurse -Force -ErrorAction SilentlyContinue
$mirrorDir = Join-Path $script:Work 'mirror'
New-Item -ItemType Directory -Path $mirrorDir -Force | Out-Null

# Serve the working-tree install.ps1 verbatim (byte-for-byte).
Copy-Item -LiteralPath $srcPs1 -Destination (Join-Path $mirrorDir 'install.ps1') -Force
$goodUrl = "http://127.0.0.1:$Port/install.ps1"

# For -SelfCheck: a mutated copy that reintroduces the pre-#572 defect -- LITERAL
# UTF-8 banner glyphs on the wire (exactly what install.ps1 embedded before the
# fix). Under the cp932 leg those bytes are mangled into kana/kanji/"?" (the
# trailing em-dash byte lands a real "?"), so the sentinel glyphs disappear from
# the rendered banner and the detector must go RED. Built at runtime so THIS
# script stays ASCII, and written ONLY to the temp mirror, never the repo (which
# encoding_test.go keeps pure-ASCII).
$badUrl = $null
if ($SelfCheck) {
    $srcText  = [System.IO.File]::ReadAllText($srcPs1)   # install.ps1 is ASCII -> lossless
    $seam     = 'if ($env:WAIRED_BANNER_SELFTEST) { Show-Banner; return }'
    if (-not $srcText.Contains($seam)) { ItDie "self-check: could not find the WAIRED_BANNER_SELFTEST seam in install.ps1 to mutate" }
    $lit      = -join ($script:Sentinels.Values)         # the 6 sentinel glyphs, LITERAL (raw UTF-8 bytes on write)
    $injected = "if (`$env:WAIRED_BANNER_SELFTEST) { Write-Host ' $lit  banner MOJITEST'; return }"
    $badText  = $srcText.Replace($seam, $injected)
    $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText((Join-Path $mirrorDir 'install-bad.ps1'), $badText, $utf8NoBom)
    $badUrl = "http://127.0.0.1:$Port/install-bad.ps1"
}

$editions = Resolve-Editions
if (-not $editions) { ItDie "no PowerShell edition found to spawn banner children" }

ItStep "serving install.ps1 on http://127.0.0.1:$Port (editions: $(($editions.Key) -join ', '))"
$mirror = Start-Mirror -RootDir $mirrorDir -ListenPort $Port
try {
    # readiness poll
    $ready = $false
    for ($i = 0; $i -lt 50 -and -not $ready; $i++) {
        try { $null = Invoke-WebRequest -UseBasicParsing -Uri $goodUrl -TimeoutSec 2; $ready = $true }
        catch { Start-Sleep -Milliseconds 200 }
    }
    if (-not $ready) { ItDie "loopback mirror did not become ready on :$Port" }

    # --- positive matrix: every edition x wire must render an intact banner ---
    foreach ($ed in $editions) {
        foreach ($wire in @('realiwr', 'cp932')) {
            $tag = "$($ed.Key)/$wire"
            $r = Invoke-BannerChild -Exe $ed.Exe -Edition $ed.Key -Wire $wire -Url $goodUrl
            if ($null -eq $r.Capture) { ItBad "$tag -- child error: $($r.Error)"; continue }
            $v = Test-BannerCapture -Capture $r.Capture
            if ($v.Ok) { ItOk "$tag -- $($v.Reason)" }
            else       { ItBad "$tag -- $($v.Reason)" }
        }
    }

    # --- negative (self) check: the mutated script's cp932 leg must be caught ---
    if ($SelfCheck) {
        $ed  = $editions[0]
        $tag = "$($ed.Key)/cp932 (injected-mojibake)"
        $r = Invoke-BannerChild -Exe $ed.Exe -Edition $ed.Key -Wire 'cp932' -Url $badUrl
        if ($null -eq $r.Capture) {
            # A parse/exec failure of the corrupted script also counts as caught.
            ItOk "$tag -- regression caught (child error: $($r.Error))"
        }
        else {
            $v = Test-BannerCapture -Capture $r.Capture
            if (-not $v.Ok) { ItOk "$tag -- regression caught ($($v.Reason))" }
            else            { ItBad "$tag -- GUARD IS BLIND: injected mojibake rendered clean" }
        }
    }
}
finally {
    if ($mirror) { Remove-Job $mirror -Force -ErrorAction SilentlyContinue }
    Remove-Item -LiteralPath $script:Work -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Host ""
if ($script:Fail -gt 0) {
    ItDie "banner render check FAILED ($script:Pass ok, $script:Fail failed)"
}
Write-Host "[banner-render] all good ($script:Pass ok, 0 failed)" -ForegroundColor Green
exit 0
