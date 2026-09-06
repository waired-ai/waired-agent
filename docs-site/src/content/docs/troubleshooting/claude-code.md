---
title: Claude Code problems
description: Claude Code is still on the cloud, is managed by an organization, says Waired cannot answer, has no Waired rows in /model, summarizes long sessions, or shows no status line.
meta:
  audience: Claude Code users whose session is not doing what they expect
  needs: A terminal on the computer in question
  time: Each fix takes a minute or two
---

Run `waired doctor` first, and press `f` to repair what it finds. Then run
`waired claude status`, which reports the state of every part of the
connection. Find your symptom below.

## Claude Code is still using the cloud

First read the footer. `→ waired: Anthropic` means this session is on an
Anthropic model. A session you have not touched is, because Claude Code's own
default is one and Waired does not change it. That is the ordinary state
after setup, not a fault. Type `/model` and select a **Waired** row. The next
turn runs on your own computers and the footer changes to
`⚡ waired: on Waired`.

If the Waired rows are not in `/model`, see
[The Waired rows are missing from /model](#the-waired-rows-are-missing-from-model).
If `waired claude status` says the integration is not enabled, enable it and
restart your Claude Code session:

```sh
sudo waired claude enable     # Windows: from an administrator terminal
```

If that ends by saying Claude Code on this computer is managed by your
organization, see [the next section](#waired-says-claude-code-is-managed-by-your-organization).

`waired claude status` shows which model new sessions start on
(`default model:`) and what the last turn did:

```
last request:       claude-opus-5 → the real Anthropic API   (2 minutes ago)
```

A turn goes to the cloud only when its model is an Anthropic one. Waired does
not send a turn there on its own, so `last request:` naming the Anthropic API
always means the session's model did.

## Waired says Claude Code is managed by your organization

`sudo waired claude enable` stops with this instead of turning routing on:

```
Claude Code on this computer is managed by your organisation, so Waired did not
change its settings. Found in /etc/claude-code/managed-settings.json:
  availableModels
  forceLoginMethod = console

Pointing ANTHROPIC_BASE_URL at Waired would also switch off the settings your
organisation delivers to every session on this computer, which is not Waired's
call to make. Ask whoever manages this computer, or use Waired from another
coding tool. `waired link` sets those up per user and touches nothing
machine-wide.
```

This is usually a work computer. Claude Code reads a machine-wide settings
file, and when whoever manages the computer has put Claude Code's
organization settings there, Waired reads them and stops. The lines under
`Found in` are what it saw, and any one of them is enough: a forced login
(`forceLoginOrgUUID`, `forceLoginMethod`, `forceLoginGatewayUrl`), a list of
allowed models (`availableModels`), a `/model` lineup (`modelPicker`), or an
`ANTHROPIC_BASE_URL` that already points somewhere other than Waired.

It stops because routing Claude Code through Waired means writing
`ANTHROPIC_BASE_URL` into that same file, and that switches off the settings
the organization delivers to every session on the computer. Whether that is
acceptable is a decision for whoever manages the computer, so Waired does not
make it, and there is no option that writes anyway.

What you can do:

- Ask whoever manages the computer. The message names the file and the
  settings involved.
- Use Waired from another coding tool on the same computer. Everything
  except the machine-wide redirect of Claude Code still works. See
  [Use Waired from OpenCode](/guides/opencode/) and
  [Use Waired from OpenClaw](/guides/openclaw/).

The routing step of `waired init` stops at the same point and prints the same
message. The rest of setup completes, and Claude Code keeps talking to the
Anthropic API directly.

## Claude Code says Waired cannot answer

A turn on a Waired row that none of your computers can serve fails at once,
inside Claude Code, with `API Error: 400` and a message that names what could
not answer. It is not sent to the Anthropic API. Every one of these messages
ends the same way: ``Pick an Anthropic model in /model to send this turn to
the cloud, or run `waired doctor` to see what is missing.`` Those are the two
ways out. The start of the message says which fix applies:

| The message starts | Meaning | What to do |
|---|---|---|
| `Waired is not set up to answer on this computer, so this turn has nowhere to run.` | No engine here, and no other computer of yours is reachable. | `waired doctor` on this computer. Start an engine here, or switch on a computer that runs one. |
| `The computer this turn is pinned to, <name>, is not answering.` | You pinned that computer with `waired worker` and it is off, asleep, or not sharing. | See [Requests stopped working after I pinned a computer](/troubleshooting/other-computers/#requests-stopped-working-after-i-pinned-a-computer). |
| `The peer <name> stopped answering after <time>.` or `The peer <name> stopped working on this request after <time>.` | The first: that computer was answering and went quiet. The second: it reported that it had stopped, or its engine is running but has stopped answering. | Check it with `waired peers list`, and `waired doctor` on that computer. |
| ``No computer on Waired runs a medium model or larger. Change the floor with `waired worker set --min-model-size`.`` | Your own minimum model size excluded every computer, this one included. | Lower or clear the minimum. See [Set a smallest model](/guides/routing/#set-a-smallest-model). |
| `Waired public share declined this turn:` followed by one of your own settings | Your Public Share settings declined it. | The message names the command. `waired public status` shows all of these settings at once, and `waired public use` changes them. |
| `Waired public share declined this turn:` followed by `no public machine is reachable right now` or `Public Share is set to use another machine only when it beats this one, and none does` | Nobody is lending a machine you can use right now, or none of them beats your own. Neither is a fault. | Wait, or pick a different row in `/model`. To stop the second one applying, run `waired public use --explicit`. |

The footer usually says it first. `⚠ waired: Waired cannot answer (local
disabled, no peer)` in red means Waired already knows nothing of yours can
take the next turn. The brackets give the state of this computer's engine
(`local disabled`, `local no_engine`, and so on) and `no peer` when no other
computer is reachable.

## The Waired rows are missing from /model

`/model` should offer **Waired**, **Waired local**, and **Waired peer** below
the Anthropic models, and **Waired public share** once Public Share is on.
Four things hide them, in the order worth checking:

1. **Claude Code has not been restarted.** The rows are read when Claude Code
   starts. Reopening `/model` in a running session does not reread them. Quit
   Claude Code and start it again.
2. **Routing is not on for this computer.** Check with `waired claude
   status`. The rows are offered only once Claude Code is pointed at Waired.

   ```sh
   sudo waired claude enable    # Windows: from an administrator terminal
   ```

3. **The rows were written for a different user.** They live in your own
   `~/.claude/settings.json` (Windows: `%USERPROFILE%\.claude\settings.json`)
   under `modelPicker`, so an install that set Waired up as `root` leaves
   them where your Claude Code never looks. `waired claude status` says which
   file it checked:

   ```
   /model rows:        not written. /home/you/.claude/settings.json
                       run `waired claude enable` as the user who runs `claude`
   ```

   When the rows are there, the same line gives their count and the file.

4. **That file already lists `/model` rows of its own.** Claude Code takes
   the whole `modelPicker` list from one place and does not merge two, so
   when your `~/.claude/settings.json` already has rows of its own, Waired
   leaves them alone and writes nothing:

   ```
   /model rows:        left alone. /home/you/.claude/settings.json already lists its own rows
   ```

   `unreadable` on the same line means the file is not JSON Waired can read.
   Once it is, run `waired claude enable` again.

Running Claude Code inside WSL2 while Waired is installed on Windows is a
separate case. They are two different systems, so use the Windows-side Claude
Code.

Until the rows are back there is nothing to pick, so a session on Claude
Code's own default stays on the Anthropic API, and the footer says
`→ waired: Anthropic`.

## Long Claude Code sessions get summarized

This is expected and healthy. Local models hold less of a conversation at
once than cloud models do, so Waired tells Claude Code the real limit, and
Claude Code summarizes older turns to fit. The session keeps working instead
of silently losing its beginning. If you briefly see “Prompt is too long”,
Claude Code recovers on its own.

If it summarizes much earlier or later than you expect, the limit passed to
Claude Code may have fallen behind after a model switch:

```sh
waired claude status
```

The **local window** line shows the limit your model handles now next to the
one Claude Code was started with. If they disagree, run
`sudo waired claude enable` again (Windows: from an administrator terminal),
then restart Claude Code.

On a computer with no engine of its own, the line reads `none here` and gives
the limit it takes from another computer instead. Nothing on this computer
holds a conversation, so the smallest limit it can reach is the honest number.
For details, see
[Long sessions get compacted](/guides/claude-code/how-turns-are-routed/#long-sessions-get-compacted).

## The status line does not show up in Claude Code

Run `waired claude status` inside the project directory. Claude Code allows
one status line, and a project-level setting (`.claude/settings.json` or
`.claude/settings.local.json`) overrides the one Waired installs for your
user. When that happens, the command names the file that is winning and
prints a line you can add to your own status-line script.

Also make sure you restarted the Claude Code session after enabling the
integration.

On Windows, a footer that went blank while `waired` itself stopped working is
a different case: Windows Application Control has refused to run
`waired.exe`. See
[If it happens after Waired is installed](/getting-started/install/windows/#if-it-happens-after-waired-is-installed).
For every form the status line takes, see
[The Waired status line](/guides/claude-code/status-line/).
