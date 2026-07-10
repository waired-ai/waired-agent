---
name: waired-doctor
description: Diagnose Waired setup problems and suggest the next command to run.
allowed-tools: Bash(waired doctor:*), Bash(waired doctor)
---

Run `waired doctor`. Read the diagnostic output and explain to the user,
in plain language, what (if anything) is wrong and which single command
they should run next to fix it.

If `waired doctor` reports `Press f to fix`, do NOT press it — surface
the suggestion and let the user decide.

Stay terse: the user already saw the icons.
