# waired-agent

Client-side source of [Waired](https://waired.ai): the `waired` CLI, the
`waired-agent` daemon (WireGuard mesh, NAT traversal, local inference
routing), the desktop tray, installers/packaging, and the shared
protocol module `github.com/waired-ai/waired-agent/proto` that the
control plane imports.

> **⚠️ NON-AUTHORITATIVE — staging snapshot**
>
> Until the #184 cutover completes in the private `waired-ai/waired`
> monorepo, **that monorepo remains the single authoritative source for
> all client code**. This repository is a populated staging snapshot:
> do not land functional client changes here — they will be overwritten
> by the next resync (`scripts/dev/split-agent-repo/split.sh` in the
> monorepo, which may force-reset this history). Repo-infrastructure
> PRs (workflows, README, lint/gitleaks config) are fine.

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
3. Bump the module in the private CP repo.

Release tags: `v*` = agent/installer releases (stable channel);
`proto/v*` = protocol module versions. The two never overlap.
