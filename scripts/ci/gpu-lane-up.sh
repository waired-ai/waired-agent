#!/usr/bin/env bash
# Create the ephemeral L4 VM that runs one GPU lane (waired-ai/waired#590).
#
# There is no self-hosted runner and no registration token: the VM is not a
# GitHub runner, it is a machine that runs one lane and pushes the result back
# through guest attributes and GCS. Nothing here needs a PAT.
#
# The instance shape — image, machine type, accelerator, service account,
# max-run-duration, labels — lives in the private infra repo's instance
# template. This file passes parameters and nothing else.
#
# Env (all repo variables, none of them secrets):
#   GH_REPO_FULL                 github.repository
#   GPU_RUNNER_TEMPLATE          instance template self-link
#   GPU_RUNNER_ZONES             comma-separated, cache zone first
#   GPU_RUNNER_CACHE_DISK        zonal cache disk name (optional)
#   GPU_RUNNER_CACHE_ZONE        the zone that disk lives in (optional)
#   GPU_RUNNER_ARTIFACT_BUCKET   bucket the VM writes logs to
#   GPU_LANE                     vllm | agentgrade
#   GPU_LANE_SHA                 commit the VM must check out
#   GPU_LANE_TARGETS             make targets (vllm lane)
#   GPU_LANE_GRADE_MODEL         ollama tag (agentgrade lane)
#   GCP_PROJECT_ID               passed explicitly rather than inherited
set -euo pipefail

: "${GH_REPO_FULL:?GH_REPO_FULL is required}"
: "${GCP_PROJECT_ID:?GCP_PROJECT_ID is required}"
: "${GPU_RUNNER_TEMPLATE:?GPU_RUNNER_TEMPLATE is required}"
: "${GPU_RUNNER_ZONES:?GPU_RUNNER_ZONES is required}"
: "${GPU_RUNNER_ARTIFACT_BUCKET:?GPU_RUNNER_ARTIFACT_BUCKET is required}"
: "${GPU_LANE:?GPU_LANE is required}"
: "${GPU_LANE_SHA:?GPU_LANE_SHA is required}"
: "${GITHUB_RUN_ID:?GITHUB_RUN_ID is required}"
: "${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"

readonly INSTANCE="waired-gpu-l4-${GITHUB_RUN_ID}-${GPU_LANE}"
readonly ARTIFACT_URI="gs://${GPU_RUNNER_ARTIFACT_BUCKET}/${GITHUB_RUN_ID}/${GPU_LANE}"
TARGETS="${GPU_LANE_TARGETS:-}"
GRADE_MODEL="${GPU_LANE_GRADE_MODEL:-}"
CACHE_DISK="${GPU_RUNNER_CACHE_DISK:-}"
CACHE_ZONE="${GPU_RUNNER_CACHE_ZONE:-}"

# Refuse to create a VM that would run nothing. The lane's own guard catches
# this too, but paying for an L4 boot to be told so is the expensive way.
case "${GPU_LANE}" in
  vllm)       [ -n "${TARGETS}" ]     || { echo "::error::GPU_LANE_TARGETS is empty; the vllm lane would run nothing"; exit 1; } ;;
  agentgrade) [ -n "${GRADE_MODEL}" ] || { echo "::error::GPU_LANE_GRADE_MODEL is empty; the agentgrade lane would measure nothing"; exit 1; } ;;
  *)          echo "::error::unknown GPU_LANE '${GPU_LANE}'"; exit 1 ;;
esac

# The cache disk is a single zonal PD, so exactly one VM may hold it read-write.
# It belongs to the vLLM lane, which is the one that re-pulls ~15 GB without it.
cache_expected=0
if [ "${GPU_LANE}" = vllm ] && [ -n "${CACHE_DISK}" ]; then
  cache_expected=1
fi

IFS=',' read -r -a zones <<< "${GPU_RUNNER_ZONES}"
created_zone=""
for raw in "${zones[@]}"; do
  zone="$(echo "${raw}" | tr -d '[:space:]')"
  [ -n "${zone}" ] || continue

  # The disk cannot follow the instance to another zone. A stockout fallback
  # therefore runs cold (~40 min instead of ~15) and must SAY so, because the
  # supervisor refuses to start when it expects a disk that never appears.
  this_run_cache="${cache_expected}"
  if [ "${this_run_cache}" = "1" ] && [ "${zone}" != "${CACHE_ZONE}" ]; then
    echo "::warning::${zone} is not the cache disk's zone; this run is cold"
    this_run_cache=0
  fi

  # NOTE: --metadata REPLACES the template's metadata map; it does not merge.
  # Measured 2026-08-22 on this exact template: an instance created with
  # --metadata=waired-probe=hello came up with that key and nothing else, so
  # every key the guest needs has to be named here. (Guest attributes happen
  # to survive because enable-guest-attributes is also set at PROJECT level,
  # but relying on that would make this depend on a setting nothing here owns.)
  #
  # --disk is NOT passed. With --source-instance-template it discards the
  # template's boot disk and substitutes a default image — measured: a
  # debian-12 10 GB root on a VM that still had its L4 and so still looked
  # healthy. The cache disk is attached after create instead.
  if gcloud compute instances create "${INSTANCE}" \
      --project="${GCP_PROJECT_ID}" \
      --source-instance-template="${GPU_RUNNER_TEMPLATE}" \
      --zone="${zone}" \
      --metadata="enable-guest-attributes=TRUE,google-logging-enabled=true,google-monitoring-enabled=true,waired-repo=${GH_REPO_FULL},waired-sha=${GPU_LANE_SHA},waired-lane=${GPU_LANE},waired-targets=${TARGETS},waired-agentgrade-model=${GRADE_MODEL},waired-cache-expected=${this_run_cache},waired-artifact-uri=${ARTIFACT_URI}" \
      --quiet; then
    created_zone="${zone}"
    cache_expected="${this_run_cache}"
    break
  fi
  echo "::warning::${zone} could not take the instance; trying the next zone"
done

if [ -z "${created_zone}" ]; then
  echo "::error::no zone in ${GPU_RUNNER_ZONES} could create ${INSTANCE}" >&2
  exit 1
fi

# Record the instance BEFORE anything else can fail. A create that succeeds and
# a boot that never reports still leaves a billing VM, and the teardown step can
# only delete what this told it about.
{
  echo "instance=${INSTANCE}"
  echo "zone=${created_zone}"
  echo "artifact_uri=${ARTIFACT_URI}"
} >> "${GITHUB_OUTPUT}"

# "Created" is not "created correctly". Both of the defects above produced a
# RUNNING VM with the right machine type and GPU, so the only thing that
# separates a good create from a silently wrong one is asking.
boot_image="$(gcloud compute disks describe "${INSTANCE}" \
  --project="${GCP_PROJECT_ID}" --zone="${created_zone}" --format='value(sourceImage)')"
case "${boot_image}" in
  *waired-gpu-runner-base*) ;;
  *) echo "::error::${INSTANCE} booted from '${boot_image}', not the baked runner image" >&2; exit 1 ;;
esac
guest_attrs="$(gcloud compute instances describe "${INSTANCE}" \
  --project="${GCP_PROJECT_ID}" --zone="${created_zone}" \
  --format='value(metadata.items.filter("key:enable-guest-attributes").extract("value"))')"
case "${guest_attrs}" in
  *TRUE*) ;;
  *) echo "::error::${INSTANCE} has no enable-guest-attributes; it cannot report anything back" >&2; exit 1 ;;
esac
echo "${INSTANCE} in ${created_zone} booted from ${boot_image##*/}"

if [ "${cache_expected}" = "1" ]; then
  # device-name is the contract with the supervisor, which waits for
  # /dev/disk/by-id/google-waired-cache.
  #
  # There is deliberately no auto-delete flag here: `instances attach-disk` has
  # none (only create does). An attached disk defaults to autoDelete=false —
  # measured, and confirmed by the cache disk surviving an instance delete — so
  # the prevent_destroy disk is safe without saying anything.
  gcloud compute instances attach-disk "${INSTANCE}" \
    --project="${GCP_PROJECT_ID}" \
    --disk="${CACHE_DISK}" --device-name=waired-cache --mode=rw \
    --zone="${created_zone}" --quiet
  echo "attached ${CACHE_DISK} as waired-cache"
fi

echo "cache_expected=${cache_expected}" >> "${GITHUB_OUTPUT}"
