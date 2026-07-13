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
It sets the MAIN conversation; subagents follow it unless set separately with
`waired claude route --subagents ...`:

- `auto` — Waired first, with a visible fallback to the real Anthropic API on error (default).
- `waired` — Waired inference only; never contacts Anthropic.
- `anthropic` — use the real Anthropic API (escape hatch when local misbehaves).

Context window: on `anthropic`, the selected model's real window applies (a
`[1m]` model gets its full 1M from the next request, even mid-session). On
`waired`/`auto`, Waired enforces the local model's effective window — an
over-window request gets a "prompt is too long" reply that makes Claude Code
summarize and retry on its own, so switching onto a smaller window
mid-session is safe.

Report the resulting policy to the user in one short line. Take no other action.
