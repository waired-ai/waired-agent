---
title: Troubleshooting
description: Find the symptom you are actually seeing, in plain words, and get the one command that fixes it.
meta:
  audience: Anyone whose Waired is not behaving
  needs: A terminal on the computer in question
  time: Find your symptom; each fix is 1–2 minutes
---

<!-- Symptom-first. The reader knows what they are seeing, not which subsystem
     owns it, so the index below is written in their words and each entry links
     to one fix. The previous version opened with a table of three diagnostic
     commands, which only helps someone who already knows what to look for. -->

## Start here

```sh
waired doctor
```

It checks every part of your setup, marks each one ✓ / ⚠ / ✗, and — press **f**
— repairs what it can. Run it before anything else on this page; it resolves
most problems on its own.

## Find your symptom

**Setting up**

- [I typed `waired` and got “command not found”](#i-typed-waired-and-got-command-not-found)
- [Setup stopped partway](#setup-stopped-partway)
- [It says I have reached the device limit](#it-says-i-have-reached-the-device-limit)
- [It says the device is “enrolled system-wide”](#it-says-the-device-is-enrolled-system-wide)
- [It said my machine can only run a very small model](#it-said-my-machine-can-only-run-a-very-small-model)

**Nothing answers**

- [No answer comes back / the engine stays “not ready”](#no-answer-comes-back)
- [Claude Code is still using the cloud](#claude-code-is-still-using-the-cloud)
- [A command says “waired-agent is not running”](#a-command-says-waired-agent-is-not-running)
- [Windows: I get a 502 error](#windows-i-get-a-502-error)

**Answers are wrong or slow**

- [Answers are very slow](#answers-are-very-slow)
- [My graphics card is not being used](#my-graphics-card-is-not-being-used)
- [I chose a model bigger than my hardware](#i-chose-a-model-bigger-than-my-hardware)
- [Long Claude Code sessions get summarized](#long-claude-code-sessions-get-summarized)

**Other computers**

- [My other computer cannot reach the AI](#my-other-computer-cannot-reach-the-ai)

**The app itself**

- [The Waired icon is missing (Linux)](#the-waired-icon-is-missing-linux)
- [The status line does not show up in Claude Code](#the-status-line-does-not-show-up-in-claude-code)

---

## I typed `waired` and got “command not found”

Either the install did not finish, or your terminal was already open when it did
and has not picked up the new command yet.

1. **Close the terminal and open a new one.** This alone fixes it most of the
   time — a running shell caches where commands live.
2. Still missing? Run the install command again; see
   [Install](/getting-started/install/). It is safe to run twice.

On Windows the command lives at `C:\Program Files\Waired\waired.exe`. If
`waired` alone does not work, that full path always does.

## Setup stopped partway

The setup page names what happened. Each message means something specific:

| What you see | What it means | What to do |
|---|---|---|
| “The setup command on … was closed before this finished. Your progress was saved.” | The terminal window running setup was closed. Some steps need administrator rights and only that window has them. | Run `sudo waired init` again (Windows: `waired init` from an administrator prompt). It resumes; nothing is lost. |
| “Setup on … needs administrator access to continue.” | Setup was started without administrator rights. | Start it again from an administrator terminal — see [Sign in and set up](/getting-started/first-run/). |
| “… has run out of disk space.” | The model did not fit. | Free some space, or pick a smaller model from the [catalog](/reference/model-catalog/). |
| “… could not finish downloading. Check its internet connection.” | The download was interrupted. | Retry. Downloads resume rather than start over. |
| “The AI software on … has not started yet.” | The engine is still coming up. | Wait a minute and retry. If it persists, see [No answer comes back](#no-answer-comes-back). |
| “This took too long on … and was stopped.” | A step exceeded its time limit. | Retry. Twice on the same step usually means this machine is too slow for that model. |

The model download is the exception to all of the above: it keeps running even
if you close the browser tab. Reopen the device at
[app.waired.ai](https://app.waired.ai) to see where it got to.

## It says I have reached the device limit

Each account can enroll a generous number of devices, and the usual cause is old
machines you no longer use still being counted.

Open [app.waired.ai](https://app.waired.ai), remove a device you no longer need,
then set up again.

Re-running setup on a machine that is **already** signed in never counts against
the limit.

## It says the device is “enrolled system-wide”

That is not an error. The device's identity is stored in a system folder only
administrators can read, so `waired status` run as a regular user cannot see it
— rather than guess, it tells you the device is enrolled and exits successfully.

To see the full status, run it with administrator rights:

```sh
sudo waired status          # Windows: from an administrator prompt
```

If instead you see `Not enrolled. Run 'waired init' to connect this device.`,
this machine really has not been set up yet — see
[Sign in and set up](/getting-started/first-run/).

## It said my machine can only run a very small model

Believe it. At that size a coding model produces broken output more often than
useful output, which is why the default answer is **No**.

The machine is still worth having in your network — it can use the AI running on
your other computers. To install a model anyway, set up again with
`--inference-enabled=true`.

## No answer comes back

Check what the engine is doing:

```sh
waired status --observability
```

The **Engine** line is the one that matters.

- **`ready`** — the model is loaded. If requests still fail, the problem is
  routing: see [Claude Code is still using the cloud](#claude-code-is-still-using-the-cloud).
- **`not ready`** — usually the model is still downloading. `waired models ls`
  shows progress; a first model is several gigabytes.
- **`not ready` after the download finished** — the model probably does not fit
  this computer's memory. Switch to a smaller one:
  [Choose which AI model runs](/guides/choose-a-model/).

Two more causes worth knowing:

- The **first** load of a model is slow — around a minute on a GPU — and looks
  like a hang. It recovers on its own.
- A **503** means routing is paused (`waired resume`) or sharing is off
  (`waired inference share on`).

Still stuck? `waired runtimes status` reports on the engine itself, and
[Going deeper](#going-deeper-logs) has the logs.

## Claude Code is still using the cloud

```sh
waired doctor          # press f to repair what it finds
waired claude status
```

`waired doctor` rebuilds the connection between Claude Code and Waired when it
is broken. If `waired claude status` says the integration is not enabled, enable
it and restart your Claude Code session:

```sh
sudo waired claude enable     # Windows: from an administrator prompt
```

`waired claude status` also names the **last fallback** and why it happened:

- `local_no_model` — no model is active on this device yet. See
  [No answer comes back](#no-answer-comes-back).
- `local_status_<code>` — your local serving returned that error just before
  falling back. `waired status --observability` has the detail.

Falling back to the cloud is deliberate: Waired would rather keep you working
than fail — and it always tells you it happened.

## A command says “waired-agent is not running”

The background service has stopped.

```sh
sudo systemctl restart waired-agent    # Linux
Restart-Service waired-agent           # Windows (administrator)
```

On macOS the system restarts it for you; if it does not come back, run
`waired doctor` or restart the computer.

A restart also clears most temporary inconsistencies, so it is worth trying
before anything more involved.

## Windows: I get a 502 error

The AI software is not installed on this computer — usually because it was
installed with `-SkipOllama` or `WAIRED_NO_OLLAMA=1`.

From an administrator prompt:

```powershell
waired runtimes install ollama
```

## Answers are very slow

```sh
waired runtimes benchmark
```

This measures what this computer actually does. If it comes out below what a
coding assistant needs, Waired offers a lighter model — accepting is usually
right.

Other things worth checking:

- **Is your graphics card being used?** See
  [My graphics card is not being used](#my-graphics-card-is-not-being-used).
- **Is the model too big for your memory?** An over-sized model runs partly on
  the processor, which is dramatically slower. `waired models ls --detail` shows
  the fit.
- **Is the answer coming from another computer?** `waired infer --explain "hi"`
  names the machine that served it, and the estimated latency.

## My graphics card is not being used

`waired doctor` reports which backend the engine chose.

Waired handles the common cases automatically: integrated AMD and Intel
graphics are enabled through Vulkan (recent Ollama versions disable them by
default and fall back to the processor silently), and discrete AMD cards use
ROCm where it is supported, falling back to Vulkan when it does not engage.

If you run your own Ollama outside Waired, set `OLLAMA_IGPU_ENABLE=1` yourself
and restart it.

Also confirm the model actually fits — memory requirements are in the
[model catalog](/reference/model-catalog/).

## I chose a model bigger than my hardware

Waired warns but does not block you. When you pick an over-sized model it shows
the shortfall (`needs 32 GB RAM (have 31 GB)`) and asks you to confirm.

- **Slightly over** — it usually runs, just slower.
- **Genuinely too big** — the engine fails to load it and reports a clear error.
  Switch back down: [Choose which AI model runs](/guides/choose-a-model/).

The recommended figures carry a safety margin, and on Apple Silicon and AMD
Strix Halo the fit is judged against the memory the graphics side can actually
address. `waired models ls --detail` shows the verdict for every model on this
machine.

## Long Claude Code sessions get summarized

This is expected and healthy. Local models hold less of a conversation at once
than cloud models do, so Waired tells Claude Code the real limit and Claude Code
summarizes older turns to fit — the session keeps working instead of silently
losing its beginning.

If you briefly see “prompt is too long”, Claude Code retries on its own.

Want the larger window for a while? `/waired-route anthropic` sends the session
to the real Anthropic API, and the full window applies from your next message.

## My other computer cannot reach the AI

```sh
waired status --observability
```

The **Mesh** line reads `enrolled / reachable / ready`. If `reachable` is 0:

1. **Are both computers signed in with the same Google account?** By far the
   most common cause. Compare the account line from `waired status` on each.
2. **Is the other computer awake, with Waired running?** Run `waired doctor`
   there.
3. **Is it sharing?** A computer only answers other devices when sharing is on:
   `waired inference share on`.

If it is reachable but never `ready`, it has no model loaded — work through
[No answer comes back](#no-answer-comes-back) on that machine.

You should not need to open ports or configure a VPN. Your computers connect
directly when the network allows it, and fall back to an encrypted [relay](/reference/glossary/#relay) when a
firewall gets in the way, automatically.

## The Waired icon is missing (Linux)

GNOME does not show icons next to the clock on its own — the Waired icon needs
the AppIndicator extension.
Setup installs one automatically when it detects GNOME, and `waired doctor`
warns when it is missing.

To install it by hand:

```sh
sudo apt install gnome-shell-extension-appindicator
gnome-extensions enable appindicatorsupport@rgcjonas.gmail.com
```

Then **log out and back in** — required on Wayland.

KDE Plasma needs nothing. MATE cannot show the icon at all.

## The status line does not show up in Claude Code

Run `waired claude status` **inside the project directory**.

Claude Code allows one status line, and a project-level setting
(`.claude/settings.json` or `.claude/settings.local.json`) overrides the one
Waired installs for your user. When that happens the command names the file that
is winning, and prints a line you can add to your own status-line script.

Also make sure you restarted the Claude Code session after enabling the
integration.

---

## Going deeper (logs)

Only after `waired doctor`:

| | |
|---|---|
| Linux | `journalctl -u waired-agent -e` |
| Windows | `Get-WinEvent -ProviderName waired-agent -LogName Application -MaxEvents 50` |
| The AI engine | `…/runtimes/ollama/logs/engine.log` under Waired's state folder — `/var/lib/waired/…` on Linux, `/Library/Application Support/waired/…` on macOS. If you brought your own Ollama: `~/.ollama/logs/server.log`. |

## Reporting a problem

`waired init --mask-pii` (or `WAIRED_PII_MASK=1` on other commands) masks your
home directory, username, hostname and account email in the output, so a
transcript or screenshot is safe to attach to an
[issue](https://github.com/waired-ai/waired-agent/issues).
