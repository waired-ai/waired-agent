# Contributing to waired-agent

Thanks for your interest in Waired. This repository is developed by the
Waired team with an open development model: external issues and pull
requests are welcome, and are reviewed on a **best-effort** basis — we
make no response-time promises, and we may decline changes that don't
fit the roadmap. For anything larger than a small fix, please open an
issue first so we can agree on the approach before you invest time.

## Developer Certificate of Origin (DCO)

Every commit must be signed off, certifying the
[Developer Certificate of Origin](https://developercertificate.org/):

```sh
git commit -s
```

This adds a `Signed-off-by: Your Name <you@example.com>` trailer. CI
rejects pull requests containing commits without it. To fix a PR that
failed the DCO check:

```sh
# sign off the last N commits, then force-push your branch
git rebase --signoff HEAD~N
git push --force-with-lease
```

By signing off you certify that you have the right to submit the work
under this repository's license (Apache-2.0) and that you understand
the contribution is public.

## Building and testing

Run these before pushing — the same commands CI runs:

```sh
gofmt -l .                        # must print nothing
go vet ./... && (cd proto && go vet ./...)
golangci-lint run
go test ./... -timeout 10m
(cd proto && go test ./...)
go build -tags prod ./... && go vet -tags prod ./...
go test -tags prod ./internal/buildflag/...
make verify-cross
make ci-lint-local
```

`make ci-lint-local` is the rest of the lint job. Beyond the commands
above, that job runs twenty standalone guard and self-test scripts out
of `scripts/ci/` — path classifications, seam guards, mirror checks —
and each one fails independently, so satisfying the one whose error
message you happened to see says nothing about the other nineteen. They
need no secrets and no network, but nothing local ran them before, which
is how a PR could pass every command on this list and still take a red
lint. The target derives its list from `ci.yml` so it cannot drift
behind a newly added guard. It needs bash 4+ (two guards use `mapfile`;
the `/bin/bash` macOS ships is 3.2 — `brew install bash`).

Two things on this list are easy to *believe* you ran. `golangci-lint`
is not vendored: if it is absent from your machine the command fails
loudly, but "I ran the checks" then covers seven of eight. And the
license check below needs network on first use, since it fetches
`go-licenses`.

`make verify-cross` matters because CI's test jobs run on Linux only:
it runs `go vet` for the Windows and macOS targets so single-OS
breakage is caught before push. When you change OS-specific behavior (paths,
services, registry, installers), keep all three OSes in sync — see
CLAUDE.md §"Cross-OS parity".

CI additionally runs a license check
(`go-licenses check --disallowed_types=forbidden,restricted`) — a new
dependency with copyleft licensing fails the lint job — and a gitleaks
secret scan (config: `.gitleaks.toml`).

## The proto module

`proto/` is a separate Go module — the wire-protocol contract imported
by the private control plane. Its dependency allowlist (stdlib +
`golang.org/x/crypto` + `golang.org/x/sys`) is enforced by CI, and
changes to it follow the public-repo-first release order described in
the README (change `proto/` here, tag `proto/vX.Y.Z`, then bump the
module in the private control-plane repo).
Never break verify/sign compatibility within a published `proto/vX.Y.Z`
version.

## Security issues

Do **not** open public issues for vulnerabilities — follow
[SECURITY.md](SECURITY.md).

## CI notes for external contributors

Pull requests from forks require maintainer approval before workflows
run. CI executes on GitHub-hosted runners (only some nightly jobs use
self-hosted hardware). The DCO and gitleaks checks run without
executing any fork code, so you get that feedback immediately.

`docs-guard.yml` also runs immediately, on every PR. If your change
touches something a user sees — the Waired app, the CLI, the install
scripts — it expects a matching update under `docs-site/`. When the
change really alters nothing a user reads, add a line to the PR body:

```
docs-not-needed: internal refactor, no change to any printed or shown text
```

PRs touching mesh / enrollment / `proto/` paths normally also run the
testnet gate (`testnet-pr.yml`): an integration test that exercises
NAT traversal against real NATs, hosted in a private repository. It is
skipped for fork PRs — the cross-repo dispatch credential is not
available to forks. A maintainer runs it after review (by pushing your
branch to this repo or dispatching the test in the private repository
manually); you don't need to do anything.

The same applies to the 3-OS install test (`installtest.yml`): it runs
on every same-repo PR but is skipped for fork PRs (it needs the
enrollment credential — a repository secret used to register a test
device — which is withheld from forks). A maintainer triggers it the
same way after review.

While the testnet gate is running, do not edit the PR body. The workflow
listens for `edited`, so a body edit restarts `testnet-pr`, and its
concurrency group cancels the run that was already waiting. Filing an
issue and adding the reference to the body counts as an edit; do it
after the gate finishes. Two things make this expensive to diagnose:
the gate is the long pole (a full cycle was measured at about an hour
and a half, most of it teardown), and the failure reads as somebody
else's problem — the job ends with `The operation was canceled.` in
`require green testnet for the PR head`, which looks like an
infrastructure fault rather than a consequence of your own last action.
Re-running does not help if the body is touched again. (Observed
2026-08-15 on #803. Same mechanism seen from the other side: a manual
`gh run rerun` replays the *original* event payload, so it cannot see a
`docs-not-needed:` line that was added afterwards.)
