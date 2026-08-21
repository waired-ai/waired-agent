#!/usr/bin/env bash
# gpu-runner-down.sh — delete the per-run L4 runner VM and its runner record.
#
# The primary teardown path. Two other things also stop the bill, deliberately,
# because a teardown that depends on GitHub being reachable is not a bound:
#
#   1. the instance template sets --max-run-duration with a DELETE termination
#      action, which is enforced by GCE and holds even if GitHub is entirely
#      down (this is the control that actually bounds the worst case);
#   2. an hourly reaper in the private infra repo sweeps orphans.
#
# So this script is best-effort by construction: every step tolerates failure
# and the script still exits 0. A red gpu-down would report "the VM is still
# billing" as a broken nightly, which is not what it means — the backstops have
# it. What must NOT happen is this failing early and skipping the delete.
#
# Environment:
#   GH_REPO_FULL          required — owner/repo the runner registered against
#   GPU_RUNNER_PAT_SECRET required — Secret Manager secret holding the PAT
#   INSTANCE              required — instance name from gpu-up's output
#   ZONE                  required — zone from gpu-up's output
set -uo pipefail

: "${GH_REPO_FULL:?GH_REPO_FULL is required (owner/repo)}"
: "${GPU_RUNNER_PAT_SECRET:?GPU_RUNNER_PAT_SECRET is required}"
: "${INSTANCE:?INSTANCE is required}"
: "${ZONE:?ZONE is required}"

# Delete the VM FIRST. It is the thing that costs money, and the runner record
# is a stale row that GitHub removes on its own after 14 days anyway. Doing it
# the other way round means a gcloud outage leaves an L4 running to save a row.
echo "deleting ${INSTANCE} in ${ZONE}"
gcloud compute instances delete "${INSTANCE}" --zone="${ZONE}" --quiet \
  || echo "::warning::could not delete ${INSTANCE}; the reaper and max-run-duration still cover it"

# The registration is not --ephemeral (one dispatch can select both GPU jobs,
# and an ephemeral runner serves exactly one), so nothing deregisters it when
# the VM disappears. Without this the repo accumulates one offline runner per
# night until GitHub's own 14-day sweep.
pat="$(gcloud secrets versions access latest --secret="${GPU_RUNNER_PAT_SECRET}" 2>/dev/null || true)"
if [ -z "${pat}" ]; then
  echo "::warning::could not read the runner PAT; leaving the runner record for the reaper"
  exit 0
fi

runner_id="$(GH_TOKEN="${pat}" gh api --paginate \
  "/repos/${GH_REPO_FULL}/actions/runners" \
  --jq ".runners[] | select(.name == \"${INSTANCE}\") | .id" 2>/dev/null | head -1)"

if [ -z "${runner_id}" ]; then
  echo "no runner record named ${INSTANCE}; nothing to deregister"
  exit 0
fi

echo "deregistering runner ${INSTANCE} (id ${runner_id})"
GH_TOKEN="${pat}" gh api -X DELETE "/repos/${GH_REPO_FULL}/actions/runners/${runner_id}" \
  || echo "::warning::could not deregister runner ${runner_id}; the reaper sweeps offline runners"

exit 0
