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
```

`make verify-cross` matters because CI's test jobs run on Linux only:
it cross-vets the tree for Windows and macOS so single-OS breakage is
caught before push. When you change OS-specific behavior (paths,
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
changes to it follow the public-first flow described in the README.
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

PRs touching mesh / enrollment / `proto/` paths normally also run a
real-NAT testnet gate (`testnet-pr.yml`), but it is skipped for fork
PRs — the cross-repo dispatch credential is not available to forks. A
maintainer runs it after review (by pushing your branch to this repo or
dispatching the private harness manually); you don't need to do
anything.

The same applies to the 3-OS install test (`installtest.yml`): it runs
on every same-repo PR but is skipped for fork PRs (it needs the cloud
enroll identity, which is withheld from forks). A maintainer arms it
the same way after review.
