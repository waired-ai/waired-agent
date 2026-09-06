---
title: Install and sign-in problems
description: The command is not found, the browser did not open, the sign-in link expired, the service is not responding, or Waired says you are signed out.
meta:
  audience: Anyone stuck between installing and being signed in
  needs: A terminal on the computer in question
  time: Each fix takes a minute or two
---

Run `waired doctor` first. It names the part that is not ready. Then find your
symptom below.

## I typed `waired` and got “command not found”

Either the install did not finish, or your terminal was already open when it
did and has not picked up the new command yet.

1. Close the terminal and open a new one. A running shell caches where
   commands live, so this alone fixes it most of the time.
2. If the command is still missing, run the install command again. It is safe
   to run twice. See [Install Waired](/getting-started/install/).

On Windows the command lives at `C:\Program Files\Waired\waired.exe`. When
`waired` alone does not work, that full path always does.

## No browser opened at sign-in, or the wrong one did

The sign-in link is always printed in the terminal before anything opens, so
you can finish by hand at any time. Copy the link and paste it into the
browser you normally use. Sign in there, and stay in that browser for the
rest of setup. The setup page works only in the browser you signed in with.

If a browser you never use opened instead, close it without signing in and
open the printed link in the one you want.

Both cases were caused by setup running with administrator rights, and are
fixed in current versions:

```sh
waired update
```

## The sign-in link expired before I finished

The link that `waired init` prints is valid for a limited time. A two-factor
prompt on a phone in another room, or a tab left open while you do something
else, can be enough to use it up. When the time runs out, the terminal stops
with:

```
waired: login expired. Run `waired init` again
```

Do exactly that:

```sh
sudo waired init        # Windows: waired init from an administrator terminal
```

Nothing is broken and nothing needs cleaning up. The command prints a fresh
link, and you sign in with that one.

If you finish signing in after the terminal has already stopped, the browser
says the sign-in link has expired and sends you back to the terminal. If you
had already reached the device list in the web console, its banner says the
sign-in expired before this computer finished registering. All three surfaces
say the same thing: the computer is registered by the command waiting in the
terminal, and that command has stopped.

## Sign-in stops because the background service is not responding

Sign-in happens through the background service. It is what talks to Waired
and what keeps this computer connected afterwards. If it is not answering,
sign-in stops rather than continuing without it:

```
Waired's background service is installed but isn't responding, so sign-in can't continue.
  Check what's wrong:  waired doctor
  Start it:            sudo systemctl start waired-agent
  Then run again:      sudo waired init
```

Work through those three lines in order. `waired doctor` names the fault. On
macOS the usual one is
[the service never starting at all](/troubleshooting/no-answer/#macos-the-background-service-never-starts).

If the message says **“Waired isn't running in the background”** instead, no
background service is registered on this computer at all, normally because
the programs are being run directly rather than installed. Start
`waired-agent` first, then run `waired init` again.

### Sign-in worked, but the setup steps did not run

Reads and writes reach the background service by different routes, so a
computer can reach it for one and not the other. When setup cannot reach it,
`waired init` says so:

```text
Warning: couldn't ask the background service about setup (…). Its setup steps will be skipped. Run `waired doctor` to see why.
```

That run skips the steps that need the background service: installing the
inference engine, connecting coding tools, and reporting progress to the
browser. Sign-in itself is unaffected.

A milder form means the question got through and only the first update did
not:

```text
Warning: couldn't tell the background service that setup is running (…). Retrying in the background. If the browser shows no progress, run `waired doctor`.
```

That one repairs itself within about ten seconds. If the browser still shows
no progress after that, run `waired doctor`.

## I signed in, but Waired says I am signed out

The Waired icon shows **Not signed in**, or the computer is missing from your
account, most often right after a restart, even though you never signed out.
Two different things look the same from the outside, and `waired doctor`
tells them apart.

**`network connection` is a ⚠.** You are signed in, and this computer has
not connected yet. Waired keeps retrying by itself, including after a restart
where the network port it normally uses was taken by something else. Give it
a minute and check again. If it never clears, restart the background service:

```sh
sudo systemctl restart waired-agent    # Linux
Restart-Service waired-agent           # Windows, administrator
```

**`device sign-in` is a ✗.** This computer's sign-in has stopped working,
and only signing in again restores it:

```sh
sudo waired init      # Linux and macOS
waired init           # Windows, administrator
```

Your models, settings, and coding-tool setup all survive this. It
re-establishes this computer's place in your account and nothing else. Local
inference keeps answering throughout. What stops is everything that needs
your account, so the computer disappears from the web console and your other
computers cannot reach it until you sign in again.

## It says I have reached the device limit

Each account can enroll a generous number of devices, and the usual cause is
old computers you no longer use still being counted.

Open [app.waired.ai](https://app.waired.ai), remove a device you no longer
need, then set up again. Re-running setup on a computer that is already
signed in never counts against the limit.

## It says the computer is “signed in system-wide”

That is not an error. The device's identity is stored in a system folder only
administrators can read, so `waired status` run as a regular user cannot see
it. Rather than guess, it tells you the device is enrolled and exits
successfully. To see the full status, run it with administrator rights:

```sh
sudo waired status          # Windows: from an administrator terminal
```

`waired doctor` says the same thing on such a computer, in its **state
directory** line, and treats it as a check it could not run rather than a
failure. See
[When the check itself cannot see everything](/getting-started/doctor/#when-the-check-itself-cannot-see-everything).

If instead you see `Not signed in. Run 'waired init' to sign in.`,
this computer has not been set up yet. See [Sign in](/getting-started/sign-in/).
