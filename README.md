# waired-agent

Client-side source of [Waired](https://waired.ai): the `waired` CLI, the
`waired-agent` daemon (mesh networking, NAT traversal, local inference
routing), the desktop tray, installers/packaging, and the shared
protocol module `github.com/waired-ai/waired-agent/proto` that the
control plane imports.

User documentation: <https://docs.waired.ai/> (authored under
`docs-site/` in this repo).

## Install

```sh
# Linux / macOS
curl -fsSL https://github.com/waired-ai/waired-agent/releases/latest/download/install.sh | sh
```

```powershell
# Windows
iwr -useb https://github.com/waired-ai/waired-agent/releases/latest/download/install.ps1 | iex
```

### Edge channel (latest `main` build, unstable)

Rebuilt on every merge to `main` — not for production use.

```sh
# Linux / macOS
curl -fsSL https://github.com/waired-ai/waired-agent/releases/latest/download/install.sh | sh -s -- --edge
```

```powershell
# Windows
$env:WAIRED_VERSION = 'edge'
iwr -useb https://github.com/waired-ai/waired-agent/releases/latest/download/install.ps1 | iex
```

Once on edge, `waired update` stays on edge; switch channels with
`waired update --edge` / `--stable`.

### Uninstall

Removes the binaries, unregisters the service, and (best-effort)
deregisters the device from your account. Local config/state is kept.

```sh
# Linux / macOS
curl -fsSL https://github.com/waired-ai/waired-agent/releases/latest/download/uninstall.sh | sh
```

```powershell
# Windows
iwr -useb https://github.com/waired-ai/waired-agent/releases/latest/download/uninstall.ps1 | iex
```

For a **clean (full-wipe) uninstall** — also delete config, keys, state,
and the bundled Ollama with its models — use `--clean` / `-Clean`
(destructive; asks to confirm, `--yes` / `-Yes` skips the prompt):

```sh
# Linux / macOS
curl -fsSL https://github.com/waired-ai/waired-agent/releases/latest/download/uninstall.sh | sh -s -- --clean
```

```powershell
# Windows — two steps: the piped iex form can't pass -Clean, so save the script first
iwr -useb https://github.com/waired-ai/waired-agent/releases/latest/download/uninstall.ps1 -OutFile $env:TEMP\uninstall.ps1
& $env:TEMP\uninstall.ps1 -Clean
```

### Clean install (full wipe, then reinstall)

One command runs the clean uninstall above and then a fresh install. It
asks to confirm before wiping (`--yes` / `-Yes` skips; Windows shows two
UAC prompts — one for the wipe, one for the install).

```sh
# Linux / macOS
curl -fsSL https://github.com/waired-ai/waired-agent/releases/latest/download/install.sh | sh -s -- --clean
```

```powershell
# Windows (the piped form can't bind -Clean, so use the env var)
$env:WAIRED_CLEAN = '1'
iwr -useb https://github.com/waired-ai/waired-agent/releases/latest/download/install.ps1 | iex
```

### Install options

Linux and macOS use `install.sh`; Windows uses `install.ps1` with the
PowerShell spelling of the same option. The environment-variable form is
how the piped one-liner passes an option — the Windows `iwr … | iex`
form **cannot bind parameters**, so either set the env var or save the
script to disk first and run it with the flag.

| `install.sh` | `install.ps1` | Environment variable | What it does |
|---|---|---|---|
| `--no-init` | `-SkipInit` | | Do **not** run `waired init` after installing. Without this, installing signs you in and sets you up in one pass. |
| `--skip-ollama` | `-SkipOllama` | `WAIRED_NO_OLLAMA=1` | Do not install the AI software. Use it when you already run your own Ollama. |
| `--skip-claude-proxy` | `-SkipClaudeProxy` | `WAIRED_NO_CLAUDE_PROXY=1` | Leave Claude Code pointed at the Anthropic API instead of your own AI. |
| `--log-level <level>` | `-LogLevel <level>` | `WAIRED_LOG_LEVEL` | Start the agent at this log detail: `debug`, `info` (default), `warn` or `error`. Change it later without reinstalling via `waired config log-level`. |
| `--mask-pii` | `-MaskPII` | `WAIRED_PII_MASK=1` | Hide your home folder, username, machine name and account email in the output, for screenshots and bug reports. Best-effort. |
| `--dry-run` | `-DryRun` | | Print every privileged command without running any of them. |
| `--yes`, `-y` | `-Yes` | | Assume yes at every prompt, including the pre-install summary. |
| `--check` | `-Check` | | Report whether a newer version is available; change nothing. |
| `--update` | `-Update` | | Update an existing install rather than installing fresh. |
| `--edge`, `--latest` | `-Edge`, `-Latest` | `WAIRED_VERSION=edge` | Install or switch to the latest `main` build. Not a stable release. |
| `--stable` | `-Stable` | | Install or switch to the latest stable release. |
| `--clean` | `-Clean` | `WAIRED_CLEAN=1` | Wipe everything first, then install fresh. Destructive; asks to confirm unless `--yes`. |
| `--control <URL>` | `-Control <URL>` | `WAIRED_CONTROL_URL` | Enroll against a specific control plane. |
| `--dev` | `-Dev` | | The built-in development control plane. For Waired development only. |
| | `-InstallDir <path>` | `WAIRED_INSTALL_DIR` | Where to install (Windows). |
| | | `WAIRED_VERSION` | Pin an exact release, e.g. `1.2.3`. |
| | | `WAIRED_NO_TRAY=1` | Do not install `waired-tray` (headless hosts). |

`uninstall.sh` / `uninstall.ps1` take `--clean`, `--yes`, `--dry-run` and
`--mask-pii`.

The scripts' own `--help` / `-Help` output is authoritative. See also
[docs.waired.ai](https://docs.waired.ai/) — [Advanced install
options](https://docs.waired.ai/reference/install-options/) and [CLI
commands](https://docs.waired.ai/reference/cli/) — and
`packaging/install/README.md`.

## Layout

```
cmd/waired         CLI
cmd/waired-agent   agent daemon
cmd/waired-tray    desktop tray
cmd/catalog-tool   model-catalog tooling
internal/          agent implementation (not importable cross-module)
proto/             shared protocol Go module (imported by the CP)
packaging/         install.sh / install.ps1, nfpm, systemd, Inno Setup
scripts/           install helpers, CI guards, dev install-test harnesses
docs-site/         public user documentation (docs.waired.ai)
```

## Build / test

```sh
go build ./... && go vet ./...
go test ./...
(cd proto && go test ./...)
make build-agent build-tray      # linux/amd64 into ./bin/
make verify-cross                # GOOS={linux,windows,darwin} go vet
```

## Protocol changes (public-first)

1. Revise `proto/` here, land the change.
2. Tag `proto/vX.Y.Z`.
3. Bump the module in the private control-plane repo.

Release tags: `v*` = agent/installer releases (stable channel);
`proto/v*` = protocol module versions. The two never overlap.

## Contributing

External issues and pull requests are welcome and reviewed on a
**best-effort** basis — see [CONTRIBUTING.md](CONTRIBUTING.md). Every
commit must carry a DCO `Signed-off-by` trailer (`git commit -s`).
Security reports go through [SECURITY.md](SECURITY.md), not public
issues.

## License

Apache-2.0 — see [LICENSE](LICENSE). Release artifacts bundle the
third-party license notices as `THIRD_PARTY_LICENSES`.

Waired uses the WireGuard® protocol via
[wireguard-go](https://git.zx2c4.com/wireguard-go/). "WireGuard" and the
"WireGuard" logo are registered trademarks of Jason A. Donenfeld; Waired
is not sponsored or endorsed by the WireGuard project. Ollama, vLLM,
Claude Code, and OpenCode are trademarks of their respective owners;
Waired integrates with them but is not affiliated with or endorsed by
their vendors.
