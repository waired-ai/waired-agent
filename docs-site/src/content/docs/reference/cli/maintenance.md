---
title: Maintenance commands
description: waired update, config, logs, version, and keygen, with what each flag does and where the logs come from.
meta:
  audience: Anyone working in a terminal, or on a computer with no screen
  needs: Waired installed
  time: Read the command you need
---

## `waired update`

```sh
waired update              # check and apply, staying on the current channel
waired update --check      # report only
waired update --yes        # apply without the installer's confirmation
waired update --edge       # switch to the latest main build
waired update --stable     # switch back to stable
waired update --force      # re-resolve from the release source (Linux: refreshes the package index; asks for sudo)
waired update --notify on|off   # the Waired app's pop-up update prompt
```

The update reads the available version from the local service, then reruns
the official installer under elevation to apply it. Linux applies through
apt, Windows through the installer's elevated swap, and macOS reruns the
install script under administrator privileges. An engine already installed
here is brought to this build's pinned version at the same time. See
[Update Waired](/getting-started/update/).

`--notify off` silences the pop-up. The update entry in the Waired app stays
either way.

## `waired config`

Changes persisted settings of the background service. Today that means the
log detail level.

```sh
waired config log-level              # show the current level
waired config log-level debug        # turn on detailed logs
waired config log-level info         # back to normal
```

The levels are `debug`, `info` (the default), `warn`, and `error`. `debug`
is the switch to flip before reproducing a problem. It takes effect
immediately, without a restart, on both the background service and the
Waired app, and it is remembered across restarts. While it is on, Waired
keeps more of the log: 128 MB per file instead of 32 MB, ten older copies
either way. Set it back to `info` when you are done. If the service is not
running, the choice is saved and applies the next time it starts.

## `waired logs`

Collects the recent logs into a single file you can attach to a bug report.

```sh
waired logs                          # writes waired-logs-<time>.txt here
waired logs -o report.txt            # choose the file
waired logs --since 30m              # how far back to look (default 1h)
waired logs --mask-pii               # redact home folder, username, hostname, and email
waired logs --full                   # every rotated copy, not only the recent 16 MB
```

It gathers the background service's log from the system log, the service's
own log file where the system keeps one, and the inference engine's log.
Older, rotated copies are included too. The files are collected newest first
up to 16 MB in total, so the result stays small enough to attach to an issue.
`--full` takes every rotated copy instead, which at `debug` verbosity can run
to hundreds of megabytes.

For the most useful report, turn on detail first, reproduce the problem, then
collect:

```sh
waired config log-level debug
# ...reproduce the problem...
waired logs --mask-pii -o report.txt
waired config log-level info
```

`--mask-pii` replaces your home folder, username, machine name, and account
email with placeholders. It is best-effort, so look over the file before
sharing it. The whole sequence is on
[Report a problem](/getting-started/report-a-problem/).

## `waired version`

```sh
waired version
waired version --json      # {version, buildSHA, os, arch}
```

## `waired keygen`

Generates a WireGuard key pair. `init` does this for you. You would run it by
hand only when building something unusual.
