---
title: Troubleshooting
description: Find the symptom you are seeing, in plain words, and go to the page that explains it and the command that fixes it.
meta:
  audience: Anyone whose Waired is not behaving
  needs: A terminal on the computer in question
  time: Find your symptom. Each fix takes a minute or two
---

<!-- Symptom-first. The reader knows what they are seeing, not which subsystem
     owns it, so the index below is written in their words. Each entry links to
     one heading on one of the six pages under /troubleshooting/. -->

## Start here

```sh
waired doctor
```

It checks every part of your setup, marks each one ✓, ⚠, or ✗, and repairs
what it can when you press `f`. Run it before anything else on this page. It
resolves most problems on its own. For what each check means, see
[Run a health check](/getting-started/doctor/).

## Find your symptom

### Installing and signing in

[Install and sign-in problems](/troubleshooting/install-and-sign-in/)

- [I typed `waired` and got “command not found”](/troubleshooting/install-and-sign-in/#i-typed-waired-and-got-command-not-found)
- [No browser opened at sign-in, or the wrong one did](/troubleshooting/install-and-sign-in/#no-browser-opened-at-sign-in-or-the-wrong-one-did)
- [The sign-in link expired before I finished](/troubleshooting/install-and-sign-in/#the-sign-in-link-expired-before-i-finished)
- [Sign-in stops because the background service is not responding](/troubleshooting/install-and-sign-in/#sign-in-stops-because-the-background-service-is-not-responding)
- [I signed in, but Waired says I am signed out](/troubleshooting/install-and-sign-in/#i-signed-in-but-waired-says-i-am-signed-out)
- [It says I have reached the device limit](/troubleshooting/install-and-sign-in/#it-says-i-have-reached-the-device-limit)
- [It says the device is “enrolled system-wide”](/troubleshooting/install-and-sign-in/#it-says-the-device-is-enrolled-system-wide)

### Setting up

[Setup problems](/troubleshooting/setup/)

- [Setup stopped partway](/troubleshooting/setup/#setup-stopped-partway)
- [Setup says the inference engine failed to start](/troubleshooting/setup/#setup-says-the-inference-engine-failed-to-start)
- [Setup says it cannot download the model you chose](/troubleshooting/setup/#setup-says-it-cannot-download-the-model-you-chose)
- [Setup said it could not complete a test generation](/troubleshooting/setup/#setup-said-it-could-not-complete-a-test-generation)
- [Waired chose a very small model for my computer](/troubleshooting/setup/#waired-chose-a-very-small-model-for-my-computer)
- [Local inference started off and I did not choose that](/troubleshooting/setup/#local-inference-started-off-and-i-did-not-choose-that)
- [It says local inference is not set up yet](/troubleshooting/setup/#it-says-local-inference-is-not-set-up-yet)
- [This computer has no inference engine](/troubleshooting/setup/#this-computer-has-no-inference-engine)
- [A model says it needs a newer inference engine](/troubleshooting/setup/#a-model-says-it-needs-a-newer-inference-engine)

### Nothing answers

[Nothing answers](/troubleshooting/no-answer/)

- [No answer comes back, or the engine stays “not ready”](/troubleshooting/no-answer/#no-answer-comes-back)
- [The Waired icon says the agent is not running](/troubleshooting/no-answer/#the-waired-icon-says-the-agent-is-not-running)
- [A command says “waired-agent is not running”](/troubleshooting/no-answer/#a-command-says-waired-agent-is-not-running)
- [macOS: the background service never starts](/troubleshooting/no-answer/#macos-the-background-service-never-starts)
- [Windows: I get a 502 error](/troubleshooting/no-answer/#windows-i-get-a-502-error)

### Claude Code

[Claude Code problems](/troubleshooting/claude-code/)

- [Claude Code is still using the cloud](/troubleshooting/claude-code/#claude-code-is-still-using-the-cloud)
- [Waired says Claude Code is managed by your organization](/troubleshooting/claude-code/#waired-says-claude-code-is-managed-by-your-organization)
- [Claude Code says Waired cannot answer](/troubleshooting/claude-code/#claude-code-says-waired-cannot-answer)
- [The Waired rows are missing from /model](/troubleshooting/claude-code/#the-waired-rows-are-missing-from-model)
- [Long Claude Code sessions get summarized](/troubleshooting/claude-code/#long-claude-code-sessions-get-summarized)
- [The status line does not show up in Claude Code](/troubleshooting/claude-code/#the-status-line-does-not-show-up-in-claude-code)

### Answers are slow, or the hardware is not used

[Slow answers and hardware](/troubleshooting/slow-or-wrong/)

- [Answers are very slow](/troubleshooting/slow-or-wrong/#answers-are-very-slow)
- [My GPU is not being used](/troubleshooting/slow-or-wrong/#my-gpu-is-not-being-used)
- [I chose a model bigger than my hardware](/troubleshooting/slow-or-wrong/#i-chose-a-model-bigger-than-my-hardware)
- [It says the GPU ran out of memory on a long prompt](/troubleshooting/slow-or-wrong/#it-says-the-gpu-ran-out-of-memory-on-a-long-prompt)
- [Windows: giving the graphics chip more memory made things worse](/troubleshooting/slow-or-wrong/#windows-giving-the-graphics-chip-more-memory-made-things-worse)

### Other computers, and the app itself

[Other computers and the app](/troubleshooting/other-computers/)

- [My other computer cannot reach the model](/troubleshooting/other-computers/#my-other-computer-cannot-reach-the-model)
- [Requests stopped working after I pinned a computer](/troubleshooting/other-computers/#requests-stopped-working-after-i-pinned-a-computer)
- [The Waired icon is missing on Linux](/troubleshooting/other-computers/#the-waired-icon-is-missing-on-linux)
- [Reading the logs](/troubleshooting/other-computers/#reading-the-logs)

## Still stuck

Follow [Report a problem](/getting-started/report-a-problem/). Turn on
detailed logs before reproducing the problem, collect them into one file, and
attach that. `waired logs --mask-pii` masks your home directory, username,
hostname, and account email, so the file is safe to attach to an
[issue](https://github.com/waired-ai/waired-agent/issues).
