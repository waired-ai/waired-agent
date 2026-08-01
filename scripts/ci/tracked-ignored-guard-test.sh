#!/usr/bin/env bash
# Self-test for tracked-ignored-guard.sh. A green tree only ever exercises
# the guard's pass path, so the case it exists to catch — a tracked file an
# ignore rule also matches — is reproduced here on a throwaway repository.
set -euo pipefail

guard="$(cd "$(dirname "$0")" && pwd)/tracked-ignored-guard.sh"
tmp=$(mktemp -d)
trap 'rm -rf "${tmp}"' EXIT

fail=0
check() { # check <expected: pass|fail> <label> <root>
  local want="$1" label="$2" root="$3" got
  if bash "${guard}" "${root}" >/dev/null 2>&1; then got=pass; else got=fail; fi
  if [ "${got}" = "${want}" ]; then
    echo "ok   ${label} (${got})"
  else
    echo "FAIL ${label}: want ${want}, got ${got}"
    fail=1
  fi
}

repo="${tmp}/repo"
mkdir -p "${repo}"
git -C "${repo}" init -q
echo 'package main' >"${repo}/main.go"
git -C "${repo}" add main.go
check pass "repository with nothing ignored" "${repo}"

# The #90 shape exactly: the build output was committed first, the ignore
# rule added afterwards. `git add` needs no -f, which is the reason this
# lands silently — the rule that would have stopped it did not exist yet.
printf 'ELF-ish\n' >"${repo}/waired"
git -C "${repo}" add waired
printf '/waired\n' >"${repo}/.gitignore"
git -C "${repo}" add .gitignore
check fail "tracked file matched by the root .gitignore" "${repo}"

# Untracking is the fix, and the file staying on disk is not a violation:
# ignored-and-untracked is what a build output is supposed to look like.
git -C "${repo}" rm -q --cached waired
check pass "same file once untracked, still on disk" "${repo}"

# Nested ignore files count too — docs-site/ carries its own, so a guard
# that only read the root one would miss half the repository.
mkdir -p "${repo}/sub"
printf '/out\n' >"${repo}/sub/.gitignore"
: >"${repo}/sub/out"
git -C "${repo}" add -f sub/out sub/.gitignore
check fail "tracked file matched by a nested .gitignore" "${repo}"
git -C "${repo}" rm -q --cached sub/out
check pass "nested case once untracked" "${repo}"

# A renamed or deleted root must not quietly disable the guard.
check fail "path that is not a git repository" "${tmp}/nope"

# And the real tree, which must be green.
check pass "this repository" "$(cd "$(dirname "$0")/../.." && pwd)"

exit "${fail}"
