---
title: FAQ
description: Short answers to the questions people ask before they install Waired, and the ones they ask afterwards.
meta:
  audience: Anyone deciding, or newly set up
  needs: Nothing
  time: Skim
---

<!-- Grouped by when the question occurs to someone: deciding, hardware,
     privacy, running it. Headings are the question as a reader would type it,
     so search lands on the answer. This is the one page where question-form
     headings are the convention; see TRANSLATION.md §Register. -->

## Deciding whether to use it

### Is it hard to set up?

One command installs it. After that you finish setup in a browser, or by
answering a few questions in a terminal. Either way takes about ten minutes,
plus the time to download a model. See the [Quickstart](/quickstart/).

### Does it cost money?

No. There is no subscription and no per-message charge. The model runs on
hardware you already own, so the cost is the electricity it uses.

### Do I need a GPU?

No, but it helps. A recent processor runs a small model at a usable speed,
and a GPU makes answers several times faster. The
[model catalog](/reference/model-catalog/) lists what each model needs. You
do not have to read it, because setup picks a model that fits.

### Which tools work with it?

Claude Code, OpenCode, and OpenClaw work after one command each. Any client
that speaks the OpenAI or Anthropic API can point at your model. See
[Use Waired from a chat app](/guides/chat-clients/).

### Is it open source?

The client, which is everything that runs on your computers, is open source
and readable on [GitHub](https://github.com/waired-ai/waired). The control
plane that introduces your devices to each other is hosted for you.

## Hardware and models

### Which models can I run?

Waired bundles a catalog of coding models and picks the best one your
computer can run. You can switch at any time. See
[Change the model](/guides/choose-a-model/).

### How does Waired choose a model for me?

It looks at your processor, memory, and GPU, and picks the highest quality
model that fits with room to spare. On a computer with a separate GPU, that
means fitting in the GPU's own memory. It then measures the real speed and
offers a lighter model if this computer cannot keep up. For details, see
[How Waired chooses a model](/guides/how-a-model-is-chosen/).

### Can I run a model that is bigger than recommended?

Yes. Waired warns you and shows the shortfall, but does not block you. A model
that is slightly over usually works and is slower. A model that is far too big
fails to load. See [Slow answers and hardware](/troubleshooting/slow-or-wrong/).

## Privacy and networking

### Is it private?

Your prompts and answers travel between your own devices over an end-to-end
encrypted connection. Waired's control plane introduces your devices to each
other and never receives what you send. The relay, used only when a direct
connection is impossible, forwards sealed data it cannot read. See
[Privacy: what leaves your computer](/concepts/privacy/).

### If my model is down, does my data go to the cloud?

No. A Claude Code turn on a **Waired** row runs on your own computers, or
fails with a reason shown at once. It is not sent to the Anthropic API. If
you want a turn in the cloud, pick an Anthropic model in `/model`. That choice
is the only thing that sends a turn to Anthropic. See
[How a Claude Code turn is routed](/guides/claude-code/how-turns-are-routed/).

### Do I need to open ports or set up a VPN?

No. Your computers connect directly when the network allows it, and fall back
to an encrypted relay when a firewall or strict NAT gets in the way. Both are
automatic.

### How does signing in work?

You sign in with Google. Every computer signed in with the same account joins
the same private network and can reach the others. There is nothing to pair
and no address to copy.

### Can I use it offline?

Once a model is downloaded, the computer running it answers with no internet
connection at all. Reaching that computer from another device needs a network
path between them. On the same home or office network, that works offline too.

## Running it

### How do I update?

Run `waired update`, or select the update notice in the Waired app when one
appears. See [Update Waired](/getting-started/update/).

### How do I remove it?

One command, about ten seconds. You choose whether to keep your downloaded
models. See [Uninstall Waired](/getting-started/uninstall/).

### Something is wrong. Where do I start?

Run `waired doctor`. It checks everything and repairs what it can. Then see
[Troubleshooting](/troubleshooting/), which is organized by symptom.
