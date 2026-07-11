# Waired one-liner installers

End-user-facing entry points. Hosted on the `waired-ai/waired-agent`
GitHub Releases (or `https://pkgs.waired.dev/…` once that DNS lands)
and run via a single copy-pasteable command.

## Linux — `install.sh`

```sh
curl -fsSL https://github.com/waired-ai/waired-agent/releases/latest/download/install.sh | sh
```

Internally it adds the Waired apt repository and `apt install`s the
`waired` (and, by default, `waired-tray`) packages.

## macOS — `install.sh`

```sh
curl -fsSL https://github.com/waired-ai/waired-agent/releases/latest/download/install.sh | sh
```

The same `install.sh` detects Darwin and runs the macOS path: it
downloads `waired-darwin-<arch>.tar.gz` + `.sha256` from the public
mirror, verifies the hash, installs `waired` + `waired-agent` (and, by
default, `waired-tray`) into `/usr/local/bin` (one `sudo` prompt for the
copy), installs **Ollama** (reuses an existing install, otherwise
downloads the official `Ollama.app` into `/Applications` — no Homebrew
required), and registers a **per-user launchd LaunchAgent** via
`waired-agent install` (no root — the agent runs in your `gui/<uid>`
session with state under `~/Library/Application Support/waired`).

The tray (`waired-tray`) is now bundled in the tarball, matching the
Windows zip and Linux `.deb`. Set `WAIRED_NO_TRAY=1` to skip it on
headless Macs. Like the Windows installer, `install.sh` does not
auto-launch the tray; launch it once (`"/usr/local/bin/waired-tray" &`,
or from Spotlight) and on first launch it registers its own per-user
LaunchAgent (`com.waired.tray.waired-tray`) so it returns at every
login. The tray runs as a menu-bar-only accessory — no Dock icon.

The binaries are unsigned (ad-hoc); `curl`-downloaded executables do not
get the Gatekeeper quarantine attribute, so they run without a
right-click-Open gesture. Code signing / notarization, a `.dmg`/`.app`
bundle, and a Homebrew formula are follow-ups (`#262`). Run as your
normal login user, not under `sudo` — `sudo` is invoked only for the
`/usr/local/bin` copy, and running the whole script as root would
register the LaunchAgent for `root` instead of you.

## Windows — `install.ps1`

```powershell
iwr -useb https://github.com/waired-ai/waired-agent/releases/latest/download/install.ps1 | iex
```

Internally it self-elevates (UAC), downloads
`waired-windows-amd64.zip` + `.sha256` from the same public mirror,
verifies the hash, extracts to `%ProgramFiles%\Waired\`, and runs
`waired-agent.exe install` (which is the single source of truth for
SCM registration, Event Log source creation, and the restrictive DACL
on `%ProgramData%\waired\`).

Users who prefer a GUI can instead double-click
`WairedSetup-<version>-x64.exe` (Inno Setup) from the same release.
The CLI one-liner is the recommended path while Authenticode signing
is not yet in place — see `docs/decisions.md`.

## After install — it just runs

The installer now drives first-run setup for you. On a normal interactive
run (a terminal is available, even via `curl | sh`):

1. it installs the packages,
2. runs **`waired init`** — sign-in, local-inference setup, model download
   (with a live progress bar), and coding-agent integration,
3. offers a quick benchmark (which doubles as a "local inference works"
   check), and
4. **enables + starts** the `waired-agent` service.

Re-running the one-liner on an already-installed host detects it and
interactively offers to **update**; if the host was installed but never
signed in, it finishes sign-in too.

```text
# one command, start to finish:
curl -fsSL https://github.com/waired-ai/waired-agent/releases/latest/download/install.sh | sh
```

Notes / escape hatches:

- **No terminal? (CI, Docker build):** sign-in is skipped, but the service
  is still enabled + started. It boots without an identity and idles until
  login, so a **non-root desktop user can finish via the tray** ("Log
  in…"), or you can run `sudo waired init` later.
- `--no-init` skips the automatic `waired init`; `--yes` accepts prompts
  (update, and `waired init --non-interactive` for the inference choices).
- On Linux, `sudo waired init` writes identity to `/var/lib/waired` (the
  dir the systemd unit reads) and chowns it to the service user, and reads
  `WAIRED_CONTROL_URL` from `/etc/waired/agent.env` when set. With nothing
  configured it falls back to the production Control Plane.
- A scheme-less Control Plane host (`--control dev.waired.net`) is accepted
  and normalised — `https://` for remote hosts, `http://` for loopback.
- The full set of enrollment flags (`--control`, `--non-interactive`,
  `--skip-deploy`, `--start-agent`, …) lives in **`waired init --help`**.

## Supported targets

| OS family             | Status                                         |
|-----------------------|------------------------------------------------|
| Debian (trixie+, sid) | supported (apt)                                |
| Ubuntu 24.04 LTS+     | supported (apt)                                |
| Fedora / RHEL         | placeholder — exits with a clear message       |
| Alpine                | placeholder                                    |
| Arch / AUR            | placeholder                                    |
| macOS 13+ (arm64/amd64) | supported via `install.sh` (unsigned tarball) |
| Windows 10 1809+ / 11 | supported via `install.ps1` + Inno `Setup.exe` |

The architecture matrix is `amd64` and `arm64` on Linux and macOS,
`amd64` on Windows (Windows arm64 deferred). Anything else exits.

## Options

| Flag         | Effect                                               |
|--------------|------------------------------------------------------|
| `--dry-run`  | Print every privileged command without running it.   |
| `--check`    | Report whether a newer release is available, then exit (read-only). PowerShell: `-Check`. |
| `--update`   | Update an existing install in place. Stays on the host's current channel by default (edge stays edge). PowerShell: `-Update`. |
| `--edge`/`--latest` | Install/switch to the latest main build (edge channel; same as `WAIRED_VERSION=edge`). PowerShell: `-Edge`/`-Latest`. |
| `--stable`   | Install/switch to the latest stable release. On `--update`/`--check` this overrides channel-preservation. PowerShell: `-Stable`. |
| `--yes`/`-y` | Assume "yes" to the update prompt (needed on non-TTY hosts). PowerShell: `-Yes`. |
| `-h`/`--help`| Print usage and exit.                                |

## Environment variables

Shared between `install.sh` and `install.ps1`:

| Variable                  | Effect                                                                            |
|---------------------------|-----------------------------------------------------------------------------------|
| `WAIRED_VERSION`          | Pin to a specific version (Linux: `waired=1.2.3`; Windows: release tag `v1.2.3`). `edge` = the latest main build (rebuilt every merge; same as `--edge` / `-Edge`); on every OS it auto-selects the edge apt repo / edge prerelease assets. Also selects the target for `--update`. |
| `WAIRED_NO_TRAY`          | If non-empty, skip `waired-tray` (Linux + macOS; Windows uses `-NoTray`). Use on headless servers. |
| `WAIRED_INSTALL_BASE_URL` | Override the URL hosting `install.sh` / `install.ps1` + the OS binaries (tests / mirrors). |
| `WAIRED_INSTALL_REPO`     | Override the GitHub repo whose Releases API resolves `latest` during `--check` / `--update` on macOS + Windows (Linux uses the apt candidate). Default `waired-ai/waired-agent`. |

macOS-only:

| Variable                   | Effect                                                                           |
|----------------------------|----------------------------------------------------------------------------------|
| `WAIRED_NO_OLLAMA`         | If non-empty, skip the Ollama install (bring your own inference engine).         |
| `WAIRED_OLLAMA_DARWIN_URL` | Override the `Ollama.app` download URL (pin a version / point at a mirror).       |
| `WAIRED_DARWIN_BINDIR`     | Override where `waired` / `waired-agent` are installed. Default `/usr/local/bin`. |

Linux-only (apt repo metadata):

| Variable                  | Effect                                                                            |
|---------------------------|-----------------------------------------------------------------------------------|
| `WAIRED_APT_BASE_URL`     | Override the apt repo base URL. Default points at the AR project endpoint.        |
| `WAIRED_APT_SUITE`        | Override the apt suite. Defaults to `waired-dev-apt` (= the AR repository id).    |
| `WAIRED_APT_COMPONENT`    | Override the apt component. Defaults to `main`. AR APT format uses `main` today.  |
| `WAIRED_APT_KEY_URL`      | Override the AR signing-key URL (region-scoped Google-managed key).               |

Windows-only:

| Variable             | Effect                                                                |
|----------------------|-----------------------------------------------------------------------|
| `WAIRED_STATE_DIR`   | Override on-disk state location. Default `%ProgramData%\waired`.      |

`WAIRED_APT_SUITE` is intentionally exposed: AR's APT format publishes
one suite per repository, so future stable/beta channel separation will
ship as a second AR repo (`waired-dev-apt-beta`) and end users will
switch tracks by setting `WAIRED_APT_SUITE=waired-dev-apt-beta` rather
than via a component flip.

## Updating

The installer detects an existing install and updates it in place;
enrolment, identity, and on-disk state are preserved across the update.

* **Check only** (read-only — no download, no privilege prompt):
  `install.sh --check`, or `install.ps1 -Check`.
* **Apply**: `install.sh --update` (add `--yes` on a non-interactive
  host), or `install.ps1 -Update` (add `-Yes`).
* **Re-running the one-liner** on a host that already has Waired
  auto-detects the install and offers the update interactively
  (`Update waired X -> Y? [Y/n]`, default yes). A non-TTY run without
  `--yes` / `-Yes` only reports and changes nothing.

How each OS applies it:

* **Linux (apt)** — delegated to `apt-get install --only-upgrade waired
  waired-tray`. The repo is already configured; the `.deb` postinst
  preserves `/etc/waired` and restarts the systemd unit.
* **macOS (tarball)** — downloads + SHA-256-verifies the latest release,
  swaps the `/usr/local/bin` binaries, and reloads the LaunchAgent
  (`launchctl kickstart`). `~/Library/Application Support/waired` is
  untouched.
* **Windows (zip)** — downloads + SHA-256-verifies the latest release,
  stops the service, overwrites the binaries in `%ProgramFiles%\Waired`
  in place (the SCM registration and state-dir DACL stay valid), then
  restarts the service. `%ProgramData%\waired` is untouched.

Version resolution:

* The **installed** version comes from `waired version --json`
  (`.version`); a binary too old to report one is treated as outdated so
  the update is still offered.
* The **latest** version comes from the GitHub Releases API of the
  mirror (`WAIRED_INSTALL_REPO`); on Linux the apt *candidate* is used
  instead. `WAIRED_VERSION=edge` follows the rolling prerelease (the
  compare degrades to "always offer"); `WAIRED_VERSION=vX.Y.Z` pins a
  tag. A future `latest.json` feed (#294) can replace the API source
  without changing the CLI surface.
* **Ollama** is managed separately: an update never touches the bundled
  engine, and a reused system Ollama is left alone.

### In-product `waired update` + tray (#293)

On an installed host the simplest path is the in-product surface, which
reuses this installer flow under the hood:

* `waired update` — checks via the local daemon, then applies if a newer
  release exists by re-running the installer above under elevation
  (Linux `pkexec`, Windows UAC, macOS `osascript … with administrator
  privileges`). `waired update --check` reports only. By default it stays
  on the host's current channel (an edge build updates to the latest edge,
  never silently to stable); `waired update --edge` / `--stable` switch or
  pin the channel. The installer is fetched from the host's current-channel
  mirror so it always understands the flags it is passed.
* The tray shows an **"⚠ Update available — install vX"** item when the
  daemon reports a newer release; clicking it runs the same elevated
  apply. A desktop notification fires when a new version is first seen
  and re-reminds at most once a day while it stays pending; the
  **"✓ Notify me about updates"** item beneath the banner (or
  `waired update --notify=off`) silences it (#294).

The daemon (unprivileged) only *checks*: `POST /waired/v1/update/check`
resolves the latest version (apt candidate on Linux, GitHub Releases API
on macOS/Windows) and compares it against `waired version`, caching the
result; `GET /waired/v1/update/status` returns the cached result for the
tray to poll cheaply. The *apply* is always client-driven under
elevation — the daemon never installs. Dev/edge builds
(`0.0.0-<sha>` and the `<core>-edge.<ts>+<sha>` / `<core>~edge.<ts>+<sha>`
edge versions) are never *proactively* flagged (the dotted-version compare
can't rank timestamped edge builds, so the tray never nags an edge host).
A manual `waired update` on an edge host still proceeds to the installer,
which stays on the edge channel and lets apt decide whether a newer edge
build exists.

The background auto-check + popup + opt-in toggle (#294) drives this same
surface on a timer: the daemon re-runs the check every 6h (Linux reads the
local apt cache — no GitHub API; macOS/Windows query the Releases API ~4×/day)
so a release published after boot surfaces without a client POST, and even
headless agents detect it (logged as `waired update available`). The prompt
preference persists at `<state-dir>/runtime/desired-update-notify` (default
on) via `POST /waired/v1/update/settings`. The apply stays client-driven —
unattended auto-apply remains blocked by code-signing (#262) and the
unprivileged Linux daemon.

## Uninstalling

A matching pair of uninstallers ships alongside the installers and is
mirrored to the same public release — `uninstall.sh` (Linux + macOS) and
`uninstall.ps1` (Windows):

```sh
# Linux / macOS
curl -fsSL https://github.com/waired-ai/waired-agent/releases/latest/download/uninstall.sh | sh
```

```powershell
# Windows
iwr -useb https://github.com/waired-ai/waired-agent/releases/latest/download/uninstall.ps1 | iex
```

Two tiers, matching apt's `remove` / `purge` split:

* **default** — remove the binaries + service registration but
  **preserve** config and state (`/etc/waired`, `/var/lib/waired`,
  `%ProgramData%\waired`, `~/Library/Application Support/waired`) so a
  re-install resumes. Linux delegates to `apt-get remove`; the apt
  source the installer added is left in place.
* **`--clean` / `-Clean`** — also delete config + state, the apt source +
  keyring the installer wrote, the legacy Claude-proxy trust, and Ollama
  (binary/app + downloaded models). Linux delegates to `apt-get purge`
  plus repo cleanup. Destructive and irreversible, so it **asks to
  confirm**; pass `--yes` / `-Yes` to skip the prompt (required on a
  non-interactive / piped shell), or `--dry-run` / `-DryRun` to preview.

The scripts don't re-implement removal: they prefer the binaries' own
`waired-agent uninstall` (SCM / launchd / systemd + Event Log) and
`waired proxy uninstall`, plus the `.deb` maintainer scripts, then clean
up only the residue the installer itself scattered (apt source, Ollama,
per-user autostart).

On Windows, if `waired-agent.exe` cannot be launched — e.g. an
Application Control Policy (Smart App Control / WDAC / AppLocker) blocks
the unsigned binary — `uninstall.ps1` automatically falls back to native
SCM removal (`sc.exe delete waired-agent` + Event Log source cleanup),
which is equivalent and needs no exe launch. No manual step is required.

On Windows, `-Clean` needs the script on disk because `iwr | iex` strips
named parameters — download it, then run `.\uninstall.ps1 -Clean`. If
Waired was installed with the GUI `WairedSetup-*.exe`, you can also
remove it from **Settings → Apps → Waired → Uninstall**; the script is
safe either way.

This pair is a bridge until packaged uninstallers (winget, a signed
`.app` / `.dmg`, an MSI) land — see the issue tracker.

## Design — common vs OS-specific

The script is a single POSIX `sh` file by design: piping a multi-file
installer through `curl | sh` makes the trust boundary subtle (the
second download has to be re-verified) and forces an extra network
round-trip. Bundling everything into one stream is what Tailscale,
Docker, and friends all do.

"Extensibility" is therefore expressed by a **function-name
convention** rather than by file layout:

* `common_*` — log/run/sudo helpers shared by every handler.
* `detect_*` — fill in `OS_KIND`, `OS_FAMILY`, `OS_NAME`,
  `OS_VERSION`, `OS_CODENAME`, `OS_ARCH`. Everything below dispatches
  on those globals.
* `linux_apt_*` — Debian / Ubuntu handler.
* `darwin_*` — macOS handler: download the ad-hoc tarball, install the
  CLI + agent + tray, install Ollama.app, register the launchd
  LaunchAgent.
* `linux_dnf_*`, `linux_apk_*`, … — future OS handlers. Add one new
  function group + one new arm in `main()`.

A separate `install.ps1` ships alongside, with the same env-var contract
(`WAIRED_VERSION`, `WAIRED_NO_TRAY`, `WAIRED_INSTALL_BASE_URL`) but
PowerShell-shaped helpers (`Common-Log`, `Common-Run`, `Detect-Platform`,
…). The two scripts share a docs surface and a release-publishing
pipeline but no source code — multiplexing sh and PowerShell into one
stream was rejected as a maintenance trap.

## Hosting

The Linux one-liner URL is
`https://github.com/waired-ai/waired-agent/releases/latest/download/install.sh`;
each `v*` tag of `waired-ai/waired-agent` publishes the entry-point
scripts as release assets via release.yml. The apt repo lives in
Artifact Registry on the `dev-waired` GCP project. The matching
`uninstall.sh` / `uninstall.ps1` (see [Uninstalling](#uninstalling)) are
published alongside, so every release ships its own removers.

The Windows one-liner URL is the same release, with `install.ps1`,
`waired-windows-amd64.zip`, `waired-windows-amd64.zip.sha256`, and
`WairedSetup-<version>-x64.exe` all uploaded as release assets. The
PowerShell script downloads the zip + sha from the same `/releases/…`
path.

The macOS tarballs (`waired-darwin-{amd64,arm64}.tar.gz` + `.sha256`)
are uploaded to the same release, so a Mac fetches them from the same
public `/releases/…` path.

See the GitHub issue tracker for open follow-ups (Terraform-managed DNS
for `pkgs.waired.dev`, winget manifest, Authenticode signing `#124`,
arm64 Windows build `#126`, macOS signing/notarization `#262`, macOS
Homebrew formula `#266`).

## Verification

Linux — run shellcheck:

```sh
shellcheck packaging/install/install.sh
```

End-to-end dry-run against a local file server:

```sh
cd packaging/install && python3 -m http.server 8000 &
WAIRED_INSTALL_BASE_URL=http://localhost:8000 \
    sh ./install.sh --dry-run
```

macOS — build the tarballs, serve them, and dry-run the Darwin path:

```sh
make dist-darwin-installer            # dist/waired-darwin-{amd64,arm64}.tar.gz + .sha256
( cd dist && python3 -m http.server 8771 & )
WAIRED_INSTALL_BASE_URL=http://127.0.0.1:8771 \
    sh packaging/install/install.sh --dry-run
```

A real local run additionally takes `WAIRED_DARWIN_BINDIR=<writable dir>`
(to avoid the `sudo` copy) and `WAIRED_NO_OLLAMA=1` (to skip the ~160 MB
Ollama.app download); the launchd round-trip itself is covered by
`WAIRED_LAUNCHD_REALHOST=1 go test ./internal/platform/service/`.

Windows — invoke the script with `-DryRun` from a local copy
(`iwr | iex` cannot pass `-DryRun` directly because the pipeline
fetches the script body as text). Either run against a checkout:

```powershell
powershell -ExecutionPolicy Bypass -File packaging\install\install.ps1 -DryRun
```

…or against a tagged release (downloads the asset list, verifies the
hash, then prints what it *would* install):

```powershell
$f = "$env:TEMP\waired-install.ps1"
iwr -useb https://github.com/waired-ai/waired-agent/releases/latest/download/install.ps1 -OutFile $f
& $f -DryRun
```

The dry-run prints every download / extract / `sc.exe` / `Stop-Service`
without executing it.

Windows — AMSI / Defender pre-publish scan. The one-liner pipes the
fetched script into `iex`, which hands the whole body to Windows
Defender's AMSI. A loader-shaped literal in the body (e.g. a contiguous
`iwr … [ScriptBlock]::Create(…)` download-and-execute cradle) can get the
*entire* script blocked with "This script contains malicious content"
(this happened — see `#552`; the fix routes self-elevation and the Ollama
helper through a temp `.ps1` + `-File` / call-operator instead). Before
publishing, scan the scripts through the real AMSI engine on a
Defender-enabled box with `scripts/dev/amsi-scan.ps1` — it calls the same
`AmsiScanString` verdict path `iex` consults (app name `PowerShell`, so
Defender's PowerShell-script signatures are in scope) without executing the
installer, and guards the result with Microsoft's AMSI test sample as a
positive control so a box with the AMSI provider off can't report a false-green:

```powershell
# Scans packaging/install/install.ps1 + scripts/install/ollama-windows.ps1.
# Exit 2 on a detection (-Strict); exit 3 if there is no live AMSI provider
# (-OnNoProvider fail). Drop -Strict / use -OnNoProvider skip for advisory.
pwsh -File scripts/dev/amsi-scan.ps1 -Strict
```

This is the same tool the CI gates run: Gate A (`.github/workflows/amsi-scan.yml`,
a windows runner) and Gate B (the Defender-live canary step in `installtest.yml`
on the #547 golden) — see `#553`.

For pinpointing *which* line trips a detection, the community tools
**AMSITrigger** / **ThreatCheck** / **DefenderCheck** bisect the file
against the engine. A static file scan
(`& "$env:ProgramFiles\Windows Defender\MpCmdRun.exe" -Scan -ScanType 3 -File <abs>`)
is a cheaper first pass but is a *content* scan, not the AMSI `iex`
context, so it can miss execution-context-only verdicts.

**Caveat:** AMSI verdicts depend on the Defender **engine + signature
version** and **cloud-delivered protection**, so a clean local scan
reduces risk but does not *guarantee* every end-user machine passes, and
a later definitions update can re-flag a script that passed before. The
durable guarantee is **Authenticode signing** (deferred — see
`docs/decisions.md`). CI coverage is the two gates above (`#553`); both stay
advisory / soft-fail for the same non-determinism reason.
