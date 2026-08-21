#!/usr/bin/env bash
# nightly-red-lanes.sh — turn one scheduled run's job results into the list of
# lanes worth waking a human for.
#
# Extracted from installtest-inference.yml's `report` job rather than left
# inline, because inline workflow bash is code no test can reach — and the
# defect this file exists to fix was precisely a classification nobody had
# ever exercised: the reporter counted `failure` and nothing else, so the GPU
# lane being SKIPPED on all 55 of its job records between 2026-07-24 and
# 2026-08-21 never reached it (waired-ai/waired#1229). Adding a second rule to
# untested inline bash would have repeated the mistake in a new place.
#
# Writes one "- <lane>" line per red lane to stdout, nothing when the night is
# clean. Exit status is 0 either way: "no red lanes" is an answer, not an
# error.
#
# Environment (every RESULT_* is a GitHub job result: success / failure /
# cancelled / skipped, or empty when the job did not exist):
#   RESULT_INFERENCE, RESULT_DAEMON_ENGINE, RESULT_ENGINE_ONLY, RESULT_ROUTING,
#   RESULT_BANNER, RESULT_GPU_UP, RESULT_VLLM
#   GPU_RUNNER_ENABLED — the repo variable, verbatim
set -euo pipefail

red=""
for pair in \
    "install+inference:${RESULT_INFERENCE:-}" \
    "daemon-path engine install:${RESULT_DAEMON_ENGINE:-}" \
    "engine installed, no model chosen:${RESULT_ENGINE_ONLY:-}" \
    "routing sentinel:${RESULT_ROUTING:-}" \
    "banner render check:${RESULT_BANNER:-}" \
    "bring up the L4 runner:${RESULT_GPU_UP:-}" \
    "vLLM install+serve:${RESULT_VLLM:-}"; do
  [ "${pair##*:}" = "failure" ] && red="${red}- ${pair%:*}"$'\n'
done

# A lane that never ran names itself, because "skipped" is not a colour anyone
# reads and `contains(needs.*.result, 'failure')` cannot see it.
#
# Only while the lane is meant to be running. With GPU_RUNNER_ENABLED unset the
# GPU lane is DORMANT BY DESIGN — that is the contract in
# docs/decisions/20260723/1910-gpu-vllm-install-serve-ci-lane.md — and a
# nightly issue every morning about a decision being honoured is how a report
# gets muted. Once somebody sets the variable they have said this lane should
# run, and a skip is then an outage.
if [ "${GPU_RUNNER_ENABLED:-}" = "true" ] && [ "${RESULT_VLLM:-}" = "skipped" ]; then
  red="${red}- vLLM install+serve (SKIPPED while GPU_RUNNER_ENABLED=true — the lane did not run at all)"$'\n'
fi

printf '%s' "${red}"
