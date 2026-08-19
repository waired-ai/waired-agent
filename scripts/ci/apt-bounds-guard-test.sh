#!/usr/bin/env bash
# Self-test for apt-bounds-guard.sh (#893).
#
# The guard's whole value is that it goes red when a bound is dropped, so
# that is what this exercises: it copies the real installers into a temp
# tree, breaks one thing at a time, and runs the guard there. Asserting
# only that the guard passes on today's files would pass just as well if
# the guard had no rule at all.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
guard="${root}/scripts/ci/apt-bounds-guard.sh"

fail=0
check() { # check <expected: pass|fail> <label> <mutation function|->
  local want="$1" label="$2" mutate="$3" got tmp
  tmp="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "rm -rf '${tmp}'" RETURN
  mkdir -p "${tmp}/packaging/install"
  cp "${root}/packaging/install/install.sh" "${root}/packaging/install/uninstall.sh" \
     "${tmp}/packaging/install/"
  [ "${mutate}" = "-" ] || "${mutate}" "${tmp}"
  if (cd "${tmp}" && bash "${guard}" >/dev/null 2>&1); then got=pass; else got=fail; fi
  if [ "${got}" = "${want}" ]; then
    echo "ok   ${label} (${got})"
  else
    echo "FAIL ${label}: want ${want}, got ${got}"
    fail=1
  fi
}

# Drop the bound from one call — the shape a future edit takes when
# somebody copies a neighbouring line and trims it.
drop_one_bound() {
  # shellcheck disable=SC2016  # matching the literal text, not expanding it
  sed -i '0,/apt-get \$APT_BOUNDS/{s/apt-get \$APT_BOUNDS/apt-get/}' \
    "$1/packaging/install/install.sh"
}

# Remove the definition entirely: every call site then expands to
# nothing, which is unbounded again while still LOOKING bounded.
drop_definition() {
  sed -i '/^APT_BOUNDS=/d' "$1/packaging/install/install.sh"
}

# A new call written in a style the file has never used. The guard has to
# fail closed on this, not only on edits to lines it already knows.
add_unbounded_call() {
  # shellcheck disable=SC2016  # writing the literal text into a fixture
  printf '\n%s\n' 'later() { $SUDO apt-get install -y something; }' \
    >> "$1/packaging/install/install.sh"
}

# And the two shapes that name apt-get without running it must stay
# quiet, or the guard becomes noise somebody learns to ignore.
add_message_and_require() {
  printf '\n%s\n%s\n' \
    'msg() { common_log "apt-get purge removes everything"; }' \
    'req() { common_require_cmd apt-get dpkg-query; }' \
    >> "$1/packaging/install/uninstall.sh"
}

check pass "today's installers"            -
check fail "one call loses its bound"      drop_one_bound
check fail "the definition is removed"     drop_definition
check fail "a new unbounded call is added" add_unbounded_call
check pass "messages and command checks"   add_message_and_require

if [ "${fail}" -ne 0 ]; then
  echo "apt-bounds-guard-test: FAILED" >&2
  exit 1
fi
echo "apt-bounds-guard-test: all passed"
