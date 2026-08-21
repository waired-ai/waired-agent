#!/usr/bin/env bash
# The GPU lane, as it runs on the ephemeral L4 VM (waired-ai/waired#590).
#
# This file is the lane body. It used to be a set of `run:` blocks in
# installtest-inference.yml, executed by a self-hosted Actions runner; the
# runner is gone, and a supervisor baked into the private runner image invokes
# this script instead. The trust level is unchanged — this is repository code
# either way, running as an unprivileged user with no sudo and no reachable
# metadata server. What changed is that the lane body now travels with the
# tests it runs, so a change here takes effect on the next run rather than on
# the next image rebuild.
#
# Contract with the supervisor (private repo, build/packer/install-gpu-runner.sh):
#   in  $WAIRED_LANE              vllm | agentgrade
#   in  $WAIRED_LANE_TARGETS      space-separated make targets (vllm lane)
#   in  $WAIRED_LANE_GRADE_MODEL  ollama tag (agentgrade lane)
#   out ./vllm-install.log ./vllm-e2e.log ./agentgrade.log ./agentgrade-report.json
#   out ./.lane-targets-run       what actually ran, for CI to compare against
#                                 what it asked for
#   out exit status               0 iff every target passed
#
# Deliberately NOT `set -e`: every target has to run even after one fails, or
# a single red target hides the state of the other four.
set -uo pipefail

: "${WAIRED_LANE:?WAIRED_LANE is required}"

DRY_RUN=0
[ "${1:-}" = "--dry-run" ] && DRY_RUN=1

# Every make target and every log path here is relative to the repo root, and
# the caller is a systemd unit whose working directory is /. Say so plainly
# rather than letting it surface as "go.mod file not found".
if [ ! -f Makefile ] || [ ! -f go.mod ]; then
  echo "gpu-lane-run: must run from the repository root (cwd=$PWD)" >&2
  exit 2
fi

# Off the cache disk on purpose. The venv is a ~6 GB build and rebuilding it
# every run is the point of this lane — it is the only thing that exercises
# vllmInstallCore. A state dir on the persistent cache would skip it and the
# lane would still look green.
STATE_DIR="${WAIRED_LANE_STATE_DIR:-/var/tmp/waired-state}"
BIN="${STATE_DIR%/}-bin/waired"

run() {
  if [ "${DRY_RUN}" = "1" ]; then echo "DRY-RUN: $*"; return 0; fi
  "$@"
}

banner() { echo "===== $* ====="; }

lane_vllm() {
  : "${WAIRED_LANE_TARGETS:?WAIRED_LANE_TARGETS is required for the vllm lane}"

  banner "build waired"
  mkdir -p "$(dirname "${BIN}")"
  run go build -o "${BIN}" ./cmd/waired || return 1

  banner "install the vLLM venv (real ~6 GB build; exercises vllmInstallCore)"
  if [ "${DRY_RUN}" = "1" ]; then
    echo "DRY-RUN: ${BIN} runtimes install vllm --yes --state-dir ${STATE_DIR}"
  else
    "${BIN}" runtimes install vllm --yes --state-dir "${STATE_DIR}" 2>&1 | tee vllm-install.log
    [ "${PIPESTATUS[0]}" -eq 0 ] || return 1
  fi

  local overall=0 target ran=""
  for target in ${WAIRED_LANE_TARGETS}; do
    banner "make ${target} (WAIRED_STATE_DIR=${STATE_DIR})"
    if [ "${DRY_RUN}" = "1" ]; then
      echo "DRY-RUN: make ${target}"
    else
      WAIRED_STATE_DIR="${STATE_DIR}" make "${target}" 2>&1 | tee -a vllm-e2e.log
      [ "${PIPESTATUS[0]}" -eq 0 ] || overall=1
    fi
    ran="${ran}${ran:+ }${target}"
  done
  # Written whatever the verdict: CI compares this against the list it sent, so
  # "the workflow named five targets" and "five targets ran" stay separable.
  printf '%s\n' "${ran}" > .lane-targets-run
  return "${overall}"
}

lane_agentgrade() {
  : "${WAIRED_LANE_GRADE_MODEL:?WAIRED_LANE_GRADE_MODEL is required for the agentgrade lane}"

  banner "grading ${WAIRED_LANE_GRADE_MODEL}"
  if [ "${DRY_RUN}" = "1" ]; then
    echo "DRY-RUN: make e2e-agentgrade MODEL=${WAIRED_LANE_GRADE_MODEL}"
    printf 'e2e-agentgrade\n' > .lane-targets-run
    return 0
  fi
  make e2e-agentgrade MODEL="${WAIRED_LANE_GRADE_MODEL}" \
       JSON="${PWD}/agentgrade-report.json" 2>&1 | tee agentgrade.log
  local rc="${PIPESTATUS[0]}"
  printf 'e2e-agentgrade\n' > .lane-targets-run

  # The harness skips itself when ollama is not on PATH and reports success
  # having measured nothing. A report that is absent or not an object is that
  # case, and it is a lane failure rather than a model verdict.
  if [ "${rc}" -eq 0 ] && ! jq -e 'type == "object"' agentgrade-report.json >/dev/null 2>&1; then
    echo "gpu-lane-run: e2e-agentgrade passed but wrote no usable report" >&2
    return 1
  fi
  return "${rc}"
}

case "${WAIRED_LANE}" in
  vllm)       lane_vllm ;;
  agentgrade) lane_agentgrade ;;
  *)          echo "gpu-lane-run: unknown lane '${WAIRED_LANE}'" >&2; exit 2 ;;
esac
