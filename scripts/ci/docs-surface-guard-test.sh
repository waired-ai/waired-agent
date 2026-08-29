#!/usr/bin/env bash
# Self-test for docs-surface-guard.sh (#1119).
#
# The guard had no self-test, and #1119 changes what it accepts as a docs
# change — not only what it watches. That half is the one worth testing:
# `make catalog-docs` writes a file under docs-site/, so before this change
# the mandatory regeneration step cleared the guard by itself, in exactly
# the case the surface list was being widened for. A test that only
# asserted "a catalog change is now watched" would have passed against
# that hole.
#
# Each case builds a two-commit repository — a base and a head — and runs
# the guard over the diff, which is the shape the workflow gives it.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
guard="${root}/scripts/ci/docs-surface-guard.sh"

fail=0

# check <expected: pass|fail> <label> <PR body> <changed path>...
check() {
  local want="$1" label="$2" body="$3"; shift 3
  local tmp got f
  tmp="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "rm -rf '${tmp}'" RETURN

  ( cd "${tmp}"
    git init -q -b main .
    git config user.email t@example.com
    git config user.name t
    # A base commit holding every path the cases touch, so each head
    # commit is a MODIFICATION and the diff is exactly the argument.
    mkdir -p proto/catalog/bundled internal/catalog internal/agentgrade cmd/waired \
             docs-site/src/data docs-site/src/content/docs/reference docs/reference
    for f in proto/catalog/bundled/m.json internal/catalog/requestshapes.json \
             internal/catalog/agentgrade.json internal/agentgrade/probe.go \
             cmd/waired/main.go docs-site/src/data/model-sizes.json \
             docs-site/package-lock.json \
             docs-site/src/content/docs/reference/model-catalog.mdx \
             docs/reference/models.md README.md; do
      printf 'base\n' > "${f}"
    done
    git add -A . >/dev/null
    git commit -qm base
    # The guard fetches origin/<BASE_REF>; a self-remote makes that real
    # rather than mocked.
    git remote add origin "${tmp}"
    git fetch -q origin main 2>/dev/null || true
    git checkout -q -b head
  )

  for f in "$@"; do printf 'changed\n' > "${tmp}/${f}"; done
  ( cd "${tmp}"
    git add -A . >/dev/null
    git commit -qm head )

  if ( cd "${tmp}" \
       && BASE_REF=main HEAD_SHA="$(git -C "${tmp}" rev-parse HEAD)" PR_BODY="${body}" \
          bash "${guard}" >/dev/null 2>&1 ); then got=pass; else got=fail; fi

  if [ "${got}" = "${want}" ]; then
    echo "ok   ${label} (${got})"
  else
    echo "FAIL ${label}: want ${want}, got ${got}"
    fail=1
  fi
}

# Unwatched paths stay unwatched: the guard must not become a tax on
# everything.
check pass "a change touching no surface at all" "" README.md
check pass "the generated repo-side model table alone" "" docs/reference/models.md

# The surfaces that were already watched still behave.
check fail "the CLI with no docs" "" cmd/waired/main.go
check pass "the CLI with a docs page" "" cmd/waired/main.go docs-site/src/content/docs/reference/model-catalog.mdx

# #1119: the catalog is a surface.
check fail "a bundled manifest with no docs" "" proto/catalog/bundled/m.json
check fail "a store import with no docs" "" internal/catalog/requestshapes.json
check fail "the measurement harness with no docs" "" internal/agentgrade/probe.go
check pass "a manifest with a hand-written docs page" "" \
      proto/catalog/bundled/m.json docs-site/src/content/docs/reference/model-catalog.mdx
check pass "a manifest with the opt-out line" "docs-not-needed: a pure file move" \
      proto/catalog/bundled/m.json

# The half a widened regex alone would have missed.
check fail "a manifest plus ONLY the generated size map" "" \
      proto/catalog/bundled/m.json docs-site/src/data/model-sizes.json
check fail "a manifest plus ONLY a lockfile bump" "" \
      proto/catalog/bundled/m.json docs-site/package-lock.json
check pass "the generated size map alongside a real page" "" \
      proto/catalog/bundled/m.json docs-site/src/data/model-sizes.json \
      docs-site/src/content/docs/reference/model-catalog.mdx

if [ "${fail}" -ne 0 ]; then
  echo "docs-surface-guard-test: FAILED" >&2
  exit 1
fi
echo "docs-surface-guard-test: all passed"
