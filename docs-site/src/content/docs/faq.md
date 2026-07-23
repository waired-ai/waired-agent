---
title: FAQ
description: The questions people ask before they install Waired, and the ones they ask afterwards.
---

<!-- Grouped by when the question occurs to someone — deciding, hardware,
     privacy, running it — rather than by feature. Headings are the question as
     a reader would type it, so search lands on the answer. -->

## Deciding whether to use it

### Is it hard to set up?

One command installs it. After that you finish setup either in a browser or by
answering two questions in a terminal — about ten minutes either way, plus the
time it takes to download a model. See the [Quickstart](/quickstart/).

### Does it cost money?

No. There is no subscription and no per-message charge. The model runs on
hardware you already own, so the cost is the electricity it uses.

### Do I need a graphics card?

No, but it helps a lot. A recent processor runs a small model well enough to be
useful; a graphics card makes answers several times faster. The
[model catalog](/reference/model-catalog/) lists what each model needs — though
you do not have to read it, because setup picks one that fits.

### Which tools work with it?

Claude Code and OpenCode work out of the box. Any client that speaks the OpenAI
or Anthropic API can point at your model — see
[Use it from a chat app](/guides/chat-clients/).

### Is it open source?

The client — everything that runs on your machines — is open source and
readable at [GitHub](https://github.com/waired-ai/waired). The coordination
service that introduces your devices to each other is hosted for you.

## Hardware and models

### Which AI models can I run?

Waired bundles a catalog of coding models and picks the best one your machine
can actually run. You can switch at any time:
[Choose which AI model runs](/guides/choose-a-model/).

### How does Waired choose a model for me?

It looks at your processor, memory and graphics card, and picks the highest
quality model that fits with room to spare — then measures the real speed and
offers a lighter one if this machine cannot keep up. It will not fill your disk:
if space is short it steps down rather than failing halfway.

### Can I run a model that is bigger than recommended?

Yes. Waired warns you and shows the shortfall, but does not block you. Slightly
over usually works and is just slower; genuinely too big fails to load. See
[I chose a model bigger than my hardware](/troubleshooting/#i-chose-a-model-bigger-than-my-hardware).

## Privacy and networking

### Is it private?

Your prompts and answers travel between your own devices over an end-to-end
encrypted connection. Waired's coordination service introduces your devices to
each other and never receives what you send; the relay, used only when a direct
connection is impossible, forwards sealed data it cannot read. Full detail:
[Privacy](/concepts/privacy/).

### What if my model is down — does my data go to the cloud?

Only for Claude Code, only when your own serving cannot answer, and never
silently: Claude Code falls back to the real Anthropic API so your turn
completes, and Waired tells you it happened. If you would rather see the error,
choose the `waired` route — see
[Use it from Claude Code](/guides/claude-code/#switch-where-requests-go).

### Do I need to open ports or set up a VPN?

No. Your computers connect directly when the network allows it, and fall back to
an encrypted relay when a firewall or strict NAT gets in the way. Both are
automatic.

### How does signing in work?

You sign in with Google. Every computer signed in with the **same account**
joins the same private network and can reach the others — that shared sign-in
is the whole mechanism. There is nothing to pair and no address to copy.

### Can I use it offline?

Once a model is downloaded, the computer running it can answer with no internet
connection at all. Reaching that computer *from another device* needs a network
path between them — on the same home or office network that works offline too.

## Running it

### How do I update?

`waired update`, or the update entry on the Waired icon when one is available.
See [Update Waired](/getting-started/update/).

### How do I remove it?

One command, about ten seconds — and you choose whether to keep your downloaded
models. See [Uninstall](/getting-started/uninstall/).

### Something is wrong. Where do I start?

`waired doctor`. It checks everything and repairs what it can. Then
[Troubleshooting](/troubleshooting/), which is organised by symptom.
