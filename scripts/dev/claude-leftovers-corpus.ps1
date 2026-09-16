# claude-leftovers-corpus.ps1 -- run packaging/install/testdata/claude-leftovers
# through uninstall.ps1's own copy of the rules (waired-agent#1398).
#
# The functions are lifted out of the shipped file rather than copied, the way
# installtest-windows.ps1 lifts Get-ExitCodeReason: a copy would test itself.
# Both installtest-pwsh.ps1 (PowerShell 7 on Linux) and installtest-windows.ps1
# (Windows PowerShell 5.1) run this, because the two differ in exactly the
# places a JSON round trip can go wrong, and the uninstaller runs under both.
#
# scripts/install/claude_leftovers_corpus_test.go holds the Go removers to the
# same cases, so a disagreement between the Go rules and this copy fails on one
# side or the other.
#
#   pwsh -NoProfile -File scripts/dev/claude-leftovers-corpus.ps1
#   powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts\dev\claude-leftovers-corpus.ps1
#
# Exit 0 when every case matches, 1 otherwise.
param(
    [string]$Script = '',
    [string]$Corpus = ''
)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
if (-not $Script) { $Script = Join-Path (Join-Path (Join-Path $root 'packaging') 'install') 'uninstall.ps1' }
if (-not $Corpus) { $Corpus = Join-Path (Join-Path (Join-Path (Join-Path $root 'packaging') 'install') 'testdata') 'claude-leftovers' }

$lift = @(
    'ConvertFrom-ClaudeJson', 'Skip-ClaudeJsonSpace', 'Read-ClaudeJsonValue', 'Read-ClaudeJsonString',
    'ConvertTo-ClaudeJson', 'Format-ClaudeJsonString', 'Test-WairedModelId', 'Test-ClaudeJsonObject',
    'Test-ClaudeJsonList', 'Test-ClaudeHookEntryIsWaired', 'Get-ClaudePickerKind', 'Edit-ClaudeLeftovers'
)
$tokens = $null
$errors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile($Script, [ref]$tokens, [ref]$errors)
if ($errors -and $errors.Count -gt 0) { throw "$Script doesn't parse: $($errors[0].Message)" }
$defs = @($ast.FindAll({ param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false))
foreach ($name in $lift) {
    $def = @($defs | Where-Object { $_.Name -eq $name })
    if ($def.Count -ne 1) { throw "$Script has $($def.Count) definitions of $name; the corpus runner needs exactly one" }
    . ([scriptblock]::Create($def[0].Extent.Text))
}

# ConvertTo-CanonicalJson sorts object keys at every level, so two documents
# that differ only in key order compare equal: the Go removers rewrite with
# sorted keys and this copy keeps the file's own order.
function ConvertTo-CanonicalValue {
    param($Value)
    if (Test-ClaudeJsonObject $Value) {
        $sorted = New-Object System.Collections.Specialized.OrderedDictionary
        foreach ($k in @($Value.Keys | Sort-Object -CaseSensitive { [string]$_ })) {
            $sorted.Add($k, (ConvertTo-CanonicalValue -Value $Value[$k]))
        }
        return ,$sorted
    }
    if (Test-ClaudeJsonList $Value) {
        $list = New-Object System.Collections.ArrayList
        foreach ($item in $Value) { [void]$list.Add((ConvertTo-CanonicalValue -Value $item)) }
        return ,$list
    }
    return ,$Value
}
function ConvertTo-CanonicalJson {
    param([string]$Text)
    if ($Text.Length -gt 0 -and [int]$Text[0] -eq 0xFEFF) { $Text = $Text.Substring(1) }
    return (ConvertTo-ClaudeJson -Value (ConvertTo-CanonicalValue -Value (ConvertFrom-ClaudeJson -Text $Text)))
}

$kinds = @(
    @{ Dir = 'managed'; Kind = 'managed'; EmptyIsAbsent = $false },
    @{ Dir = 'user-settings'; Kind = 'user'; EmptyIsAbsent = $true },
    @{ Dir = 'retired-cache'; Kind = 'cache'; EmptyIsAbsent = $false }
)
$utf8 = New-Object System.Text.UTF8Encoding($false)
$failures = 0
$ran = 0
foreach ($k in $kinds) {
    $cases = @(Get-ChildItem -LiteralPath (Join-Path $Corpus $k.Dir) -Directory | Sort-Object Name)
    if ($cases.Count -eq 0) { throw "no cases under $($k.Dir)" }
    foreach ($case in $cases) {
        $ran++
        $label = "$($k.Dir)/$($case.Name)"
        # Decoded the way Read-ClaudeSettingsText decodes a real file.
        $text = [System.IO.File]::ReadAllText((Join-Path $case.FullName 'input.json'))
        $problem = ''
        try {
            $edit = Edit-ClaudeLeftovers -Kind $k.Kind -Text $text
            $gotAbsent = $edit.State -eq 'delete'
            $gotText = $null
            if ($edit.State -eq 'rewrite') { $gotText = $edit.Text }
            if ($null -ne $gotText -and $k.EmptyIsAbsent -and (ConvertTo-CanonicalJson -Text $gotText) -eq '{}') { $gotAbsent = $true }
            if (Test-Path -LiteralPath (Join-Path $case.FullName 'expected.absent')) {
                if (-not $gotAbsent) { $problem = "want the file removed, got state $($edit.State)" }
            } elseif (Test-Path -LiteralPath (Join-Path $case.FullName 'expected.unchanged')) {
                if ($edit.State -ne 'unchanged' -and $edit.State -ne 'unreadable') {
                    $problem = "want the file untouched, got state $($edit.State)"
                }
            } else {
                $want = [System.IO.File]::ReadAllText((Join-Path $case.FullName 'expected.json'))
                if ($edit.State -ne 'rewrite') {
                    $problem = "want a rewrite, got state $($edit.State)"
                } elseif ((ConvertTo-CanonicalJson -Text $gotText) -cne (ConvertTo-CanonicalJson -Text $want)) {
                    $problem = "result differs from expected.json`n got: $gotText`nwant: $want"
                } elseif ($utf8.GetString($utf8.GetBytes($gotText)) -cne $gotText) {
                    $problem = 'result does not survive UTF-8'
                }
            }
        } catch {
            $problem = "threw: $($_.Exception.Message)"
        }
        if ($problem) {
            $failures++
            Write-Output "FAIL $label -- $problem"
        } else {
            Write-Output "ok   $label"
        }
    }
}
Write-Output "claude-leftovers corpus: $ran cases, $failures failed ($($PSVersionTable.PSVersion))"
if ($failures -gt 0) { exit 1 }
exit 0
