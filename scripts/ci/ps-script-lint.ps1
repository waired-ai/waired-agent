#!/usr/bin/env pwsh
# ps-script-lint.ps1 -- PSScriptAnalyzer over the PowerShell scripts that ship
# to end users, plus the harness that exercises them. The PowerShell half of
# `make install-script-lint`; run it with `make ps-script-lint`.
#
# The rules that do not apply to a Windows installer are declared, with a reason
# each, in PSScriptAnalyzerSettings.psd1 next to this file. That file is the
# record -- not inline suppressions scattered through the scripts.
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$Root = (& git -C $PSScriptRoot rev-parse --show-toplevel).Trim()
$Settings = Join-Path $PSScriptRoot 'PSScriptAnalyzerSettings.psd1'

# Everything that reaches a user machine as PowerShell:
#   packaging/install/*.ps1     the `iwr | iex` one-liner and its uninstaller
#   scripts/install/*.ps1       embedded in the agent binary (scripts/install/embed.go)
#                               and written out at runtime
#   scripts/dev/installtest-*   not shipped, but it is the thing asserting on
#                               the above, so it is held to the same bar
$Targets = @(
    'packaging/install/install.ps1'
    'packaging/install/uninstall.ps1'
    'scripts/install/ollama-windows.ps1'
    'scripts/install/waired-agent-windows.ps1'
    'scripts/dev/installtest-pwsh.ps1'
    'scripts/ci/ps-script-lint.ps1'
)

if (-not (Get-Module -ListAvailable PSScriptAnalyzer)) {
    Write-Error 'PSScriptAnalyzer not found. Install-Module PSScriptAnalyzer -Scope CurrentUser'
    exit 1
}
Import-Module PSScriptAnalyzer

# One file at a time: -Path takes a single path, and a missing target should say
# which one rather than failing the whole run opaquely.
$findings = @()
foreach ($rel in $Targets) {
    $full = Join-Path $Root $rel
    if (-not (Test-Path -LiteralPath $full)) {
        Write-Error "target not found: $rel"
        exit 1
    }
    $findings += Invoke-ScriptAnalyzer -Path $full -Settings $Settings
}

if ($findings.Count -gt 0) {
    $findings |
        Select-Object @{ n = 'File'; e = { Split-Path -Leaf $_.ScriptPath } }, Line, Severity, RuleName, Message |
        Format-Table -AutoSize | Out-String -Width 200 | Write-Host
    Write-Host ("PSScriptAnalyzer: {0} finding(s) across {1} file(s)" -f $findings.Count, $Targets.Count) -ForegroundColor Red
    Write-Host 'Fix them, or -- if the rule genuinely does not apply to an installer --' -ForegroundColor Red
    Write-Host 'add it to scripts/ci/PSScriptAnalyzerSettings.psd1 WITH the reason.' -ForegroundColor Red
    exit 1
}
Write-Host ("PSScriptAnalyzer: OK ({0} files)" -f $Targets.Count) -ForegroundColor Green
