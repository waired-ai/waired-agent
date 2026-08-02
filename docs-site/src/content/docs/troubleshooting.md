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
- [No browser opened at sign-in, or the wrong one did](#no-browser-opened-at-sign-in)
- [Sign-in stops because the background service is not responding](#sign-in-stops-because-the-background-service-is-not-responding)
- [Setup stopped partway](#setup-stopped-partway)
- [It says I have reached the device limit](#it-says-i-have-reached-the-device-limit)
- [It says the device is “enrolled system-wide”](#it-says-the-device-is-enrolled-system-wide)
- [It said my machine can only run a very small model](#it-said-my-machine-can-only-run-a-very-small-model)
- [Setup said it could not complete a test generation](#setup-said-it-could-not-complete-a-test-generation)

**Nothing answers**

- [I signed in, but Waired says I am signed out](#i-signed-in-but-waired-says-i-am-signed-out)
- [No answer comes back / the engine stays “not ready”](#no-answer-comes-back)
- [Claude Code is still using the cloud](#claude-code-is-still-using-the-cloud)
- [The Waired icon says the agent is not running](#the-waired-icon-says-the-agent-is-not-running)
- [A command says “waired-agent is not running”](#a-command-says-waired-agent-is-not-running)
- [macOS: the background service never starts](#macos-the-background-service-never-starts)
- [macOS: it says the AI software is damaged](#macos-it-says-the-ai-software-is-damaged)
- [Windows: I get a 502 error](#windows-i-get-a-502-error)

**Answers are wrong or slow**

- [Answers are very slow](#answers-are-very-slow)
- [My graphics card is not being used](#my-graphics-card-is-not-being-used)
- [I chose a model bigger than my hardware](#i-chose-a-model-bigger-than-my-hardware)
- [The Waired entries are missing from /model](#the-waired-entries-are-missing-from-model)
- [Long Claude Code sessions get summarized](#long-claude-code-sessions-get-summarized)

**Other computers**

- [My other computer cannot reach the AI](#my-other-computer-cannot-reach-the-ai)
- [Requests stopped working after I pinned a computer](#requests-stopped-working-after-i-pinned-a-computer)

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

<a id="no-browser-opened-at-sign-in"></a>

## No browser opened at sign-in, or the wrong one did

The sign-in link is always printed in the terminal before anything opens, so you
can finish by hand at any time: copy that link and paste it into the browser you
normally use. Sign in there, and stay in that same browser for the rest of
setup — the setup page only works in the browser you signed in with.

If a browser you never use opened instead, close it without signing in and open
the printed link in the one you want.

Both cases were caused by setup running with administrator rights, and are fixed
in current versions:

```sh
waired update
```

## Sign-in stops because the background service is not responding

Signing in happens *through* the background service: it is what talks to Waired,
and what keeps this computer connected afterwards. If it is not answering,
sign-in stops rather than continuing without it:

```
Waired's background service is installed but isn't responding, so sign-in can't continue.
  Check what's wrong:  waired doctor
  Start it:            sudo systemctl start waired-agent
  Then run again:      sudo waired init
```

Work through those three lines in order. `waired doctor` names the actual fault;
on macOS the usual one is [the service never starting at
all](#macos-the-background-service-never-starts).

Earlier versions signed in anyway when this happened. It looked like it worked —
but the computer ended up signed in and unable to finish setup in the browser,
with nothing on the machine itself explaining why. Stopping with a message you
can act on replaced that.

If the message says **“Waired isn't running in the background”** instead, no
background service is registered on this computer at all — normally because the
programs are being run directly rather than installed. Start `waired-agent`
first, then run `waired init` again.

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

## Setup said it could not complete a test generation

At the end of setup Waired asks the AI a short question to check the speed of
this machine. This message means the question was asked and no answer came
back — so setup could not measure anything, and it will not pretend the AI is
working.

Almost always the AI engine itself stopped. Check it:

```sh
waired status
waired doctor
```

`waired status` shows the engine's own reason on the line for the engine. If it
crashed, the detail is in its log — see [Going deeper (logs)](#going-deeper-logs).

The rest of Waired is unaffected: your device stays signed in, and it can still
use the AI running on your other computers. Once the engine is healthy, measure
again with:

```sh
waired runtimes benchmark
```

## I signed in, but Waired says I am signed out

The Waired icon shows “Not signed in”, or the computer is missing from your
account — most often right after a restart, even though you never signed out.

Two different things look the same from the outside. `waired doctor` tells them
apart:

```sh
waired doctor
```

**`network connection` is a ⚠** — you *are* signed in and this computer simply
has not connected yet. Waired keeps retrying by itself, including after a
restart where the network port it normally uses was taken by something else, so
give it a minute and check again. If it never clears, restart the background
service:

```sh
sudo systemctl restart waired-agent    # Linux
Restart-Service waired-agent           # Windows (administrator)
```

**`device sign-in` is a ✗** — this computer's sign-in really has stopped
working and only signing in again restores it:

```sh
sudo waired init      # Linux / macOS
waired init           # Windows (administrator)
```

Your models, settings and coding-tool setup all survive this — it re-establishes
this computer's place in your account and nothing else. Local AI keeps answering
throughout; what stops is everything that needs your account, so the computer
disappears from the web console and your other devices cannot reach it until
you sign in again.

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
- **`engine failed`** — the AI engine stopped on its own. Waired restarts it for
  you (up to three times), so this usually clears within a minute; the reason it
  stopped is shown on the same line. If it keeps happening, Waired stops
  restarting and says so — fix what the reason points at, then start it again:

  ```sh
  waired inference engine start
  ```

  A model that is too large for this computer is the usual cause; the engine's
  own log has the detail (see [Going deeper](#going-deeper-logs)). While the
  engine is down this computer stops offering the AI to your other machines, so
  they fail over instead of waiting on it.

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

## The Waired icon says the agent is not running

Open the Waired menu and choose **Start the Waired agent…**. Your computer asks
for administrator access — that is the operating system's own prompt, and it is
required because the background service belongs to the whole computer, not to
one account. If you would rather run it yourself, **Copy start command** puts
the right command for this computer on your clipboard.

Two things this menu tells you apart:

- **“Waired agent is starting…”** — normal. On Windows the service is set to
  start a couple of minutes *after* you sign in, so the Waired icon is up before
  it is. Nothing is wrong; you can wait, or start it now.
- **“Waired agent is not running”** — it should be up and is not. Start it from
  the menu, and if it does not come back, run `waired doctor`.

Stopping the service by hand does not stick: it starts again with the computer.

## A command says “waired-agent is not running”

The background service has stopped.

```sh
sudo systemctl restart waired-agent    # Linux
Restart-Service waired-agent           # Windows (administrator)
```

On macOS the system restarts it for you; if it does not come back, run
`waired doctor` or restart the computer — and if it has *never* started, see
[the next section](#macos-the-background-service-never-starts).

A restart also clears most temporary inconsistencies, so it is worth trying
before anything more involved.

### Windows: it stopped starting at boot on its own

Windows can block Waired's background service at startup, because Waired's
programs are not yet signed with a certificate Windows recognises. When Smart
App Control is switched on it decides at boot, without a network check to fall
back on, and the service does not start. It is not consistent: the same
computer may start normally on the next boot.

Until Waired ships signed programs, start the service from the Waired menu when
this happens — nothing is damaged, and manually starting the same service
works.

## macOS: the background service never starts

The installer finished, but the service never comes up — and reinstalling,
even with `--clean`, changes nothing. That combination usually means macOS
has the service marked as **disabled**. A Waired version that was installed
and then removed between 15 July 2026 and this release left that mark behind,
and it survives uninstalling, reinstalling and restarting the computer.

Check for it:

```sh
sudo launchctl print-disabled system | grep waired
```

`"com.waired.agent" => true` means it is disabled. Clear the mark and start
the service:

```sh
sudo launchctl enable system/com.waired.agent
sudo launchctl bootstrap system /Library/LaunchDaemons/com.waired.agent.plist
```

Installing or updating Waired now clears this for you, so you should only need
these commands on a machine where the installer itself cannot be run.

## macOS: it says the AI software is damaged

You see a macOS dialog saying **“Ollama” is damaged and can’t be opened. You
should move it to the Trash** — and it comes back every time you dismiss it.
Setup never gets past “Preparing to download the model…”.

Nothing is actually corrupted. A Waired version installed before this release
wrote a small bookkeeping file inside the Ollama app, and macOS treats *any*
addition to a signed app as tampering. It then refuses to launch it, so
Waired’s attempts to start the AI engine are killed one after another.

Removing that one file fixes it — nothing is re-downloaded:

```sh
sudo waired doctor --fix
```

`waired doctor` reports the problem as **AI engine app signature** and names the
file it will remove. Signing in again (`sudo waired init`) repairs it too.

To confirm afterwards:

```sh
codesign --verify --deep --strict /Applications/Ollama.app
```

Silence means the app is intact again. Installing or updating Waired now keeps
its bookkeeping outside the app, so this cannot happen again.

If `waired doctor` reports the same check but points at
`waired runtimes install ollama` instead, the app is unusable for some other
reason and there is no file of ours to remove — reinstall it:

```sh
sudo waired runtimes install ollama
```

Setup checks this now: an AI engine macOS refuses to run is reported as a
failed step with the reason, instead of a green “OK” over software that never
starts.

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

First, see what Waired found:

```sh
waired models ls --detail
```

The first line names your card and its memory. If it says `no GPU` on a
computer that has one, the card was never detected — and everything after
that, including which model you were given, was sized for the processor.

Waired handles the common cases automatically: integrated AMD and Intel
graphics are enabled through Vulkan (recent Ollama versions disable them by
default and fall back to the processor silently), and discrete AMD cards use
ROCm where it is supported, falling back to Vulkan when it does not engage.

NVIDIA cards are found through the driver itself, not by looking for
`nvidia-smi` on your `PATH` — the background service does not inherit the
`PATH` from your terminal, so a card is still found when the tool is not on
it. If your card is genuinely not showing up, point Waired straight at the
tool and restart the service:

On Linux, `sudo systemctl edit waired-agent` and add:

```ini
[Service]
Environment=WAIRED_NVIDIA_SMI=/usr/bin/nvidia-smi
```

On Windows, in an administrator PowerShell:

```powershell
[Environment]::SetEnvironmentVariable(
  'WAIRED_NVIDIA_SMI', 'C:\Windows\System32\nvidia-smi.exe', 'Machine')
```

Then restart the service (see
[“waired-agent is not running”](#a-command-says-waired-agent-is-not-running))
and run `waired models ls --detail` again. On Windows a full reboot is the
surest way to make the service pick up a new machine-wide variable.

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

The recommended figures carry a safety margin. On Apple Silicon and AMD Strix
Halo the fit is judged against the memory the graphics side can actually
address; on a computer with a separate graphics card, what Waired picks *for*
you is judged against the card's own memory — so a model that only fits by
spilling into system RAM is one you have to choose deliberately.
`waired models ls --detail` shows the verdict for every model on this machine.

## The Waired entries are missing from /model

`/model` should offer **Waired auto**, **Waired local** and **Waired cloud**
below the Anthropic names. Three things hide them, in the order worth checking:

1. **Claude Code has not been restarted.** The list is read once at startup. Quit
   Claude Code and start it again.
2. **Routing is not on for this computer.** Check with `waired claude status`; the
   entries are only offered once Claude Code is pointed at Waired.

   ```sh
   sudo waired claude enable    # Windows: from an administrator prompt
   ```

3. **`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` is set.** Any value hides the
   entries, even when everything else is correct. Unset it and restart Claude
   Code, or use `/waired-route` instead — it works regardless and does the same
   three things.

Running Claude Code inside WSL2 while Waired is installed on Windows is a
separate case: they are two different systems, so use the Windows-side Claude
Code.

Routing itself is unaffected by any of this — the status line still shows which
AI answered, and `/waired-route` still switches where requests go.

## Long Claude Code sessions get summarized

This is expected and healthy. Local models hold less of a conversation at once
than cloud models do, so Waired tells Claude Code the real limit and Claude Code
summarizes older turns to fit — the session keeps working instead of silently
losing its beginning.

If you briefly see “prompt is too long”, Claude Code retries on its own.

**Summarizing much earlier or later than you expect?** The limit is passed to
Claude Code when you connect it, so it can fall behind after you switch models:

```sh
waired claude status
```

The **local window** line shows the limit your model handles now next to the one
Claude Code was started with. If it says they disagree, re-run
`sudo waired claude enable` (Windows: from an administrator prompt), then restart
Claude Code.

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

## Requests stopped working after I pinned a computer

```sh
waired worker get
```

Pinning is a firm instruction: **use that computer, and no other**. So when the
pinned computer is asleep, offline or not sharing, Waired does not quietly run
the work somewhere else — you get an error instead. That is deliberate. Silently
answering from a different machine would mean a request you sent to your big GPU
box was really handled by the laptop in front of you, with no sign of it.

The Waired icon says the same thing: **Worker: `<name>` (pinned) — unavailable,
requests are not served here**.

Claude Code is the one exception, and only on the `auto` route: rather than
failing the turn, it finishes it with the real Anthropic API and adds a note to
the conversation saying so. Switch that class to the `waired` route if you would
rather see the error — see [Claude Code](/guides/claude-code/).

To fix it, either wake the pinned computer (check it with `waired peers list`
and `waired doctor` on that machine — see
[My other computer cannot reach the AI](#my-other-computer-cannot-reach-the-ai)),
or stop pinning:

```sh
waired worker set --mode=auto
```

## The Waired icon is missing (Linux)

GNOME does not show icons next to the clock on its own — the Waired icon needs
the AppIndicator extension.
Setup installs one when it finds GNOME on the computer, and Waired checks again
each time you sign in — an extension that is present but switched off is
switched back on for you.

If the icon is still missing, this fixes it:

```sh
waired doctor --fix
```

It reports what is wrong, asks before changing anything, and installs or
switches on the extension as needed.

To do the same by hand:

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
| macOS | `/Library/Logs/waired-agent.err.log`, or `sudo log show --predicate 'process == "waired-agent"' --last 10m`. Waired caps that file at 1 MB and keeps the five previous ones beside it as `waired-agent.err.log.0.gz`, `.1.gz` and so on — look there for anything older (`gzcat`). |
| Windows | `Get-WinEvent -ProviderName waired-agent -LogName Application -MaxEvents 50` |
| The AI engine | `…/runtimes/ollama/logs/engine.log` under Waired's state folder — `/var/lib/waired/…` on Linux, `/Library/Application Support/waired/…` on macOS. If you brought your own Ollama: `~/.ollama/logs/server.log`. |

## Reporting a problem

Follow [Report a problem](/getting-started/report-a-problem/): turn on detailed
logs **before** reproducing it, collect them into one file, and attach that.
Doing it in that order matters — the detail that explains a bug is not written
down unless you ask for it first.

`waired init --mask-pii` (or `WAIRED_PII_MASK=1` on other commands) masks your
home directory, username, hostname and account email in the output, so a
transcript or screenshot is safe to attach to an
[issue](https://github.com/waired-ai/waired-agent/issues).
