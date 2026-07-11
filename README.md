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

Edge channel (latest `main` build): pass `--edge` / `-Edge` or set
`WAIRED_VERSION=edge`. Uninstall: same URLs with `uninstall.sh` /
`uninstall.ps1`. Details: [docs.waired.ai](https://docs.waired.ai/) and
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
