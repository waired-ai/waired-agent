---
title: Setup problems
description: Setup stopped, the engine failed to start, the model could not be downloaded, or the computer ended up with a small model or no model at all.
meta:
  audience: Anyone whose setup did not end the way they expected
  needs: A terminal on the computer in question
  time: Each fix takes a minute or two
---

Run `waired doctor` first. It names the part that is not ready. Then find your
symptom below.

## Setup stopped partway

The setup page names what happened. Each message means something specific:

| The page says | Meaning | What to do |
|---|---|---|
| “The setup command on … was closed before this finished. Your progress was saved.” | The terminal running setup was closed. Some steps need administrator rights, and only that terminal has them. | Run `sudo waired init` again (Windows: `waired init` from an administrator terminal). It resumes, and nothing is lost. |
| “Setup has not been run on … yet, so its coding tools are not connected.” | Nobody has run the setup command on that computer. A web page cannot write into your home folder or change a machine-wide setting. | Run `sudo waired init` there. Everything else on that computer can be set up from the browser. This one step cannot. |
| “Setup has not been run on … yet, so its inference engine is not installed.” | The same thing, one step earlier. Installing the engine needs administrator rights that only the setup command has. | Run `sudo waired init` there. Nothing was interrupted. This is a first run that has not happened yet. |
| “Setup on … needs administrator access to continue.” | Setup was started without administrator rights. | Start it again from an administrator terminal. See [Sign in](/getting-started/sign-in/). |
| “… has run out of disk space.” | The model did not fit on disk. | Free some space, or pick a smaller model from the [catalog](/reference/model-catalog/). |
| “… could not finish downloading. Check its internet connection.” | The download failed for a network reason: a name that would not resolve, a connection that dropped, a certificate that would not verify. | Retry. Downloads resume rather than start over. |
| “The inference engine on … is an older version than this model needs.” | The model needs a newer engine than this computer has. | Run `waired update` on that computer, or pick another model. |
| “This took too long on … and was stopped.” | A step exceeded its time limit. | Retry. Twice on the same step usually means this computer is too slow for that model. |
| “Something went wrong on ….” | Waired could not put a name to what happened. | Retry. If it keeps happening, run `waired doctor` on that computer, or read the logs. See [Reading the logs](/troubleshooting/other-computers/#reading-the-logs). |

If the coding-tools step is the one that failed, `waired link --force all` on
that computer both repairs it and clears the row. You do not have to run
setup again.

The model download is the exception to all of the above. It keeps running
even if you close the browser tab. Reopen the computer's page at
[app.waired.ai](https://app.waired.ai) to see where it got to.

If you are watching from the terminal, `waired init` and `waired models pull`
print the reason on the line that reports the failure:

```text
qwen3-8b-instruct: failed — no space left on device
```

## Setup says the inference engine failed to start

In the terminal, setup stops waiting for the model and tells you the engine
is what went wrong:

```
The inference engine failed to start, so qwen3.5-4b can't download.
ollama: process exited during startup: signal: killed
Run `waired doctor` for details. `waired status` shows the current state.
```

The second line is the engine's own account of what happened, printed as it
was recorded, often with the last lines of the engine's log after it. Read
that part first.

Sign-in is finished at this point. The computer is on your network and
everything except local inference works. Waired keeps trying in the
background, so the download can still start on its own once the engine runs.
`waired init` finishes with exit code `3` for this, so a script can tell it
apart from a sign-in that did not happen:

| Exit code | Meaning |
|---|---|
| `0` | Signed in, and local inference is running, or was never asked for. |
| `3` | Signed in, but local inference is not running on this computer. |
| `1` | Setup did not finish. Sign-in itself failed. |
| `130` | You interrupted it with Ctrl-C. |

Common causes:

- **Another Ollama is already using the port.** `waired runtimes status`
  names the version it found. Quit it, or set `inference.ollama_port` in
  `agent.json` to a free port.
- **Something that is not an Ollama is using that port.** Waired cannot adopt
  it, and `waired status` names the address:

  ```
  ⚠ ollama: another program is already listening on 127.0.0.1:9475, the port the
    inference engine was told to use — set inference.ollama_port in agent.json to
    a free port
  ```

  Quit whatever holds it, or set `inference.ollama_port` to a free port and
  restart the service. The vLLM engine says the same for its own port,
  `inference.vllm_port`.
- **The engine keeps crashing.** After a few crashes Waired stops restarting
  it and says so. `waired status` and `waired runtimes ls` say **gave up** in
  place of the engine's state:

  ```
  runtimes:       ollama 0.33.3 (gave up, ctx 32k q8_0)
  ⚠ ollama: engine repeatedly crashed; not retrying — …
  ```

  Deal with the cause, then run `waired inference engine start`.
- **The vLLM engine never started at all.** It needs its own setup finished:
  the Python environment built with `waired runtimes install vllm`, and a
  model chosen that ships a version that engine can serve. When one of those
  is missing, Waired names which piece is missing.

`waired doctor` checks all of these in one pass, and
`sudo waired doctor --fix` asks the background service to start the engine
and prints the reason it is not running.

## Setup says it cannot download the model you chose

Some models do not run on some computers. When the background service turns
the choice down, the terminal says so straight away:

```
Waired can't download qwen3.6-35b-a3b on this computer.
the engine on this device is too old for this model
Update Waired with `waired update`, or pick a different model in the Waired console.
```

The middle line is the reason as the background service recorded it. The last
line depends on that reason:

- **The inference engine is older than the model needs.** Run
  `waired update` on that computer. The download starts on its own
  afterwards. This is the only reason an update fixes.
- **Anything else.** That computer cannot serve the model at all, or
  downloads are turned off on it. Pick another model in the browser, or run
  `waired models ls --detail` to see which ones fit.

Sign-in is finished either way, and the setup page shows the same reason on
the model row, so you can pick again there.

A similar-looking line means something different:

```
Waired hasn't started downloading qwen3.6-35b-a3b yet. It keeps trying in the background.
```

That one is not a refusal. The download had not begun by the time the
terminal stopped watching, and it carries on in the background.
`waired status` shows where it got to.

## Setup said it could not complete a test generation

At the end of setup, Waired asks the model a short question to check the
speed of this computer. This message means no answer came back, so setup
could not measure anything.

Almost always the inference engine itself stopped. Check it:

```sh
waired status
waired doctor
```

`waired status` shows the engine's own reason on the engine line. If it
crashed, the detail is in its log. See
[Reading the logs](/troubleshooting/other-computers/#reading-the-logs).

The rest of Waired is unaffected. Your computer stays signed in and can use
the models on your other computers. Once the engine is healthy, measure again:

```sh
waired runtimes benchmark
```

## Waired chose a very small model for my computer

That is the largest model this computer can hold while keeping a full coding
session in memory, and Waired runs it. A model that fits but has to throw
away most of a long conversation is not the better choice.

To see what you got and why:

```sh
waired models ls --detail
```

The **SIZE** column says which class of GPU a model is for, and **FIT** says
whether this computer can hold it. Anything in the list is yours to choose:

```sh
waired models use <model>
```

A bigger model runs, but spends the conversation reloading itself from
system RAM, which you feel most on long coding sessions.
`waired inference off` stops running models here entirely. The computer stays
in your network and can use the models on your other computers.

## Local inference started off and I did not choose that

Ask the computer why:

```sh
waired inference status
```

When Waired is the one that decided, the answer says so:

```
Local inference: off
  This computer is below the recommended spec for local inference.
  per request           210.4 s or more
  target                45 s or less
  It can still use the models running on your other computers.
  Turn it on with `waired inference on`.
```

Where that number comes from: as soon as the inference engine is installed,
and before anything downloads a full-size model, Waired downloads a small one
and times a realistic request on it, three times, taking the middle result.
On a computer that is a long way under the mark, it stops sooner and says
`210.4 s or more` instead of an exact number. For details, see
[How Waired chooses a model](/guides/how-a-model-is-chosen/#the-timing-that-can-leave-local-inference-off).

It is a starting point, not a verdict. The computer still joins your network
and can use the models on your other computers. Turn local inference on
whenever you want:

```sh
waired inference on
```

In the Waired app the same choice is **Run models on this computer**. Once
you have made that choice, Waired keeps it.

If `waired inference status` reports **off** and gives no reason, nothing on
this computer decided it. It was chosen during setup, with the installer's
`--inference-enabled false`, or with `waired inference off`.

## It says local inference is not set up yet

```sh
waired inference status
```

```
Local inference: not set up yet — this device is not signed in. Run `waired init`.
```

This is the state between installing Waired and signing in. Nothing is wrong
and there is no setting to change. The computer has no account to run models
for yet. [Sign in](/getting-started/sign-in/), and the answer becomes **on**
or **off**.

## This computer has no inference engine

Some computers never get one. Waired installs the engine only when you said
this computer should run models itself. `waired models ls --detail` says so
above the table:

```
Host: Intel Arc 8 GB VRAM / 63 GB RAM · no inference engine installed

! No inference engine is installed on this computer, so it cannot run a model itself.
  Requests go to your other computers instead.
  Install one with `sudo waired runtimes install ollama`.
  The verdicts below are what this computer would run once an engine is installed.
```

This is normal, not a fault. The computer stays signed in and keeps working.
Your requests are answered by the other computers in your network. To install
an engine at any time, from an administrator terminal:

```sh
sudo waired runtimes install ollama
```

Picking a model in the Waired app on a computer with no engine offers to
install the engine first. If you expected an engine here, the likeliest
reason is that setup was answered with "do not run models on this computer".
See [Run setup again](/getting-started/set-up-again/).

## A model says it needs a newer inference engine

Some models run only on a recent build of the inference engine, and the row
says so:

```
qwen3.8-27b   27B   medium   ✗ needs Ollama 0.32.13 (this computer has 0.31.1)
```

This is not about memory. The engine on this computer is older than the model
needs. Waired manages the engine for you, so the fix is the ordinary update:

```sh
waired update
```

The update brings the engine to the version this build of Waired ships with,
and the row clears afterwards. Until it does, Waired keeps choosing a model
the current engine can run.

The row can end two other ways:

- **`(this computer's version could not be read)`**: the engine is here but
  has never been started. Run `waired inference engine start` and look again.
- **`(no inference engine on this computer)`**: there is no engine here. See
  [This computer has no inference engine](#this-computer-has-no-inference-engine).
