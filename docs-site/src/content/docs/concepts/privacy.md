---
title: Privacy
description: What stays on your machines, what the control plane and relay can see, and why Waired never silently breaks your coding agent.
---

Waired is built so that **your prompts and replies stay on your own devices**.

## Your data path

When one of your devices uses another's model, the request travels straight
between them over an end-to-end encrypted WireGuard link. It does not pass
through any Waired-hosted service.

- **The control plane** only introduces your machines to each other. It
  distributes peer public keys and endpoints via a signed Network Map. It never
  receives your prompts or completions.
- **The relay**, used only when a direct connection isn't possible, forwards
  encrypted WireGuard datagrams. It cannot decrypt them — it sees ciphertext,
  not content.

In short: the coordination service hands out keys; the conversation happens
directly between your devices.

## No silent fallback

Waired deliberately avoids "quietly send your data somewhere else" behavior. The
one place a fallback exists — the [Claude Code integration](/guides/coding-agents/) —
is **fail-open and visible**: if your local serving is down, Claude Code falls
back to the real Anthropic API so it keeps working, and you can see the routing
state at any time with `waired claude status` or `waired doctor`. You stay in
control of when your own model is used versus the cloud.

## Hybrid mode is an explicit middle ground

`waired claude route anthropic --subagents waired` opts the Claude Code **main
conversation** into the real Anthropic API while subagents keep running on your
own devices. Privacy-wise this sits **between** all-local and all-Anthropic, and
it only happens because you set it: the bulk file reading subagents do stays on
Waired, but whatever the main conversation itself reads or discusses — including
the summaries subagents report back — goes to Anthropic, exactly as it would
without Waired. To keep a class strictly on your own hardware, choose the
`waired` route for it (`--subagents waired` above keeps subagents local even
while the main conversation uses Anthropic). The default remains
everything on Waired.

## Cost and ownership

The model runs on hardware you already own, so there's no per-message bill and
no subscription. The Waired **client** is open source — you can read exactly
what runs on your machines on [GitHub](https://github.com/waired-ai/waired). The
coordination service that introduces your devices is the part hosted for you
(the same split Tailscale uses).

## Sharing controls

You decide which devices offer their engine to the rest of your network:

- `waired inference share off` keeps an engine private to its own machine while
  still letting you use it locally.
- `waired pause` takes a device out of routing entirely.

See [Sharing vs. pausing](/reference/cli/#sharing-vs-pausing).
