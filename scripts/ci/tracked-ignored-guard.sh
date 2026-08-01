#!/usr/bin/env bash
# tracked-ignored-guard.sh — no file may be tracked by git and matched by an
# ignore rule at the same time.
#
# A 20MB compiled `waired` (a stray `go build ./cmd/waired` with no -o) sat
# tracked at the repo root for two weeks (#90). `.gitignore` named it the whole
# time, but gitignore has no effect on a path git already tracks, so the entry
# read as protection while providing none: every root-level `go build` rewrote
# the tracked file, and the next `git add -A` swept a fresh ~20MB blob into the
# commit. That happened three times (#17, #56, #100), and this is a public repo,
# so every one of them is permanent.
#
# The contradiction is the whole signal — an ignore rule naming a tracked path
# means either the rule is a lie or the file should not be in the tree. This
# fails lint on both, which is the one check `.gitignore` cannot perform for
# itself.
#
# Usage: tracked-ignored-guard.sh [repo-root]   (default: this repository)
#
# `--exclude-standard` also honours core.excludesFile, so a local run can see a
# developer's global ignores; CI, which has none, is the authority.
set -euo pipefail

root="${1:-$(cd "$(dirname "$0")/../.." && pwd)}"

# A renamed or missing root is an error, not a silent pass.
if ! git -C "${root}" rev-parse --git-dir >/dev/null 2>&1; then
  echo "::error::tracked-ignored-guard: not a git repository: ${root}" >&2
  exit 1
fi

# -c lists tracked files; -i narrows that to the ones an ignore rule matches.
offenders="$(git -C "${root}" ls-files -c -i --exclude-standard)"

if [ -n "${offenders}" ]; then
  echo "::error::tracked files are also gitignored (regression of #90) — untrack each with \`git rm --cached <path>\`, or drop the ignore rule that names it:" >&2
  printf '%s\n' "${offenders}" | sed 's/^/  /' >&2
  exit 1
fi

echo "tracked-ignored-guard: ok — no tracked file is gitignored"
