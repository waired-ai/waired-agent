# PSScriptAnalyzer settings for the shipped PowerShell surface.
# Used by `make ps-script-lint` and CI's installer-pwsh job.
#
# This file is the single record of which rules do not apply to a Windows
# installer and WHY. A rule whose exclusion cannot be justified in a sentence
# does not belong here -- fix the code instead. Two findings were fixed rather
# than excluded when this gate was introduced (uninstall.ps1's Remove-Service
# shadowed the pwsh 6.1+ cmdlet of that name; installtest-pwsh.ps1 had an
# -Args parameter shadowing the automatic $args), which is the standard.
@{
    # Warning and up. Error alone would catch essentially nothing: neither #177
    # (an unquoted argv token) nor #192 (state lost across UAC) is an Error, and
    # a gate that cannot see the two bugs that motivated it is not a gate.
    Severity = @('Error', 'Warning')

    ExcludeRules = @(
        # The installer's output IS the user interface -- an operator watching a
        # console, not a caller consuming objects. Write-Output would put every
        # log line on the success stream, so `iwr ... | iex` and any function
        # whose result is assigned would fold the log into their value.
        # install.ps1's Invoke-WairedInit already carries that scar in a comment.
        'PSAvoidUsingWriteHost'

        # Every instance is a deliberate best-effort in a teardown or probe path:
        # Stop-Transcript during the failure trap, the status-marker write the
        # elevated child leaves for its parent, the HKLM install-dir lookup on a
        # host that has no such key. Failing any of them louder than the thing
        # already going wrong is a regression, not a fix.
        'PSAvoidUsingEmptyCatchBlock'

        # -WhatIf/-Confirm on internal helpers of a standalone script. The script
        # already has the equivalent, one level up and applied uniformly:
        # -DryRun, routed through Common-Run. Adding ShouldProcess to each helper
        # would give an end user two different ways to ask for a preview.
        'PSUseApprovedVerbs'          # see below -- kept adjacent for context
        'PSUseShouldProcessForStateChangingFunctions'

        # Naming mirrors install.sh one-for-one on purpose (Common-Log/common_log,
        # Extract-Zip, Emo, Glyph). CLAUDE.md's cross-OS parity rule is that the
        # two installers stay readable side by side; approved-verb names would
        # break every pairing to satisfy a convention no end user ever sees.
        # (PSUseApprovedVerbs is listed above so the two naming rules sit
        # together.)
        'PSUseSingularNouns'

        # Scope-naive: it does not see a script-level parameter consumed inside a
        # function, which is how every one of these is used. Verified by hand --
        # e.g. install.ps1's -OllamaGpuMode (Export-InstallState:336,
        # Import-InstallState:400) and uninstall.ps1's -Yes (Confirm-Clean:380)
        # are all live.
        'PSReviewUnusedParameter'

        # The opposite of what this repo requires. `iwr -useb ... | iex` coerces
        # the downloaded bytes through the client's ANSI code page, so a BOM
        # arrives as a stray "?" -- the Japanese-Windows banner mojibake.
        # scripts/install/encoding_test.go enforces BOM-less pure ASCII on the
        # scripts that ship that way, and it is the authority here.
        'PSUseBOMForUnicodeEncodedFile'
    )
}
