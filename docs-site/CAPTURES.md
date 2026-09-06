# Screenshots

Pages reference these by file name under `public/img/`. Until a file exists,
`Screenshot.astro` renders a labelled placeholder, so a missing file never
breaks a page. Dropping the PNG in with the exact name below is the only step
needed to finish it.

| File | What it shows | Where it is used |
|---|---|---|
| `setup-wizard-progress.png` | The browser setup page mid-run, English UI: the engine and timing steps complete, “Download the model” in progress with a byte count and a transfer rate, “Benchmark the inference speed” waiting. | Quickstart, Set up in the browser |
| `setup-wizard-progress-ja.png` | The same state with the console's Japanese UI selected. | クイックスタート, ブラウザでセットアップする |
| `app-ready.png` | The Waired app menu once set up: **Connected**, the account, and **Engine: ready** naming the active model. | Quickstart, Meet the Waired app, Check that it works |
| `claude-code-statusline.png` | The Claude Code footer with the Waired status line, naming the local model that answered. | Quickstart, Use Waired from Claude Code |
| `app-not-signed-in.png` | The Waired app menu before sign-in: **○ Not signed in** and **Sign in…**. | The Waired app menu, Sign in |

The web console is dark-only and bilingual, so the two NAVI captures are
taken in both languages. The Waired app and the Claude Code footer are
English on every system, so one capture serves both languages.

Captured so far:

- The two NAVI files, taken with Playwright against the development console
  during a real model download, with the device renamed and the account chip
  and pre-release banner hidden in the DOM.
- `app-ready.png`, taken on a Mac in the dark appearance at 2×. The menu was
  opened and read through System Events (which reports each row's rectangle),
  captured with `screencapture -R` on just the menu's rectangle, and the
  account row was repainted in the image: the text pixels were covered with
  the text-free strip to their right on the same row, so the menu's
  translucent gradient continues, and `you@example.com` was drawn there in
  the system menu font. Both steps need permissions the owner grants once on
  that Mac: Accessibility for the process that drives System Events, and
  Screen Recording for the process that captures (when they run from a
  Terminal window, Terminal itself holds both).

- `app-not-signed-in.png`, taken the same way on the same Mac after signing
  the device out with `sudo waired logout --yes` and restarting the
  background service. The menu carries no account or device name in that
  state, so nothing was repainted.

Still to capture: `claude-code-statusline.png`. Wait for a release whose
segment carries the `⚡` prefix the docs quote; the 0.0.3-rc5 build prints
the segment without it. Until then that page shows the labelled placeholder.

## Rules for the captures

- **No real identifiers.** This repository is public. Use a generic device
  name (`my-desktop`), and mask or crop anything showing a real email,
  device ID, overlay IP, or hostname. Masking in the page's DOM before the
  capture (renaming the device, hiding the account chip and the
  pre-release banner) is fine. Changing the *state* the page shows is not:
  a capture shows something that actually happened.
- **Default window size**, no personal wallpaper or third-party icons next
  to the clock in frame.
- **Crop tight** to the thing being described. A full-screen shot of a 4K
  display is unreadable in a docs column.
- 2× (Retina/HiDPI) PNG, then let the site scale it down.
- Re-capture when the UI it shows changes. A stale screenshot is worse than
  no screenshot, because readers trust it over the text.

## Naming

The Waired app's menu is `app-*.png`, never `tray-*.png`. The user-facing
term is **the Waired app**, and the docs do not use the word "tray" except in
the program name `waired-tray`.
