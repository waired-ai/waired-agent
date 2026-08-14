#!/usr/bin/env bash
# resolve-build-version-test.sh — self-test for resolve-build-version.sh.
#
# The rewrite it guards is the one the workflow's own header comment
# declared and the tag branch never implemented, through nine releases
# (waired-agent#780). The declaration was there; the execution was not.
# So this asserts the ORDERING the .deb versions end up in, with dpkg
# where dpkg is available, rather than only the string shape.
set -euo pipefail

ROOT="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)"
SUBJECT="${ROOT}/scripts/ci/resolve-build-version.sh"

fails=0

# resolve <expected-semver> <expected-debver> — runs the subject with the
# REF_*/BUILD_* already exported by the caller.
expect() {
  local want_semver="$1" want_debver="$2" out
  out="$(bash "${SUBJECT}")"
  local got_semver got_debver
  got_semver="$(printf '%s\n' "${out}" | sed -n 's/^semver=//p')"
  got_debver="$(printf '%s\n' "${out}" | sed -n 's/^debver=//p')"
  if [ "${got_semver}" != "${want_semver}" ]; then
    echo "FAIL semver: got '${got_semver}', want '${want_semver}'" >&2
    fails=$((fails + 1))
  fi
  if [ "${got_debver}" != "${want_debver}" ]; then
    echo "FAIL debver: got '${got_debver}', want '${want_debver}'" >&2
    fails=$((fails + 1))
  fi
}

# --- tag builds --------------------------------------------------------
export REF_TYPE=tag

REF_NAME=v0.0.3      expect "0.0.3"      "0.0.3"
REF_NAME=v0.0.3-rc1  expect "0.0.3-rc1"  "0.0.3~rc1"
REF_NAME=v0.0.3-rc10 expect "0.0.3-rc10" "0.0.3~rc10"
REF_NAME=v1.2.3      expect "1.2.3"      "1.2.3"
# A tag with no leading v is accepted, same as before.
REF_NAME=0.0.3-rc1   expect "0.0.3-rc1"  "0.0.3~rc1"
# Every hyphen, not just the first: with only the first rewritten the
# result would be "0.0.2~rc8-dev", whose LAST hyphen dpkg would still
# read as a revision separator.
REF_NAME=v0.0.2-rc8-dev expect "0.0.2-rc8-dev" "0.0.2~rc8~dev"

# --- edge builds -------------------------------------------------------
export REF_TYPE=branch BUILD_TS=20260610143000 BUILD_SHA=abc1234

LATEST_TAG=v0.0.3-rc1 expect "0.0.3-edge.20260610143000+abc1234" "0.0.3~edge.20260610143000+abc1234"
LATEST_TAG=v1.2.3     expect "1.2.3-edge.20260610143000+abc1234" "1.2.3~edge.20260610143000+abc1234"
# No tags in the clone yet: the core falls back to 0.0.0.
LATEST_TAG=''         expect "0.0.0-edge.20260610143000+abc1234" "0.0.0~edge.20260610143000+abc1234"

unset REF_TYPE REF_NAME BUILD_TS BUILD_SHA LATEST_TAG

# --- the ordering those strings have to produce ------------------------
# This is the product contract (waired-agent#780): a prerelease sorts
# below the release it leads to, rcN sorts by its number, and an edge
# build sorts below the stable it is based on. Asserted against dpkg
# itself, which is what apt uses to pick a candidate.
if command -v dpkg >/dev/null 2>&1; then
  ordered=(
    "0.0.2~rc8~dev"
    "0.0.2~rc9"
    "0.0.2"
    "0.0.3~edge.20260610143000+abc1234"
    "0.0.3~edge.20260610143001+abc1234"
    "0.0.3~rc1"
    "0.0.3~rc2"
    "0.0.3~rc10"
    "0.0.3"
  )
  for ((i = 0; i < ${#ordered[@]} - 1; i++)); do
    a="${ordered[i]}"
    b="${ordered[i + 1]}"
    if ! dpkg --compare-versions "${a}" lt "${b}"; then
      echo "FAIL ordering: dpkg does not place '${a}' below '${b}'" >&2
      fails=$((fails + 1))
    fi
  done
else
  echo "note: dpkg not available; skipped the ordering assertions" >&2
fi

if [ "${fails}" -ne 0 ]; then
  echo "resolve-build-version-test: ${fails} failure(s)" >&2
  exit 1
fi
echo "resolve-build-version-test: ok"
