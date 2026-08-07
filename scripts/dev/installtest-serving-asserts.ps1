#!/usr/bin/env pwsh
# installtest-serving-asserts.ps1 -- the Windows third of
# installtest-serving-asserts.sh. Runs Assert-ServingEngine
# (installtest-windows.ps1) over the shared scenario table and prints one
# normalized transcript line per assert; the .sh compares that against the
# Linux and macOS transcripts. See the .sh for the whole rationale.
#
# The function is lifted out of installtest-windows.ps1 with the AST rather
# than copied here, so what runs IS what the leg runs. The script cannot be
# dot-sourced: it installs an agent.
#
# Get-NetTCPConnection / Get-CimInstance / Invoke-RestMethod do not exist (or
# do not apply) on the Linux runner this executes on, so they are shadowed by
# functions -- PowerShell resolves functions ahead of cmdlets.
$ErrorActionPreference = 'Stop'

$root = (& git -C $PSScriptRoot rev-parse --show-toplevel).Trim()
$src = Join-Path $root 'scripts/dev/installtest-windows.ps1'
$ast = [System.Management.Automation.Language.Parser]::ParseFile($src, [ref]$null, [ref]$null)
$fn = $ast.FindAll({
    param($n)
    $n -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq 'Assert-ServingEngine'
}, $true)
if (-not $fn) { Write-Error "Assert-ServingEngine not found in $src"; exit 1 }
Invoke-Expression $fn[0].Extent.Text

# The two paths the .sh normalizes to <BIN> and <OTHER>. $StateDir carries no
# drive letter: this runs under pwsh on the LINUX CI runner, where 'C:\...'
# makes Join-Path fail with "a drive with the name 'C' does not exist". The
# exact spelling does not matter -- both paths are announced on the marker lines
# below and normalized out of the transcript.
$StateDir = '/ProgramData/waired'
$BundledBin = Join-Path $StateDir 'runtimes\ollama\bin\ollama.exe'
$ForeignBin = Join-Path '/Program Files' 'Ollama\ollama.exe'
Write-Output "#BIN $BundledBin"
Write-Output "#OTHER $ForeignBin"

$script:Lines = @()
function ItOk  { param([string]$m) $script:Lines += "ok   $m" }
function ItBad { param([string]$m) $script:Lines += "FAIL $m" }
function ItLog { param([string]$m) }
# The poll loop is deadline-based (`while ((Get-Date) -lt $deadline)`), so
# stubbing Start-Sleep alone would leave the not-answering scenario BUSY
# SPINNING for the full 180 s rather than skipping it. Advance a fake clock
# instead, and let the real loop reach its real deadline in a few iterations.
function Start-Sleep { param([int]$Seconds) }
$script:Clock = [datetime]'2026-01-01T00:00:00Z'
function Get-Date { $script:Clock = $script:Clock.AddSeconds(30); $script:Clock }

$script:Version = $null     # $null = the port refuses
$script:Status  = $null     # $null = the mgmt API refuses
$script:Conn    = $null     # $null = no listener could be identified
$script:ExePath = $null

function Invoke-RestMethod {
    param([string]$Uri, [int]$TimeoutSec)
    if ($Uri -like '*9475/api/version*') {
        if ($null -eq $script:Version) { throw 'connection refused' }
        return $script:Version
    }
    if ($null -eq $script:Status) { throw 'connection refused' }
    return $script:Status
}
function Get-NetTCPConnection {
    param([int]$LocalPort, [string]$State, [string]$ErrorAction)
    if ($null -eq $script:Conn) { throw 'no listener' }
    return $script:Conn
}
function Get-CimInstance {
    param([string]$ClassName, [string]$Filter, [string]$ErrorAction)
    if ($null -eq $script:ExePath) { throw 'access denied' }
    return [pscustomobject]@{ ExecutablePath = $script:ExePath }
}
function Get-Content { param([string]$LiteralPath, [int]$Tail, [string]$ErrorAction) @() }
function Test-Path   { param([string]$LiteralPath) $false }

function New-Status {
    param([string]$Mode, [string]$Pinned)
    [pscustomobject]@{ runtimes = [pscustomobject]@{ ollama = [pscustomobject]@{
        mode = $Mode; live_version = '0.31.1'; pinned_version = $Pinned } } }
}
$good = New-Status -Mode 'spawned' -Pinned '0.31.1'

function Invoke-Scenario {
    param([string]$Name)
    $script:Lines = @()
    Assert-ServingEngine -Context 'ctx'
    Write-Output "# $Name"
    $script:Lines | ForEach-Object { Write-Output $_ }
}

# --- the shared scenario table (same order as installtest-serving-asserts.sh)
$script:Version = [pscustomobject]@{ version = '0.31.1' }
$script:Status = $good
$script:Conn = [pscustomobject]@{ OwningProcess = 4242 }
$script:ExePath = $BundledBin
Invoke-Scenario 'all-good'

$script:ExePath = $ForeignBin
Invoke-Scenario 'foreign-binary-on-the-port'

$script:ExePath = $BundledBin
$script:Version = [pscustomobject]@{ version = '0.24.0' }
Invoke-Scenario 'version-mismatch'

$script:Version = [pscustomobject]@{ version = '0.31.1' }
$script:Status = New-Status -Mode 'adopted' -Pinned '0.31.1'
Invoke-Scenario 'adopted-engine'

$script:Status = $null
Invoke-Scenario 'daemon-silent'

$script:Status = $good
$script:Conn = $null
Invoke-Scenario 'listener-unidentifiable'

$script:Conn = [pscustomobject]@{ OwningProcess = 4242 }
$script:Version = $null
Invoke-Scenario 'engine-not-answering'
