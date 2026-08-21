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
| [`waired models`](#waired-models) | What is downloaded, download more, choose which one runs, stop a download, delete some |
| [`waired runtimes`](#waired-runtimes) | The AI software itself, and a speed test |
| [`waired inference`](#waired-inference) | Run AI models here or not; start / stop the engine; share it with your other computers |
| [`waired worker`](#waired-worker) | Which computer answers your requests |
| [`waired peers`](#waired-peers) / [`ping`](#waired-ping) | Your other computers |
| [`waired public`](#waired-public) | Lend and borrow spare computers with other Waired users |
| [`waired link`](#waired-link--unlink) / [`unlink`](#waired-link--unlink) | Connect your coding tools |
| [`waired claude`](#waired-claude) | Where Claude Code runs, and switching it live |
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
| `--skip-claude-route` | Finish setup but leave Claude Code talking to the Anthropic API. Skills and plugins still install; turn routing on later with `waired claude enable`. |
| `--skip-integration` | Skip the coding-tool setup entirely (no Claude Code or OpenClaw changes). |
| `--device-name <name>` | Report a name of your choosing instead of this computer's hostname. Used when the computer first joins; renaming afterwards is done in the [web console](/guides/web-console/), and re-running `waired init` no longer overwrites that. |
| `--control <URL>` | Sign in against a specific control plane instead of the default. See [Advanced install options](/reference/install-options/). |
| `--auth-key <key>` | Sign in with an auth key instead of a browser, for servers and containers. Also accepts `file:/path/to/key`, or reads `$WAIRED_AUTH_KEY` when the flag is omitted. Create one under **Settings → Auth keys** in the [web console](/guides/web-console/). See [Sign in and set up](/getting-started/first-run/#servers-and-containers-auth-keys). |
| `--force-reauth` | Sign in again on a computer that is already signed in. Without it, `waired init` picks up where setup left off and leaves the existing sign-in alone — including when you pass `--auth-key`, which is then not used. |

`waired init --help` is the authoritative list; it also carries developer and
CI-only flags not shown here.

Running it again on a computer that is already signed in is safe: it resumes
setup rather than signing in from scratch, so you can run it as many times as
you like. Waired signs in again by itself only when the existing sign-in has
expired beyond repair.

**Exit codes**, for scripts:

| Code | Meaning |
|---|---|
| `0` | Signed in, and local AI is running (or was never asked for). |
| `3` | Signed in, but local AI is not running on this computer — the AI engine could not be installed, or it would not stay up. The sign-in itself is finished; see [Setup says the AI engine failed to start](/troubleshooting/#setup-says-the-ai-engine-failed-to-start). |
| `1` | Setup did not finish — sign-in itself failed. |
| `130` | Interrupted with Ctrl-C. |

`3` is deliberately separate from `1`: the computer really is signed in and on
your network, and re-running sign-in would not change anything about the engine.

Turning engine installs off yourself is **not** one of these. On a computer
where `WAIRED_NO_OLLAMA` is set — which is what `--skip-ollama` /
`-SkipOllama` does — `waired init` skips the engine, says so, and exits `0`.
Nothing went wrong, so nothing is reported as an error.

Nor is a model that has not finished downloading. Setup waits a bounded time
for it; past that it hands the terminal back, says so, and exits `0` while the
background service carries on with the transfer. The computer gets local AI a
few minutes later without you doing anything, so a script must not treat it as
a failed install — `waired status` is what reports the progress.

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

On a computer that runs models, the `Inference:` block reports what the engine
is doing right now:

```
Inference:
  state:          ready
  runtimes:       ollama 0.32.13 (ready, ctx 200k q8_0)
  model loaded:   ollama: qwen3:8b-q4_K_M (kept until unloaded)
  first token:    35.4s, 12 minutes ago (fastest seen here: 2.6s)
  models ready:   qwen3-8b-instruct
```

`model loaded:` says whether the weights are in memory. `first token:` says how
long the last answer took to start, next to the fastest this computer has
started with the same model since Waired last restarted. The pair is the useful
part: a model can be loaded and still re-read your whole prompt, and that is the
difference between the two figures above.

Both are measurements, not verdicts — what counts as a good first token depends
on the model and the machine, so the numbers are shown and the judgement is left
to you. The row is left out when there is nothing measured to show, which is the
normal state on a fresh install and on a computer whose requests are all
answered by another one.

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

Renewing is the same `waired init` you ran the first time — it recognises that
this computer is already signed in, confirms before continuing, and replaces
only the sign-in. Your settings, your AI software and this computer's place in
your network all stay as they are; it stays the same device on your device
list. Waired has to be running in the background for it to work, because the
background service is what holds the sign-in.

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

`--explain` also reports how old the peer information behind its figures was,
as `map_age_ms` — which is how you tell a figure that is wrong from one that is
just out of date.

When another computer answers, `--explain` names it the way
[`waired peers list`](#waired-peers) does — its name, with the `DEVICE-ID`
alongside — so you can take either one straight to `waired worker set --pin`.
A public machine is named only by the pseudonym Waired shows for it.

The reasons say why this computer's own engine was not the answer. That is a
different question from whether its model is ready: pinned to another node,
your own copy can be ready and still not be consulted, because a pin names a
computer rather than a model.

### `waired models`

```sh
waired models ls                  # what is downloaded, and what is active
waired models ls --detail         # the whole catalog, with what fits this computer
waired models pull <model-id>     # download one
waired models use <model-id>      # make this the model the computer runs
waired models cancel <model-id>   # stop a download that is running
waired models rm <model-id>       # delete one, freeing several GB
waired models refresh             # is there a better pick for this machine?
waired models check-agent         # will this model work with a coding agent?
```

`ls` shows what each model weighs on disk under **SIZE**, which is how you
find what `rm` would give you back. The figure comes from the AI engine, so a
model that is downloaded but whose engine is stopped shows `-` — unknown, not
zero.

`pull` waits until the model is ready. A model that runs here but is not the one
Waired would choose takes a confirmation — `--yes` skips that prompt in a
script. A model this computer does not have the memory for asks you to confirm
once more, showing the shortfall, with No as the default — loading it is
expected to fail after the download completes. `--yes` alone does not skip that
one; a script that really means it passes `--yes --force`. `rm` also confirms
first. Model IDs come from the [model catalog](/reference/model-catalog/).

`use` sets which model this computer actually runs — the one that answers.
`pull` only fetches weights; a model can be downloaded without being the one
in service. The switch applies without restarting: the model already running
keeps answering until the new one is ready, and when the weights are not on
disk yet `use` starts that download and says so.

```
waired models use qwen3.5-4b
qwen3.5-4b will run on this computer once it finishes downloading.
The current model keeps answering until then.
```

It returns as soon as the daemon has accepted the choice; `--wait` polls until
the new model is actually serving, for a script that needs the switch finished
before it goes on. The over-spec and does-not-fit confirmations work exactly as
they do for `pull`, including `--yes` and `--yes --force`.

`cancel` stops a download that is running — the way out of a multi-gigabyte
pull started by mistake. It asks nothing first: you are stopping something this
computer is in the middle of fetching, which is what you just said you did not
want. It prints the job it stopped:

```
cancelled download: model=qwen3.5-9b job=job_a761d6a4ca1a
```

and, when nothing was downloading, says so and stops there:

```
no download in progress for qwen3.5-9b
```

The part already downloaded stays on disk, so pulling the same model again
resumes rather than starting over. Reclaiming it means deleting the model with
`rm` after it finishes downloading — a download that never finished leaves data
`rm` cannot name yet.

You do not have to cancel before removing: `rm` stops a download of that model
first and tells you it did.

`check-agent` asks a question the other commands do not: not "does this model
fit" and not "is it fast enough", but "can a coding agent actually drive it?"

Coding agents work by asking the model to call tools — read this file, search
for that. Some models answer beautifully in a chat window and then, given a
real tool list, describe the tool call in prose instead of making it, or ask
for tools that were never offered. When that happens you see the agent print
blocks of raw JSON at you, or announce it is about to do something and then do
nothing. Nothing is broken on your machine; the model simply cannot follow the
format.

The check sends a few real requests through this computer and reports what came
back:

```sh
waired models check-agent                  # the model this computer is serving
waired models check-agent <model-id>       # a specific one
waired models check-agent --json out.json  # full result, for a bug report
```

It takes about a minute and needs the model downloaded first. It exits non-zero
when the model is unreliable, so it can gate a script. If the check cannot run
at all — the model is not downloaded, the service is not running — it says so
separately rather than blaming the model.

### `waired runtimes`

The AI software that loads and runs models, as opposed to the models
themselves.

```sh
waired runtimes ls
waired runtimes status
waired runtimes install [engine]
waired runtimes upgrade <engine>   # bring an installed engine to this build's version
waired runtimes uninstall <engine>
waired runtimes benchmark         # measure this computer's real speed
```

`benchmark` is the interesting one: it measures actual throughput and, if a
different model would suit this machine better, offers the swap, names both
models and says which direction it is offering.

`upgrade` is what `waired update` runs for you, and it is worth knowing the
difference from `install`: it changes an engine this computer already has, and
does nothing at all on a computer that has none.

For vLLM, `upgrade` is a rebuild rather than a swap. The new environment is
built next to the one in use and takes over only once it is ready, so nothing
stops answering while it runs — but an update that moves the vLLM version
downloads about 4 GB and can take 5 to 15 minutes, and it needs about 8 GB free
while both are on disk. The old one is removed afterwards. A computer that has
never installed vLLM is not affected by any of this.

### `waired inference`

```sh
waired inference on               # run AI models on this computer
waired inference off
waired inference status

waired inference engine start     # start the AI engine
waired inference engine stop      # stop it and free the memory it is holding
waired inference engine status

waired inference share on         # let your other computers use this one's AI
waired inference share off
waired inference share status

waired inference memory status    # the memory figure model choices are based on
waired inference memory remeasure # take that figure again

waired inference unload           # free the model's memory, keep answering
waired inference residency        # how long the model stays in memory
waired inference residency 30m    # ...change it ("always" keeps it loaded)
```

`on` / `off` is the whole question of whether this computer runs models at all.
Turning it **on** installs the AI engine and downloads the chosen model if they
are not here yet, so the first `on` can take a while; turning it **off** leaves
everything on disk and stops answering locally. It survives restarts, and it
works even when the background service is not answering — the choice is saved
and applied at the next start.

One kind of machine starts with this **off**: one that measured too slow to
answer a coding question in reasonable time. `status` names the reason when
Waired is the one that decided — see [Local AI started off and I did not choose
that](/troubleshooting/#local-ai-started-off-and-i-did-not-choose-that). A
machine with little memory is not in that group any more; it gets the largest
model it can hold, which may be a very small one — see [Waired chose a very
small model for my
machine](/troubleshooting/#waired-chose-a-very-small-model-for-my-machine).

`unload` and `engine stop` both give memory back, and they are not the same
thing. `unload` frees the model and leaves the engine running, so this computer
keeps answering — the next question loads the model again and takes longer than
usual. `engine stop` stops the engine itself, so nothing is answered here until
you start it again. Reach for `unload` when you want the memory for something
else for a while; reach for `engine stop` when you want this computer out of the
way entirely. `share off` keeps your own use working while closing it to your
other machines. See [Stop using your AI for a while](/guides/pause/).

**Waired keeps the model in memory once it is loaded, and does not drop it
after a period of no questions.** That is deliberate: reloading it costs
anywhere from about 17 seconds to about a minute before the first word of the
answer appears, depending on the machine and the model, and most of that cost
cannot be avoided by loading it again in the background.

`residency` is where you change that. With no argument it prints the setting in
force:

```text
Model stays in memory: always.
```

With a duration it sets one — `waired inference residency 30m`, `8h`, and so
on. `always` (or `0`) returns to keeping the model loaded, which is the default.

If a model is in memory when you change the setting, the change reaches it
straight away and it is not reloaded. If nothing is in memory, Waired restarts
the AI engine so that the next model to load gets the new setting — which costs
nothing, because there is no loaded model to lose. Either way the setting is
saved and survives a restart. The same setting is `idle_timeout` in
`agent.json`, `WAIRED_INFERENCE_IDLE_TIMEOUT`, and `--inference-idle-timeout`;
you can also set it in the Waired app under **Inference → Keep model in
memory**.

On some computers the AI engine keeps the model for exactly as long as it is
running, and there is no timer to set. `residency` and `unload` say so rather
than pretending:

```text
The AI engine on this computer holds the model for as long as the engine runs,
so there is no idle timeout to set here.
To free the memory, stop the engine: `waired inference engine stop`
```

The Waired app leaves out **Keep model in memory** and **Unload model** on such
a computer, for the same reason. `waired inference engine stop` is how you get
the memory back there, and it gives back all of it — the engine's as well as
the model's.

`memory status` shows how much memory was free the last time Waired looked, and
when that was. That figure — not what is free right now — is what every "does
this model fit" decision on this computer is based on. Waired looks each time
the background service starts, before it loads anything, and keeps the largest
figure it has seen: if it happens to look while something big is running, the
low reading is discarded rather than inherited by every later model choice.

`memory remeasure` takes the measurement again and makes it the one in force,
whether it is larger or smaller — the way to bring the figure *down* on a
machine that has permanently less memory to give than it used to. It refuses
while an AI engine is loaded, because that engine's memory would be counted
against the machine — stop it first with `waired inference engine stop`.

### `waired worker`

Where *this* computer's requests go.

```sh
waired worker get
waired worker set --mode=auto            # this computer's AI if it has one, else another (default)
waired worker set --mode=local-only      # never use another computer
waired worker set --mode=peer-preferred  # prefer another computer, fall back to this one
waired worker set --mode=peer-only       # only another computer; fail rather than run here
waired worker set --pin=<peer>           # always this one (implies --mode=pinned)
```

`<peer>` is a computer's name, or the identifier from the `DEVICE-ID` column of
`waired peers list`. Names come from each computer's own hostname; you can
change one in the [web console](/guides/web-console/).

You are choosing a **computer**, not a model. Whichever computer answers, the
answer comes from the model that computer runs — the two do not have to match,
and a laptop with no AI of its own can send every request to the machine that
has one. Name a model with `--model` and you get that model, from whatever
computer has it; pin a computer *and* name a model it does not run, and the
computer you pinned wins, because that is the one you chose. `waired infer
--explain` prints which computer answered and which model it used.

If the computer you pinned is switched off or unreachable, requests fail
instead of quietly going somewhere else — you asked for that machine, so
Waired tells you when it cannot have it.

### `waired peers`

```sh
waired peers list
```

Your other computers, with each one's address, engine, graphics card and model
— which is how you find a name to pass to `worker set --pin`. Two computers
reporting the same name get a number added to the second one, so every name in
the list is unique.

**MODEL** is the model that computer runs. **MODELS** next to it is the same
model under the name its AI software uses, which differs between Ollama and
vLLM.

**WORKER-CAPABLE** is what each computer reports about itself: whether it says
it can answer right now, and when it says it cannot, why — for example
`no (downloading)` while it is still fetching its model, or
`no (engine not answering)` when its own AI software did not respond to it.
These reports reach you over your Waired account, not over the private network
between your computers, so a `yes` is a claim, not something this computer
checked.

`no (stale)` means that computer stopped reporting in. Waired prints how old a
report has to be to count as stale underneath the table, so you do not have to
guess. A computer that is switched off keeps its row until you remove it from
your network — the list is who is *on* your network, not who is awake.

When this computer has not heard back from one of the computers in the list, a
line under the table says so:

```
This computer has had no reply from: office-desktop.
WORKER-CAPABLE is what each computer reports about itself, not something this
computer checked. Run `waired doctor` to measure this computer's connection.
```

If it has heard from none of them, the first line reads `This computer has had
no reply from any computer listed above.` — which usually means the problem is
here rather than out there. The note is a hint and not a verdict: a reply
proves the connection works, but its absence can also be a computer that is
simply switched off. `waired doctor` is the one that measures.

One cause is named directly, because nothing else can work until it is fixed:

```
This computer's key does not match the one your network has for it, so no other
computer can reach it. Run `waired init` to register this device again.
```

`waired worker get` reports the same two things for the computer you pinned:
a `model:` line, and a `status:` line that spells out the reason when it is
not serving.

### `waired ping`

```sh
waired ping <peer>
```

Checks that this computer can actually reach another over the private network.

When the other computer does not answer, the error names it, so a silent
peer reads differently from a problem with Waired on this machine.

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
waired public use --min-model-size small|medium|large   # only machines running a model of at least this size
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
waired link openclaw
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
waired claude route                          # show
waired claude route waired                   # your own AI only
waired claude route anthropic                # the real Anthropic API
waired claude route auto                     # prefer yours, fall back
waired claude route anthropic --sub waired   # split them
waired claude route --main auto              # move only the main conversation
```

The argument sets **all of Claude Code**: the main conversation moves, and
subagents go back to following it. `--main` and `--sub` each set one of them and
leave the other alone, so `--sub` is how you split them and a plain route is how
you stop. Splitting them is genuinely useful — see
[Use it from Claude Code](/guides/claude-code/). In a session, `/waired-route`
does the same thing. *Which* of your machines serves is
[`waired worker`](#waired-worker), not this.

With no argument it prints the current routes and, once Waired has answered a
request, a `last served` line naming the model, whether this device or a peer
answered, and when. A fallback to the real Anthropic API gets its own line.

```sh
waired claude statusline install [--wrap]
waired claude statusline remove
```

Manages the footer line showing the current route and, when your own hardware
answered, the model that did. `enable` installs it already; `--wrap` wraps an
existing status line rather than replacing it — with a PowerShell script on
Windows and a shell script elsewhere, since that is what Claude Code can start
on each. `waired claude disable` restores your own line and removes both.

`waired claude status` reports the status line and the fallback notice as
`installed, but not in the form this computer runs` when they were written for
another operating system's shell — a Windows computer set up by a Waired older
than this one. `sudo waired claude enable` (Windows: from an administrator
prompt) rewrites them.

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
waired update --force      # re-resolve authoritatively (Linux: refreshes the package index; asks for sudo)
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
remembered across restarts. While it is on, Waired also keeps more of the log —
128 MB per file instead of 32 MB, ten older copies either way — so a problem you
only notice days later is still in there. Set it back to `info` when you are
done so the logs stay small. If the service is not running, the choice is saved
and applies the next time it starts.

### `waired logs`

Collects the recent logs into a single file you can attach to a bug report.

```sh
waired logs                          # writes waired-logs-<time>.txt here
waired logs -o report.txt            # choose the file
waired logs --since 30m              # how far back to look (default 1h)
waired logs --mask-pii               # redact home dir / username / hostname / email
waired logs --full                   # every rotated copy, not just the recent 16 MB
```

It gathers the background service's log (from the system log), the service's own
log file where the system keeps one, and the AI engine's log. On macOS that
second part is `/Library/Logs` — plus the app's under `~/Library/Logs`; on
Windows it is `logs\waired-agent.log` under the state folder, which is where
everything below a warning is written. Older, already-rotated copies are
included too, so a problem that started before the last rotation is still in the
report. The files are collected newest-first up to 16 MB in total, so the result
stays small enough to attach to an issue; `--full` takes every rotated copy
instead, which at `debug` verbosity can run to hundreds of megabytes. For the
most useful report, turn on detail first, reproduce the problem, then collect
it:

```sh
waired config log-level debug
# ...reproduce the problem...
waired logs --mask-pii -o report.txt
waired config log-level info
```

`--mask-pii` replaces your home folder, username, machine name and account
email with placeholders — the same masking as `waired init --mask-pii`, and on
by default when `WAIRED_PII_MASK=1`. It is best-effort, so look over the file
before sharing it either way — it can still contain other local paths.

The whole sequence, including what to do when the problem happens during
install and what else to attach, is on [Report a
problem](/getting-started/report-a-problem/).

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
- **`inference on` / `off`** decides whether this computer runs AI models at
  all. Off, it still uses the AI on your other computers.
- **`inference share on` / `off`** controls only whether your *other computers*
  can use this one's AI. With sharing off, `waired infer` still works here.

On a private workstation you might keep sharing **off** and stay unpaused; on a
dedicated GPU box you would turn sharing **on** so your laptop can use it.
