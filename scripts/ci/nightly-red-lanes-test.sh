#!/usr/bin/env bash
# Self-test for nightly-red-lanes.sh.
#
# The rule this file mainly exists for is the skipped-GPU-lane one, and it is
# tested in BOTH directions on purpose. A test that only proves the report
# fires cannot tell "the rule works" from "the rule fires on everything", and
# the dormancy contract depends on it staying quiet — a nightly issue every
# morning about a decision being honoured is how a report gets muted
# (waired-ai/waired#1229).
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SUT=scripts/ci/nightly-red-lanes.sh

pass=0
fail=0

# green() names the all-clear baseline once so a case states only its delta.
green() {
  RESULT_INFERENCE=success
  RESULT_DAEMON_ENGINE=success
  RESULT_ENGINE_ONLY=success
  RESULT_ROUTING=success
  RESULT_BANNER=success
  RESULT_VLLM=skipped
  RESULT_SAC=success
  RESULT_SAC_ISG=success
  GPU_RUNNER_ENABLED=""
  export RESULT_INFERENCE RESULT_DAEMON_ENGINE RESULT_ENGINE_ONLY \
         RESULT_ROUTING RESULT_BANNER RESULT_VLLM RESULT_SAC RESULT_SAC_ISG \
         GPU_RUNNER_ENABLED
}

check() {
  local name="$1" want="$2" got
  # The sentinel is not decoration: `$(...)` strips trailing newlines, and
  # every non-empty expectation here ENDS in one. Without it the harness
  # reports a failure for output that is byte-for-byte correct — which is what
  # it did on first run.
  got="$(bash "$SUT"; printf X)"
  got="${got%X}"
  if [ "$got" = "$want" ]; then
    echo "  ok   ${name}"
    pass=$((pass + 1))
  else
    echo "  FAIL ${name}"
    echo "       want: $(printf '%q' "$want")"
    echo "       got:  $(printf '%q' "$got")"
    fail=$((fail + 1))
  fi
}

# --- the dormant lane stays quiet ----------------------------------------
green
check "a dormant GPU lane is not a red night" ""

# --- an enabled lane that did not run IS a red night ---------------------
green
GPU_RUNNER_ENABLED=true
check "an enabled GPU lane that skipped is reported" \
  "- vLLM install+serve (SKIPPED while GPU_RUNNER_ENABLED=true — the lane did not run at all)
"

# The inverse of the case above: with the variable set AND the lane actually
# running, the skip rule must not fire. Without this, "enabled" alone would be
# enough to report every night.
green
GPU_RUNNER_ENABLED=true
RESULT_VLLM=success
check "an enabled GPU lane that ran green is not reported" ""

# --- ordinary failures ---------------------------------------------------
green
RESULT_INFERENCE=failure
check "a failed lane is reported" "- install+inference
"

green
RESULT_INFERENCE=failure
RESULT_ROUTING=failure
check "two failed lanes are both reported" "- install+inference
- routing sentinel
"

# The skip rule matches RESULT_VLLM exactly, so an unset one would match
# neither "failure" nor "skipped" and the night would go unreported again —
# in a new place, and invisibly to every case above. Assert the guard by
# removing the variable and requiring a non-zero exit.
green
unset RESULT_VLLM
if bash "$SUT" >/dev/null 2>&1; then
  echo "  FAIL an unwired RESULT_VLLM must not be silently tolerated"
  fail=$((fail + 1))
else
  echo "  ok   an unwired RESULT_VLLM is refused"
  pass=$((pass + 1))
fi

# Both rules can fire at once, and the failure must not swallow the skip.
green
GPU_RUNNER_ENABLED=true
RESULT_BANNER=failure
check "a failure and a silent skip are both reported" "- banner render check
- vLLM install+serve (SKIPPED while GPU_RUNNER_ENABLED=true — the lane did not run at all)
"

# --- sac-audit (waired-agent#1112) ---------------------------------------
# It was in the report job's `needs` — so its failure DID file an issue —
# and in neither the env block nor the lane table, so the issue's "Red
# lanes:" list came out empty and named nothing. Exactly the shape the
# workflow's own comment beside `needs` says leaving a scheduled lane out of
# this table produces.
green
RESULT_SAC=failure
check "a sac-only red names the lane" "- SAC signing audit
"

# The reputation lane is a second nightly-only job in the same shape, added
# with it rather than after it for the reason the case above exists at all:
# waired-agent#1112 was a lane that reached `needs:` and never reached this
# table, so a red night filed an issue with an empty list.
green
RESULT_SAC_ISG=failure
check "a sac-isg-only red names the lane" "- SAC reputation verdict
"

green
RESULT_SAC=failure
RESULT_SAC_ISG=failure
check "both SAC lanes red are listed in table order" "- SAC signing audit
- SAC reputation verdict
"

# --- statuses that are not failures --------------------------------------
# `cancelled` is what a run gets when somebody stops it or the whole workflow
# times out. Reporting it would file an issue about a human's own decision.
green
RESULT_ROUTING=cancelled
check "a cancelled lane is not a red night" ""

echo "nightly-red-lanes-test: ${pass} passed, ${fail} failed"
[ "${fail}" -eq 0 ]
