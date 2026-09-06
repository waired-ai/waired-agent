---
title: Slow answers and hardware
description: Answers are very slow, the GPU is not used, a model is bigger than the hardware, the GPU runs out of memory on long prompts, or a Windows setting that reserves memory for the GPU made things worse.
meta:
  audience: Anyone whose model answers, but slowly or with errors
  needs: A terminal on the computer in question
  time: Each fix takes a minute or two
---

Run `waired doctor` first. It names the part that is not ready. Then find your
symptom below.

## Answers are very slow

```sh
waired runtimes benchmark
```

This measures what this computer does. If it comes out below what a coding
assistant needs, Waired offers a lighter model. Accepting is usually right.

Other things worth checking:

- **Is your GPU being used?** See [My GPU is not being used](#my-gpu-is-not-being-used).
- **Is the model too big for your memory?** An over-sized model runs partly
  on the processor, which is much slower. `waired models ls --detail` shows
  the fit.
- **On an AMD Ryzen AI Max computer, how much memory is reserved for
  graphics?** Reserving a lot makes things worse. See
  [Windows: giving the graphics chip more memory made things worse](#windows-giving-the-graphics-chip-more-memory-made-things-worse).
- **Is the answer coming from another computer?** `waired infer --explain "hi"`
  names the computer that served it.
- **Is it the first turn of a Claude Code session?** That one is the
  expensive one. The whole conversation, your instructions, and the file
  contents have to be read before any word comes back, and on a laptop or
  an older GPU that can take several minutes. Later turns in the same
  session are far quicker. See
  [A slow computer is not a failure](/guides/claude-code/how-turns-are-routed/#a-slow-computer-is-not-a-failure).

## My GPU is not being used

First, see what Waired found:

```sh
waired models ls --detail
```

The first line names your GPU and its memory. If it says `no GPU` on a
computer that has one, the GPU was never detected, and everything after
that, including which model you were given, was sized for the processor.

Waired handles the common cases automatically. Integrated AMD and Intel
graphics are enabled through Vulkan, and discrete AMD GPUs use ROCm where it
is supported, falling back to Vulkan when it does not engage.

NVIDIA GPUs are found through the driver itself, not by looking for
`nvidia-smi` on your `PATH`. If your GPU is not showing up, point Waired
straight at the tool and restart the service.

On Linux, run `sudo systemctl edit waired-agent` and add:

```ini
[Service]
Environment=WAIRED_NVIDIA_SMI=/usr/bin/nvidia-smi
```

On Windows, in an administrator PowerShell:

```powershell
[Environment]::SetEnvironmentVariable(
  'WAIRED_NVIDIA_SMI', 'C:\Windows\System32\nvidia-smi.exe', 'Machine')
```

Then restart the service and run `waired models ls --detail` again. On
Windows a full reboot is the surest way to make the service pick up a new
machine-wide variable. See
[A command says “waired-agent is not running”](/troubleshooting/no-answer/#a-command-says-waired-agent-is-not-running)
for the restart commands.

Also confirm the model fits. Memory requirements are in the
[model catalog](/reference/model-catalog/).

## I chose a model bigger than my hardware

Waired warns but does not block you. When you pick an over-sized model it
shows the shortfall, for example `needs 32 GB RAM (have 31 GB)`, and asks
you to confirm.

- **Slightly over**: it usually runs, and is slower.
- **Far too big**: the engine fails to load it and reports an error. Switch
  back down. See [Change the model](/guides/choose-a-model/).

The recommended figures carry a safety margin. On Apple Silicon and AMD Strix
Halo, the fit is judged against the memory the graphics side can address. On
a computer with a separate GPU, what Waired picks for you is judged against
the GPU's own memory, so a model that fits only by spilling into system RAM
is one you have to choose deliberately. `waired models ls --detail` shows the
verdict for every model on this computer.

## It says the GPU ran out of memory on a long prompt

You find this out while using the computer, not during setup. A turn fails
partway through a long conversation, and after that the model's row in
`waired models ls --detail` reads `! running here with a warning`, with the
engine's own sentence printed under the table. That sentence starts
`this computer's GPU ran out of memory serving a request at this model and window`.

This is not the same as slow. Short prompts work. The computer runs out of
VRAM once the conversation gets long, and a coding session gets long quickly.

Waired does not change anything on its own. The engine keeps serving, the
next short request works, and Waired keeps the warning where you see it: in
`waired models ls --detail`, in `waired status`, and in `waired doctor`.

The model is too big for this computer at the length you need. Switch to a
lighter model. See [Change the model](/guides/choose-a-model/). Waired does
not offer a lighter model automatically in this case, on purpose. That
suggestion is for a computer that measured slow. Running out of memory is a
different problem, and a smaller model at the same conversation length is
not always the fix.

## Windows: giving the graphics chip more memory made things worse

On an AMD Ryzen AI Max (Strix Halo) computer, the graphics side and the
processor share one pool of memory, and a setting decides how much of that
pool is handed to the graphics side up front. Turning it up looks like the
way to run a bigger model. It does the opposite.

Windows reserves a matching amount of system RAM behind every graphics
allocation. So a model needs room on the graphics side and the same amount
again in the memory Windows still sees. Hand 96 GB of a 128 GB machine to the
graphics side, and Windows is left with about 31 GB, which is then the real
limit on model size. A larger model starts loading, runs out, and pages to
disk for tens of minutes without answering.

Measured on a 128 GB Ryzen AI Max+ 395 with one 76 GB model, changing nothing
but this setting:

| Memory reserved for the GPU | What happened |
|---|---|
| 96 GB | never finished loading, 28 minutes, no answer |
| 512 MB | loaded in 15 seconds, then ran at full speed |

Reserving less costs nothing. The graphics side reaches the rest of the
memory anyway, and on this kind of computer it is the same physical memory at
the same speed either way.

So set it low. In the BIOS, leave the VRAM size on **Auto**. It is usually
called **UMA Frame Buffer Size**. Then, in AMD Software: Adrenalin Edition,
open **Performance**, **Tuning**, **System**, then **Variable Graphics
Memory**, and choose the smallest option. Restart, and check what Waired sees
now:

```sh
waired models ls --detail
```

The first line should report a much larger figure than before. If it still
shows the small leftover, the BIOS is fixing the split itself rather than
leaving it to the driver. Set it back to **Auto** there.
