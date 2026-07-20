---
title: CLI commands
description: Reference for every waired command — setup, models and inference, coding-agent integration, routing control, and maintenance.
---

The `waired` CLI talks to the local `waired-agent` service. Run
`waired --help`, or `waired <command> --help` (also `-h`) on any command or
subcommand, for grouped, command-specific help.

## Setup & identity

| Command | What it does |
|---|---|
| `waired init` | Enroll this device into your Waired network (Google sign-in). Run once per machine. See [First run](/getting-started/first-run/). Add `--mask-pii` (or `WAIRED_PII_MASK=1`) to mask your home dir, username, hostname and account email in the output — best-effort, for pasting a transcript into a screenshot or bug report. |
| `waired status` | Show daemon + identity status. Add `--observability` for the live engine/mesh snapshot. On a service install the device state is root-owned, so run it with `sudo` (elevated on Windows) to see full status — without elevation it reports that the device is enrolled system-wide and exits 0 instead of guessing. |
| `waired doctor` | Diagnose your setup with ✓/⚠/✗ checks; press `f` to repair anything fixable (`--fix` to repair non-interactively). |
| `waired auth status` | Show the device token state + expiry and suggest re-init if needed. Needs `sudo` on a service install, like `waired status`. |
| `waired logout` | Remove this device's identity + secrets so the next `init` re-enrolls cleanly. |

## Models & inference

| Command | What it does |
|---|---|
| `waired models` | Manage local models: `ls` / `pull` / `rm` / `refresh`. `ls` shows the download inventory; `ls --detail` shows the catalog with recommended specs, hardware fit, and selection criteria (see [Model catalog](/reference/model-catalog/)). `pull` asks for confirmation when the model exceeds this host's recommended spec; pass `--yes` / `-y` to skip that prompt in scripts. |
| `waired runtimes` | Manage inference runtimes: `ls` / `install` / `uninstall` / `refresh` / `status` / `benchmark`. |
| `waired infer "<prompt>"` | Run a one-shot inference request through the Local Gateway. Add `--explain` for an Auto-Selector routing dry-run. |
| `waired inference share <on\|off\|status>` | Toggle whether this engine is offered to mesh peers. See [below](#sharing-vs-pausing). |
| `waired public status` | Show whether this computer is shared publicly with other Waired users, and whether it may use other people's public machines. Add `--json` for the raw objects. |
| `waired public share` / `unshare` | Share this computer publicly so other Waired users can run work on it, or stop. `share --max-clients N` caps how many guests may use it at once; `unshare` cuts off any work others are running on it right now. |
| `waired public use [--auto\|--explicit\|--off] [--min-tier N] [--main on\|off] [--sub on\|off]` | Show or change whether this computer uses other people's public machines. With no flags it just shows the current settings. The first time you enable it, a one-time privacy warning is shown in the terminal that you must read and accept. |
| `waired worker get` / `set` | Choose where outbound inference flows: `set --mode=auto\|local-only\|peer-preferred`, or `set --pin=<peer>`. |
| `waired peers list` | List known mesh peers (DeviceID, IP, engine, GPU, model) — useful for picking a `--pin` target. |

## Coding agents

| Command | What it does |
|---|---|
| `waired link [agent]` | Set up the per-user coding-agent integration (Claude Code skills + OpenCode plugin + gateway token). See [Coding agents](/guides/coding-agents/). |
| `waired unlink [agent]` | Remove the integration (surgical — only undoes what `link` added). |
| `waired codeui open` | Open the bundled OpenCode coding agent in your browser, on your real project, served by your local inference. Runs as you; only you can use it (other users on the machine and the network are blocked). Useful flags: `--project DIR`, `--no-browser` (headless/SSH), `--auth basic`. `status` / `url` / `stop` manage the running instance. |
| `waired claude <enable\|disable\|status>` | Manage the Claude Code managed-settings integration (Linux/macOS/Windows): `enable` points Claude Code at your local inference (also done by `waired init`), installs the `/waired-route` slash command, a footer status line showing the route, and a per-turn fallback notice; `disable` reverts them; `status` shows the current state. No credential is written, so your claude.ai subscription is preserved; serving fails open to the real API when local serving is down. `enable`/`disable` need elevation (`sudo`, or run elevated on Windows). |
| `waired claude route [auto\|waired\|anthropic] [--subagents same\|auto\|waired\|anthropic]` | Show or set where Claude Code runs, live (no restart). The argument sets the **main conversation**; `--subagents` sets subagents independently (default `same` = follow main). `auto` prefers Waired and falls back to the real Anthropic API on failure; `waired` uses your Waired inference only and never contacts Anthropic; `anthropic` always uses the real API with your Claude sign-in. Hybrid: `waired claude route anthropic --subagents waired` runs the main conversation on Anthropic while bulk subagent work stays on Waired (see [Privacy](/concepts/privacy/)). *Which* Waired node serves (this device or a mesh peer) follows `waired worker`. Also available in-session as `/waired-route`. |
| `waired claude statusline [install [--wrap]\|remove]` | Manage the Claude Code footer segment that shows the current route (and, when serving locally, the model that answered the last request). `enable` adds it automatically (asking first if you already have a status line); `install --wrap` wraps an existing one; `remove` reverts. The bare command is what Claude Code runs each turn. |

## Routing control

| Command | What it does |
|---|---|
| `waired pause` | Pause Waired routing — the local gateway stops redirecting Anthropic/OpenAI calls. Persisted across restarts. |
| `waired resume` | Undo `pause` and restore overlay routing. |

## Network & maintenance

| Command | What it does |
|---|---|
| `waired ping <peer>` | Send an overlay ping to a peer via the daemon. |
| `waired keygen` | Generate a WireGuard key pair (`init` normally does this for you). |
| `waired version` | Print the build version (`--json` for `{version, buildSHA, os, arch}`). |
| `waired update` | Check for and apply an update, staying on the host's current channel (`--check` to report only, `--yes` to apply non-interactively, `--edge` / `--stable` to switch channel). See [Install → Update](/getting-started/install/#update). |

## Global flags

These apply to `status` / `ping` / `models` / `runtimes` / `infer`:

- `--mgmt <url>` — Local Management API base URL (default `http://127.0.0.1:9476`).
- `--gateway <url>` — Local Gateway base URL for `waired infer` (default `http://127.0.0.1:9479`, the local no-token gateway; when pointing it at the token-protected `http://127.0.0.1:9473`, `waired infer` attaches the gateway token automatically if it can read it).

## Sharing vs. pausing

These two controls are independent:

- **`waired pause` / `resume`** stops *all* overlay routing — both mesh routing
  and the local inference gateway return 503 while paused. Use it to
  temporarily take the device out of the loop.
- **`waired inference share on` / `off`** controls only whether *other peers*
  can use your engine. With sharing off, you can still run inference yourself
  (`waired infer` works); peers just can't reach your model.

So on a private workstation you might keep sharing **off** but stay unpaused; on
a dedicated GPU box you'd turn sharing **on** so your other devices can use it.
