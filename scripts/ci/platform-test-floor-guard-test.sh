#!/usr/bin/env bash
# Self-test for platform-test-floor-guard.sh: red on a package with no
# test file, green once one exists, and indifferent to a directory that
# holds no Go code of its own.
set -euo pipefail

guard="$(cd "$(dirname "$0")" && pwd)/platform-test-floor-guard.sh"
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

# A package with an implementation and no test at all — the browser
# package's state through #181/#182/#183.
mkdir -p "${tmp}/platform/browser"
echo 'package browser' >"${tmp}/platform/browser/browser_linux.go"
echo 'package browser' >"${tmp}/platform/browser/browser_windows.go"
check fail "package with no test file" "${tmp}/platform"

# One test file clears it.
echo 'package browser' >"${tmp}/platform/browser/browser_test.go"
check pass "package with a test file" "${tmp}/platform"

# A directory that only contains other packages is not itself a package,
# so it must not be demanded a test.
mkdir -p "${tmp}/platform/group/inner"
echo 'package inner' >"${tmp}/platform/group/inner/inner.go"
echo 'package inner' >"${tmp}/platform/group/inner/inner_test.go"
check pass "intermediate directory holding no Go files" "${tmp}/platform"

# A _test.go on its own still counts: the point is that a test has
# somewhere to live, not that production code exists first.
mkdir -p "${tmp}/platform/testonly"
echo 'package testonly' >"${tmp}/platform/testonly/x_test.go"
check pass "package that is only tests" "${tmp}/platform"

# A missing root is an error, not a silent pass — a renamed directory
# must not quietly disable the guard.
check fail "missing root" "${tmp}/platform/nope"

# And the real tree, which must be green.
repo="$(cd "$(dirname "$0")/../.." && pwd)"
check pass "the repository's own internal/platform" "${repo}/internal/platform"

exit "${fail}"
