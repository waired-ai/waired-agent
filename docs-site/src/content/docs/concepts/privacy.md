---
title: "Privacy: what leaves your computer"
description: What stays on your own devices by default, what each opt-in sharing step means, what reaches Anthropic, and why Waired never sends your data anywhere on its own.
meta:
  audience: Anyone who wants to know what leaves their computer
  needs: Nothing
  time: 6 minutes
---

By default, Waired keeps your prompts and replies on your own devices.
Anything beyond that, a team, public nodes, or a cloud API, is an explicit
choice with its own consent step. Nothing is shared silently.

## Your data path

When one of your devices uses another's model, the request travels straight
between them over an end-to-end encrypted WireGuard link. It does not pass
through any Waired-hosted service.

- **The control plane** introduces your computers to each other. It
  distributes public keys and endpoints in a signed Network Map. It never
  receives your prompts or completions.
- **The relay**, used only when a direct connection is not possible,
  forwards encrypted WireGuard packets. It cannot decrypt them.

In short, the control plane hands out keys, and the conversation happens
directly between your devices.

## Sharing beyond your own devices

Your requests can run in three places. Each step outward is a separate
opt-in with its own consent step and an immediate off switch.

- **Your own devices.** The default. Requests run only on computers signed
  in with your account.
- **Your team**, if you join one. Requests may also run on teammates'
  computers, and you appear to them by your real name. The same caveat
  applies as on any computer you do not own: its owner could see what you
  send.
- **Public nodes**, if you enable Public Share. Requests may run on computers
  shared by strangers, who see you only under a stable nickname. See
  [Public Share](/public-share/) for what the other side can and cannot see,
  and every control you have.

## What reaches Anthropic

The Claude Code integration writes one setting, `ANTHROPIC_BASE_URL`, so that
Claude Code's requests pass through Waired on this computer. Waired does not
store or use your claude.ai credentials. Only a turn whose model is an
Anthropic model reaches Anthropic, and it reaches Anthropic with your own
sign-in, as it would without Waired. A turn on a Waired row runs on your
computers or fails with a reason. It is not sent to Anthropic (decision
recorded in the project as
`docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md`).

Waired has no affiliation with or endorsement from Anthropic, and Anthropic
does not support routing Claude Code to non-Claude models through any
gateway. See [Use Waired from Claude Code](/guides/claude-code/).

## No silent fallback

Waired never moves your data somewhere else on its own, and that includes the
cloud. In Claude Code, each turn runs where the model you picked in `/model`
says. If your own computers cannot answer a Waired turn, it fails and tells
you why. The only thing that sends a turn to Anthropic is your choosing an
Anthropic model. `waired claude status` shows where the last turn went.

Public and team routing never happen silently either. They exist only after
you opted in and accepted the consent message, and `waired public status`
shows the current state at any time.

## Mixing your own computers and the cloud

The unit of choice is a Claude Code session. Pick a Waired row in one session
and an Anthropic model in another, and each stays where you put it. Whatever
a session on an Anthropic model reads or discusses goes to Anthropic, as it
would without Waired. A session on a Waired row stays on your own hardware.
No setting mixes the two inside one session.

## Cost and ownership

The model runs on hardware you already own, so there is no per-message bill
and no subscription. The Waired client is open source, and you can read what
runs on your computers on [GitHub](https://github.com/waired-ai/waired). The
control plane that introduces your devices is the part hosted for you.

## Sharing controls

You decide which computers offer their model beyond their own keyboard:

- `waired share off` keeps a computer's model private to that computer. It
  stops serving your other computers and public guests alike, immediately.
- The **Sharing** card on the computer's page in the
  [web console](/guides/web-console/) decides who a sharing computer is
  offered to: your other computers, people outside your account, or neither.
- `waired pause` takes a computer out of routing entirely.
- `waired public use --off` stops your own requests from ever using public
  nodes.

See [Share a computer with your other devices](/guides/sharing/) and
[Pause or stop Waired](/guides/pause/).
