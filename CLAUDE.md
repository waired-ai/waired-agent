# Repository Rules (waired-agent)

This is the authoritative repository for Waired's client code. It is
**public**: never commit tokens, keys, real device identifiers, or
captured enrollment payloads — including in test fixtures. CI runs a
gitleaks secret scan (config: `.gitleaks.toml`).

## Session start

* At the start of every new session, pull `origin/main`
  (`git fetch origin && git pull --ff-only origin main` on `main`;
  update / rebase from `main` on a topic branch). Check `git status`
  and `git branch --show-current` first to confirm where you are.
  Never start implementation work from a stale base.

## Workflow

* Prefer writing or updating tests before implementation.
* At each meaningful work boundary, run relevant tests, update
  documentation, and git commit.
* If tests are skipped or test-first work is not practical, briefly
  record why in the PR body.
* Keep implementation, tests, and documentation clean. Periodically
  remove obsolete files unless they should remain as historical
  context.

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

## Cross-OS parity (linux / windows / darwin)

Most regressions to date were one OS silently behaving differently
(waired#746–#758):

* Prefer portable code. In shared (untagged) files: no direct
  `os.Geteuid()` (-1 on Windows — `== 0` gates go dead), no hardcoded
  `/etc`-style paths, no `path.Join` on filesystem paths. Route
  OS-varying decisions through a function taking `runtime.GOOS`, with
  a table test over all three values (see `initStateDirMode` +
  cmd/waired/init_defaults_test.go).
* Unavoidable per-OS code (state dirs, systemd/launchd/SCM, registry,
  autostart) goes in `_windows.go`/`_linux.go`/`_darwin.go` files,
  preferably under `internal/platform/`; a new set must cover all
  three OSes (impl, or a stub whose behavior is stated in a comment).
  For "both Unixes" tag `linux || darwin`, not `!windows`.
* A one-OS feature or fix is **not done** until the other two are
  checked and either changed in the same PR or covered by an
  OS-labeled issue saying why deferred / not applicable.
* install.sh/uninstall.sh changes mirror to install.ps1/uninstall.ps1
  (and waired-setup.iss where applicable), and vice versa.

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

## Branching and concurrent development

* Unless the user explicitly says to work on `main`, create a branch via
  `git worktree` and make changes there:
    ```sh
    git worktree add .worktrees/<topic> -b <type>/<issue>-<short-description>
    cd .worktrees/<topic>
    ```
  Clean up the worktree (`git worktree remove`) once the branch has been
  merged or abandoned.
* **Branch naming** — `<type>/<issue>-<short-description>` (kebab-case,
  lowercase): `<type>` ∈ `feat` / `fix` / `docs` / `refactor` / `test` /
  `ci` / `build` / `chore` / `perf`; issue number right after the prefix
  (e.g. `fix/42-windows-service-restart`), omitted when there is no
  tracking issue.
* Multiple developers and AI agents may be operating against this same
  local checkout in parallel. Watch for signs that files you are touching
  are being modified concurrently (unexpected `git status` entries, mtime
  changes on files you did not edit, other in-flight worktrees on the same
  paths). If you see such signs, **stop immediately**, surface the
  conflict to the user, and do not overwrite concurrent work.

## Commits / checks

* DCO: every commit needs a `Signed-off-by` trailer — commit with
  `git commit -s` (CI-enforced; rebase recipe in CONTRIBUTING.md).
* Before push, run the checks in CONTRIBUTING.md §"Building and
  testing" — they mirror ci.yml's lint / unit / build jobs.

## Submitting a PR

When a unit of work is complete and the local checks above pass, open a
pull request via `gh pr create` — don't leave the branch sitting on the
remote. Link the resolving issue with `Fixes #N` when applicable.

After `gh pr create` (or any push that updates an open PR), verify both
of the following before handing off — passing local checks is necessary
but not sufficient:

* **Conflicts**: `gh pr view <PR#> --json mergeable,mergeStateStatus`
  must show `MERGEABLE`; resolve conflicts against the base branch
  immediately (`UNKNOWN` = still computing; wait and re-query).
* **CI**: `gh pr checks <PR#> --watch` until all required checks pass.
  If a check fails, investigate and push a fix on the same branch — do
  not hand off a red PR.

## Work Log (PR body + commit messages)

The work narrative lives in the **PR body** and the **squash commit
message**, not in repo files.

* PR body: motivation, work performed, results/verification, and refs
  (issues, knowledge notes, key source paths). Update it as work
  progresses.
* Squash commit message: substantive (what + why), so `git log --grep`
  works as the offline, in-clone search over past work.
* Digging up past context: `git log --grep '<keyword>'` (or
  `git log -- <path>`) → take the `(#N)` suffix → `gh pr view N` for
  the full narrative.

## Public-repo cautions

* Fork PRs only run CI after maintainer approval (they would otherwise
  execute on the self-hosted `sv-mag-agent` runners). Do not weaken the
  fork-PR approval policy or move the DCO / gitleaks jobs off
  GitHub-hosted runners.
* The real-NAT testnet harness lives in the private monorepo; this
  repo gates on it via `scripts/ci/testnet-require-green-remote.sh`
  (secret `WAIRED_TESTNET_TOKEN`) at three points: per-PR
  (testnet-pr.yml — armed when the diff touches
  `scripts/ci/testnet-relevant-paths.txt`; `run-testnet` label forces;
  fork PRs skip), release tags, and nightly. New `internal/` packages
  must be classified into that list or
  `testnet-nonrelevant-packages.txt` (with reason) —
  `testnet-gate-guard.sh` fails lint until you do.

## Documentation

* `docs-site/` is the public user help site (docs.waired.ai) — keep it
  current when changing anything a user sees (CLI flags, install flow,
  model catalog, troubleshooting). English canonical, `ja/` mirror.
  Internal architecture depth stays in the monorepo's dev-docs-site.

## Knowledge Notes

Knowledge notes are optional. Use them for useful findings discovered
during work, especially repository-specific details or information not
obvious from public documentation or prior knowledge. **This repo is
public** — no secrets, real device identifiers, internal hostnames, or
private-infra details in notes.

* Location: `docs/knowledges/YYYYMMDD/HHMM-<slug>.md` — one file per
  note. Filename: `HHMM` is 24h zero-padded; `<slug>` is kebab-case
  ASCII (≤ ~40 chars, no Japanese); the body stays Japanese.
* Cross-references use the file path directly.
* Corrections: If a recent note is wrong, correct it in place and add a
  short correction note inside the same file.

```markdown
# Title (YYYYMMDD HH:MM)

## Issue

## Learnings

## Refs
- https://example.com
- https://github.com/waired-ai/waired-agent/pull/NNN
```

## Decision Log

Decision records are optional. Use them for meaningful technical,
architectural, or operational decisions made during work. The same
public-repo caution as Knowledge Notes applies.

* Location: `docs/decisions.md` (append new entries at the top)
* Update previous decisions when they change.
* Prefer concise entries that explain context, decision, and
  consequences.

```markdown
## Title (YYYYMMDD HH:MM)

### Status
Accepted | Superseded | Rejected | Deferred

### Context

### Decision

### Consequences

### Refs
- PR / issue links
```

## TODO / Deferred Issues

Track follow-ups and scope cuts that surface during implementation as
**GitHub Issues** (<https://github.com/waired-ai/waired-agent/issues>).

* Label new issues with the matching **component** label (`agent` /
  `installer` / `inference` / `ci` / `doc`) and add `actionable` once
  scope and approach are clear enough for a coding agent to start
  without user input.
* Primary intake for new coding-agent work:
  `gh issue list --state open --label actionable`.
* Close from the resolving PR via `Fixes #N`, or manually with
  `--reason completed` and a comment pointing at the PR / commit.

## Cleanup

Regularly remove obsolete implementation code, tests, scripts, and
documentation. Keep materials that are useful for historical context,
migration history, or explaining past decisions. If cleanup removes
something non-trivial, mention it in the PR body.

## Ambiguity

When requirements are ambiguous, make a small, safe, reversible
assumption and record it. Ask for clarification only when the ambiguity
could cause destructive, security-sensitive, or large architectural
consequences.
