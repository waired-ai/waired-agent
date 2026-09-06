---
title: Coding tool commands
description: waired link, unlink, and claude, including the status line and subagent subcommands, and what the retired commands say when you run them.
meta:
  audience: Anyone working in a terminal, or on a computer with no screen
  needs: Waired installed
  time: Read the command you need
---

## `waired link` and `unlink`

```sh
waired link                  # set up every coding tool found
waired link claude-code
waired link opencode
waired link openclaw
waired link --force all      # reapply, even where nothing seems to have changed
waired link --dry-run        # print what would change, change nothing
waired unlink <agent>
```

`link` writes the per-user integration for each tool: skills for Claude
Code, and a plugin for OpenCode and OpenClaw. The files activate the moment
the tool is installed, so linking a tool that is not installed yet is fine.
`unlink` undoes only what `link` added. Where `link` had to change a config
file you already had, which only OpenClaw has, the copy it took first is
kept, and `unlink` prints where. See
[Use Waired from OpenCode](/guides/opencode/) and
[Use Waired from OpenClaw](/guides/openclaw/).

## `waired claude`

```sh
waired claude status
sudo waired claude enable            # point Claude Code at Waired (init does this too)
sudo waired claude enable --no-statusline
sudo waired claude disable
```

`enable` and `disable` need administrator rights. No credential is written,
so your claude.ai subscription is unaffected. See
[Use Waired from Claude Code](/guides/claude-code/).

On a computer where an organization already manages Claude Code, `enable`
writes nothing and says so. It reads the machine-wide settings file first,
and any of `forceLoginOrgUUID`, `forceLoginMethod`, `forceLoginGatewayUrl`,
`availableModels`, `modelPicker`, or an `ANTHROPIC_BASE_URL` that is not
Waired's own loopback address means somebody other than you configured Claude
Code on it. See
[Waired says Claude Code is managed by your organization](/troubleshooting/claude-code/#waired-says-claude-code-is-managed-by-your-organization).

`enable` writes the machine-wide setting and, in your own
`~/.claude/settings.json`, the Waired rows of `/model` under `modelPicker`. A
list already there that is not Waired's is left alone. It does not set a
default model, so a session you have not touched starts on Claude Code's own
default. `disable` removes the rows, the status line, and the subagent
setting.

One kind of request goes to the Anthropic API whatever the session's model
is: the safety check Claude Code's auto mode runs, a classifier that scores
each tool call to decide whether it may proceed. Claude Code chooses that
model itself, so Waired cannot stand in for a permission decision. If
Anthropic cannot be reached, that check fails.

### What `status` prints

```
managed settings:   /etc/claude-code/managed-settings.json (present)
ANTHROPIC_BASE_URL: http://127.0.0.1:9472
expected base URL:  http://127.0.0.1:9472
gateway listener:   127.0.0.1:9472 (listening)
local window:       200704  (managed settings: 200704)
/model rows:        6 rows
                    /home/you/.claude/settings.json
statusline:         waired segment installed
subagents:          follow their own model
default model:      not set — Claude Code uses its own, which is a real Anthropic model
last request:       waired → Waired   (2 minutes ago)
last served:        2026-09-04T01:52:11+09:00 — qwen3.5-9b (peer sv-mag)
waired node:        auto (this device or a mesh peer)   (change with `waired worker`)
```

| Row | Meaning |
|---|---|
| `managed settings:` | The machine-wide file, and whether it is present. |
| `ANTHROPIC_BASE_URL:` | What the file points at. `(not set)` when routing is off, or `unreadable. This file isn't JSON Waired can parse.` |
| `local window:` | The context window this computer's engine holds, next to the one passed to Claude Code. The row says when they disagree. On a computer with no engine, it reads `none here` and gives the limit taken from another computer. |
| `/model rows:` | How many Waired rows are in your settings file, or `not written`, `left alone` (the file lists rows of its own), or `unreadable`. |
| `statusline:` | `waired segment installed`, `wrapping your existing statusLine`, `not waired (custom: …)`, `not installed`, or `installed but shadowed here by <file> (<scope> scope)`. |
| `subagents:` | `follow their own model`, `on Waired`, or `left alone. CLAUDE_CODE_SUBAGENT_MODEL=<value> isn't Waired's`. |
| `default model:` | The model new sessions start on, and where that sends them. |
| `last request:` | The model id the last turn carried, which side that id sent it to, and when. |
| `last served:` | What answered it, on which computer. |
| `waired node:` | Which of your computers takes a turn addressed to Waired. Change it with `waired worker`. |

A row reading `installed, but not in the form this computer runs` means the
status line or the `/model` refresh hook was written for another operating
system's shell. `sudo waired claude enable` rewrites them.

### `waired claude statusline`

```sh
waired claude statusline                 # print the segment, as Claude Code calls it
waired claude statusline install         # add the segment to ~/.claude/settings.json
waired claude statusline install --wrap  # wrap an existing statusLine instead of skipping it
waired claude statusline remove          # remove Waired's segment (restores a wrapped one)
```

`enable` installs the segment already. `--wrap` wraps an existing status line
rather than replacing it. `disable` restores your own line and removes it.
See [The Waired status line](/guides/claude-code/status-line/).

### `waired claude subagents`

```sh
waired claude subagents            # report the current setting
waired claude subagents follow     # each subagent runs where its own model says
waired claude subagents waired     # every subagent runs on your computers
```

The switch is written to your own `~/.claude/settings.json`, so it needs no
administrator rights, and it applies to `claude` sessions started after it:

```
Subagents run on Waired (/home/you/.claude/settings.json).
Restart any running `claude` session to pick it up.
```

See [Choose where subagents run](/guides/claude-code/subagents/).

## Retired commands

Older notes and scripts may still name these. Each one says what replaced
it:

| Command | What it says |
|---|---|
| `waired claude route` | ``(removed) pick where a turn runs in Claude Code's /model.`` |
| `waired claude node` | ``(removed) use /model to choose a side and 'waired worker' to choose a node.`` |
| `waired claude fallback` | ``(removed) Waired never sends a turn to Anthropic on its own.`` |
| `waired proxy` | ``waired proxy was removed in favour of managed settings; use waired claude <enable|disable|status>`` |
