# Source-only helper: clear_stale_tflock.
#
# Detects an orphan terraform GCS-backend state-lock object and force-removes
# it when it is older than STALE_LOCK_MIN minutes (default 30). A
# legitimate in-flight run cannot hold a lock that long given the
# testnet workflow's 20-minute job timeout, so an older lock is
# unambiguously left behind by a cancelled / crashed run and is safe
# to drop.
#
# Required exported env: PROJECT_ID, TF_STATE_BUCKET, SLOT.
# Optional env: STALE_LOCK_MIN (default 30).
#
# Usage (from testnet-up.sh / testnet-down.sh, after `terraform init`
# and before `terraform apply`):
#
#   source "$ROOT/scripts/dev/lib/tf-stale-lock.sh"
#   clear_stale_tflock "envs/dev-workload/${SLOT}"

clear_stale_tflock() {
  local prefix="${1:?clear_stale_tflock: prefix required}"
  : "${PROJECT_ID:?clear_stale_tflock: PROJECT_ID required}"
  : "${TF_STATE_BUCKET:?clear_stale_tflock: TF_STATE_BUCKET required}"
  local stale_min="${STALE_LOCK_MIN:-30}"
  local lock_uri="gs://${TF_STATE_BUCKET}/${prefix}/default.tflock"

  local created_iso
  created_iso=$(gcloud --project="${PROJECT_ID}" storage objects describe \
    "${lock_uri}" --format='value(creation_time)' 2>/dev/null || true)
  if [[ -z "${created_iso}" ]]; then
    return 0
  fi

  local created_epoch now_epoch age_min
  created_epoch=$(date -u -d "${created_iso}" +%s 2>/dev/null || echo 0)
  if (( created_epoch == 0 )); then
    return 0
  fi
  now_epoch=$(date -u +%s)
  age_min=$(( (now_epoch - created_epoch) / 60 ))

  if (( age_min < stale_min )); then
    printf '==> tflock present but fresh (age=%dm < %dm); leaving in place\n' \
      "${age_min}" "${stale_min}" >&2
    return 0
  fi

  printf '==> stale tflock detected (age=%dm >= %dm); force-removing %s\n' \
    "${age_min}" "${stale_min}" "${lock_uri}" >&2
  gcloud --project="${PROJECT_ID}" storage rm "${lock_uri}" --quiet >/dev/null
}
