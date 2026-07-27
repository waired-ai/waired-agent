#!/usr/bin/env bash
# Self-test for base-ref-guard.sh. Everything the guard decides comes
# from two environment variables, so the whole decision table runs here
# with no PR, no git state and no network.
set -euo pipefail

guard="$(cd "$(dirname "$0")" && pwd)/base-ref-guard.sh"

fail=0
check() { # check <expected: pass|fail> <label> <base_ref> <body>
  local want="$1" label="$2" base="$3" body="$4" got
  if BASE_REF="${base}" PR_BODY="${body}" bash "${guard}" >/dev/null 2>&1; then
    got=pass
  else
    got=fail
  fi
  if [ "${got}" = "${want}" ]; then
    echo "ok   ${label} (${got})"
  else
    echo "FAIL ${label}: want ${want}, got ${got}"
    fail=1
  fi
}

check pass "base main"                    main    ""
check pass "base main, body irrelevant"   main    "some prose"
check fail "base is a feature branch"     feat/x  ""
check fail "base is another release line" v1.x    ""

# The opt-out and its shape.
check pass "declared stacked"             feat/x  "stacked-on: feat/x (#123)"
check pass "declared, case-insensitive"   feat/x  "Stacked-On: feat/x (#123)"
check pass "declared amid other body text" feat/x "Fixes #1

stacked-on: feat/parent (#123)

More prose."
check fail "declared with nothing after the colon" feat/x "stacked-on:"
check fail "mentioned in prose, not declared" feat/x \
  "I wondered whether stacked-on: was the right call here"

# A branch merely NAMED main must not pass — only the base itself.
check fail "base named like main"         main-2  ""
check fail "base is a main-prefixed path" feature/main ""

# Missing context is an error, not a silent pass: a workflow that stops
# setting BASE_REF must go red, not quietly approve everything.
if BASE_REF="" PR_BODY="" bash "${guard}" >/dev/null 2>&1; then
  echo "FAIL missing BASE_REF: want fail, got pass"
  fail=1
else
  echo "ok   missing BASE_REF (fail)"
fi

exit "${fail}"
