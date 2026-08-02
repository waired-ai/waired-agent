#!/usr/bin/env bash
# docs-preview-select-test.sh — table test for the channel-selection rules in
# scripts/ci/docs-preview-channels.mjs (#429).
#
# These are product contracts, not a record of today's behaviour: the selector
# decides what gets DELETED from the shared Firebase project that also serves
# https://docs.waired.ai. The `live` and non-`pr-` cases below are the pins that
# keep a future edit from widening the blast radius, and the OPEN_PRS cases pin
# the fail-closed rule (an unreachable GitHub API must abort a sweep, never look
# like "no PR is open"). Everything runs offline against synthetic JSON.
#
# Runs in ci.yml's lint job, like the other *-test.sh guards in this directory.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
helper="${here}/docs-preview-channels.mjs"

fails=0
NOW_FIXED="2026-08-02T12:00:00Z"

# A site whose channels cover every branch: live, an open PR, two closed PRs,
# an expired channel belonging to an OPEN PR, and a hand-made channel.
channels_json() {
  cat <<'JSON'
{
  "status": "success",
  "result": {
    "channels": [
      {"name": "projects/p/sites/s/channels/live"},
      {"name": "projects/p/sites/s/channels/pr-415", "expireTime": "2026-08-09T10:23:06Z"},
      {"name": "projects/p/sites/s/channels/pr-172", "expireTime": "2026-08-01T13:55:54Z"},
      {"name": "projects/p/sites/s/channels/pr-303", "expireTime": "2026-08-04T15:29:57Z"},
      {"name": "projects/p/sites/s/channels/pr-428", "expireTime": "2026-08-01T09:00:00Z"},
      {"name": "projects/p/sites/s/channels/staging-demo", "expireTime": "2026-08-01T09:00:00Z"}
    ]
  }
}
JSON
}

# check <name> <expected-newline-separated-ids> [env assignments...] -- runs
# `select` over channels_json() and compares stdout.
check() {
  local name="$1" want="$2"; shift 2
  local got status=0
  got="$(channels_json | env NOW="${NOW_FIXED}" "$@" node "${helper}" select)" || status=$?
  if (( status != 0 )); then
    echo "FAIL ${name}: selector exited ${status}"
    fails=$((fails + 1))
    return
  fi
  if [[ "${got}" != "${want}" ]]; then
    echo "FAIL ${name}"
    echo "  want: $(printf '%s' "${want}" | tr '\n' ' ')"
    echo "  got : $(printf '%s' "${got}" | tr '\n' ' ')"
    fails=$((fails + 1))
    return
  fi
  echo "ok   ${name}"
}

# check_fails <name> [env assignments...] — the selector must refuse (non-zero)
# and emit nothing.
check_fails() {
  local name="$1"; shift
  local got status=0
  got="$(channels_json | env NOW="${NOW_FIXED}" "$@" node "${helper}" select 2>/dev/null)" || status=$?
  if (( status == 0 )); then
    echo "FAIL ${name}: expected a non-zero exit, got 0 (stdout: ${got})"
    fails=$((fails + 1))
  elif [[ -n "${got}" ]]; then
    echo "FAIL ${name}: refused but still printed: ${got}"
    fails=$((fails + 1))
  else
    echo "ok   ${name}"
  fi
}

# --- sweep mode ------------------------------------------------------------
# pr-415 is open and unexpired -> kept. pr-172 (expired) and pr-303 (closed)
# go. pr-428 is OPEN but its channel already expired: the URL is dead either
# way and the next push recreates it, so it goes too.
check "sweep: expired + closed PRs, open PR kept" \
  "$(printf 'pr-172\npr-303\npr-428')" \
  OPEN_PRS='[415,428,128]'

# `live` is the production docs site. No input may ever select it, and a
# channel that is not pr-<N> is invisible even when expired.
check "sweep: live and non-pr channels are never selected" \
  "$(printf 'pr-415\npr-172\npr-303\npr-428')" \
  OPEN_PRS='[]'

# --- fail-closed -----------------------------------------------------------
check_fails "sweep: unset OPEN_PRS aborts"      OPEN_PRS=
check_fails "sweep: malformed OPEN_PRS aborts"  OPEN_PRS='not json'
check_fails "sweep: non-array OPEN_PRS aborts"  OPEN_PRS='415'

# --- single-PR reap --------------------------------------------------------
# PR_ONLY ignores expiry and the open/closed question entirely: the caller is
# the PR's own close event.
check "reap: selects exactly its own channel" "pr-415" PR_ONLY=415
check "reap: a PR with no channel is a no-op"  ""       PR_ONLY=999999
# A PR number is never allowed to name something other than its own channel.
check "reap: does not match a prefix of another PR" "" PR_ONLY=41

# --- malformed input -------------------------------------------------------
if printf 'not json' | node "${helper}" select 2>/dev/null; then
  echo "FAIL malformed stdin: expected a non-zero exit"
  fails=$((fails + 1))
else
  echo "ok   malformed stdin aborts"
fi

# --- site resolution -------------------------------------------------------
# The site id is read from docs-site/.firebaserc, never hardcoded, so renaming
# a Hosting site does not silently point the sweep at the wrong one.
site="$(FIREBASE_PROJECT_ID=dev-waired FIREBASERC="${here}/../../docs-site/.firebaserc" \
  node "${helper}" site)"
if [[ -n "${site}" ]]; then
  echo "ok   site resolves from .firebaserc (${site})"
else
  echo "FAIL site: empty result"
  fails=$((fails + 1))
fi
if FIREBASE_PROJECT_ID=no-such-project FIREBASERC="${here}/../../docs-site/.firebaserc" \
   node "${helper}" site >/dev/null 2>&1; then
  echo "FAIL site: an unknown project should abort"
  fails=$((fails + 1))
else
  echo "ok   site: an unknown project aborts"
fi

if (( fails > 0 )); then
  echo "docs-preview-select-test: ${fails} failure(s)" >&2
  exit 1
fi
echo "docs-preview-select-test: all checks passed"
