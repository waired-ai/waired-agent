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
* **Concurrent proto development** (CI-enforced;
  `docs/decisions/20260719/0000-concurrent-proto-development.md`):
  proto changes ship as their **own small PR**, only after the tracking
  issue carries a settled wire-contract field table. **Additive-only**
  vs the latest tag (`proto-guard`): no removals / retypes / retags /
  const-value or signature changes; fields added to published structs
  must be `omitempty` (or `json:"-"`); pin byte-identity with a
  canonical-JSON test. Tags are cut automatically per proto merge
  (`proto-tag.yml`; patch — minor/major via workflow_dispatch). While
  iterating, never depend on unmerged proto: in-tree `replace` here, a
  temporary `replace` or merged-main pseudo-version (never a branch
  hash) in the CP repo. Gate new map fields behind a `proto/signer`
  capability constant (e.g. `CapabilityPublicShareV1`).

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

## Test discipline

Most of the 2026-07 install-review escapes were test *shape*, not missing
tests (waired#932 G7):

* **Put the seam below the behaviour under test.** Route OS/environment
  decisions through an untagged `(GOOS, facts) -> plan` function and
  table-test all three values — `initStateDirMode` is the model, and this
  is now the default, not a suggestion. A fake placed at the defect
  boundary means the subject never runs.
* **Fakes take and record the real arguments.** A fake that drops a
  parameter is a defect: it makes the failing case unwritable. Corollary:
  a `var xFn = realFn` seam needs a table test on `realFn`, or the real
  one is never called by any test.
* **Declare pins.** A test that pins behaviour states in a comment
  whether it is a product contract or a record of today's behaviour —
  and a product contract cites its ratifying source (issue,
  decision-log entry, or owner comment; §Vocabulary and provenance).
  Without one it is a record of today's behaviour. A PR
  that inverts an existing test says so in the PR body first.
* **Seal machine-global state in `TestMain`, not per test** (#386). A
  clean CI runner hides every dependency on the developer's machine: the
  OS well-known binary paths (`download.SwapCandidatesForTest(nil)`), the
  machine-wide Claude Code settings (`claudemanaged.SwapPathForTest`),
  and `$HOME` — `os.UserCacheDir` reads `HOME` on darwin, `LocalAppData`
  on Windows and `XDG_CACHE_HOME` elsewhere, so sealing one of the three
  seals nothing on the other two. Put the swap in a package
  `seams_test.go` `TestMain`; an opt-in helper only protects the tests
  that remember to call it. Where the sealed thing is itself shared
  across a package's tests, also take a fresh one per test. Never dodge
  host state with
  `if err == nil { t.Skip("host has …") }`: that cannot tell a
  contaminated host from a subject that wrongly succeeded, and it
  disables the assertion precisely on the machine editing the code.
* **A guard's exemption is keyed at the site, not by position** (#1103).
  Put the reason on the line it excuses — `// grey: <why>`
  (`internal/gui/tray/tray.go`), `// glyph: <why>` (`cmd/waired`),
  `//nolint:<linter> // reason` — or key it by something an edit cannot
  move (path + expression as in `scripts/ci/mgmtclientguard`, type +
  field name as in `scripts/ci/protoconsumer`). A `file:line` key is a
  coordinate nobody decided: the glyph guard's single entry was
  re-derived by all three commits that touched it after it was
  introduced, always as collateral in an unrelated change, and it
  collected a rebase conflict for every concurrent lane that grew the
  file above it. Check both directions — an exemption that matches no
  site is a stale claim, and nothing was reading it.

## Vocabulary and provenance

Agent-coined terms have propagated through docs and issues until they
read as ratified policy (waired#1056). The rules
below bind chat — what a session says — as much as docs, issues,
comments, and code. A term is coined in chat first and the docs copy
it; nothing reviews chat, so the rule is the only control there.

* Use established engineering terms. Do not coin new ones (product
  proper nouns excepted). The test: is this the term a practitioner
  outside the waired project would use, or one the glossary /
  TRANSLATION.md rulings pin? If neither, do not say or write it. If
  a concept truly needs a name, prefer a plain-word phrase and define
  it at first use.
* A term's presence in this project's own docs, issues, or session
  memory is not evidence that it is correct. Frequency inside the
  repos proves nothing, and the dated records under `docs/decisions`
  and `docs/knowledges` stay frozen on purpose, so a banned term keeps
  reading as house vocabulary there. Bare 「窓」 for context window was
  banned twice (#473 §4, waired#1072 §3) and kept re-entering sessions
  from the records that preserve it; the term is 「コンテキストウィンドウ」.
* Normative wording ("contract", "must never", policy claims) requires
  its ratifying source — an issue, decision-log entry, or owner
  comment — cited inline. No citable source → write it as a record of
  today's behaviour, not a rule.
* Term rulings live where the next writer looks — and that is where to
  check a doubtful term before using it: here,
  `docs-site/TRANSLATION.md` (ja mirror terms; private monorepo terms
  go in its dev-docs glossary's deprecated-vocabulary table). A ruling
  is done only when recorded there. Dated records stay frozen — the
  table maps old → current.
* Docs quote product output verbatim — a wording fix changes the
  product string (and its docs together), never the quote alone.

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

* Unless the user explicitly says to work on `main`, work in an
  isolated worktree:
    ```sh
    git -C <repo-root> worktree add <abs-repo>/.claude/worktrees/<topic> \
        -b <type>/<issue>-<short-description> origin/main
    ```
  then `EnterWorktree(path: <abs path>)` to pin the session cwd there.
  Remove with `git worktree remove` once merged or abandoned
  (`ExitWorktree` only removes its own). `.worktrees/` is legacy.
* **Never squash onto a branch name** — `git reset --soft origin/main`
  re-bases on wherever that ref points *now*, staging a concurrent
  session's merge as a revert of its files. Squash against the recorded
  base (`git merge-base HEAD origin/main` → `git reset --mixed <sha>`),
  stage by name, and rebase as a separate step.
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

**Granularity**: default to **one PR per reviewable unit of work** — a
change together with its tests and docs — not one per file, layer, or
commit. Split only for a concrete reason:

* `proto/` contract changes — required, see §Modules,
* a mechanical or generated change that would drown a semantic one,
* a change that must land and be verified before the next can depend on
  it,
* CI cost — a path-gated job (per-PR testnet) that would otherwise
  attach to unrelated work. Say so in the PR body when this is why.

"It could be split" is not a reason. Prefer one PR a reviewer can hold in
their head over three they must reassemble.

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

* Fork PRs only run CI after maintainer approval. CI runs on
  GitHub-hosted runners; only `installtest-inference.yml`'s nightly
  Windows/macOS inference legs and banner check use self-hosted
  hardware, and those are schedule/dispatch-only — never reachable
  from fork PRs. Do not weaken the fork-PR approval policy, and keep
  the DCO job checkout-free (it must give fork PRs feedback without
  executing fork code).
* The real-NAT testnet harness lives in the private monorepo; this
  repo gates on it via `scripts/ci/testnet-require-green-remote.sh`
  (secret `WAIRED_TESTNET_TOKEN`) at three points: per-PR
  (testnet-pr.yml — armed when the diff touches
  `scripts/ci/testnet-relevant-paths.txt`; `run-testnet` label forces;
  fork PRs skip), release tags, and nightly. New `internal/` packages
  must be classified into that list or
  `testnet-nonrelevant-packages.txt` (with reason) —
  `testnet-gate-guard.sh` fails lint until you do.
* The 3-OS install test (`installtest.yml`) runs on EVERY same-repo PR
  (no paths filter; fork PRs get a skip). Windows contract asserts tied
  to open issues soft-fail (WARN) — a fix PR flips the matching
  `$ContractBlocking` line in `scripts/dev/installtest-windows.ps1` to
  make its assert blocking. Nightly: `installtest-inference.yml`
  (inference tail + routing sentinel + banner render check).

## Documentation

* `docs-site/` is the public user help site (docs.waired.ai) — keep it
  current when changing anything a user sees on ANY surface: the CLI
  (commands, flags, prompts, printed wording), the install / first-run
  flow, **the Waired app** (`internal/gui/` — menus, icon states,
  dialogs, status text), the model catalog, troubleshooting. GUI-only is
  not an exemption: on a desktop the app is what the user calls Waired.
  English canonical, `ja/` mirror (CI gates it — see the next bullet). ja
  terminology is pinned in `docs-site/TRANSLATION.md` — follow it,
  never re-derive a term choice while (re)translating a page.
  Internal architecture depth stays in the monorepo's dev-docs-site.
* English canonical means the pair is written together: a PR that changes
  an English page changes its `ja/` counterpart in the same PR
  (`scripts/ci/i18n-pair-guard.sh`). An English edit that needs no
  Japanese one carries a `translation-not-needed: <reason>` line in the
  PR body. `npm run i18n:check` is the separate question of whether the
  mirror is complete and the two sides still have the same shape.
* `docs-guard.yml` enforces both of the above: touching those surfaces
  without `docs-site/` fails unless the PR body carries a
  `docs-not-needed: <reason>` line.

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

* Location: `docs/decisions/YYYYMMDD/HHMM-<slug>.md` — **one file per
  decision**, same layout as Knowledge Notes. Filename: `HHMM` is 24h
  zero-padded (`0000` when the decision carries no time); `<slug>` is
  kebab-case ASCII (≤ ~44 chars, no Japanese); the body stays Japanese.
* Never collect decisions into a single append-only file again — a shared
  reverse-chronological log puts every concurrent PR on the same insertion
  point, and no line-based merge can keep the entries apart.
* Update previous decisions when they change; edit the file in place.
* Cross-references use the file path directly. When one decision replaces
  another, link both ways (`superseded_by` on the old, `supersedes` on the
  new) — `scripts/ci/decision-log-guard.py` fails lint on a one-way or
  dangling link, and on front-matter that disagrees with `## Status`.
  Link partial supersessions too — `## Status` says which parts, and
  `status:` follows that prose, so a partly-superseded decision stays
  `accepted` and just gains `superseded_by`.
* Prefer concise entries that explain context, decision, and
  consequences.

```markdown
---
status: accepted          # accepted | superseded | rejected | deferred
superseded_by:            # optional, repo-relative paths
  - docs/decisions/YYYYMMDD/HHMM-<slug>.md
supersedes:               # optional, the mirror of the above
  - docs/decisions/YYYYMMDD/HHMM-<slug>.md
---

# Title (YYYYMMDD HH:MM)

## Status
Accepted | Superseded | Rejected | Deferred

## Context

## Decision

## Consequences

## Refs
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
