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

# The wall clock is what actually bounded the observed stall — the apt
# options alone did not — so losing it has to be caught.
drop_the_clock() {
  # shellcheck disable=SC2016  # the literal text is the point, not its value
  sed -i 's/timeout "\$APT_TIMEOUT" apt-get/apt-get/' "$1/packaging/install/install.sh"
}

# ...and losing the options has to be caught too. They are what keeps a
# quiet connection or a held lock from eating the whole timeout twice.
drop_the_options() {
  # shellcheck disable=SC2016  # the literal text is the point, not its value
  sed -i 's/apt-get \$APT_BOUNDS/apt-get/' "$1/packaging/install/install.sh"
}

# Removing a definition leaves every use expanding to nothing: still
# unbounded, while every line still LOOKS bounded.
drop_definition() {
  sed -i '/^APT_TIMEOUT=/d' "$1/packaging/install/install.sh"
}

# A second call site is the shape this takes in practice — somebody
# copies a neighbouring line rather than calling the helper. The rule is
# "one invocation", precisely so that copying cannot satisfy it.
add_second_call_site() {
  # shellcheck disable=SC2016  # the literal text is the point, not its value
  printf '\n%s\n' 'later() { $SUDO apt-get install -y something; }' \
    >> "$1/packaging/install/install.sh"
}

# Even a *bounded* second call site is a finding: two places to keep
# right is how the first one drifts.
add_bounded_second_call_site() {
  # shellcheck disable=SC2016  # the literal text is the point, not its value
  printf '\n%s\n' 'later() { timeout "$APT_TIMEOUT" apt-get $APT_BOUNDS install -y x; }' \
    >> "$1/packaging/install/install.sh"
}

# And the shapes that name apt-get without running it must stay quiet, or
# the guard becomes noise somebody learns to ignore.
add_message_and_require() {
  printf '\n%s\n%s\n%s\n' \
    'msg() { common_log "apt-get purge removes everything"; }' \
    'req() { common_require_cmd apt-get dpkg-query; }' \
    '# apt-get install foo   <- an example in a comment' \
    >> "$1/packaging/install/uninstall.sh"
}

# The clock only bounds a command it can still kill. Without needrestart
# suspended, its prompt reads the terminal from `timeout`'s own process
# group, SIGTTIN stops the whole group — timeout included — and the bound
# above never fires (#1097). Dropping the suspension has to be caught in
# both installers, so break each one in turn.
drop_needrestart_install() {
  sed -i 's/ NEEDRESTART_SUSPEND=1//' "$1/packaging/install/install.sh"
}
drop_needrestart_uninstall() {
  sed -i 's/ NEEDRESTART_SUSPEND=1//' "$1/packaging/install/uninstall.sh"
}

# The redirect is the second half: it turns any OTHER read of the terminal
# into EOF rather than a stop. Suspending needrestart removes the prompt we
# know about; this removes the class (#1097).
drop_stdin_redirect() {
  sed -i 's| </dev/null;| ;|' "$1/packaging/install/install.sh"
}

check pass "today's installers"              -
check fail "the wall clock is dropped"       drop_the_clock
check fail "the apt options are dropped"     drop_the_options
check fail "a definition is removed"         drop_definition
check fail "a second, unbounded call site"   add_second_call_site
check fail "a second, bounded call site"     add_bounded_second_call_site
check fail "needrestart suspension dropped (install.sh)"   drop_needrestart_install
check fail "needrestart suspension dropped (uninstall.sh)" drop_needrestart_uninstall
check fail "the stdin redirect is dropped"   drop_stdin_redirect
check pass "messages, comments, PATH checks" add_message_and_require

if [ "${fail}" -ne 0 ]; then
  echo "apt-bounds-guard-test: FAILED" >&2
  exit 1
fi
echo "apt-bounds-guard-test: all passed"
