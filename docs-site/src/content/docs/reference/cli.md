---
title: CLI commands
description: Every waired command, grouped by what you are trying to do, with the flags that apply everywhere and the pages that describe each group.
meta:
  audience: Anyone working in a terminal, or on a computer with no screen
  needs: Waired installed
  time: Find the command, then open its page
---

Everything on these pages can also be done from
[the Waired app](/guides/waired-app/), except where noted. Run
`waired <command> --help` for the full flag list of any command. These pages
cover what the flags are for.

## Commands by group

### [Setup and sign-in commands](/reference/cli/setup/)

| Command | What it does |
|---|---|
| [`waired init`](/reference/cli/setup/#waired-init) | Sign this computer in and set it up |
| [`waired status`](/reference/cli/setup/#waired-status) | Is everything working? |
| [`waired doctor`](/reference/cli/setup/#waired-doctor) | Check every part, and repair most of it |
| [`waired auth status`](/reference/cli/setup/#waired-auth-status) | When does this computer's sign-in expire? |
| [`waired logout`](/reference/cli/setup/#waired-logout) | Remove this computer's identity |

### [Model and engine commands](/reference/cli/models/)

| Command | What it does |
|---|---|
| [`waired infer`](/reference/cli/models/#waired-infer) | Send one request to your model, right now |
| [`waired models`](/reference/cli/models/#waired-models) | What is downloaded, download more, choose which one runs, stop a download, delete some |
| [`waired runtimes`](/reference/cli/models/#waired-runtimes) | The inference engine itself, and a benchmark |
| [`waired inference`](/reference/cli/models/#waired-inference) | Run models here or not, start or stop the engine, keep-alive, memory |

### [Routing and sharing commands](/reference/cli/routing/)

| Command | What it does |
|---|---|
| [`waired share`](/reference/cli/routing/#waired-share) | Whether this computer is lent out at all |
| [`waired worker`](/reference/cli/routing/#waired-worker) | Which computer answers your requests |
| [`waired peers`](/reference/cli/routing/#waired-peers) and [`waired ping`](/reference/cli/routing/#waired-ping) | Your other computers |
| [`waired public`](/reference/cli/routing/#waired-public) | Lend and borrow spare computers with other Waired users |
| [`waired pause`](/reference/cli/routing/#waired-pause-and-resume) and [`waired resume`](/reference/cli/routing/#waired-pause-and-resume) | Stop and restart routing |

### [Coding tool commands](/reference/cli/coding-tools/)

| Command | What it does |
|---|---|
| [`waired link`](/reference/cli/coding-tools/#waired-link-and-unlink) and [`waired unlink`](/reference/cli/coding-tools/#waired-link-and-unlink) | Connect your coding tools |
| [`waired claude`](/reference/cli/coding-tools/#waired-claude) | Point Claude Code at Waired, the status line, and where subagents run |

### [Maintenance commands](/reference/cli/maintenance/)

| Command | What it does |
|---|---|
| [`waired update`](/reference/cli/maintenance/#waired-update) | Install a newer Waired |
| [`waired config`](/reference/cli/maintenance/#waired-config) | Turn detailed logging on or off |
| [`waired logs`](/reference/cli/maintenance/#waired-logs) | Save recent logs to a file for a bug report |
| [`waired version`](/reference/cli/maintenance/#waired-version) | Which build is this? |
| [`waired keygen`](/reference/cli/maintenance/#waired-keygen) | Generate a key pair by hand |

## Flags that apply nearly everywhere

| Flag | Meaning |
|---|---|
| `--mgmt <url>` | Where the background service is listening. The default is `http://127.0.0.1:9476`. |
| `--gateway <url>` | Where your model answers, for `waired infer`. The default is `http://127.0.0.1:9473`. |
| `--state-dir <dir>` | Where Waired keeps identity and secrets. Also settable as `WAIRED_STATE_DIR`. |

A command group typed without a verb, for example `waired models` on its own,
prints its help and exits with an error, so a script cannot mistake it for
having done something.

<a id="sharing-vs-pausing"></a>

## Three controls people mix up

- **`pause` and `resume`** stop everything on this computer. Routing and
  local inference both stop answering. Use it to take the computer out of
  the loop.
- **`inference on` and `off`** decide whether this computer runs models at
  all. Off, it still uses the models on your other computers.
- **`share on` and `off`** control only whether anyone else, your other
  computers or public guests, can use this computer's model. With sharing
  off, `waired infer` still works here.

On a private workstation you might keep sharing off and stay unpaused. On a
dedicated GPU computer you would turn sharing on so your laptop can use it.
See [Pause or stop Waired](/guides/pause/).
