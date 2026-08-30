---
title: Public Share
description: What sharing your computer with other Waired users means — what the other side can and cannot see, why sharing is required, and every control you have.
meta:
  audience: Anyone considering Public Share, in either direction
  needs: Nothing — read this before turning it on
  time: 10 minutes
---

This page is the full disclosure behind the consent message you saw when
enabling Public Share. Each point below states what happens — and why.

## What Public Share is

Public Share is **off by default and strictly opt-in**. When you turn it on,
you can run inference on other Waired users' spare computers ("public nodes"),
and other Waired users can run work on yours. The people using your node are
guests; you appear to each other only under an automatically assigned
nickname. To use public nodes you must also share one of yours — see
[Why you must share to use](#why-you-must-share-to-use).

## What the other side can and cannot see

### The owner could see what you send

**The owner of a public node could read what you send to it.** Your request is
processed in plain form in that computer's memory, and its owner fully
controls that computer. The Waired client is open source (Apache-2.0) and
modifiable — so not logging your requests is our policy and the official app's
default behavior, **not a technical guarantee**. The official app does not
write your prompts or replies to logs or disk.

The consequence is simple: **do not send secrets, passwords, personal data, or
private code through public nodes.**

### Leftover traces fade on their own; nothing actively erases them

While a model runs, it keeps a short-term cache (the KV cache) of recent requests (a
cache) to respond faster. On a public node that cache is overwritten by later
requests and freed when the model is unloaded — but **in this version, nothing
actively erases it**. Today's model runtimes offer no way to erase one request
selectively, and force-unloading the model to clear the cache would disrupt
the owner's own work.

### Answers from public nodes are not verified

**This version does not verify that a public node ran the model faithfully**,
or that it returned honest, full-quality output. Your controls: set a minimum
model size (`--min-model-size`), use explicit mode so public nodes are only
used when you say so, and judge results yourself.

### Your nickname is stable, so patterns can be linked

Each pair of accounts gets a fixed nickname — the other side never sees your
name, email, or account. But **because the nickname does not change, the same
counterpart can recognize your usage pattern over time**: when you tend to be
active, and how much you use.

### When your IP address is visible

The consent message says the other side "may" see your IP address. Here is
exactly when. When your computer and the public node connect **directly** (see
[Architecture](/concepts/architecture/)), each side can see the other's public
IP address, from which an approximate region and internet provider can be
inferred. When traffic goes through a **relay** — used when a direct
connection is not possible — the other side sees the relay's address, not
yours. Which one happens is automatic and depends on both networks, so you
cannot count on either: **treat your IP address as possibly visible whenever
you use or share public nodes.** (Relayed traffic stays end-to-end encrypted;
the relay cannot read it.)

### What Waired itself records

Waired records **request counts, token counts, duration, and which model** —
kept under your nickname so both sides can see usage totals. Never what was
asked or answered. As the web console puts it: "Waired never records what was
asked or answered — only how many requests, how many tokens, and how long they
took." Prompts and replies never touch Waired's servers
([Privacy](/concepts/privacy/)).

## Why you must share to use

Public Share works only if people contribute. **Using public nodes requires at
least one of your computers to be shared publicly and online.** Accepting the
consent message records your consent — it does not turn sharing on anywhere.
The product says so when you accept: "To use other people's computers you must
share one of yours. Turn on public sharing for a computer in the Waired
console." There is no ratio or quota — one shared computer qualifies your
whole account.

## Turning it on and off

Public sharing is turned on and off in the **web console**: the device page's
**Sharing** card has a **People outside your account** switch, and the
**Public share** tab shows usage by nickname with a per-node "Stop sharing".
There is no command or menu item for it — the CLI and the Waired app only
report it. `waired public status` prints the state:

```text
Sharing this computer publicly: on
Guest limit: automatic
Public sharing is turned on and off in the Waired console.
```

The computer itself keeps one switch of its own: whether it lends itself out
at all. `waired share off` (or **Stop sharing this computer** in the Waired
app) stops every kind of serving at once — public guests included — and the
console cannot turn it back on. See [`waired share`](/reference/cli/#waired-share).

- **Stopping is immediate — a kill switch.** Turning sharing off cuts off any
  guest requests running at that moment and cancels every guest's access for
  that computer. You can turn it back on at any time.
- **Max guests** is how many guests may use the computer at once, set in the
  console. Automatic — the default — keeps one slot free for you. You can
  raise it up to the computer's full capacity — and whatever you set, **your
  own work takes priority when the computer is busy**: a guest never blocks
  you for long, and new guest work is paused while you are using it.
- The two console switches are linked: turning public sharing **on** also
  turns on sharing with your own other computers, and turning **Your other
  computers** off also turns public sharing off — a computer you are not
  lending to your own machines is not lent to strangers either.

## Choosing when public nodes are used

`waired public use` (or **Public computers → Use public computers** in the Waired app) controls when
your requests may go to public nodes:

- **off** (the default) — public nodes are never used.
- **auto** — a public node is used only when its model is better than the best
  your own computers offer.
- **explicit** — public nodes are allowed whenever the filters below allow
  them.

Extra controls (CLI): `--min-model-size small|medium|large` only uses machines
running a model of at least that size;
`--main on|off` and `--sub on|off` allow or deny public nodes for the main
conversation and sub-agents separately — for example, keep
your main assistant off public nodes while sub-agents may use them.

Your own computers — and, if you are on a team, your teammates' — are always
preferred over public nodes. Teammates' shared computers are separate from
Public Share: teammates see your real name, not a nickname, and no reciprocity
is required.

## Known limitations

- The first request to a public node takes a few extra seconds to connect
  (cold start).
- Owners come first: a public node can pause taking new guest work at any
  moment, without notice. Your request then falls back to other nodes or
  retries.
- If an owner stops sharing while your request is running, the request fails
  and partial output is discarded.

## Your consent

Consent is recorded once, together with the version of the message you
accepted. **If the wording ever changes in a meaningful way, you will be asked
again** before public nodes are used. You can see usage under nicknames at any
time in the web console's **Public share** tab.
