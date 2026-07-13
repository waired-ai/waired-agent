# Repository Rules (waired-agent)

This is the authoritative repository for Waired's client code. It is
**public**: never commit tokens, keys, real device identifiers, or
captured enrollment payloads — including in test fixtures. CI runs a
gitleaks secret scan (config: `.gitleaks.toml`).

## Modules

* Root module `github.com/waired-ai/waired-agent` — binaries only;
  builds against the in-tree `proto/` via a permanent `replace`.
* Nested module `github.com/waired-ai/waired-agent/proto` — the shared
  wire-protocol contract imported by the private control plane and
  relay. Dependency allowlist (CI-enforced): stdlib +
  `golang.org/x/crypto` (+ its `golang.org/x/sys` transitive), nothing
  else. Packages must remain outside any `internal/` path.
* Protocol changes are public-first: change `proto/` here → tag
  `proto/vX.Y.Z` → bump in the CP repo. Never break verify/sign
  compatibility within a published version.

## Tags / releases

* `v*` — agent releases (never directory-prefixed). Pushing the tag
  runs release.yml: cross-repo testnet gate against the private
  monorepo, 4-OS build matrix, APT publish, and a GitHub Release whose
  assets are the public download point
  (`/releases/latest/download/install.sh`).
* Every merge to `main` republishes the moving `edge` prerelease
  (edge.yml); `docs-site/**` changes deploy https://docs.waired.ai/
  (deploy-docs.yml).
* `proto/vX.Y.Z` — proto module versions (Go subdirectory tag scheme).

## Commits / checks

* DCO: every commit needs a `Signed-off-by` trailer — commit with
  `git commit -s` (CI-enforced; rebase recipe in CONTRIBUTING.md).
* Before push, run the checks in CONTRIBUTING.md §"Building and
  testing" — they mirror ci.yml's lint / unit / build jobs.

## Public-repo cautions

* Fork PRs only run CI after maintainer approval (they would otherwise
  execute on the self-hosted `sv-mag-agent` runners). Do not weaken the
  fork-PR approval policy or move the DCO / gitleaks jobs off
  GitHub-hosted runners.
* The real-NAT testnet harness lives in the private monorepo; agent
  releases are gated on it via
  `scripts/ci/testnet-require-green-remote.sh` (secret
  `WAIRED_TESTNET_TOKEN`). For cross-repo behaviour changes
  (enrollment, disco/punch, relay fallback, proto), dispatch a
  monorepo-side testnet run with `agent_ref=<your sha>` before merging
  when in doubt.

## Documentation

* `docs-site/` is the public user help site (docs.waired.ai) — keep it
  current when changing anything a user sees (CLI flags, install flow,
  model catalog, troubleshooting). English canonical, `ja/` mirror.
  Internal architecture depth stays in the monorepo's dev-docs-site.
