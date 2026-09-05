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
above, that job runs a couple of dozen standalone guard and self-test
scripts out of `scripts/ci/` — path classifications, seam guards, mirror
checks — and each one fails independently, so satisfying the one whose
error message you happened to see says nothing about the rest. They need
no secrets and no network, but nothing local ran them before, which is
how a PR could pass every command on this list and still take a red
lint. The target derives its list from `ci.yml` so it cannot drift
behind a newly added guard — which is also why the count above is
approximate: a sentence naming an exact number is a copy of the list,
and copies of the list are what this target exists to stop keeping. It needs bash 4+ (two guards use `mapfile`;
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

### The GPU test mandate

If you change vLLM serving — the installer (`internal/runtime/vllm_*`),
the adapter, the serve flags, or the sizing that decides them — and you
have an NVIDIA host, run `make e2e-vllm` before the change ships. It is
the only thing that observes whether the KV pool, the prefill chunk and
the engine's own start-up actually did what the change intended; unit
tests see the argv, not the engine.

```sh
export WAIRED_STATE_DIR=/some/scratch/state   # keeps it off the daemon's
waired runtimes install vllm --yes --state-dir "$WAIRED_STATE_DIR"
make e2e-vllm            # smoke + the realistic AWQ pass
make e2e-vllm-clamp      # #675, the max-model-len clamp
make e2e-vllm-fp8        # #676, fp8 KV on Ada+
make e2e-vllm-spec       # #677, ngram speculative decode
make e2e-vllm-power      # #821, the engine releases the GPU
```

No sudo is needed: `WAIRED_STATE_DIR` is honoured verbatim, so the venv
lands somewhere you own. The host does need `g++` and a CUDA toolkit —
vLLM compiles kernels at engine start — and the installer says so if
they are missing.

Without a GPU these targets cannot run, and nothing in the per-PR gate
substitutes for them. The nightly `installtest-inference` workflow
carries the automated half on an L4 VM it creates per run; that lane
was dormant from 2026-07-24 to 2026-08-21 and ran zero times, which is
why this paragraph exists in four places rather than one (Makefile help,
this file, the e2e source, and the decision itself — see
`waired/docs/decisions/` "GPU テスト実行義務"). One of the four had
already gone missing.

CI additionally runs a license check
(`go-licenses check --disallowed_types=forbidden,restricted`) — a new
dependency with copyleft licensing fails the lint job — and a gitleaks
secret scan (config: `.gitleaks.toml`).

### Adding a model to the catalog

The bundled catalog is `proto/catalog/bundled/*.json`. Until now the only
written-down version of this procedure lived in decision records, and the
one automated path that adds models — `catalog-radar`, which opens a draft
PR per candidate — pointed reviewers at a GPU lane that grades nothing
about a new model.

```sh
catalog-tool compute --repo <hf repo>          # footprint numbers, never hand-typed
catalog-tool draft --spec <spec>               # the manifest
catalog-tool tier --format text                # freeze mode; --rerank is not for this
catalog-tool validate --all
make catalog-docs                              # the docs freshness gate reads this
```

Then measure it. Two gates block the PR, and both want the same run:

```sh
make e2e-agentgrade MODEL=<ollama tag> JSON=/tmp/r.json
catalog-tool agentgrade --import /tmp/r.json --host <hardware class> --retrieved $(date -u +%F)
catalog-tool shapes     --import /tmp/r.json --host <hardware class> --retrieved $(date -u +%F)
```

`--host` names a hardware CLASS, never a machine — this repository is
public. It has to be one of `catalog.HostClasses`
(`internal/catalog/hostclass.go`); adding a class is a line of diff in the
same PR. Neither command takes an engine version: both read it off the
report, which read it off the runtime adapter. Add `--run-url` when the
report came from a CI run — the GPU lane's job summary prints both
commands ready to paste, with the run already filled in.

No GPU to hand? Dispatch the lane and download its report:

```sh
gh workflow run installtest-inference.yml -f os=none -f agentgrade_model=<ollama tag>
```

What the two gates ask:

- `catalog-tool agentgrade --check --require-pass` — can this model drive a
  coding agent's tool-call format? Measured, never asserted from reputation:
  the model that started it advertised `tool_use`, shipped the standard
  template, and handed coding agents raw JSON as prose.
- `catalog-tool shapes --check --require-accepted` — does this model's chat
  template *render* the request shapes real clients send? qwen3.8-27b passed
  the grade above and then failed every real Claude Code turn, because
  Claude Code puts a `role:"system"` at the END of `messages[]` and that
  model's renderer refused it. **A model that refuses a shape is one we do
  not offer**; there is no exemption for it.

A model no runner can host is declared in `agentgrade.json`'s `unmeasurable`
map with a reason, which is a stated decision rather than silence.

The source id is checked for you: the `catalog-sources` lane resolves every
`repo_id` and `tag` against Hugging Face and the ollama registry, because a
repo id can satisfy every string check and not exist — one did, for months.

Two traps worth knowing. The engine pin
(`internal/runtime/ollama_version.go`) gates which models can be pulled at
all; too old and the registry refuses with `412`. And `make e2e-agentgrade`
stamps the harness revision from git, appending `-dirty` for a modified
tree — an import refuses a dirty stamp, so measure from a committed tree.

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

The same check enforces the other half of the documentation rule: English is
canonical and `docs-site/src/content/docs/ja/` mirrors it 1:1, so a PR that
changes an English page writes its Japanese counterpart in the same PR. When an
English edit genuinely needs no Japanese one — a reworded English sentence whose
translation was already right — say so the same way:

```
translation-not-needed: reworded the English only; the ja sentence already says this
```

Terminology in the Japanese mirror is pinned in `docs-site/TRANSLATION.md`;
follow it rather than re-deriving a term choice while translating.

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
