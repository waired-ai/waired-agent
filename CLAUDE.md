# Repository Rules (waired-agent)

This is the **authoritative** repository for Waired's client code (the
#184 split from the private `waired-ai/waired` monorepo is complete).
It is public: never commit tokens, keys, real device identifiers, or
captured enrollment payloads — including in test fixtures.

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

## Tags / releases

* `v*` — agent releases (stable channel installers). Never prefix with
  a directory. Pushing the tag runs release.yml: a cross-repo testnet
  gate against the private monorepo (real-NAT validation), the 4-OS
  build matrix, APT publish, and a GitHub Release whose assets are the
  public download point (`/releases/latest/download/install.sh`).
* Every merge to `main` republishes the moving `edge` prerelease
  (edge.yml) and, for `docs-site/**` changes, deploys
  https://docs.waired.ai/ (deploy-docs.yml).
* `proto/vX.Y.Z` — proto module versions (Go subdirectory tag scheme).

## Commits (DCO)

Every commit needs a `Signed-off-by` trailer — commit with
`git commit -s`. CI enforces this on pull requests (see
CONTRIBUTING.md for the rebase recipe when a check fails).

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

CI also runs a license check
(`go-licenses check --disallowed_types=forbidden,restricted`); a new
dependency with copyleft licensing fails the lint job.

## Public-repo cautions

* Fork PRs only run CI after maintainer approval (they would otherwise
  execute on the self-hosted `sv-mag-agent` runners). Do not weaken the
  fork-PR approval policy or move the DCO job off GitHub-hosted runners.
* The real-NAT testnet harness lives in the private monorepo; agent
  releases are gated on it via
  `scripts/ci/testnet-require-green-remote.sh` (secret
  `WAIRED_TESTNET_TOKEN`). Cross-repo behaviour changes (enrollment,
  disco/punch, relay fallback, proto) should get a monorepo-side testnet
  dispatch with `agent_ref=<your sha>` before merging when in doubt.

## Documentation

* `docs-site/` is the public user help site (docs.waired.ai) — keep it
  current when changing anything a user sees (CLI flags, install flow,
  model catalog, troubleshooting). English canonical, `ja/` mirror.
* Internal architecture depth stays in the monorepo's dev-docs-site;
  don't document coordination/relay internals here.

## Secrets

Never commit secrets. `gitleaks` config lives at `.gitleaks.toml`; CI
runs it and GitHub push protection is enabled.
