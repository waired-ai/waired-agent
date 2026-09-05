# PSScriptAnalyzer settings DELTA for the PowerShell that never reaches a user
# machine: scripts/dev/ and packaging/windows/.
#
# This is not a standalone settings file. It holds only what the tooling group
# ADDS to PSScriptAnalyzerSettings.psd1; ps-script-lint.ps1 imports both and
# merges them. Written that way on purpose -- a second self-contained copy of
# the shipped rules would be a second thing to keep current, and it is exactly
# the copy nobody would remember to update.
#
# Why a separate group at all (waired-agent#1224): each rule below is one the
# harnesses and the VM/packaging scripts break deliberately, as their method.
# Putting them in the one settings file would silence them for the SHIPPED
# installers too, where they are the rules that earn their keep -- no shipped
# script uses Invoke-Expression, Start-Job or ConvertTo-SecureString at all, and
# a day when one does is a day someone should be told.
#
# Same standard as the shipped file: a rule whose exclusion cannot be justified
# in a sentence does not belong here -- fix the code instead. Five findings were
# fixed rather than excluded when this group was introduced: the #1051 role-
# guidance assert that installtest-windows.ps1 and installtest-macos.sh declared
# and never ran, a $Event parameter in sac-verdict.ps1 shadowing the automatic
# variable of that name, a dead DefenderCalled read next to it, and two capture
# variables that existed only to swallow console output.
@{
    ExcludeRules = @(
        # The harness lifts a function out of the SHIPPED script by text or AST
        # and runs that -- Invoke-Expression is how, and it is the whole point:
        # the asserts then drive install.ps1's own ConvertTo-NativeArg,
        # Get-TrayAutostartPlan and friends rather than a copy of them that
        # would go on passing after the real one changed.
        # (installtest-windows.ps1 x10, installtest-serving-asserts.ps1,
        # installtest-model-ready-asserts.ps1.)
        'PSAvoidUsingInvokeExpression'

        # The same method seen from the other side: installtest-serving-asserts.ps1
        # shadows Start-Sleep, Get-Date, Invoke-RestMethod, Get-CimInstance,
        # Get-Content and Test-Path so the lifted body runs against a clock and a
        # daemon it controls. A double that cannot shadow is a copy of the code
        # under test.
        'PSAvoidOverwritingBuiltInCmdlets'

        # Throwaway credentials for throwaway machines: installtest-windows.ps1
        # mints local accounts with a random password and deletes them in the
        # same run (it needs a real logon to exercise the UAC-filtered token),
        # and packaging/windows/hyperv/ drives a disposable local Hyper-V guest
        # whose credential Invoke-Command -VMName wants as a PSCredential.
        # Accepting a SecureString instead would only move the plaintext into
        # the shell history of whoever invokes the script by hand.
        'PSAvoidUsingConvertToSecureStringWithPlainText'
        'PSAvoidUsingPlainTextForPassword'

        # Blind to `param()` + -ArgumentList, which is how every one of these is
        # written -- `Start-Job -ScriptBlock { param($x) ... } -ArgumentList $x`
        # declares $x, and the rule reports it as undeclared anyway. The rest of
        # the hits are $env:* and loop variables that belong to the REMOTE side
        # of an Invoke-Command and must not be captured from here. All checked by
        # hand. No shipped script starts a job or a remote session at all.
        'PSUseUsingScopeModifierInNewRunspaces'

        # Scope-naive in the same way PSReviewUnusedParameter is (see the shipped
        # file): it cannot see a script-level variable that a lifted function
        # body closes over. installtest-swap.ps1 says so in a comment right above
        # the five it reports -- "The names those functions close over". The one
        # other hit is a mirror declaration that PowerShell is not meant to read:
        # installtest-windows.ps1's $StatusFieldsRe exists for
        # harness-failure-strings-guard.sh's sed, and the guard's own header
        # explains why those field names cannot be asserted at runtime.
        #
        # This rule found a real defect on its way in -- the #1051 assert missing
        # from two of the three harnesses -- so the check it was doing there is
        # kept, in the place that can do it for all three: the "declared and
        # used" pass in scripts/ci/harness-failure-strings-guard.sh. Bash has no
        # unused-variable lint at all, so the guard covers what this rule never
        # could.
        'PSUseDeclaredVarsMoreThanAssignments'
    )
}
