# Screenshots still to capture

Pages reference these by file name. Until a file exists, `Screenshot.astro`
renders a labelled placeholder — the page is never broken, and dropping the PNG
into `public/img/` with the exact name below is the only step needed to finish
it. No page edit, no second review.

| File | What to capture | Where it is used |
|---|---|---|
| `setup-wizard-progress.png` | The browser setup page mid-run: “Install the AI software” done, “Download the AI model” in progress with a byte bar, “Check the speed” waiting. | Quickstart, Sign in and set up |
| `claude-code-statusline.png` | The Claude Code footer with the Waired status line, naming the local model that answered. | Quickstart |
| `tray-not-signed-in.png` | The Waired menu before sign-in: “○ Not signed in” + “Log in…”. | Direction C quickstart |
| `tray-ready.png` | The Waired menu once set up: account, connected, Inference naming the active model. | Direction C quickstart, Choose which model runs |
| `tray-models.png` | The **Models** submenu open, with the active model marked. | Choose which model runs |
| `windows-smartscreen.png` | The blue “Windows protected your PC” dialog with **More info** expanded so **Run anyway** is visible. | Install on Windows, Direction C quickstart |
| `installer-summary.png` | The installer's pre-flight summary screen. | Direction C quickstart |

## Rules for the captures

- **No real identifiers.** This repository is public. Use a throwaway account,
  a generic device name (`my-desktop`), and crop anything showing a real email,
  device ID, overlay IP or hostname. `waired init --mask-pii` masks those in
  terminal output.
- **Light theme**, default window size, no personal wallpaper or third-party
  menu-bar icons in frame.
- **Crop tight** to the thing being described. A full-screen shot of a 4K
  display is unreadable in a docs column.
- 2× (Retina/HiDPI) PNG, then let the site scale it down.
- Re-capture when the UI it shows changes — a stale screenshot is worse than no
  screenshot, because readers trust it over the text.
