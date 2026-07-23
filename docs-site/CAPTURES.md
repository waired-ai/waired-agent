# Screenshots still to capture

Pages reference these by file name. Until a file exists, `Screenshot.astro`
renders a labelled placeholder — the page is never broken, and dropping the PNG
into `public/img/` with the exact name below is the only step needed to finish
it. No page edit, no second review.

Ordered by how much they matter: the first three are on the pages every new
user reads, and are worth capturing before anything else.

| File | What to capture | Where it is used |
|---|---|---|
| `setup-wizard-progress.png` | The browser setup page mid-run: “Install the AI software” done, “Download the AI model” in progress with a byte bar, “Check the speed” waiting. | Quickstart, Sign in and set up |
| `app-ready.png` | The Waired app menu once set up: connected, account, and **Inference** naming the active model. | Quickstart, The Waired app, Check it works |
| `claude-code-statusline.png` | The Claude Code footer with the Waired status line, naming the local model that answered. | Quickstart, Use it from Claude Code |
| `app-not-signed-in.png` | The Waired app menu before sign-in: “○ Not signed in” + “Log in…”. | Quickstart (Japanese) |

## Rules for the captures

- **No real identifiers.** This repository is public. Use a throwaway account,
  a generic device name (`my-desktop`), and crop anything showing a real email,
  device ID, overlay IP or hostname. `waired init --mask-pii` masks those in
  terminal output.
- **Light theme**, default window size, no personal wallpaper or third-party
  icons next to the clock in frame.
- **Crop tight** to the thing being described. A full-screen shot of a 4K
  display is unreadable in a docs column.
- 2× (Retina/HiDPI) PNG, then let the site scale it down.
- Re-capture when the UI it shows changes — a stale screenshot is worse than no
  screenshot, because readers trust it over the text.

## Naming

The Waired app's menu is `app-*.png`, never `tray-*.png`. The user-facing term
for it is **the Waired app**, and the docs do not use the word "tray" anywhere
— see the terminology note in the sidebar comment of `astro.config.mjs`.
