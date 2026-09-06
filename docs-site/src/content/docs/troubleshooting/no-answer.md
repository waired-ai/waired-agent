---
title: Nothing answers
description: No answer comes back, the engine stays not ready, the background service is not running, or a request gets a 502.
meta:
  audience: Anyone whose model has stopped answering
  needs: A terminal on the computer in question
  time: Each fix takes a minute or two
---

Run `waired doctor` first. It names the part that is not ready. Then find your
symptom below.

## No answer comes back

Check what the engine is doing:

```sh
waired status --observability
```

The **Engine** line is the one that matters.

- **`ready`**: the model is loaded. If requests still fail, the problem is
  routing. See [Claude Code is still using the cloud](/troubleshooting/claude-code/#claude-code-is-still-using-the-cloud).
- **`not ready`**: usually the model is still downloading. `waired models ls`
  shows the progress. A first model is several gigabytes.
- **`not ready` after the download finished**: the model probably does not
  fit this computer's memory. Switch to a smaller one. See
  [Change the model](/guides/choose-a-model/).
- **`engine failed`**: the inference engine stopped on its own. Waired
  restarts it for you, up to three times, so this usually clears within a
  minute. The reason it stopped is shown on the same line. If it keeps
  happening, Waired stops restarting it and says so. Fix what the reason
  points at, then run `waired inference engine start`. A model too large for
  this computer is the usual cause.

Two more causes worth knowing:

- The model has to be loaded into memory before it can answer, and the first
  request after the engine starts is the one that waits for it.
- A **503** means routing is paused (`waired resume`) or sharing is off
  (`waired share on`).

### Is it working, or is it stuck?

`waired status` answers both halves:

```
  model loaded:   ollama: no (the next request reloads it)
  serving now:    0 requests
```

- **`model loaded:`** says whether the model is in memory. `no` means the
  next request loads it first, and that request is the slow one.
- **`serving now:`** says how many requests this computer is working on. A
  coding tool that has said nothing for a while plus `0 requests` means the
  wait is not on this computer at all. Look at routing, not at the model.
- **`last turn:`** says how long the last answer took to start. It appears
  once this computer has answered something.

Claude Code shows the same thing in its footer while it works:
`⚡ waired: on Waired (qwen3-8b-instruct) · model not loaded`. When this
computer is the one answering, the connection is held open until the answer
starts, however long the load takes. If the footer says
`⚠ waired: Waired cannot answer (…)` instead, nothing of yours can take the
turn. See [Claude Code says Waired cannot answer](/troubleshooting/claude-code/#claude-code-says-waired-cannot-answer).

Still stuck? `waired runtimes status` reports on the engine itself, and
[Reading the logs](/troubleshooting/other-computers/#reading-the-logs) has
the logs.

## The Waired icon says the agent is not running

Open the Waired menu and select **Start the Waired agent…**. Your computer
asks for administrator access. That is the operating system's own prompt,
and it is required because the background service belongs to the whole
computer. To run the command yourself, **Copy start command** puts the right
command for this computer on your clipboard.

Two things this menu tells apart:

- **Waired agent is starting…** is normal. On Windows the service is set to
  start a couple of minutes after you log in, so the Waired icon is up before
  it is. You can wait, or start it now.
- **Waired agent is not running** means it should be up and is not. Start it
  from the menu, and if it does not come back, run `waired doctor`.

Stopping the service by hand does not stick. It starts again with the
computer.

## A command says “waired-agent is not running”

The background service has stopped.

```sh
sudo systemctl restart waired-agent    # Linux
Restart-Service waired-agent           # Windows, administrator
```

On macOS the system restarts it for you. If it does not come back, run
`waired doctor` or restart the computer. If it has never started, see
[the next section](#macos-the-background-service-never-starts).

A restart also clears most temporary inconsistencies, so it is worth trying
before anything more involved.

### Windows: it stopped starting at boot on its own

Windows can block Waired's background service at startup, because Waired's
programs are not yet signed with a certificate Windows recognizes. When Smart
App Control is on, it decides at boot, without a network check to fall back
on, and the service does not start. It is not consistent. The same computer
may start normally on the next boot.

Until Waired ships signed programs, start the service from the Waired menu
when this happens. Nothing is damaged, and starting the same service by hand
works. See
[If Windows refuses to run a program](/getting-started/install/windows/#if-windows-refuses-to-run-a-program).

## macOS: the background service never starts

The installer finished, but the service never comes up, and reinstalling,
even with `--clean`, changes nothing. That combination usually means macOS
has the service marked as disabled. A Waired version that was installed and
then removed between 15 July 2026 and this release left that mark behind,
and it survives uninstalling, reinstalling, and restarting the computer.

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

Installing or updating Waired now clears this for you, so you should need
these commands only on a computer where the installer itself cannot be run.

## Windows: I get a 502 error

The inference engine is not installed on this computer, usually because it
was installed with `-SkipOllama` or `WAIRED_NO_OLLAMA=1`. From an
administrator terminal:

```powershell
waired runtimes install ollama
```
