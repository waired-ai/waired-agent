---
title: FAQ
description: Common questions about Waired — setup, privacy, hardware, cost, models, sign-in, firewalls, offline use, and which tools work.
---

## Is it hard to set up?

No. You run one short command, sign in with Google, and your coding agent is
linked automatically. You don't need any networking knowledge. See
[Install](/getting-started/install/).

## Is it private?

Yes. Your prompts and replies travel straight between your own machines over an
end-to-end encrypted link. They never reach our servers, and even the relay
can't read them. See [Privacy](/concepts/privacy/).

## What do I need?

A computer to run the model — a GPU helps, but a recent CPU runs a 7B model
fine — plus your everyday laptop. The client machine you type from needs no
special hardware.

## Does it cost money?

The software is free, and the model runs on hardware you already own, so there's
no per-message bill and no subscription.

## Which AI models work?

Any local model you can run with **Ollama**, plus larger models on **vLLM** for
NVIDIA/AMD GPU servers. Waired bundles a coding model by default and you can
swap in another anytime — see the [model catalog](/reference/model-catalog/)
and [Switch the bundled model](/guides/switch-model/).

## Which tools work?

Claude Code and OpenCode are set up automatically by `waired link`. Any other
OpenAI- or Anthropic-compatible client works too, by pointing it at the
[Local Gateway](/guides/chat-clients/).

## Is it open source?

The Waired **client** is open source — you can read exactly what runs on your
machines on [GitHub](https://github.com/waired-ai/waired). The coordination service
that introduces your devices is the part hosted for you.

## How does signing in work?

`waired init` signs you in with Google and enrolls the device. Enroll every
device with the **same Google account** — that shared identity is what puts them
on the same private network and lets one use another's model.

## What if my network has a strict firewall or NAT?

Devices try a direct UDP link first. When a strict NAT or firewall blocks that,
they fall back automatically to a relay that forwards the *encrypted* traffic.
You don't configure anything; connectivity works either way. (Waired does not
use UPnP or NAT-PMP to open ports.)

## Can I use it offline after setup?

Running a model on the same machine you're typing on works locally. Using a
*remote* device's model needs network connectivity, because the control plane is
what discovers peers and keeps the signed network map current. The control plane
never sees your prompts — it only hands out keys and endpoints.

## How does Waired pick a model for me?

The Auto-Selector chooses the highest-quality model that fits your hardware's
memory, favoring efficient Mixture-of-Experts models on shared-memory machines.
Preview a routing decision with `waired infer --explain "say hi"`.

## What happens if my local model is down?

For Claude Code, the [managed-settings integration](/guides/coding-agents/) is
**fail-open**: if local serving is paused, disabled, or unavailable, requests go
to the real Anthropic API so Claude keeps working — and the routing state is
always visible via `waired claude status` and `waired doctor`. Because no
credential is written into managed settings, your claude.ai subscription stays
in use throughout. Waired never silently breaks your coding agent.
