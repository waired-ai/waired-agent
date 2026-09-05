#!/usr/bin/env pwsh
# ps-script-lint.ps1 -- PSScriptAnalyzer over EVERY PowerShell script in this
# repository. The PowerShell half of `make install-script-lint`; run it with
# `make ps-script-lint`.
#
# The rules that do not apply are declared, with a reason each, in
# PSScriptAnalyzerSettings.psd1 (the shipped surface) and
# PSScriptAnalyzerSettings.Tooling.psd1 (the delta for everything else) next to
# this file. Those files are the record -- not inline suppressions scattered
# through the scripts.
#
# The target set is DERIVED, not enumerated (waired-agent#1224). It used to be a
# hand-kept list of five paths under a comment that claimed to cover
# `scripts/dev/installtest-*`; by the time anyone checked, three files matching
# that claim were missing from the list, one of them the 5,600-line harness that
# carries the Windows contract asserts. The prose was the rule and the list was
# the execution, and they had drifted -- the same escape as waired-agent#1119.
# The shell half of this pair had already made this decision and written down
# why (scripts/ci/install-script-lint.sh: "Every CI script, discovered rather
# than enumerated. The hand-kept list this replaced had drifted eight scripts
# behind"); only the PowerShell half was left enumerating, and it drifted the
# same way. This applies that decision here.
#
# `git ls-files` rather than a filesystem glob: a developer's untracked scratch
# .ps1 is not this gate's business, and the tracked set is what CI lints.
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$Root = (& git -C $PSScriptRoot rev-parse --show-toplevel).Trim()
$ShippedSettingsPath = Join-Path $PSScriptRoot 'PSScriptAnalyzerSettings.psd1'
$ToolingDeltaPath    = Join-Path $PSScriptRoot 'PSScriptAnalyzerSettings.Tooling.psd1'

# Two groups, because "does this rule apply" has two different answers and one
# settings file cannot hold both.
#
#   SHIPPED  Everything that reaches a user machine as PowerShell, plus this
#            gate itself:
#              packaging/install/  the `iwr | iex` one-liner and its uninstaller
#              scripts/install/    embedded in the agent binary
#                                  (scripts/install/embed.go), written out at runtime
#              scripts/ci/         this script -- a gate is held to the bar it sets
#            Strictest: it gets PSScriptAnalyzerSettings.psd1 unmodified.
#
#   TOOLING  Everything else in the tree. None of it reaches a user machine; all
#            of it either exercises what does or builds and stages it:
#              scripts/dev/        the installtest harnesses and the dev probes
#              packaging/windows/  the zip build, the smoke runners, the Hyper-V
#                                  test-VM scripts
#            Gets the shipped rules PLUS the delta in
#            PSScriptAnalyzerSettings.Tooling.psd1 -- rules whose subject is a
#            method these scripts use on purpose (lifting a shipped function
#            body out and running IT, shadowing a cmdlet to measure the real
#            one, minting a throwaway account) and which would be silenced for
#            the SHIPPED scripts too if they lived in the one settings file.
#
# NOT excluded any more: scripts/dev/installtest-serving-asserts.ps1 and
# scripts/dev/installtest-swap.ps1. They were named here as unlintable because
# their method is what PSAvoidOverwritingBuiltInCmdlets and
# PSAvoidUsingInvokeExpression forbid. That reasoning was sound and now lives in
# the Tooling delta instead, where it silences those rules for the harnesses
# without touching the shipped scripts -- so both files ARE linted, and every
# rule they do not trip still applies to them. Re-checking the pair while doing
# this showed the argument had also gone stale on one of them: installtest-swap.ps1
# reports neither named rule any more (waired-agent#1224).
$Groups = @(
    @{ Name = 'shipped'; Prefixes = @('packaging/install/', 'scripts/install/', 'scripts/ci/') }
    @{ Name = 'tooling'; Prefixes = @('scripts/dev/', 'packaging/windows/') }
)

if (-not (Get-Module -ListAvailable PSScriptAnalyzer)) {
    Write-Error 'PSScriptAnalyzer not found. Install-Module PSScriptAnalyzer -Scope CurrentUser'
    exit 1
}
Import-Module PSScriptAnalyzer

$Shipped = Import-PowerShellDataFile -LiteralPath $ShippedSettingsPath
$Delta   = Import-PowerShellDataFile -LiteralPath $ToolingDeltaPath
# Merged, not duplicated: the Tooling file holds only what it ADDS, so the
# shipped rules exist in exactly one place and cannot rot in a second copy.
$Tooling = $Shipped.Clone()
$Tooling.ExcludeRules = @($Shipped.ExcludeRules) + @($Delta.ExcludeRules)
$Settings = @{ shipped = $Shipped; tooling = $Tooling }

$tracked = @(& git -C $Root ls-files -- '*.ps1')
if ($tracked.Count -eq 0) {
    Write-Error 'no tracked .ps1 files found -- this gate would pass vacuously'
    exit 1
}

# Every tracked .ps1 has to land in a group. A file in neither is the failure
# this change exists to remove: it would be silently unlinted and nothing would
# say so. Adding a .ps1 under a new directory is a decision about which bar it is
# held to, so make the person adding it take that decision.
$assigned = @{}
foreach ($g in $Groups) { $assigned[$g.Name] = @() }
$unclassified = @()
foreach ($rel in $tracked) {
    $group = $null
    foreach ($g in $Groups) {
        foreach ($p in $g.Prefixes) {
            if ($rel.StartsWith($p, [StringComparison]::Ordinal)) { $group = $g.Name; break }
        }
        if ($group) { break }
    }
    if ($group) { $assigned[$group] += $rel } else { $unclassified += $rel }
}

if ($unclassified.Count -gt 0) {
    Write-Host 'PSScriptAnalyzer: these tracked .ps1 files are in no lint group:' -ForegroundColor Red
    $unclassified | ForEach-Object { Write-Host "  $_" -ForegroundColor Red }
    Write-Host 'Add the directory to $Groups in scripts/ci/ps-script-lint.ps1 -- to' -ForegroundColor Red
    Write-Host 'shipped if the file reaches a user machine, to tooling otherwise.' -ForegroundColor Red
    exit 1
}

# An empty group means a directory moved and this gate went quiet rather than
# red. A lint that checks nothing prints the same green as one that checked
# everything, which is how a target list drifts unnoticed in the first place.
foreach ($g in $Groups) {
    if ($assigned[$g.Name].Count -eq 0) {
        Write-Error ("lint group '{0}' matched no files -- did {1} move?" -f $g.Name, ($g.Prefixes -join ', '))
        exit 1
    }
}

# One file at a time: -Path takes a single path, and a missing target should say
# which one rather than failing the whole run opaquely.
$findings = @()
$total = 0
foreach ($g in $Groups) {
    foreach ($rel in $assigned[$g.Name]) {
        $full = Join-Path $Root $rel
        if (-not (Test-Path -LiteralPath $full)) {
            Write-Error "target not found: $rel"
            exit 1
        }
        $findings += Invoke-ScriptAnalyzer -Path $full -Settings $Settings[$g.Name]
        $total++
    }
}

if ($findings.Count -gt 0) {
    $findings |
        Select-Object @{ n = 'File'; e = { Split-Path -Leaf $_.ScriptPath } }, Line, Severity, RuleName, Message |
        Format-Table -AutoSize | Out-String -Width 200 | Write-Host
    Write-Host ("PSScriptAnalyzer: {0} finding(s) across {1} file(s)" -f $findings.Count, $total) -ForegroundColor Red
    Write-Host 'Fix them, or -- if the rule genuinely does not apply -- add it to' -ForegroundColor Red
    Write-Host 'scripts/ci/PSScriptAnalyzerSettings.psd1 (shipped) or' -ForegroundColor Red
    Write-Host 'scripts/ci/PSScriptAnalyzerSettings.Tooling.psd1 (everything else) WITH the reason.' -ForegroundColor Red
    exit 1
}
Write-Host ("PSScriptAnalyzer: OK ({0} files: {1})" -f $total,
    (($Groups | ForEach-Object { "{0} {1}" -f $assigned[$_.Name].Count, $_.Name }) -join ', ')) -ForegroundColor Green
