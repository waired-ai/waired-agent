---
name: waired-route
description: Switch where this Claude Code session's requests run — Waired inference, the real Anthropic API, or automatic.
argument-hint: [auto|waired|anthropic]
allowed-tools: Bash(waired claude route:*)
disable-model-invocation: true
---

!`waired claude route $ARGUMENTS`

The command above switched (or, with no argument, printed) Waired's routing
for Claude Code — this takes effect on your next request, no restart needed.
An argument sets ALL of Claude Code: the main conversation moves, and subagents
go back to following it. To move just one, run `waired claude route --main ...`
or `waired claude route --sub ...` from a terminal:

- `auto` — Waired first, with a visible fallback to the real Anthropic API on error (default).
- `waired` — Waired inference for turns that do not name a model; auto mode's safety check still goes to Anthropic.
- `anthropic` — the real Anthropic API (escape hatch when local misbehaves).

Naming a model in `/model` decides that session on its own: a Waired entry runs
on your computers, and any other model runs on the real Anthropic API, whatever
this setting says. This setting covers everything you did not name.

Context window: a session running on Waired gets the local model's effective
window — an over-window request gets a "prompt is too long" reply that makes
Claude Code summarize and retry on its own, so moving onto a smaller window
mid-session is safe. To work with a model's full window instead, pick that
model in `/model`: the turn goes to the real Anthropic API and its real window
applies from the next message. Changing this setting will not do it for a
session whose model already names where it runs.

Report the resulting policy to the user in one short line. Take no other action.
