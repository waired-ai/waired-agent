---
title: Public Share
description: What sharing your computer with other Waired users means, what the other side can and cannot see, why sharing is required to use it, and every control you have.
meta:
  audience: Anyone considering Public Share, in either direction
  needs: Nothing. Read this before turning it on
  time: 10 minutes
---

This page is the full disclosure behind the consent message you see when you
enable Public Share. Each section states what happens, and why.

## What Public Share is

Public Share is off by default and strictly opt-in. When you turn it on, you
can run inference on other Waired users' spare computers, called public
nodes, and other Waired users can run work on yours. The people using your
computer are guests. You appear to each other only under an automatically
assigned nickname. To use public nodes you must also share one of yours. See
[Why you must share to use](#why-you-must-share-to-use).

## What the other side can and cannot see

### The owner could see what you send

The owner of a public node could read what you send to it. Your request is
processed in plain form in that computer's memory, and its owner fully
controls that computer. The Waired client is open source under Apache-2.0
and can be modified, so not logging your requests is our policy and the
official app's default behavior, not a technical guarantee. The official app
does not write your prompts or replies to logs or disk.

The consequence is simple. Do not send secrets, passwords, personal data, or
private code through public nodes.

### Leftover traces fade on their own

While a model runs, it keeps a short-term cache of recent requests, the KV
cache, to respond faster. On a public node that cache is overwritten by later
requests and freed when the model is unloaded. In this version, nothing
actively erases it. Today's model runtimes offer no way to erase one request
selectively, and force-unloading the model to clear the cache would disrupt
the owner's own work.

### Answers from public nodes are not verified

This version does not verify that a public node ran the model faithfully, or
that it returned honest, full-quality output. Your controls: set a minimum
model size with `--min-model-size`, use explicit mode so public nodes are
used only when you say so, and judge results yourself.

### Your nickname is stable, so patterns can be linked

Each pair of accounts gets a fixed nickname. The other side never sees your
name, email, or account. Because the nickname does not change, the same
counterpart can recognize your usage pattern over time: when you tend to be
active, and how much you use.

### When your IP address is visible

The consent message says the other side "may" see your IP address. Here is
exactly when. When your computer and the public node connect directly, each
side can see the other's public IP address, from which an approximate region
and internet provider can be inferred. When traffic goes through a relay,
used when a direct connection is not possible, the other side sees the
relay's address, not yours. Which one happens is automatic and depends on
both networks, so you cannot count on either. Treat your IP address as
possibly visible whenever you use or share public nodes. Relayed traffic
stays end-to-end encrypted, and the relay cannot read it. See
[Architecture](/concepts/architecture/).

### What Waired itself records

Waired records request counts, token counts, duration, and which model, kept
under your nickname so both sides can see usage totals. It never records
what was asked or answered. As the web console puts it: "Waired never records
what was asked or answered — only how many requests, how many tokens, and how
long they took." Prompts and replies never touch Waired's servers. See
[Privacy: what leaves your computer](/concepts/privacy/).

## Why you must share to use

Public Share works only if people contribute. Using public nodes requires at
least one of your computers to be shared publicly and online. Accepting the
consent message records your consent. It does not turn sharing on anywhere.
The product says so when you accept: "To use other people's computers you
must share one of yours. Turn on public sharing for a computer in the Waired
console." There is no ratio or quota. One shared computer qualifies your
whole account.

## Turning it on and off

Public sharing is turned on and off in the web console. The computer's page
has a **Sharing** card with a **People outside your account** switch, and the
**Public share** tab shows usage by nickname with a per-computer **Stop
sharing**. There is no command or menu item for it. The CLI and the Waired
app only report it. `waired public status` prints the state:

```text
Sharing this computer publicly: on
Guest limit: automatic
Public sharing is turned on and off in the Waired console.
```

The computer itself keeps one switch of its own: whether it lends itself out
at all. `waired share off`, or **Stop sharing this computer** in the Waired
app, stops every kind of serving at once, public guests included, and the
console cannot turn it back on. See
[Share a computer with your other devices](/guides/sharing/).

- **Stopping is immediate.** Turning sharing off cuts off any guest requests
  running at that moment and cancels every guest's access to that computer.
  You can turn it back on at any time.
- **Max guests** is how many guests may use the computer at once, set in the
  console. Automatic, the default, keeps one slot free for you. You can
  raise it up to the computer's full capacity. Whatever you set, your own
  work takes priority when the computer is busy. A guest never blocks you
  for long, and new guest work is paused while you are using it.
- The two console switches are linked. Turning public sharing on also turns
  on sharing with your own other computers, and turning **Your other
  computers** off also turns public sharing off.

## Choosing when public nodes are used

`waired public use`, or **Public computers** in the Waired app, controls when
your requests may go to public nodes:

- **off**, the default. Public nodes are never used.
- **auto**. A public node is used only when its model is better than the best
  your own computers offer.
- **explicit**. Public nodes are allowed whenever the filters below allow
  them.

Extra controls in the CLI: `--min-model-size small|medium|large` only uses
computers running a model of at least that size. `--main on|off` and
`--sub on|off` allow or deny public nodes for the main conversation and
subagents separately, for example to keep your main assistant off public
nodes while subagents may use them.

Your own computers are always preferred over public nodes.

## Known limitations

- The first request to a public node takes a few extra seconds to connect.
- Owners come first. A public node can pause taking new guest work at any
  moment, without notice. Your request then falls back to other nodes or
  retries.
- If an owner stops sharing while your request is running, the request fails
  and partial output is discarded.

## Your consent

Consent is recorded once, together with the version of the message you
accepted. If the wording ever changes in a meaningful way, you are asked
again before public nodes are used. You can see usage under nicknames at any
time in the web console's **Public share** tab.
