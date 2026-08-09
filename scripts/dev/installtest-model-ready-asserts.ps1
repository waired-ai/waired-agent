#!/usr/bin/env pwsh
# installtest-model-ready-asserts.ps1 -- the Windows third of
# installtest-model-ready-asserts.sh. Runs Get-ModelReadyState
# (installtest-windows.ps1) over the shared fixture table and prints one
# transcript line per payload; the .sh compares that against the Linux and
# macOS transcripts. See the .sh for the whole rationale.
#
# The function is lifted out of installtest-windows.ps1 with the AST rather
# than copied here, so what runs IS what the leg runs. The script cannot be
# dot-sourced: it installs an agent.
#
# The fixtures are the same files the two .sh copies read
# (scripts/dev/testdata/inference-status/), parsed with ConvertFrom-Json --
# which is what the leg itself does, since Invoke-RestMethod returns objects.
# So this checks the same payload through the real Windows reading path, not a
# transliteration of the shell one.
$ErrorActionPreference = 'Stop'

$root = (& git -C $PSScriptRoot rev-parse --show-toplevel).Trim()
$src = Join-Path $root 'scripts/dev/installtest-windows.ps1'
$ast = [System.Management.Automation.Language.Parser]::ParseFile($src, [ref]$null, [ref]$null)
$fn = $ast.FindAll({
    param($n)
    $n -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq 'Get-ModelReadyState'
}, $true)
if (-not $fn) { Write-Error "Get-ModelReadyState not found in $src"; exit 1 }
Invoke-Expression $fn[0].Extent.Text

$fixtures = Get-ChildItem -Path (Join-Path $root 'scripts/dev/testdata/inference-status') -Filter '*.json' |
    Sort-Object Name

foreach ($f in $fixtures) {
    $raw = Get-Content -Raw -LiteralPath $f.FullName
    # An empty payload is the unreachable-daemon fixture: ConvertFrom-Json
    # throws on '', and the leg's own `catch { }` is what swallows that, so
    # $null is the state the real code sees.
    $status = $null
    if ($raw -and $raw.Trim()) { $status = $raw | ConvertFrom-Json }
    Write-Output ("{0} {1}" -f $f.BaseName, (Get-ModelReadyState $status))
}
