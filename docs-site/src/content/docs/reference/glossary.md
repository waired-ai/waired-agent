---
title: Glossary
description: Plain-language definitions of the Waired-specific and model-specific words used in this documentation.
meta:
  audience: Anyone who met a word they did not know
  needs: Nothing
  time: Look up one term
---

<!-- Every coined or borrowed term gets one plain sentence, ordered by when
     you meet it rather than alphabetically. Terms the product itself surfaces
     each have an entry below; other pages qualify them at first use per
     docs-site/TRANSLATION.md. -->

You do not need to read this page. It is here for the moment a word on
another page stops you.

## Words you meet while setting up

<a id="waired-app"></a>
**Waired app**
: The icon in the menu bar on macOS, and next to the clock on Windows and
  Linux. It is one menu, and it is how you use Waired day to day on a
  computer with a screen. See [Meet the Waired app](/getting-started/meet-the-app/).

<a id="background-service"></a>
**Background service**
: The part of Waired that keeps this computer connected and runs the
  inference engine. It starts with the computer, before anyone logs in. In
  error messages it is called `waired-agent`.

<a id="sign-in"></a>
**Sign in**
: Signing in with Google adds this computer to your private network. Every
  computer signed in with the same Google account can reach the others.

<a id="your-network"></a>
**Your network**
: The private, encrypted connection between your own computers. Nobody else
  can join it, and it is not reachable from the public internet.

<a id="device"></a>
**Device**
: One computer in your network. Your desktop, your laptop, and your work
  machine are three devices.

<a id="administrator-rights"></a>
**Administrator rights**
: Permission to change the whole computer rather than only your own account.
  Waired needs it to install the background service and the inference
  engine. On macOS and Linux this is what `sudo` means. On Windows it is
  the "Do you want to allow this app to make changes?" window.

<a id="auth-key"></a>
**Auth key**
: A password that lets a computer join your network without a browser, for
  servers and containers. You create it in the web console. See
  [Set up a server with an auth key](/getting-started/servers-and-auth-keys/).

<a id="web-console"></a>
**Web console**
: [app.waired.ai](https://app.waired.ai), where you see every computer on
  your network and change the settings that apply across them. The Waired
  app calls it **Admin Console**.

## Models and inference

<a id="model"></a>
**Model**
: A large language model. Its weights are a multi-gigabyte file of learned
  parameters. `qwen3.8-27b` is a model. Larger models give better answers
  and need more memory.

<a id="inference"></a>
**Inference**
: Producing an answer from a model. "Running inference on this computer"
  means the answer is computed here.

<a id="inference-engine"></a>
**Inference engine**
: The program that loads a model into memory and runs it. Waired installs
  and manages [Ollama](https://ollama.com) for you, and vLLM on Linux
  servers with an NVIDIA or AMD GPU.

<a id="memory-vram"></a>
**Memory and VRAM**
: A model has to fit in memory to run. On a computer with a separate GPU,
  that means the GPU's own memory, VRAM. On Apple Silicon and some AMD
  chips, memory is shared between the processor and the graphics side, so
  the whole pool counts.

<a id="toks"></a>
**tok/s (tokens per second)**
: How fast answers come out. A token is roughly three-quarters of a word.

<a id="context-window"></a>
**Context window**
: How much of the conversation the model can consider at once. Local models
  have smaller windows than cloud models, which is why long Claude Code
  sessions get summarized to fit. That is normal.

<a id="llm"></a>
**LLM**
: Large language model, the class of model Waired runs. "Model" on these
  pages always means an LLM.

<a id="weights"></a>
**Weights**
: The learned parameters that make up a model, stored as one or more large
  files. "Downloading the model" downloads its weights. "The model is loaded"
  means the weights are in memory.

<a id="kv-cache"></a>
**KV cache**
: The memory a model uses while it works on a request, one entry per token
  of the conversation so far. It grows with the context window, which is why
  the memory a model needs is more than the size of its weights.

<a id="quantization"></a>
**Quantization and variant**
: A build of a model with its weights stored at lower precision, for example
  4-bit as a `q4` GGUF file, so it needs less memory. The catalog lists each
  model's variants per inference engine. "No Ollama variant" means the
  catalog has no build of that model Ollama can load.

<a id="ttft"></a>
**Time to first token (TTFT)**
: How long a request waits before the first word of the answer appears. It
  covers loading the weights if they are not in memory and reading the
  prompt. `waired status` prints it as `first token:`.

<a id="keep-alive"></a>
**Keep-alive**
: How long the inference engine keeps a model loaded after the last request.
  **Always** never unloads it. A timeout frees the memory, and the next
  request pays to load the weights again.

<a id="unified-memory"></a>
**Unified memory**
: One pool of memory shared by the processor and the GPU, as on Apple
  Silicon and some AMD chips. There is no separate VRAM figure. The whole
  pool is what a model can use.

<a id="benchmark"></a>
**Benchmark**
: The measurement Waired takes of how fast this computer answers with a
  model. It runs at the end of setup and whenever you run
  `waired runtimes benchmark`.

## Words you meet when using it from another computer

<a id="direct-connection"></a>
**Direct connection**
: Your two computers talking to each other straight across the internet,
  encrypted end to end. This is the normal case.

<a id="relay"></a>
**Relay**
: When a firewall or router blocks a direct connection, traffic goes through
  a Waired server instead. It is still encrypted end to end. The relay
  passes along sealed data it cannot read.

<a id="mesh"></a>
**Mesh**
: The private network your devices form with each other. Every device can
  reach every other directly, with no central hub in the middle. Pages here
  say "the Waired mesh" at first mention.

<a id="overlay"></a>
**Overlay network**
: A private network built on top of your existing internet connection.
  Nothing about your router or Wi-Fi changes.

<a id="peer"></a>
**Peer**
: Any other device in your mesh, seen from the one you are on.
  `waired peers list` names them. Pages here say "a peer device" at first
  mention.

<a id="sharing"></a>
**Sharing**
: Whether this computer lends itself out at all, the one switch that lives
  on the computer. Off stops serving everyone at once. Who a sharing
  computer is offered to is set in the web console. See
  [Share a computer with your other devices](/guides/sharing/).

<a id="capacity"></a>
**Capacity**
: How many requests a computer serves at once, set in the web console as
  **Max concurrent requests**. Each parallel slot reserves its own VRAM.

<a id="pausing"></a>
**Pausing**
: Temporarily stopping Waired from handling anything on this computer,
  without uninstalling or changing settings. `waired resume` undoes it.

<a id="public-share"></a>
**Public Share**
: An opt-in feature that offers your model to people outside your own
  account, and lets you use theirs. Off by default, and turned on and off in
  the web console. See [Public Share](/public-share/).

## Words for the coding-tool setup

<a id="coding-agent"></a>
**Coding agent**
: Claude Code, OpenCode, OpenClaw, and similar tools that write and edit code
  for you. When the product's own output says "coding tools", these pages
  quote that.

<a id="routing"></a>
**Routing**
: Which computer answers a given request. Among your own computers,
  `waired worker` decides. Whether a Claude Code turn goes to your computers
  or to Anthropic is the model you pick in `/model`, and only that.

<a id="status-line"></a>
**Status line**
: The segment Waired adds to Claude Code's footer, saying where this
  session's turns run and which model answered the last one. See
  [The Waired status line](/guides/claude-code/status-line/).

<a id="falling-back"></a>
**Falling back**
: When the computer Waired chose for a request cannot serve it after all,
  Waired sends the request to another of your computers and records it. The
  Waired app's **Recent activity** lists them. It never means the cloud. A
  Claude Code turn on a Waired row that none of your computers can answer
  fails and says so.

<a id="notice"></a>
**Notice**
: A message Waired shows about this computer, as a row in the Waired app, in
  `waired status`, and in `waired doctor`. It stays as long as it is true.
  See [Notices](/guides/notices/).

## Words you may see in error messages

<a id="waired-agent"></a>
**Waired agent**
: The background service. "waired-agent is not running" means it stopped.
  Restart it, or run `waired doctor`.

<a id="gateway"></a>
**Gateway**
: The local address on your own computer (`127.0.0.1:…`) that coding tools
  and chat apps send requests to. It never leaves your computer unless the
  answer has to come from another of your devices.

<a id="worker"></a>
**Worker**
: The computer that answers a request. The Waired app shows it as
  **Worker: `<name>`**, so you can tell which of your computers is doing the
  work.

<a id="control-plane"></a>
**Control plane**
: The Waired-run service that tells your devices how to find each other. It
  handles sign-in, device lists, and the Network Map only. Your prompts and
  answers never pass through it. `waired status` prints it as
  `Control Plane:`. See [Privacy: what leaves your computer](/concepts/privacy/).

<a id="coordination-service"></a>
**Coordination service**
: The older name for the [control plane](#control-plane) in earlier versions
  of these pages. The two names refer to one thing.

<a id="network-map"></a>
**Network Map**
: The signed list of the devices on your network, with their public keys,
  addresses, and relay URLs, that the control plane sends to each device so
  they can find and trust each other.
