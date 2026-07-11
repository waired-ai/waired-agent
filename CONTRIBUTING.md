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

See the checks list in [CLAUDE.md](CLAUDE.md) — the same commands CI
runs:

```sh
gofmt -l .                        # must print nothing
go vet ./... && (cd proto && go vet ./...)
go test ./... -timeout 10m
(cd proto && go test ./...)
go test -tags prod ./internal/buildflag/...
make verify-cross
```

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
run (most CI executes on our self-hosted runners). The DCO check runs
on GitHub-hosted runners, so you get that feedback immediately.
