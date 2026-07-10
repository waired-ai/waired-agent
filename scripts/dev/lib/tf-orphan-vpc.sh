# Source-only helper: import_orphan_testnet_subnets.
#
# Recovers from a cancelled / crashed `terraform apply` that managed to
# create the per-slot testnet subnets in GCP but never persisted them
# to the remote state file. The next apply would otherwise hit
# `googleapi: Error 409: alreadyExists` on each subnet and abort.
#
# Renamed from import_orphan_testnet_vpcs (pre-shared-VPC era): under
# the shared-VPC architecture (foundation owns waired-dev-testnet-a
# and waired-dev-testnet-b, workload owns the per-slot /28 subnet
# inside each), only subnets can orphan from workload state. The VPCs
# themselves never appear in workload state.
#
# For each side (a / b), if the subnet `waired-dev-testnet-{a,b}-${SLOT}`
# exists in GCP but isn't tracked in terraform state, import it at
# `module.testnet_${side}[0].google_compute_subnetwork.subnet` so the
# pending apply treats it as a known-existing resource instead of a
# new create.
#
# Required exported env: PROJECT_ID, SLOT, REGION (default
# asia-northeast1), TF_STATE_BUCKET, TAG (current image SHA), and the
# working directory must be infra/terraform/envs/dev-workload after
# `terraform init`.

import_orphan_testnet_subnets() {
  : "${PROJECT_ID:?import_orphan_testnet_subnets: PROJECT_ID required}"
  : "${SLOT:?import_orphan_testnet_subnets: SLOT required}"
  local region="${REGION:-asia-northeast1}"

  local side name addr
  for side in a b; do
    name="waired-dev-testnet-${side}-${SLOT}"
    addr="module.testnet_${side}[0].google_compute_subnetwork.subnet"

    # Does the subnet exist in GCP?
    if ! gcloud --project="${PROJECT_ID}" compute networks subnets describe "${name}" \
        --region="${region}" --format='value(name)' >/dev/null 2>&1; then
      continue
    fi

    # Already tracked in state?
    if terraform state show "${addr}" >/dev/null 2>&1; then
      continue
    fi

    printf '==> orphan subnet %s in GCP but not in state — importing into %s\n' \
      "${name}" "${addr}" >&2
    terraform import \
      -var="slot=${SLOT}" \
      -var="tf_state_bucket=${TF_STATE_BUCKET}" \
      -var=workload_enabled=true \
      -var=testnet_enabled=true \
      -var="image_tag_control=${TAG:-}" \
      -var="image_tag_relay=${TAG:-}" \
      -var="image_tag_agent=${TAG:-}" \
      -var="agent_artifact_filename=" \
      "${addr}" "projects/${PROJECT_ID}/regions/${region}/subnetworks/${name}"
  done
}

