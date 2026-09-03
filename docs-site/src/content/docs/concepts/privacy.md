---
title: Privacy
description: What stays on your machines by default, what each opt-in sharing step means, and why Waired never silently sends your data anywhere.
meta:
  audience: Anyone who wants to know what leaves their computer
  needs: Nothing
  time: 10 minutes
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
- **The [relay](/reference/glossary/#relay)**, used only when a direct connection isn't possible, forwards
  encrypted WireGuard datagrams. It cannot decrypt them — it sees ciphertext,
  not content.

In short: the control plane hands out keys; the conversation happens
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

Waired deliberately avoids "quietly send your data somewhere else" behavior,
and that includes the cloud. In the
[Claude Code integration](/guides/claude-code/), each turn runs where the
model you picked in `/model` says: a **Waired** entry runs it on your own
computers, an Anthropic model sends it to the real Anthropic API. Waired does
not move a turn to the other side on its own — if your own computers cannot
answer a Waired turn, it **fails and tells you why** rather than going to the
cloud, and the only thing that sends a turn to Anthropic is your choosing an
Anthropic model. `waired claude status` shows where the last turn went. Public
and team routing never happens silently either: it exists only after you
explicitly opted in and accepted the consent message, and you can see the
current state at any time with `waired public status`. You stay in control of
when your own model is used versus anyone else's.

## Mixing your own computers and the cloud

The unit of choice is a Claude Code session. Pick a **Waired** entry in one
session and Opus in another, and each stays where you put it. Whatever a
session on an Anthropic model reads or discusses goes to Anthropic, exactly as
it would without Waired; a session on a Waired entry stays on your own
hardware. There is no setting that mixes the two inside one session.

## Cost and ownership

The model runs on hardware you already own, so there's no per-message bill and
no subscription. The Waired **client** is open source — you can read exactly
what runs on your machines on [GitHub](https://github.com/waired-ai/waired). The
control plane that introduces your devices is the part hosted for you
(the same split Tailscale uses).

## Sharing controls

You decide which devices offer their engine beyond their own keyboard:

- `waired share off` keeps a computer's engine private to that machine while
  still letting you use it locally — it stops serving your other computers and
  public guests alike, immediately, cutting off any work in flight.
- The **Sharing** card on the computer's page in the
  [web console](/guides/web-console/) decides who a lending computer is
  offered to: your other computers, people outside your account, or neither
  ([Public share](/public-share/)).
- `waired pause` takes a device out of routing entirely.
- `waired public use --off` stops your own requests from ever using public
  nodes.

See [Sharing vs. pausing](/reference/cli/#sharing-vs-pausing).
