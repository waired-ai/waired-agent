# Repository Rules (waired-agent)

> **NON-AUTHORITATIVE until the #184 cutover** (tracked in the private
> `waired-ai/waired` repo): all functional client code changes land in
> the monorepo, which resyncs this repo via
> `scripts/dev/split-agent-repo/split.sh`. Until then, only
> repo-infrastructure changes (workflows, lint config, README) may be
> made directly here.

## Modules

* Root module `github.com/waired-ai/waired-agent` — binaries only,
  nobody imports it. It builds against the in-tree `proto/` via a
  `replace` directive (permanent by design).
* Nested module `github.com/waired-ai/waired-agent/proto` — the shared
  wire-protocol contract imported by the private control plane and
  relay. Dependency allowlist: **stdlib + `golang.org/x/crypto`**
  (+ its `golang.org/x/sys` transitive) — nothing else; CI enforces
  this. Packages must remain outside any `internal/` path.
* Protocol changes are public-first: change `proto/` here → tag
  `proto/vX.Y.Z` → bump in the CP repo. Never break verify/sign
  compatibility within a published version.

## Tags

* `v*` — agent releases (stable channel installers). Never prefix with
  a directory.
* `proto/vX.Y.Z` — proto module versions (Go subdirectory tag scheme).

## Checks before push

```sh
gofmt -l .                        # must print nothing
go vet ./... && (cd proto && go vet ./...)
golangci-lint run
go test ./... -timeout 10m
(cd proto && go test ./...)
go test -tags prod ./internal/buildflag/...
make verify-cross
```

## Secrets

This repo is slated to become public. Never commit tokens, keys, real
device identifiers, or captured enrollment payloads — including in test
fixtures. `gitleaks` config lives at `.gitleaks.toml`.
