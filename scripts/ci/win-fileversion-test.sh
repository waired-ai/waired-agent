#!/usr/bin/env bash
# win-fileversion-test.sh — self-test for win-fileversion.sh.
#
# The subject exists because every shipped Windows binary reported itself
# as 0.0.0.0 (waired-agent#1209). A converter that answers 0.0.0.0 for an
# input it cannot read would reproduce exactly that defect while looking
# like it works, so the refusals below are the point of this file, not an
# afterthought.
set -euo pipefail

ROOT="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)"
SUBJECT="${ROOT}/scripts/ci/win-fileversion.sh"

fails=0

# ok <input> <expected>
ok() {
  local in="$1" want="$2" got
  if ! got="$(bash "${SUBJECT}" "${in}" 2>/dev/null)"; then
    echo "FAIL ${in}: exited non-zero, want '${want}'" >&2
    fails=$((fails + 1))
    return
  fi
  if [ "${got}" != "${want}" ]; then
    echo "FAIL ${in}: got '${got}', want '${want}'" >&2
    fails=$((fails + 1))
  fi
}

# rejected <input> — the subject must fail rather than answer.
rejected() {
  local in="$1" got
  if got="$(bash "${SUBJECT}" "${in}" 2>/dev/null)"; then
    echo "FAIL ${in}: answered '${got}', want a non-zero exit" >&2
    fails=$((fails + 1))
  fi
}

# --- the shapes resolve-build-version.sh actually emits -----------------
# Tag builds (REF_TYPE=tag): the semver is the tag with v stripped.
ok 0.0.3      0.0.3.0
ok 0.0.3-rc1  0.0.3.0
ok 0.0.3-rc10 0.0.3.0
ok 1.2.3      1.2.3.0
# Edge builds: <core>-edge.<ts>+<sha>.
ok 0.0.3-edge.20260610143000+abc1234 0.0.3.0
ok 0.0.0-edge.20260610143000+abc1234 0.0.0.0
# The Makefile's default when nothing sets VERSION.
ok 0.0.0-dev.gabc1234 0.0.0.0
# A tag spelled with the v, which resolve-build-version.sh also accepts.
ok v1.2.3 1.2.3.0
# Every hyphen is prerelease, not just the first (v0.0.2-rc8-dev is a tag
# resolve-build-version-test.sh pins).
ok 0.0.2-rc8-dev 0.0.2.0

# --- inputs that must NOT silently become 0.0.0.0 -----------------------
rejected ''
rejected edge
rejected 1.2
rejected 1.2.3.4
rejected 0.0.x
rejected -rc1
# 16-bit fields: 65536 wraps to 0 inside the resource rather than failing,
# so the converter refuses it here where it is still visible.
rejected 65536.0.0
ok 65535.0.0 65535.0.0.0

# No argument at all is a usage error, not an answer.
if bash "${SUBJECT}" >/dev/null 2>&1; then
  echo "FAIL (no argument): answered, want a non-zero exit" >&2
  fails=$((fails + 1))
fi

if [ "${fails}" -gt 0 ]; then
  echo "win-fileversion-test: ${fails} failure(s)" >&2
  exit 1
fi
echo "win-fileversion-test: ok"
