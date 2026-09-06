---
title: Other computers and the app
description: Another computer cannot reach the model, requests fail after pinning a computer, the Waired icon is missing on Linux, and where the logs are.
meta:
  audience: Anyone with more than one computer, or a missing icon
  needs: A terminal on the computer in question
  time: Each fix takes a minute or two
---

Run `waired doctor` first. It names the part that is not ready. Then find your
symptom below.

## My other computer cannot reach the model

```sh
waired status --observability
```

The **Mesh** line reads `enrolled / reachable / ready`. If `reachable` is 0:

1. **Are both computers signed in with the same Google account?** This is by
   far the most common cause. Compare the account line from `waired status`
   on each.
2. **Is the other computer awake, with Waired running?** Run `waired doctor`
   there.
3. **Is it sharing?** A computer answers other computers only when its own
   sharing switch is on, which `waired share status` shows and
   `waired share on` turns on, and when the web console's **Sharing** card
   offers it to **Your other computers**. See
   [Share a computer with your other devices](/guides/sharing/).

If it is reachable but never `ready`, it has no model loaded. Work through
[No answer comes back](/troubleshooting/no-answer/#no-answer-comes-back) on
that computer.

If everything looks reachable and requests still do not arrive, run
`waired doctor`. Its **mesh peers** line does not take the network's word for
it. It sends a real request to each computer and reports what came back:

```
⚠ mesh peers — 2/3 reported reachable, but only 0 answered a ping.
  No reply from mac-mini, work-laptop. Inference cannot route to a peer that
  does not answer; check NAT traversal and relay connectivity
```

That line means the two computers are listed as connected but nothing gets
through to them. Work through the three checks above on the named computers.

You should not need to open ports or configure a VPN. Your computers connect
directly when the network allows it, and fall back to an encrypted relay when
a firewall gets in the way.

## Requests stopped working after I pinned a computer

```sh
waired worker get
```

Pinning is a firm instruction: use that computer, and no other. So when the
pinned computer is asleep, offline, or not sharing, Waired does not run the
work somewhere else. You get an error instead. That is deliberate. Silently
answering from a different computer would mean a request you sent to your
big GPU box was really handled by the laptop in front of you, with no sign of
it.

The Waired app says the same thing: **Worker: `<name>` (pinned) —
unavailable, requests aren't served here**. Claude Code gets the same
answer. The turn fails at once and names the computer:

```
API Error: 400 The computer this turn is pinned to, sv-mag, is not answering. Pick an Anthropic model in /model to send this turn to the cloud, or run `waired doctor` to see what is missing.
```

To fix it, either wake the pinned computer, checking it with
`waired peers list` and `waired doctor` on that computer, or stop pinning:

```sh
waired worker set --mode=auto
```

If the pinned computer is back and turns still fail with that message, give
it about a minute. When the Waired background service on that computer
restarts, it has to announce itself to your account again before your other
computers send it work. Nothing on that computer needs fixing.

## The Waired icon is missing on Linux

GNOME does not show icons next to the clock on its own. The Waired icon
needs the AppIndicator extension. Setup installs one when it finds GNOME on
the computer, and Waired checks again each time you log in. An extension
that is present but switched off is switched back on for you.

If the icon is still missing, this fixes it:

```sh
waired doctor --fix
```

It reports what is wrong, asks before changing anything, and installs or
switches on the extension as needed. To do the same by hand:

```sh
sudo apt install gnome-shell-extension-appindicator
gnome-extensions enable appindicatorsupport@rgcjonas.gmail.com
```

Then log out and back in, which Wayland requires. KDE Plasma needs nothing.
MATE cannot show the icon at all.

## Reading the logs

Only after `waired doctor`. `waired logs` collects all of the following into
one file. See [Report a problem](/getting-started/report-a-problem/).

| | |
|---|---|
| Linux | `journalctl -u waired-agent -e` |
| macOS | `/Library/Logs/waired-agent.err.log`, or `sudo log show --predicate 'process == "waired-agent"' --last 10m`. Waired caps that file at 32 MB and keeps ten previous ones beside it as `waired-agent.err.log.0.gz`, `.1.gz`, and so on. At `debug` the cap rises to 128 MB. |
| Windows | `logs\waired-agent.log` under Waired's state folder, `C:\ProgramData\waired\logs\…` for the usual service install, which takes an elevated PowerShell to read. Same caps as macOS. `Get-WinEvent -ProviderName waired-agent -LogName Application -MaxEvents 50` is the short version, with warnings and errors only. |
| The inference engine | `…/runtimes/ollama/logs/engine.log` under Waired's state folder: `/var/lib/waired/…` on Linux, `/Library/Application Support/waired/…` on macOS, `C:\ProgramData\waired\…` on Windows. |
