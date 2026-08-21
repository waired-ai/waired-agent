#!/usr/bin/env bash
# Delete the GPU lane VM (waired-ai/waired#590).
#
# Best-effort by construction, and it exits 0 even when a step fails. A red
# teardown would report "the VM is still costing money" as "the lane broke",
# and those are different things — one is a bill, the other is a regression.
#
# This is the primary teardown but NOT the bound on cost. Two backstops carry
# that, and neither depends on GitHub being reachable:
#   1. the instance template's max_run_duration + instance_termination_action
#      = DELETE, enforced by GCE;
#   2. the hourly reaper in the private infra repo.
#
# There is no runner record to deregister any more: the VM is not a GitHub
# runner. That deletion — and the PAT it needed — is gone with it.
set -uo pipefail

: "${INSTANCE:?INSTANCE is required}"
: "${ZONE:?ZONE is required}"

# The cache disk is attached with --no-auto-delete and carries prevent_destroy,
# so deleting the instance detaches it rather than destroying it.
gcloud compute instances delete "${INSTANCE}" --zone="${ZONE}" --quiet \
  || echo "::warning::could not delete ${INSTANCE}; max-run-duration and the reaper still cover it"

exit 0
