#!/usr/bin/env bash
# gpu-runner-up.sh — create the per-run L4 runner VM for the GPU lane.
#
# There is no standing GPU runner (waired-ai/waired#590). This creates one for
# the life of the run from an instance template that lives in the PRIVATE infra
# repo, so the shape of a host that executes repository code — image, disks,
# service account, scopes, max-run-duration — is reviewed there rather than
# here. This script supplies only a name and a one-hour registration token.
#
# Why this exists at all rather than a permanently-registered runner: this repo
# is public, and a standing `[gpu, l4]` runner would be a standing target gated
# only by VM power state. A per-run VM means the labels resolve to nothing
# outside a run a maintainer started.
#
# It is ALSO the queue guard. `runs-on:` with no matching runner does not skip
# — GitHub queues the job for 24 hours before failing it, and this workflow's
# concurrency group (cancel-in-progress: false) makes the next nightly wait
# behind it. Callers depend on this job via `needs:`, so "no runner" becomes a
# skipped job in minutes (waired-ai/waired#1229).
#
# Registration is deliberately NOT --ephemeral / JIT. One dispatch can select
# both GPU jobs (gpu != off AND agentgrade_model != ''), and an ephemeral
# runner serves exactly one job — the second would queue to the 24-hour
# timeout. The VM, not the runner protocol, is the isolation boundary: it is
# deleted at the end of the run either way.
#
# Environment:
#   GH_REPO_FULL          required — owner/repo to register the runner against
#   GPU_RUNNER_TEMPLATE   required — instance template self-link (private repo)
#   GPU_RUNNER_ZONES      required — comma-separated zone preference order
#   GPU_RUNNER_PAT_SECRET required — Secret Manager secret holding the PAT
#   GITHUB_RUN_ID         required — names the instance and the runner
#   READY_TIMEOUT_SECONDS optional — default 720
#   GITHUB_OUTPUT         required — instance/zone are handed to gpu-down
set -euo pipefail

: "${GH_REPO_FULL:?GH_REPO_FULL is required (owner/repo)}"
: "${GPU_RUNNER_TEMPLATE:?GPU_RUNNER_TEMPLATE is required}"
: "${GPU_RUNNER_ZONES:?GPU_RUNNER_ZONES is required (comma-separated)}"
: "${GPU_RUNNER_PAT_SECRET:?GPU_RUNNER_PAT_SECRET is required}"
: "${GITHUB_RUN_ID:?GITHUB_RUN_ID is required}"
: "${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"

readonly INSTANCE="waired-gpu-l4-${GITHUB_RUN_ID}"
readonly READY_TIMEOUT="${READY_TIMEOUT_SECONDS:-720}"

# The registration token is minted here and passed as instance metadata. The
# boot script consumes it and then firewalls the runner user off the metadata
# server, so job code can read neither the spent token nor mint a VM service
# account token. Keep the PAT itself out of every log: `set -x` is never on in
# this script for that reason.
echo "minting a runner registration token for ${GH_REPO_FULL}"
pat="$(gcloud secrets versions access latest --secret="${GPU_RUNNER_PAT_SECRET}")"
reg_token="$(GH_TOKEN="${pat}" gh api -X POST \
  "/repos/${GH_REPO_FULL}/actions/runners/registration-token" --jq .token)"
if [ -z "${reg_token}" ]; then
  echo "::error::registration token came back empty" >&2
  exit 1
fi
echo "::add-mask::${reg_token}"

# Zone fallback. The cache disk is zonal, so a fallback zone runs COLD (no uv
# cache, no HF weights) — a ~40 minute run instead of ~15. That is a
# degradation, not a failure: an L4 stockout should cost wall clock, not a red
# nightly on a lane nobody is watching.
created_zone=""
IFS=',' read -r -a zones <<< "${GPU_RUNNER_ZONES}"
for zone in "${zones[@]}"; do
  zone="$(echo "${zone}" | tr -d '[:space:]')"
  [ -n "${zone}" ] || continue
  echo "creating ${INSTANCE} in ${zone}"
  if gcloud compute instances create "${INSTANCE}" \
      --source-instance-template="${GPU_RUNNER_TEMPLATE}" \
      --zone="${zone}" \
      --metadata="waired-runner-token=${reg_token},waired-runner-name=${INSTANCE},waired-runner-repo=${GH_REPO_FULL}" \
      --quiet; then
    created_zone="${zone}"
    break
  fi
  echo "::warning::${zone} could not take the instance; trying the next zone"
done

if [ -z "${created_zone}" ]; then
  echo "::error::no zone in ${GPU_RUNNER_ZONES} could create ${INSTANCE}" >&2
  exit 1
fi

# Record the instance BEFORE waiting for readiness. A create that succeeds and
# a boot that never registers still leaves a billing VM, and gpu-down can only
# delete what this told it about.
{
  echo "instance=${INSTANCE}"
  echo "zone=${created_zone}"
} >> "${GITHUB_OUTPUT}"

# Readiness is the guest attribute the boot script sets after `config.sh`
# returns — not the instance's RUNNING state, which says only that the
# hypervisor started it. Polling the GitHub runners API instead would need the
# PAT to stay live for the whole wait; this needs nothing but the VM.
echo "waiting up to ${READY_TIMEOUT}s for ${INSTANCE} to register"
deadline=$(( SECONDS + READY_TIMEOUT ))
while [ "${SECONDS}" -lt "${deadline}" ]; do
  if gcloud compute instances get-guest-attributes "${INSTANCE}" \
      --zone="${created_zone}" --query-path=waired/runner-ready \
      --format='value(value)' 2>/dev/null | grep -q .; then
    echo "runner ${INSTANCE} registered after $(( SECONDS ))s"
    exit 0
  fi
  sleep 10
done

echo "::error::${INSTANCE} did not report waired/runner-ready within ${READY_TIMEOUT}s" >&2
exit 1
