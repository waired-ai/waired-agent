---
title: Privacy
description: What stays on your machines by default, what each opt-in sharing tier means, and why Waired never silently sends your data anywhere.
---

By default, Waired keeps **your prompts and replies on your own devices**.
Anything beyond that — a team, public nodes, a cloud API — is an explicit,
consented choice. Nothing is shared silently.

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

## Sharing beyond your own devices

Your requests can run in three places, and each step outward is a separate,
explicit opt-in with its own consent step — and an immediate off switch.

- **Your own devices** — the default. Requests run only on machines enrolled
  with your account. Nobody else is involved.
- **Your team**, if you join one. Requests may also run on teammates'
  computers, and you appear to them by your real name. The same honest caveat
  applies as on any machine you don't own: the computer's owner could see what
  you send.
- **Public nodes**, if you enable Public Share. Requests may run on computers
  shared by strangers, who see you only under a stable nickname. The full
  disclosure — what the other side can and cannot see, and every control you
  have — is at [Public share](/public-share/).

## No silent fallback

Waired deliberately avoids "quietly send your data somewhere else" behavior.
The one place a cloud fallback exists — the
[Claude Code integration](/guides/claude-code/) — is **fail-open and
visible**: if your local serving is down, Claude Code falls back to the real
Anthropic API so it keeps working, and you can see the routing state at any
time with `waired claude status` or `waired doctor`. Public and team routing
never happens silently either: it exists only after you explicitly opted in
and accepted the consent message, and you can see the current state at any
time with `waired public status`. You stay in control of when your own model
is used versus anyone else's.

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

You decide which devices offer their engine beyond their own keyboard:

- `waired inference share off` keeps an engine private to its own machine while
  still letting you use it locally.
- `waired pause` takes a device out of routing entirely.
- `waired public share` / `waired public unshare` turn public sharing of a
  computer on and off — `unshare` takes effect immediately, cutting off any
  guest work in flight ([Public share](/public-share/)).
- `waired public use --off` stops your own requests from ever using public
  nodes.

See [Sharing vs. pausing](/reference/cli/#sharing-vs-pausing).
