#!/usr/bin/env bash
# Self-test for installer-mirror-guard.sh. It exercises decide() with a
# changed-file list on stdin, so no git state, no PR and no network are
# involved — the decision is the thing under test.
set -euo pipefail

# shellcheck source=scripts/ci/installer-mirror-guard.sh
source "$(cd "$(dirname "$0")" && pwd)/installer-mirror-guard.sh"

fail=0
check() { # check <expected: pass|fail> <label> <body> <changed files...>
  local want="$1" label="$2" body="$3" got
  shift 3
  if printf '%s\n' "$@" | PR_BODY="${body}" decide >/dev/null 2>&1; then got=pass; else got=fail; fi
  if [ "${got}" = "${want}" ]; then
    echo "ok   ${label} (${got})"
  else
    echo "FAIL ${label}: want ${want}, got ${got}"
    fail=1
  fi
}

sh=packaging/install/install.sh
ps=packaging/install/install.ps1
unsh=packaging/install/uninstall.sh
unps=packaging/install/uninstall.ps1

check pass "neither half touched" "" README.md cmd/waired/main.go
check pass "both halves touched" "" "${sh}" "${ps}"
check fail "install.sh alone" "" "${sh}"
check fail "install.ps1 alone" "" "${ps}"
check fail "uninstall.sh alone" "" "${unsh}"
check fail "uninstall.ps1 alone" "" "${unps}"

# Pairs are independent: a balanced install pair does not excuse an
# unbalanced uninstall pair.
check fail "install balanced, uninstall not" "" "${sh}" "${ps}" "${unsh}"

# The opt-out, and its shape.
check pass "opted out with a reason" "mirror-not-needed: launchd-only teardown" "${sh}"
check pass "opt-out is case-insensitive" "Mirror-Not-Needed: launchd only" "${sh}"
check pass "opt-out amid other body text" \
  "Fixes #1

mirror-not-needed: apt source, no Windows equivalent

More prose." "${sh}"
check fail "opt-out with no reason" "mirror-not-needed:" "${sh}"
check fail "opt-out mentioned but not declared" \
  "I considered whether mirror-not-needed: applies here" "${sh}"

# A path that merely resembles one half must not arm or clear the guard.
check pass "a different install.sh elsewhere" "" docs-site/install.sh

exit "${fail}"
