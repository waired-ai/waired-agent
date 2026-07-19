---
title: Troubleshooting
description: The three commands to run when something looks wrong, a symptom-to-fix table, and fixes for the most common Waired issues.
---

When something looks off, three commands diagnose almost everything. Run them in
order.

| # | Command | What it tells you |
|---|---|---|
| 1 | `waired doctor` | ✓/⚠/✗ for gateway token, paused state, engine readiness, mesh peer, and coding-agent integration. In a terminal, press `f` to repair what's fixable (`--fix` to repair non-interactively). |
| 2 | `waired status --observability` | Live snapshot: engine, share, paused, mesh (enrolled / reachable / ready), and the last inference's decision + latency. Add `-o json` for machine-readable output. |
| 3 | `waired claude status` | Whether the Claude Code managed-settings integration is enabled, and where the `managed-settings.json` file lives. |

:::tip[Always run `waired doctor` first]
Waired is designed so failures are visible rather than silent. `waired doctor`
is the front door — run it before digging into logs.
:::

## Symptom → first command

| Symptom | Start with | What's happening |
|---|---|---|
| Claude Code uses the cloud, not your local model | `waired doctor` → `waired claude status` | If gateway token / paused / engine / mesh peer shows ✗, serving is failing open to Anthropic. Press `f` to rebuild the token + skills. If managed settings aren't enabled, enable them (see below). |
| `waired infer` returns 503 `waired_paused` | `waired resume` | Routing is paused; resume it. |
| `waired infer` returns 503 `waired_inference_disabled` | `waired inference share on` | Sharing is off. (Fine to leave off for local-only use — your own `waired infer` still works.) |
| `Engine: not ready` | `waired runtimes status` + `waired models ls` | The runtime is down or the model is still pulling. |
| A mesh peer doesn't appear / `reachable=false` | `waired status --observability -o json` | Check enrollment, control-plane sync, and WireGuard reachability. |
| The system tray icon doesn't appear (Linux) | `waired doctor` | On GNOME the tray needs an AppIndicator host extension. The `system tray host` line tells you when one is missing (see below). |
| `waired status` says the device is "enrolled system-wide" / needs elevation | `sudo waired status` | On a service install the device state is root-owned; re-run elevated to see the full status (see below). |
| A command says "waired-agent is not running" | `waired doctor` | The local daemon isn't reachable — restart the service (see [Going deeper](#going-deeper-logs)). |

## Common fixes

### `waired status` says the device is enrolled system-wide (needs elevation)

On a service install (the normal `sudo waired init` flow on Linux/macOS, or the
Windows service), the device state lives in a system directory — `/var/lib/waired`
on Linux, `%ProgramData%\waired` on Windows — that only root (SYSTEM +
Administrators on Windows) can read. Running `waired status` or `waired auth
status` as a regular user can't read it, so rather than guess, the command
reports that the device **is enrolled system-wide** and exits 0 — a status query
isn't a failure. Re-run it elevated — `sudo waired status` (Windows: from an
elevated Administrator prompt) — to see the full identity and daemon status.

If you instead see `Not enrolled. Run \`waired init\` to connect this device.`
with exit code 0, the machine really has no enrollment — run `waired init`.

### `waired init` says you've reached the device limit

Each account can enroll a generous number of devices. If `waired init` stops
with a **device limit reached** message, you've hit that cap — almost always
because old machines you no longer use are still enrolled. Open the web admin,
remove a device you no longer need, then run `waired init` again. Re-running
`waired init` on a machine that is **already** enrolled (to re-authenticate)
never counts against the limit.

### Claude Code (or OpenCode) isn't using my model

Run `waired doctor`. If it flags the gateway token or integration, press `f` (or
run `waired link all`) to rebuild it. If `waired claude status` shows the
managed-settings integration isn't enabled, enable it: `sudo waired claude enable`
(Linux/macOS) or, elevated, `waired claude enable` (Windows). See
[Coding agents](/guides/coding-agents/).

`waired claude status` also shows the **last fallback** and its reason when
requests went to the real Anthropic API instead of your model:

- `local_no_model` — no local model is active on this device yet. Check
  `waired status` and pick/pull a model.
- `local_status_<code>` — the local serving path errored with that HTTP status
  just before the fallback; `waired status --observability` has the detail.

### Long Claude Code sessions get compacted (or "prompt is too long")

This is expected and healthy. Local models have a smaller context window than
Claude's hosted models, so Waired tells Claude Code the real window and Claude
Code auto-compacts (summarizes) the conversation to fit — a long session keeps
working instead of silently dropping the start of your context. If you briefly
see a "prompt is too long" notice, Claude Code compacts and retries on its own;
no action is needed. Want the larger window? `/waired-route anthropic` sends the
session to the real Anthropic API instead — the selected model's full window
(1M on a `[1m]` model) applies from your next request, even in the same
session. See [Coding agents](/guides/coding-agents/).

### The waired status line doesn't show up in Claude Code

Run `waired claude status` **inside the project directory**. Claude Code uses a
single status line with strict precedence, and a project-level
`.claude/settings.local.json` / `.claude/settings.json` outranks the user-level
entry Waired installs. If yours is shadowed, the status output names the file
and prints a one-liner you can append to that status-line script to show the
route. Also make sure you restarted the Claude session after
`waired claude enable`.

### Engine stays "not ready"

During sign-in, `waired init` shows the engine come up step by step — "Starting
the inference engine…", "Preparing to download <model>…", then a live
"Downloading <model>: NN%  X.X GB / Y.Y GB (Z MB/s)" bar, then "<model> ready".
A large first model is several GB, so the download step can take a while; that is
normal and the bar keeps moving.

If it instead sits at "Waiting for the inference engine to start…" and then
prints that the engine still isn't up, the local engine didn't come up — run
`waired status` to see the current state, and `waired doctor` (or
`journalctl -u waired-agent -e` on Linux) for details. The model may also still
be downloading — `waired models ls` shows progress. If the runtime itself is
down, `waired runtimes status` shows details. The first load of a model is slow
(a cold CUDA load can take ~60 seconds, and ROCm is similar); the engine retries
automatically and recovers.

### A model won't load on an integrated GPU

Recent Ollama versions disable integrated GPUs (AMD Radeon iGPUs, Intel iGPU)
by default and silently fall back to CPU unless `OLLAMA_IGPU_ENABLE=1` is set.
Waired now recognises integrated AMD GPUs (Radeon 780M/760M and similar) and
Intel iGPUs and starts the engine on the Vulkan backend with that flag set
**automatically** — no manual step is normally needed. `waired doctor` shows the
selected backend. On Linux a mobile-APU iGPU is invisible to the profiler
without `rocm-smi`; Waired still enables the Vulkan path from the CPU model.
Discrete AMD cards use ROCm where Ollama supports them (bundled on Linux, an
installer overlay on Windows) and fall back to Vulkan if ROCm does not engage.
If you run your own Ollama outside Waired's control, set `OLLAMA_IGPU_ENABLE=1`
yourself and restart it. Also confirm the model fits — see the RAM/VRAM columns
in the [model catalog](/reference/model-catalog/).

### "This machine can only run a very small model" — should I enable it?

On a low-RAM machine, `waired init` may report that only a very small model
fits, and ask whether to enable local inference anyway (the default is **No**).
At that size a local coding model is very low quality and often produces broken
output, so it is usually not worth running — decline, and Waired still works as
a secure gateway/relay (it can route to a capable peer on your mesh). Similarly,
if the end-of-init benchmark finds the model is too slow and the only lighter
option is that tiny model, it offers to turn local inference off. You can revisit
either choice later with `waired runtimes benchmark`, or by re-running
`waired init` on better hardware.

Note: the benchmark now measures the model's pure decode rate, excluding
request overhead, so tok/s figures read higher than in earlier releases
(which understated fast machines). The first start after upgrading re-measures
once instead of reusing the old cached number.

### I picked a model bigger than my hardware recommends

Waired lets you run a model that exceeds your host's recommended spec — it
**warns but does not block**. When you pull or switch to such a model you get a
one-time confirmation showing the shortfall (e.g. `needs 32 GB RAM (have 31
GB)`); confirm to proceed. Pass `--yes` to `waired models pull` to skip the
prompt in scripts.

Inference itself is never blocked for being over-spec: the model loads and
runs, though it may be slower or — if the weights genuinely do not fit — fail
to load in the engine (a clear engine error rather than a pre-emptive refusal).
Earlier releases returned a `422 hardware_insufficient` at request time even
when the model was only marginally over the recommended memory; that
inference-time block is gone.

The recommended RAM/VRAM figures carry a safety margin, and on unified-memory
machines (Apple Silicon, AMD Strix Halo) fit is judged against the
GPU-addressable pool rather than leftover system RAM. To see the fit for every
model on this host, run `waired models ls --detail` or open the
[model catalog](/reference/model-catalog/).

### The system tray icon doesn't appear (Linux / GNOME)

GNOME has no built-in system tray, so the `waired-tray` icon only renders when an
**AppIndicator host extension** is installed and enabled. `waired init` installs
and enables one automatically when it detects GNOME, and `waired doctor` flags a
`system tray host` warning when none is present. To set it up by hand:

```sh
sudo apt install gnome-shell-extension-appindicator
gnome-extensions enable appindicatorsupport@rgcjonas.gmail.com
```

Then **log out and back in** (required on Wayland) so GNOME loads the extension.
KDE Plasma has a tray host built in and needs nothing; MATE cannot show the icon
at all — use GNOME (with the extension) or KDE to see it.

### Windows: `waired infer` returns 502 (Ollama not installed)

The current installer bundles Ollama by default, but a host installed with
`-SkipOllama` / `WAIRED_NO_OLLAMA=1` (or by an older installer) won't have the
engine. Add it from an elevated prompt with `waired runtimes install ollama`,
or install it directly:

```powershell
iwr -useb https://github.com/waired-ai/waired-agent/releases/latest/download/ollama-windows.ps1 | iex
```

### A peer isn't reachable

Run `waired status --observability -o json` and check each peer's `reachable`
and `last_check`. Then confirm:

- Both devices enrolled under the **same Google account** (same network). Compare
  the `account` / `network` lines from `waired status` on each.
- WireGuard can connect — `waired peers list` shows the peer's endpoint; suspect
  a firewall or NAT if direct UDP can't open. Waired falls back to a relay
  automatically, so connectivity should still work even when the direct path
  doesn't.
- The Network Map is current — if `waired status` shows a stale map, restart the
  agent.

## Going deeper (logs)

Only after `waired doctor`:

- **Linux:** `journalctl -u waired-agent -e`
- **Windows:** `Get-WinEvent -ProviderName waired-agent -LogName Application -MaxEvents 50`
- **Ollama (bundled engine):** the engine log under waired's state dir —
  `…/runtimes/ollama/logs/engine.log` (Linux: `/var/lib/waired/…`, macOS:
  `/Library/Application Support/waired/…`) for repeated 503s during model load.
  If you brought your own Ollama (`--skip-ollama` + reuse), see
  `~/.ollama/logs/server.log` instead.

A `Restart-Service waired-agent` (Windows) or `systemctl restart waired-agent`
(Linux) resolves most transient inconsistencies.
