---
title: Routing and sharing commands
description: waired share, worker, peers, ping, public, pause, and resume, with what each one changes and what its output means.
meta:
  audience: Anyone working in a terminal, or on a computer with no screen
  needs: Waired installed
  time: Read the command you need
---

## `waired share`

Whether this computer lends itself out at all. The one sharing switch that
lives on the computer.

```sh
waired share on
waired share off        # stop serving everyone, cutting off work running now
waired share status
```

Turning it off stops every kind of serving straight away. Your other
computers stop being answered, anyone using this computer from outside your
account is cut off, and requests running at that moment are not finished.
Your own use of this computer is unaffected. Nothing in the web console can
turn the switch back on. Only this command, or **Share this computer** in
the Waired app, does.

Who the computer is offered to while the switch is on is set in the
[web console](/guides/web-console/). `status` reports the whole picture:

```text
Sharing this computer: on
Your other computers: on
People outside your account: off
Who this computer is shared with is set in the Waired console.
```

The first line is this computer's own switch. When the saved choice and the
live state differ, a second line explains, for example `Paused because the
Waired app is not running. It resumes when the app starts.` The next two
lines are what the console decided, and read `not known yet` until the
service has heard from it. A `Guest limit: N at once` line appears when a
guest limit has been set. See
[Share a computer with your other devices](/guides/sharing/).

## `waired worker`

Where this computer's requests go.

```sh
waired worker get
waired worker set --mode=auto            # this computer's model if it has one, else another (default)
waired worker set --mode=local-only      # never use another computer
waired worker set --mode=peer-preferred  # prefer another computer, fall back to this one
waired worker set --mode=peer-only       # only another computer; fail rather than run here
waired worker set --pin=<peer>           # always this one (implies --mode=pinned)

waired worker set --prefer=speed         # answer as fast as possible (default)
waired worker set --prefer=size          # use the biggest model available
waired worker set --min-model-size=medium  # skip computers running a smaller model
waired worker set --min-model-size=""      # no minimum (default)
```

`waired worker get` prints all of it:

```
mode:           auto
prefer:         speed
smallest model: any
```

`<peer>` is a computer's name, or the identifier from the `DEVICE-ID` column
of `waired peers list`. You are choosing a computer, not a model. Whichever
computer answers, the answer comes from the model that computer runs. If the
computer you pinned is switched off or unreachable, requests fail instead of
going somewhere else. For what each setting does, see
[Choose which computer answers](/guides/routing/).

When the pinned computer is not serving, `waired worker get` says why on its
`status:` line.

## `waired peers`

```sh
waired peers list
waired peers list --json
```

Your other computers, with each one's address, engine, GPU, and model. This
is how you find a name to pass to `worker set --pin`. Two computers reporting
the same name get a number added to the second one.

**MODEL** is the model that computer runs. **MODELS** next to it is the same
model under the name its inference engine uses. **WORKER-CAPABLE** is what
each computer reports about itself: whether it says it can answer right now,
and when it cannot, why, for example `no (downloading)` or
`no (engine not answering)`. These reports reach you over your account, not
over the private network between your computers, so a `yes` is a claim, not
something this computer checked. `no (stale)` means that computer stopped
reporting in. A computer that is switched off keeps its row until you remove
it from your network.

When this computer has not heard back from one of the computers in the list,
a line under the table says so:

```
This computer has had no reply from: office-desktop.
WORKER-CAPABLE is what each computer reports about itself, not something this
computer checked. Run `waired doctor` to measure this computer's connection.
```

One cause is named directly, because nothing else can work until it is fixed:

```
This computer's key doesn't match the one your network has for it, so no other
computer can reach it. Run `waired init` to sign this computer in again.
```

## `waired ping`

```sh
waired ping <peer>
```

Checks that this computer can reach another over the private network. When
the other computer does not answer, the error names it.

## `waired public`

Lending your spare capacity to other Waired users, and borrowing theirs. Off
unless you turn it on. Read [Public Share](/public-share/) first.

Sharing this computer publicly is turned on and off in the
[web console](/guides/web-console/), not here. `waired public status` reports
it, along with your own settings for using other people's computers.

```sh
waired public status
waired public use                      # show your current settings
waired public use --auto               # use others' computers when they beat your own
waired public use --explicit           # only when you specifically ask
waired public use --off
waired public use --min-model-size small|medium|large   # only computers running a model of at least this size
waired public use --main on|off --sub on|off            # allow or deny public nodes for the main conversation and subagents
```

The first time you enable `use`, a one-time privacy warning appears in the
terminal that you have to read and accept.

`waired public status` starts with the sharing side, `Sharing this computer
publicly: on|off`, then `Guest limit: N at once` or `Guest limit: automatic`,
and a reminder that public sharing is turned on and off in the web console.
When this computer's own switch is off, it adds the line that explains why
nothing is shared:

```text
Sharing is off on this computer, so nothing is shared. Turn it back on with `waired share on`.
```

## `waired pause` and `resume`

```sh
waired pause
waired resume
```

Pausing stops all routing on this computer. It stops answering, and a turn
you sent to Waired fails unless another of your computers takes it. The
Waired app's **Pause Waired** is the same switch. The setting survives
restarts. When the background service is not running, the
choice is saved and applied at the next start:

```
The background service isn't running. pause is saved and applies on the next start.
```

See [Pause or stop Waired](/guides/pause/) for the four different things
"turn it off" can mean.
