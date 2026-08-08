---
title: Words used in this documentation
description: Plain-language definitions of every Waired-specific and AI-specific word you will meet in these docs.
meta:
  audience: Anyone who hit a word they did not know
  needs: Nothing
  time: Look up one term
---

<!-- Every coined or borrowed term gets one plain sentence, ordered by when
     you meet it rather than alphabetically. Terms the product itself surfaces
     (mesh, peer, overlay, worker, control plane, enrollment) each have an
     entry below; other pages qualify them at first use ("the Waired mesh",
     "a peer device") per docs-site/TRANSLATION.md. -->

You do not need to read this page. It is here for the moment a word in another
page stops you.

## Words you meet while setting up

<a id="waired-app"></a>
**Waired app**
: The two things the installer puts on your computer: a background service that
  stays connected, and the `waired` command you type. You mostly interact with
  the Waired icon — in the menu bar on macOS, next to the clock on Windows
  and Linux.

<a id="sign-in"></a>
**Sign in / enroll**
: Signing in with Google adds this computer to your private network. Every
  computer signed in with the *same* Google account can reach the others.

<a id="your-network"></a>
**Your network**
: The private, encrypted connection between your own computers. Nobody else can
  join it, and it is not reachable from the public internet.

<a id="device"></a>
**Device**
: One computer in your network. Your desktop, your laptop and your work machine
  are three devices.

<a id="administrator-rights"></a>
**Administrator rights**
: Permission to change the whole computer rather than just your own account.
  Waired needs it once, to install a background service that starts with the
  computer. On macOS and Linux this is what `sudo` means; on Windows it is the
  blue "Do you want to allow this app to make changes?" window.

## Words you meet around the AI itself

<a id="model"></a>
**Model**
: The AI itself — a multi-gigabyte file of learned parameters. `qwen3.6-27b`
  is a model. Bigger models give better answers and need more memory.

<a id="inference"></a>
**Inference**
: The act of producing an answer from a model. "Running inference on this
  computer" means the answer is computed here.

<a id="inference-engine"></a>
**Inference engine**
: The program that loads a model into memory and runs it. Waired installs and
  manages [Ollama](https://ollama.com) for you.

<a id="memory-vram"></a>
**Memory / VRAM**
: A model has to fit in memory to run. On a computer with a separate graphics
  card that means the card's own memory (**VRAM**). On Apple Silicon and some
  AMD chips, memory is shared between the processor and graphics
  ("unified memory"), so the whole pool counts.

<a id="toks"></a>
**tok/s (tokens per second)**
: How fast answers come out. A token is roughly three-quarters of a word.
  Below about 15 tok/s a coding assistant starts to feel slow.

<a id="context-window"></a>
**Context window**
: How much of the conversation the model can consider at once. Local models have
  smaller windows than cloud models, which is why long Claude Code sessions get
  summarized to fit — that is normal, and nothing is lost from the answer.

## Words you meet when using it from another computer

<a id="direct-connection"></a>
**Direct connection**
: Your two computers talking to each other straight across the internet,
  encrypted end to end. This is the normal case.

<a id="relay"></a>
**Relay**
: When a firewall or router blocks a direct connection, traffic goes through a
  Waired server instead. It is still encrypted end to end — the relay passes
  along sealed data it cannot read.

<a id="mesh"></a>
**Mesh**
: The private network your devices form with each other — every device can
  reach every other directly, with no central hub in the middle. App menus
  call it the mesh (**Share engine to mesh**); pages here say "the Waired
  mesh" at first mention.

<a id="overlay"></a>
**Overlay network**
: A private network built on top of your existing internet connection.
  Waired's mesh is one: nothing about your router or Wi-Fi changes — the
  overlay is just encrypted links between your own devices.

<a id="peer"></a>
**Peer**
: Any other device in your mesh, seen from the one you are on.
  `waired peers list` names them; pages here say "a peer device" at first
  mention.

<a id="sharing"></a>
**Sharing**
: Whether your *other* devices are allowed to use this computer's AI. Turn it on
  for a desktop that should serve your laptop; leave it off to keep the AI to
  this machine.

<a id="pausing"></a>
**Pausing**
: Temporarily stopping Waired from handling anything on this computer, without
  uninstalling or changing settings. `waired resume` undoes it.

<a id="public-share"></a>
**Public share**
: An opt-in, separate feature that offers your AI to people outside your own
  account. Off by default. See [Public share](/public-share/).

## Words for the coding-agent setup

<a id="coding-agent"></a>
**Coding agent**
: Claude Code, OpenClaw, and similar tools that write and edit code for you.

<a id="routing"></a>
**Routing**
: Which AI answers a given request — your own computer, another of your
  computers, or the cloud provider. `waired claude route` shows and changes it.

<a id="falling-back"></a>
**Falling back**
: When your own AI cannot answer (still downloading, out of memory, computer
  asleep), Claude Code quietly uses the real Anthropic API instead so you are
  not blocked. Waired always tells you when this happened rather than hiding it.

## Words you may see in error messages

<a id="waired-agent"></a>
**Waired agent / daemon**
: The background service. "waired-agent is not running" means it stopped —
  restart it, or run `waired doctor`.

<a id="gateway"></a>
**Gateway**
: The local address on your own computer (`127.0.0.1:...`) that coding agents
  send requests to. It never leaves your machine unless the answer has to come
  from another of your devices.

<a id="worker"></a>
**Worker**
: The computer that actually answers a request — the worker machine. The
  Waired icon shows it as **Worker: `<name>`**, so you can tell which of your
  machines is doing the work.

<a id="coordination-service"></a>
**Coordination service**
: The Waired-run service that tells your devices how to find each other. It
  handles sign-in and device lists only — your prompts and answers never pass
  through it. The [architecture](/concepts/architecture/) page calls the same
  service the **control plane**. See [Privacy](/concepts/privacy/).

<a id="control-plane"></a>
**Control plane**
: Another name for the [coordination service](#coordination-service) — the
  hosted service that signs devices in and distributes the Network Map. The
  two names refer to one thing.

<a id="network-map"></a>
**Network Map**
: The signed list of the devices on your network — their public keys,
  addresses, and relay URLs — that the control plane sends to each device so
  they can find and trust each other.
