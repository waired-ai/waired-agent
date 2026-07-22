---
title: Words used in this documentation
description: Plain-language definitions of every Waired-specific and AI-specific word you will meet in these docs.
---

<!-- PROTOTYPE (docs IA revision). Demonstrates: every coined or borrowed term
     gets one plain sentence, ordered by when you meet it rather than
     alphabetically, with the internal jargon we currently leak (mesh, peer,
     overlay, control plane, enrollment) mapped onto words a reader already
     knows. -->

You do not need to read this page. It is here for the moment a word in another
page stops you.

## Words you meet while setting up

**Waired app**
: The two things the installer puts on your computer: a background service that
  stays connected, and the `waired` command you type. You mostly interact with
  the icon in your menu bar / system tray.

**Sign in / enroll**
: Signing in with Google adds this computer to your private network. Every
  computer signed in with the *same* Google account can reach the others.

**Your network**
: The private, encrypted connection between your own computers. Nobody else can
  join it, and it is not reachable from the public internet.

**Device**
: One computer in your network. Your desktop, your laptop and your work machine
  are three devices.

**Administrator rights**
: Permission to change the whole computer rather than just your own account.
  Waired needs it once, to install a background service that starts with the
  computer. On macOS and Linux this is what `sudo` means; on Windows it is the
  blue "Do you want to allow this app to make changes?" window.

## Words you meet around the AI itself

**Model**
: The AI itself — a multi-gigabyte file of learned parameters. `qwen3-coder-30b`
  is a model. Bigger models give better answers and need more memory.

**Inference**
: The act of producing an answer from a model. "Running inference on this
  computer" means the answer is computed here.

**Inference engine**
: The program that loads a model into memory and runs it. Waired installs and
  manages [Ollama](https://ollama.com) for you.

**Memory / VRAM**
: A model has to fit in memory to run. On a computer with a separate graphics
  card that means the card's own memory (**VRAM**). On Apple Silicon and some
  AMD chips, memory is shared between the processor and graphics
  ("unified memory"), so the whole pool counts.

**tok/s (tokens per second)**
: How fast answers come out. A token is roughly three-quarters of a word.
  Below about 15 tok/s a coding assistant starts to feel slow.

**Context window**
: How much of the conversation the model can consider at once. Local models have
  smaller windows than cloud models, which is why long Claude Code sessions get
  summarized to fit — that is normal, and nothing is lost from the answer.

## Words you meet when using it from another computer

**Direct connection**
: Your two computers talking to each other straight across the internet,
  encrypted end to end. This is the normal case.

**Relay**
: When a firewall or router blocks a direct connection, traffic goes through a
  Waired server instead. It is still encrypted end to end — the relay passes
  along sealed data it cannot read.

**Sharing**
: Whether your *other* devices are allowed to use this computer's AI. Turn it on
  for a desktop that should serve your laptop; leave it off to keep the AI to
  this machine.

**Pausing**
: Temporarily stopping Waired from handling anything on this computer, without
  uninstalling or changing settings. `waired resume` undoes it.

**Public share**
: An opt-in, separate feature that offers your AI to people outside your own
  account. Off by default. See [Public share](/public-share/).

## Words for the coding-agent setup

**Coding agent**
: Claude Code, OpenCode, and similar tools that write and edit code for you.

**Routing**
: Which AI answers a given request — your own computer, another of your
  computers, or the cloud provider. `waired claude route` shows and changes it.

**Falling back**
: When your own AI cannot answer (still downloading, out of memory, computer
  asleep), Claude Code quietly uses the real Anthropic API instead so you are
  not blocked. Waired always tells you when this happened rather than hiding it.

## Words you may see in error messages

**Waired agent / daemon**
: The background service. "waired-agent is not running" means it stopped —
  restart it, or run `waired doctor`.

**Gateway**
: The local address on your own computer (`127.0.0.1:...`) that coding agents
  send requests to. It never leaves your machine unless the answer has to come
  from another of your devices.

**Coordination service**
: The Waired-run service that tells your devices how to find each other. It
  handles sign-in and device lists only — your prompts and answers never pass
  through it. See [Privacy](/concepts/privacy/).
