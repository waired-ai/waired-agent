---
title: Model and engine commands
description: waired infer, models, runtimes, and inference, with what each verb does, the confirmations a script has to answer, and what the output means.
meta:
  audience: Anyone working in a terminal, or on a computer with no screen
  needs: Waired installed
  time: Read the command you need
---

## `waired infer`

Sends one prompt to your model and prints the answer. The fastest way to
prove the whole path works.

```sh
waired infer "say hi"
waired infer "say hi" --explain    # show which computer and model would answer, without asking
waired infer "say hi" --model <model-id>
```

`--explain` names the computer that would answer, the way
[`waired peers list`](/reference/cli/routing/#waired-peers) does, with its
`DEVICE-ID` alongside, so you can pass either to `waired worker set --pin`.
It also lists the computers that were excluded and why, and reports how old
the information behind its figures was, as `map_age_ms`. A public computer is
named only by the pseudonym Waired shows for it.

## `waired models`

```sh
waired models ls                  # what is downloaded, and what is active
waired models ls --detail         # the whole catalog, with what fits this computer
waired models pull <model-id>     # download one
waired models use <model-id>      # make this the model the computer runs
waired models cancel <model-id>   # stop a download that is running
waired models rm <model-id>       # delete one, freeing several GB
waired models refresh             # is there a better pick for this computer?
waired models check-agent         # will this model work with a coding agent?
```

**`ls`** shows what each model weighs on disk under **SIZE**, which is how
you find what `rm` would give you back. The figure comes from the inference
engine, so a model that is downloaded but whose engine is stopped shows `-`.
With `--detail`, every model in the catalog is listed with the memory it
needs, whether it fits this computer, and which one Waired would choose. The
table prints its own legend. Where the symbols cannot be written, they come
out as ASCII (`●` as `*`, `→` as `->`, `◦` as `o`, `↓` as `v`).

**`pull`** waits until the model is ready. A model that runs here but is not
the one Waired would choose takes a confirmation. `--yes` skips that prompt.
A model this computer does not have the memory for asks you to confirm once
more, showing the shortfall, with No as the default. `--yes` alone does not
skip that one. A script that means it passes `--yes --force`. Model IDs come
from the [model catalog](/reference/model-catalog/).

**`use`** sets which model this computer runs. `pull` only fetches weights.
The switch applies without a restart. The model already running keeps
answering until the new one is ready, and when the weights are not on disk
yet, `use` starts that download and says so:

```
waired models use qwen3.5-4b
qwen3.5-4b will run on this computer once it finishes downloading.
The current model keeps answering until then.
```

It returns as soon as the service has accepted the choice. `--wait` polls
until the new model is serving. The confirmations work as they do for `pull`.

**`cancel`** stops a download that is running, and asks nothing first. It
prints the job it stopped, or says `no download in progress for <model>`. A
`pull` that was waiting on that download stops too and exits non-zero. The
part already downloaded stays on disk, so pulling the same model again
resumes. Cancelling does not undo a `use`. If you had chosen that model, it
stays your choice and applies when the weights arrive.

**`rm`** deletes a model's files and confirms first, or takes `--yes`. It
stops a download of that model first if one is running. If another model on
this computer shares the same files, they are kept and only the entry is
removed.

**`refresh`** says whether a better model pick is available for this
computer than the one it runs.

**`check-agent`** asks a question the other commands do not: can a coding
agent drive this model? Coding agents work by asking the model to call tools.
Some models answer well in a chat window and then, given a real tool list,
describe the tool call in prose instead of making it. The check sends a few
real requests through this computer and reports what came back:

```sh
waired models check-agent                  # the model this computer is serving
waired models check-agent <model-id>       # a specific one
waired models check-agent --json out.json  # full result, for a bug report
```

It takes about a minute and needs the model downloaded first. It exits
non-zero when the model is unreliable, so it can gate a script.

## `waired runtimes`

The inference engine that loads and runs models, as opposed to the models
themselves.

```sh
waired runtimes ls
waired runtimes status
waired runtimes install [engine]    # ollama or vllm, auto-picked by hardware
waired runtimes upgrade <engine>    # bring an installed engine to this build's version
waired runtimes uninstall <engine>
waired runtimes refresh             # re-evaluate the engine and model picks
waired runtimes benchmark           # measure this computer's real speed
```

**`benchmark`** measures throughput with the model this computer runs. If a
different model would suit it better, it offers the swap, names both models,
and says which direction it is offering. See
[Change the model](/guides/choose-a-model/#switch-models).

**`upgrade`** is what `waired update` runs for you. It changes an engine this
computer already has, and does nothing on a computer that has none. For
vLLM, `upgrade` is a rebuild rather than a swap. The new environment is built
next to the one in use and takes over only once it is ready, so nothing stops
answering while it runs. An update that moves the vLLM version downloads
about 4 GB, takes 5 to 15 minutes, and needs about 8 GB free while both are
on disk.

## `waired inference`

```sh
waired inference on               # run models on this computer
waired inference off
waired inference status

waired inference engine start     # start the inference engine
waired inference engine stop      # stop it and free the memory it is holding
waired inference engine status

waired inference memory status    # the memory figure model choices are based on
waired inference memory remeasure # take that figure again

waired inference unload           # free the model's memory, keep answering
waired inference residency        # keep-alive: how long the model stays loaded
waired inference residency 30m    # change it; "always" keeps it loaded
```

**`on` and `off`** decide whether this computer runs models at all. Turning
it on downloads the chosen model if it is not here yet, so the first `on` can
take a while. Turning it off leaves everything on disk and stops answering
locally. On a computer with no inference engine, `on` says so and offers to
run `waired init`, which is what installs one. The setting survives
restarts, and it works even when the background service is not answering.
`status` names the reason when Waired is the one that decided. See
[Local inference started off and I did not choose that](/troubleshooting/setup/#local-inference-started-off-and-i-did-not-choose-that).

**`unload` and `engine stop`** both give memory back, and they are not the
same thing. `unload` frees the model and leaves the engine running, so this
computer keeps answering and the next request loads the model again.
`engine stop` stops the engine itself, so nothing is answered here until you
start it again. See [Pause or stop Waired](/guides/pause/).

**`residency`** is the keep-alive: how long the model stays in memory after
the last request. The default is `always`, because reloading a model costs
the next request anywhere from about 17 seconds to about a minute. With no
argument it prints the setting in force:

```text
Keep-alive: always (the model stays loaded).
```

With a duration such as `30m` or `8h` it sets one. `always` or `0` returns to
keeping the model loaded. If a model is in memory when you change the
setting, the change reaches it straight away. The same setting is
`idle_timeout` in `agent.json`, `WAIRED_INFERENCE_IDLE_TIMEOUT`, and
`--inference-idle-timeout`, and **Keep-alive** in the Waired app.

On a computer whose engine keeps the model for exactly as long as it runs,
there is no timer to set, and `residency` and `unload` say so:

```text
The inference engine on this computer holds the model for as long as the engine runs,
so there is no idle timeout to set here.
To free the memory, stop the engine: `waired inference engine stop`
```

**`memory status`** shows how much memory was free the last time Waired
looked, and when. That figure is what every "does this model fit" decision
on this computer is based on. Waired looks each time the background service
starts, before it loads anything, and keeps the largest figure it has seen.
**`memory remeasure`** takes the measurement again and makes it the one in
force, whether larger or smaller. It refuses while an engine is loaded,
because that engine's memory would be counted against the computer. Stop the
engine first with `waired inference engine stop`.
