#Requires -Version 5.1
<#
.SYNOPSIS
    Pack the Windows release zip and write its SHA-256 checksum file.

.DESCRIPTION
    Helper invoked from the `dist-windows-installer` Makefile target.
    Lives in PowerShell so it can call Compress-Archive (which has no
    POSIX-portable analogue) without leaking platform-specific quoting
    into Make.

    Inputs:
        -SourceDir : staging directory whose contents become the zip root.
        -OutZip    : absolute path to the zip to create.

    Outputs:
        $OutZip                    (zip with $SourceDir/* at the root)
        "$OutZip.sha256"           ("<hex>  <basename>" line, matching
                                    the format `sha256sum` emits -- the
                                    PowerShell installer reads only the
                                    first whitespace-separated field, so
                                    using the same shape on both sides
                                    keeps things consistent.)

    Idempotent: overwrites both files if present.

.EXAMPLE
    powershell -NoProfile -File packaging\windows\make-zip.ps1 `
        -SourceDir dist\windows-amd64 `
        -OutZip dist\waired-windows-amd64.zip
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$SourceDir,
    [Parameter(Mandatory=$true)][string]$OutZip
)

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'

if (-not (Test-Path -LiteralPath $SourceDir -PathType Container)) {
    throw "SourceDir not found: $SourceDir"
}

$outDir = Split-Path -Parent (Resolve-Path -LiteralPath (Split-Path -Parent $OutZip)).Path
if (-not (Test-Path -LiteralPath $outDir)) {
    New-Item -ItemType Directory -Path $outDir -Force | Out-Null
}

if (Test-Path -LiteralPath $OutZip) {
    Remove-Item -LiteralPath $OutZip -Force
}

# Use a wildcard pattern under SourceDir so the archive root contains
# the files directly (waired.exe, waired-agent.exe, ...) rather than a
# wrapper directory named after the staging folder.
Compress-Archive -Path (Join-Path $SourceDir '*') -DestinationPath $OutZip -Force

$hash = (Get-FileHash -LiteralPath $OutZip -Algorithm SHA256).Hash.ToLowerInvariant()
$base = Split-Path -Leaf $OutZip
$shaPath = "$OutZip.sha256"
# Two-space separator matches GNU coreutils' sha256sum format.
Set-Content -LiteralPath $shaPath -Value "$hash  $base" -Encoding ASCII -NoNewline

Write-Host "==> $OutZip"
Write-Host "==> $shaPath ($hash)"
