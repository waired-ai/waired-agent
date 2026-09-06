---
title: Setup and sign-in commands
description: waired init, status, doctor, auth, and logout, with the flags that matter, the exit codes, and what each one prints.
meta:
  audience: Anyone working in a terminal, or on a computer with no screen
  needs: Waired installed
  time: Read the command you need
---

## `waired init`

Signs this computer in and sets it up. The installer normally runs it for
you, so you type it yourself to resume an interrupted setup, to set up a
computer installed with `--no-init`, or to sign in again.

```sh
sudo waired init            # macOS, Linux
waired init                 # Windows, from an administrator terminal
```

It needs administrator rights because it installs the inference engine.
While it is running, it is also what performs the steps the browser setup
page asks for, so leave the window open until setup finishes. See
[Sign in](/getting-started/sign-in/) and
[Set up in the terminal](/getting-started/set-up-in-the-terminal/).

| Flag | What it is for |
|---|---|
| `--mask-pii` | Hides your home folder, username, machine name, and account email in the output, for pasting into a bug report. Best-effort. |
| `--non-interactive` | Asks nothing and takes the defaults. For scripted installs. |
| `--no-browser` | Prints the sign-in link and a pairing code instead of opening a browser. For SSH. |
| `--inference-enabled=true` or `=false` | Answers "run models on this computer?" without asking. |
| `--inference-bundled-model-id <id>` | Pins a model instead of choosing from the list. |
| `--skip-claude-route` | Finishes setup but leaves Claude Code talking to the Anthropic API. Skills and plugins still install. Turn routing on later with `waired claude enable`. |
| `--skip-integration` | Skips the coding-tool setup entirely. |
| `--device-name <name>` | Reports a name of your choosing instead of this computer's hostname. Used when the computer first joins. Renaming afterwards is done in the [web console](/guides/web-console/). |
| `--control <URL>` | Signs in against a specific control plane. See [Advanced install options](/reference/install-options/). |
| `--auth-key <key>` | Signs in with an auth key instead of a browser, for servers and containers. Also accepts `file:/path/to/key`, or reads `$WAIRED_AUTH_KEY` when the flag is omitted. See [Set up a server with an auth key](/getting-started/servers-and-auth-keys/). |
| `--force-reauth` | Signs in again on a computer that is already signed in. Without it, `waired init` resumes setup and leaves the existing sign-in alone, including when you pass `--auth-key`. |

`waired init --help` is the authoritative list. It also carries developer
and CI-only flags not shown here.

Running it again on a computer that is already signed in is safe. It resumes
setup rather than signing in from scratch. See
[Run setup again](/getting-started/set-up-again/).

**Exit codes**, for scripts:

| Code | Meaning |
|---|---|
| `0` | Signed in, and local inference is running, or was never asked for. |
| `3` | Signed in, but local inference is not running on this computer: the inference engine could not be installed, or it would not stay up. See [Setup says the inference engine failed to start](/troubleshooting/setup/#setup-says-the-inference-engine-failed-to-start). |
| `1` | Setup did not finish. Sign-in itself failed. |
| `130` | Interrupted with Ctrl-C. |

`3` is separate from `1` on purpose. The computer is signed in and on your
network, and running sign-in again would not change anything about the
engine. Two states that are not errors exit `0`: an engine install you turned
off yourself with `WAIRED_NO_OLLAMA`, and a model that has not finished
downloading when setup hands the terminal back. `waired status` reports the
download's progress.

## `waired status`

The quick "is it working" check.

```sh
waired status
waired status --observability     # engine, model, and your other computers
waired status --observability -o json
```

On a normal desktop install the state belongs to the system, so run it with
`sudo`, or from an administrator terminal on Windows, to see everything.
Without administrator rights it reports that the computer is signed in system-wide and
stops there.

On a computer that runs models, the `Inference:` block reports what the
engine is doing right now:

```
Inference:
  state:          ready
  runtimes:       ollama 0.33.3 (ready, ctx 200k q8_0)
  model loaded:   ollama: qwen3:8b-q4_K_M (kept until unloaded)
  first token:    35.4s, 12 minutes ago (fastest seen here: 2.6s)
  models ready:   qwen3-8b-instruct
```

`model loaded:` says whether the weights are in memory. `first token:` says
how long the last answer took to start, next to the fastest this computer
has started with the same model since Waired last restarted. A model can be
loaded and still reread your whole prompt, and that is the difference between
the two figures. The row is left out when there is nothing measured to show.

Below that, `Notices:` is anything Waired has to say about this computer:

```
Notices:
  ⚠ Lighter model recommended — switch to qwen3-8b-instruct
    This computer answers at 42 tok/s with qwen3-30b-a3b, below the 60 tok/s floor.
  ⬆ Update available — install v0.9.3
    This computer runs v0.9.1.
```

The block is left out when there is nothing to say. For every notice, see
[Notices](/guides/notices/).

## `waired doctor`

Checks every part of the setup, prints ✓, ⚠, or ✗ per check, and offers to
repair what it can when you press `f`. For the full page, see
[Run a health check](/getting-started/doctor/).

```sh
waired doctor
waired doctor --fix              # repair without asking, for scripts and SSH
```

Anything Waired has to tell you about this computer is among those rows, as
a ⚠. They never change the exit code.

## `waired auth status`

Shows the sign-in state and when it expires, and tells you to run `init`
again if it needs renewing. Needs elevation on a service install, like
`status`.

Renewing is the same `waired init` you ran the first time. It recognizes that
this computer is already signed in, confirms before continuing, and replaces
only the sign-in. Your settings, your inference engine, and this computer's
place in your network stay as they are. Waired has to be running in the
background for it to work, because the background service is what holds the
sign-in.

## `waired logout`

Removes this computer's identity and secrets, so the next `waired init`
enrolls it as a new device. This is not a temporary measure. To stop using
Waired for a while, see [Pause or stop Waired](/guides/pause/).

When the background service is running it performs the sign-out, so it stops
serving the old sign-in immediately rather than carrying on until its access
token expires. When nothing is running — during an uninstall, say — the command
does the same work itself.
