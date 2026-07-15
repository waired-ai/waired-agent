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

Details and all installer flags: [docs.waired.ai](https://docs.waired.ai/)
and `packaging/install/README.md`.

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
