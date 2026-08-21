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

cd "$(dirname "$0")/../.."
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
  RESULT_GPU_UP=skipped
  RESULT_VLLM=skipped
  GPU_RUNNER_ENABLED=""
  export RESULT_INFERENCE RESULT_DAEMON_ENGINE RESULT_ENGINE_ONLY \
         RESULT_ROUTING RESULT_BANNER RESULT_GPU_UP RESULT_VLLM \
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
RESULT_GPU_UP=success
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

# gpu-up is in the list because a failure there means the GPU lane did not run
# and nothing else would say so — vllm-install is `skipped` in that case, and
# the skip rule is gated on the variable.
green
RESULT_GPU_UP=failure
check "a failed gpu-up is reported" "- bring up the L4 runner
"

# Both rules can fire at once, and the failure must not swallow the skip.
green
GPU_RUNNER_ENABLED=true
RESULT_BANNER=failure
check "a failure and a silent skip are both reported" "- banner render check
- vLLM install+serve (SKIPPED while GPU_RUNNER_ENABLED=true — the lane did not run at all)
"

# --- statuses that are not failures --------------------------------------
# `cancelled` is what a run gets when somebody stops it or the whole workflow
# times out. Reporting it would file an issue about a human's own decision.
green
RESULT_ROUTING=cancelled
check "a cancelled lane is not a red night" ""

echo "nightly-red-lanes-test: ${pass} passed, ${fail} failed"
[ "${fail}" -eq 0 ]
