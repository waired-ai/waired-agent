#!/usr/bin/env bash
# testnet-require-green-remote-test.sh — self-test for the cross-repo
# testnet gate's verdict table.
#
# That verdict is the one thing in CI that can either release an unverified
# client or block a verified one, and every input it reads comes from the
# GitHub API of a repo this one cannot see. A stub `gh` on PATH exercises
# the real decision without a token, a network call, or a ~25-minute GCP
# testnet run.
#
# Same arrangement as nightly-file-issue-test.sh / base-ref-guard-test.sh.
# `sleep` and `date` are stubbed too: the subject waits 90 s in a settle
# window and polls on a clock, and a self-test that actually waited would
# not be run.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
subject="${root}/scripts/ci/testnet-require-green-remote.sh"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

pass=0; fail=0
ok()  { printf '  ok   %s\n' "$*"; pass=$((pass+1)); }
bad() { printf '  FAIL %s\n' "$*" >&2; fail=$((fail+1)); }

sha="$(printf 'a%.0s' {1..40})"

mkdir -p "${work}/bin"

# Stub `gh`. RUNS holds the runs-list lines ("<id> <status> <conclusion>"),
# JOBS_<id> holds that run's testnet job conclusions, one per line, with
# the literal `null` standing for a job GitHub never finalised. A dispatch
# records that it happened rather than doing anything.
cat >"${work}/bin/gh" <<'STUB'
#!/usr/bin/env bash
# Faithful enough to test the subject's jq: the fixtures describe API
# STATE, this builds the JSON the API would return, and the subject's own
# --jq expression runs over it. A stub that pre-digested the answer would
# leave the filter — where waired-agent#697 actually lived — untested.
set -euo pipefail
if [ "$1" = "workflow" ]; then           # gh workflow run …
  printf 'dispatched\n' >> "${DISPATCH_LOG}"
  exit 0
fi
url=""; filter=""
prev=""
for a in "$@"; do
  case "${prev}" in --jq) filter="$a";; esac
  case "$a" in repos/*) url="$a";; esac
  prev="$a"
done
emit() { # $1 = JSON document
  if [ -n "${filter}" ]; then printf '%s' "$1" | jq -r "${filter}"; else printf '%s' "$1"; fi
}
case "${url}" in
  *"/actions/workflows/testnet.yml/runs"*)
    # RUNS lines: "<id> <status> <conclusion>"; empty conclusion -> null.
    doc='{"workflow_runs":['
    first=1
    while read -r id status conclusion; do
      [ -z "${id}" ] && continue
      [ ${first} -eq 1 ] || doc="${doc},"
      first=0
      if [ -z "${conclusion}" ]; then c=null; else c="\"${conclusion}\""; fi
      doc="${doc}{\"id\":${id},\"status\":\"${status}\",\"conclusion\":${c},\"display_title\":\"testnet [agent:${AGENT_SHA}] [${DISPATCH_REASON:-none}]\"}"
    done <<< "${RUNS:-}"
    emit "${doc}]}"
    exit 0
    ;;
  *"/actions/runs/"*"/jobs"*)
    id="${url#*/actions/runs/}"; id="${id%%/jobs*}"
    if [ "${JOBS_FAIL:-}" = "${id}" ]; then exit 1; fi   # transient API error
    eval "jobs=\"\${JOBS_${id}:-}\""
    doc='{"jobs":['
    first=1
    while read -r conclusion; do
      [ -z "${conclusion}" ] && continue
      [ ${first} -eq 1 ] || doc="${doc},"
      first=0
      if [ "${conclusion}" = "null" ]; then c=null; else c="\"${conclusion}\""; fi
      doc="${doc}{\"name\":\"testnet (up → verify → down)\",\"conclusion\":${c}}"
    done <<< "${jobs}"
    emit "${doc}]}"
    exit 0
    ;;
esac
exit 1
STUB
chmod +x "${work}/bin/gh"

# The subject sleeps 15 s six times in the settle window and poll_s between
# rounds; a self-test may not.
printf '#!/usr/bin/env bash\nexit 0\n' >"${work}/bin/sleep"
chmod +x "${work}/bin/sleep"

# …and it decides when to give up from a wall clock. With a real `date` the
# give-up branch (180 s after the dispatch) is unreachable in a test, so the
# script would always exit down the timeout path instead and the verdict
# under test would never be printed. This clock jumps 200 s per reading.
cat >"${work}/bin/date" <<'STUB'
#!/usr/bin/env bash
f="${CLOCK_FILE}"
now="$(cat "${f}" 2>/dev/null || echo 0)"
now=$((now + 200))
printf '%s' "${now}" > "${f}"
printf '%s\n' "${now}"
STUB
chmod +x "${work}/bin/date"

run_subject() { # prints combined output
  # RUNS / JOBS_* / JOBS_FAIL are read by the stub, which is a child
  # process, so they have to cross as environment rather than as shell
  # variables of this script.
  PATH="${work}/bin:${PATH}" \
  GH_TOKEN=stub \
  TESTNET_REPO=owner/repo \
  WAIT_TIMEOUT_S="${TIMEOUT_S:-5400}" \
  POLL_INTERVAL_S=0 \
  DISPATCH_LOG="${work}/dispatched" \
  CLOCK_FILE="${work}/clock" \
  AGENT_SHA="${sha}" \
  RUNS="${RUNS:-}" \
  JOBS_1="${JOBS_1:-}" \
  JOBS_2="${JOBS_2:-}" \
  JOBS_FAIL="${JOBS_FAIL:-}" \
  bash "${subject}" "${sha}" 2>&1
}

expect() { # <name> <want-exit> <want-substring> ; env for the stub in the caller
  local name="$1" want_exit="$2" want_sub="$3" out status
  : >"${work}/dispatched"
  echo 0 >"${work}/clock"
  set +e
  out="$(run_subject)"
  status=$?
  set -e
  if [[ "${status}" -ne "${want_exit}" ]]; then
    bad "${name}: exit ${status}, want ${want_exit} — output: ${out}"
    return
  fi
  if [[ -n "${want_sub}" && "${out}" != *"${want_sub}"* ]]; then
    bad "${name}: output did not contain '${want_sub}' — output: ${out}"
    return
  fi
  ok "${name}"
}

echo "testnet-require-green-remote verdict table:"

# PRODUCT CONTRACT (the script's own header): green for agent X = a
# dispatch run whose "testnet (…)" JOB concluded success. The run's own
# conclusion is not enough, because a gate-skipped run also succeeds.
RUNS="1 completed success" JOBS_1="success" \
  expect "a run whose testnet job succeeded releases" 0 "testnet job succeeded"

RUNS="1 completed success" JOBS_1="" \
  expect "a gate-skipped run (no testnet job) does not release" 1 "did not produce a green"

RUNS="1 completed success" JOBS_1="failure" \
  expect "a failed testnet job does not release" 1 "did not produce a green"

RUNS="1 completed success" JOBS_1="skipped" \
  expect "a skipped testnet job does not release" 1 "did not produce a green"

# waired-agent#697: observed on #671 — the run was completed/success while
# its testnet job sat unfinalised, and `first // "none"` made that
# indistinguishable from the gate-skipped run above, so a green testnet
# was reported as "did not succeed".
RUNS="1 completed success" JOBS_1="null" \
  expect "an unfinalised job conclusion on a successful run releases" 0 "never finalised"

# The matrix: element 0 is not a vote.
RUNS="1 completed success" JOBS_1="$(printf 'success\nfailure')" \
  expect "a green leg does not carry a red one" 1 "did not produce a green"

RUNS="1 completed success" JOBS_1="$(printf 'null\nsuccess')" \
  expect "an unfinalised leg beside a green one still releases" 0 "never finalised"

# A run that failed outright is never consulted at job level.
RUNS="1 completed failure" JOBS_1="success" \
  expect "a failed run does not release on a stale green job" 1 "did not produce a green"

# A transient API error while reading jobs must not read as "not green"
# and must not kill the script under set -e.
RUNS="1 completed success" JOBS_1="success" JOBS_FAIL=1 \
  expect "an API failure reading jobs is retried, not treated as red" 1 "could not read the testnet jobs"

# An in-flight run is waited on rather than dispatched over: the gate waits
# it out instead of dispatching a second one. (TIMEOUT_S small so the wait
# ends; the stubbed clock reaches any deadline in two readings.)
RUNS="1 in_progress " TIMEOUT_S=1 \
  expect "an in-progress run is waited for" 1 "still in progress"

echo
printf 'passed %d, failed %d\n' "${pass}" "${fail}"
[[ "${fail}" -eq 0 ]]
