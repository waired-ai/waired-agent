---
title: CLI commands
description: Every waired command, grouped by what you are trying to do — with the flags that matter and what each one prints.
meta:
  audience: Anyone working in a terminal, or on a machine with no screen
  needs: Waired installed
  time: Skim the index, read the section you need
---

Everything on this page can also be done from
[the Waired app](/guides/waired-app/), except where noted. Run
`waired <command> --help` for the full flag list of any command — this page
covers what the flags are *for*.

## Index

| Command | What it does |
|---|---|
| [`waired init`](#waired-init) | Sign this computer in and set it up |
| [`waired status`](#waired-status) | Is everything working? |
| [`waired doctor`](#waired-doctor) | Check every part, and repair most of it |
| [`waired auth status`](#waired-auth-status) | When does this computer's sign-in expire? |
| [`waired logout`](#waired-logout) | Remove this computer's identity |
| [`waired infer`](#waired-infer) | Ask your AI something, right now |
| [`waired models`](#waired-models) | What is downloaded, download more, delete some |
| [`waired runtimes`](#waired-runtimes) | The AI software itself, and a speed test |
| [`waired inference`](#waired-inference) | Start / stop the engine; share it with your other computers |
| [`waired worker`](#waired-worker) | Which computer answers your requests |
| [`waired peers`](#waired-peers) / [`ping`](#waired-ping) | Your other computers |
| [`waired public`](#waired-public) | Lend and borrow spare computers with other Waired users |
| [`waired link`](#waired-link--unlink) / [`unlink`](#waired-link--unlink) | Connect your coding tools |
| [`waired claude`](#waired-claude) | Where Claude Code runs, and switching it live |
| [`waired codeui`](#waired-codeui) | A coding agent in your browser |
| [`waired pause`](#waired-pause--resume) / [`resume`](#waired-pause--resume) | Stop and restart routing |
| [`waired update`](#waired-update) | Install a newer Waired |
| [`waired config`](#waired-config) | Turn detailed logging on or off |
| [`waired logs`](#waired-logs) | Save recent logs to a file for a bug report |
| [`waired version`](#waired-version) | Which build is this? |
| [`waired keygen`](#waired-keygen) | Generate a key pair by hand |

---

## Setting up and signing in

### `waired init`

Signs this computer in and sets it up. Run once per machine — the installer
normally runs it for you, so you only type it yourself to resume an interrupted
setup or to set up a machine installed with `--no-init`.

```sh
sudo waired init            # macOS, Linux
waired init                 # Windows, from an Administrator terminal
```

It needs administrator rights because it installs the AI software. **While it
is running it is also what performs the steps the browser setup page asks for**
— so leave the window open until setup finishes. See
[Sign in and set up](/getting-started/first-run/).

| Flag | Why you would use it |
|---|---|
| `--mask-pii` | Hides your home folder, username, machine name and account email in the output, for pasting into a bug report. Best-effort. |
| `--non-interactive` | Asks nothing; takes the defaults. For scripted installs. |
| `--no-browser` | Prints the sign-in link instead of opening a browser. For SSH. |
| `--inference-enabled=true\|false` | Answers "run AI models on this computer?" without asking. |
| `--share-with-mesh=true\|false` | Answers "let your other devices use this computer's AI?" without asking. |

### `waired status`

The quick "is it working" check.

```sh
waired status
waired status --observability     # engine, model, and your other computers
waired status --observability -o json
```

On a normal desktop install the state belongs to the system, so run it with
`sudo` (or from an elevated terminal on Windows) to see everything. Without
elevation it reports that the device is enrolled system-wide and stops there,
rather than guessing.

### `waired doctor`

Checks every part of the setup, prints ✓ / ⚠ / ✗ per check, and offers to
repair what it can when you press **f**. Full page:
[Run a health check](/getting-started/doctor/).

```sh
waired doctor
waired doctor --fix              # repair without asking (scripts, SSH)
```

### `waired auth status`

Shows the sign-in state and when it expires, and tells you to re-run `init` if
it needs renewing. Needs elevation on a service install, like `status`.

### `waired logout`

Removes this computer's identity and secrets, so the next `waired init` enrolls
it cleanly as a new device. This is not a temporary measure — to stop using
Waired for a while, see [`pause`](#waired-pause--resume).

---

## Models and inference

### `waired infer`

Sends one prompt to your AI and prints the answer. The fastest way to prove the
whole path works.

```sh
waired infer "say hi"
waired infer "say hi" --explain    # show which machine and model would answer, without asking
```

### `waired models`

```sh
waired models ls                  # what is downloaded, and what is active
waired models ls --detail         # the whole catalog, with what fits this computer
waired models pull <model-id>     # download one
waired models rm <model-id>       # delete one, freeing several GB
waired models refresh             # is there a better pick for this machine?
```

`pull` waits until the model is ready and asks for confirmation if the model is
bigger than this computer is rated for — `--yes` skips that prompt in a script.
`rm` also confirms first. Model IDs come from the
[model catalog](/reference/model-catalog/).

### `waired runtimes`

The AI software that loads and runs models, as opposed to the models
themselves.

```sh
waired runtimes ls
waired runtimes status
waired runtimes install [engine]
waired runtimes uninstall <engine>
waired runtimes benchmark         # measure this computer's real speed
```

`benchmark` is the interesting one: it measures actual throughput and, if a
different model would suit this machine better, offers the swap and names both
models with their quality tier so you can weigh speed against quality.

### `waired inference`

```sh
waired inference engine start     # load the model
waired inference engine stop      # free the memory it is holding
waired inference engine status

waired inference share on         # let your other computers use this one's AI
waired inference share off
waired inference share status
```

`engine stop` is the memory-pressure escape hatch; `share off` keeps your own
use working while closing it to your other machines. See
[Stop using your AI for a while](/guides/pause/).

### `waired worker`

Where *this* computer's requests go.

```sh
waired worker get
waired worker set --mode=auto            # this computer's AI if it has one, else another (default)
waired worker set --mode=local-only      # never use another computer
waired worker set --mode=peer-preferred  # prefer another computer
waired worker set --pin=<peer>           # always this one (implies --mode=pinned)
```

### `waired peers`

```sh
waired peers list
```

Your other computers, with each one's address, engine, graphics card and model
— which is how you find a name to pass to `worker set --pin`.

### `waired ping`

```sh
waired ping <peer>
```

Checks that this computer can actually reach another over the private network.

### `waired public`

Lending your spare capacity to other Waired users, and borrowing theirs. Off
unless you turn it on. **Read [Public share](/public-share/) first** — the
owner of a public computer can read what you send it.

```sh
waired public status
waired public share --max-clients N    # offer this computer
waired public unshare                  # stop, cutting off work running now
waired public use                      # show your current settings
waired public use --auto               # use others' machines when they beat your own
waired public use --explicit           # only when you specifically ask
waired public use --off
waired public use --min-tier N         # only machines at or above this quality tier
waired public use --main on|off --sub on|off
```

The first time you enable `use`, a one-time privacy warning appears in the
terminal that you have to read and accept.

---

## Coding tools

### `waired link` / `unlink`

```sh
waired link                  # set up every coding tool found
waired link claude-code
waired link opencode
waired link openclaw
waired unlink <agent>
```

`link` also creates the key your other tools need — see
[Use it from a chat app](/guides/chat-clients/). `unlink` is surgical: it
undoes only what `link` added.

### `waired claude`

```sh
waired claude status
sudo waired claude enable     # point Claude Code at your AI (init does this too)
sudo waired claude disable
```

`enable` / `disable` need administrator rights. No credential is written, so
your claude.ai subscription is unaffected.

Switching where it runs, live and without a restart:

```sh
waired claude route                                # show
waired claude route waired                         # your own AI only
waired claude route anthropic                      # the real Anthropic API
waired claude route auto                           # prefer yours, fall back
waired claude route anthropic --subagents waired   # split them
```

The argument sets the **main conversation**; `--subagents` sets subagents
independently. Splitting them is genuinely useful — see
[Use it from Claude Code](/guides/claude-code/). In a session, `/waired-route`
does the same thing. *Which* of your machines serves is
[`waired worker`](#waired-worker), not this.

```sh
waired claude statusline install [--wrap]
waired claude statusline remove
```

Manages the footer line showing the current route and, when your own hardware
answered, the model that did. `enable` installs it already; `--wrap` wraps an
existing status line rather than replacing it.

### `waired codeui`

A coding agent in your browser, on your real project, answered by your AI.
Nothing to install.

```sh
waired codeui open
waired codeui open --project DIR
waired codeui open --no-browser     # print the address instead (SSH)
waired codeui url
waired codeui status
waired codeui stop
```

It runs as you, and only you can use it — other users on the machine and other
computers on the network are refused.

---

## Routing, updates, and the rest

### `waired pause` / `resume`

```sh
waired pause
waired resume
```

Pausing stops **all** routing: your tools go back to the cloud, and your own AI
stops answering too. It survives restarts. See
[Stop using your AI for a while](/guides/pause/) for the four different things
"turn it off" can mean.

### `waired update`

```sh
waired update              # check and apply, staying on the current channel
waired update --check      # report only
waired update --yes        # apply without the installer's confirmation
waired update --edge       # switch to the latest main build
waired update --stable     # switch back to stable
waired update --force      # ignore the cached check result
waired update --notify on|off   # the app's pop-up update prompt
```

See [Update Waired](/getting-started/update/). `--notify off` silences the
pop-up; the update entry in the Waired app stays either way.

### `waired config`

Change persisted agent settings. Today that means the **log detail level**.

```sh
waired config log-level              # show the current level
waired config log-level debug        # turn on detailed logs
waired config log-level info         # back to normal
```

The levels are `debug`, `info` (the default), `warn` and `error`. `debug` is
the switch to flip before reproducing a problem: it takes effect immediately —
**no restart** — on both the background service and the Waired app, and is
remembered across restarts. Set it back to `info` when you are done so the logs
stay small. If the service is not running, the choice is saved and applies the
next time it starts.

### `waired logs`

Collects the recent logs into a single file you can attach to a bug report.

```sh
waired logs                          # writes waired-logs-<time>.txt here
waired logs -o report.txt            # choose the file
waired logs --since 30m              # how far back to look (default 1h)
```

It gathers the background service's log (from the system log) and the AI
engine's log. For the most useful report, turn on detail first, reproduce the
problem, then collect it:

```sh
waired config log-level debug
# ...reproduce the problem...
waired logs -o report.txt
waired config log-level info
```

Look over the file before sharing it — it can contain local file paths or your
username.

### `waired version`

```sh
waired version
waired version --json      # {version, buildSHA, os, arch}
```

### `waired keygen`

Generates a WireGuard key pair. `init` does this for you — you would only run
it by hand when building something unusual.

---

## Flags that apply nearly everywhere

| Flag | Meaning |
|---|---|
| `--mgmt <url>` | Where the background service is listening (default `http://127.0.0.1:9476`). |
| `--gateway <url>` | Where your AI answers, for `waired infer` (default `http://127.0.0.1:9479`, the loopback address that needs no key). |
| `--state-dir <dir>` | Where Waired keeps identity and secrets. Also settable as `WAIRED_STATE_DIR`. |

<a id="sharing-vs-pausing"></a>

## Two controls people mix up

- **`pause` / `resume`** stops *everything* — mesh routing and your own local
  AI both stop answering. Use it to take the computer out of the loop.
- **`inference share on` / `off`** controls only whether your *other computers*
  can use this one's AI. With sharing off, `waired infer` still works here.

On a private workstation you might keep sharing **off** and stay unpaused; on a
dedicated GPU box you would turn sharing **on** so your laptop can use it.
