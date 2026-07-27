#!/usr/bin/env bash
# platform-test-floor-guard.sh — every package under internal/platform/
# must carry at least one _test.go.
#
# Why this exists: internal/platform/browser shipped implementations for
# all three OSes with no test file at all, and then produced #181, #182
# and #183 — three variants of "the browser opens as the wrong user".
# There was nothing to fail. Seven of the repo's packages were in that
# state; internal/platform/ is where the OS-specific ones live, so it is
# where the absence costs the most.
#
# The floor is deliberately low. One test file is not coverage, and this
# guard makes no claim that it is. It only removes the state where a new
# per-OS package can be added with no place for a test to go, which is
# how the browser package stayed untested through three separate PRs.
#
# The root is an argument so the self-test can point it at a fixture:
# the decision under test is "does this tree satisfy the floor", and the
# tree has to be a parameter for that decision to be testable at all
# (CLAUDE.md §Test discipline).
#
# Usage: platform-test-floor-guard.sh [root]   (default: internal/platform)
set -euo pipefail

root="${1:-internal/platform}"

if [ ! -d "${root}" ]; then
  echo "::error::platform-test-floor-guard: ${root} does not exist" >&2
  exit 1
fi

missing=()
checked=0
while IFS= read -r dir; do
  # Only directories that actually hold Go code; a parent directory that
  # merely contains packages is not itself a package.
  if ! compgen -G "${dir}/*.go" >/dev/null; then
    continue
  fi
  checked=$((checked + 1))
  if ! compgen -G "${dir}/*_test.go" >/dev/null; then
    missing+=("${dir}")
  fi
done < <(find "${root}" -type d | sort)

if [ ${#missing[@]} -gt 0 ]; then
  echo "::error::packages under ${root} with no test file at all:" >&2
  printf '  %s\n' "${missing[@]}" >&2
  cat >&2 <<'EOF'

Every package under internal/platform/ needs at least one _test.go.

internal/platform/browser had three OSes' worth of implementation and no
test file, then produced #181, #182 and #183 — three variants of the
browser opening as the wrong user. Nothing could have gone red.

One test file is not coverage and this guard does not pretend otherwise.
It exists so a per-OS package cannot reach main with nowhere for a test
to live. Per CLAUDE.md §Cross-OS parity the natural first test is a
table over runtime.GOOS against the package's (GOOS, facts) -> plan
function.
EOF
  exit 1
fi

echo "platform-test-floor-guard: OK (${checked} packages under ${root}, all with tests)"
