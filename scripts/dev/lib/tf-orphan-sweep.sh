# Source-only helper: sweep_orphan_testnet_resources.
#
# Generalises tf-orphan-vpc.sh's "subnet exists in GCP but not in state"
# recovery to the *rest* of a slot's per-resource set. Recovers from a
# cancelled / SIGKILL'd `terraform apply` (the cancel-in-progress race in
# issue #470) that created GCP resources but never persisted them to the
# remote state file. The next `terraform apply` would otherwise hit
# `googleapi: Error 409: ... already exists` on each deterministically-
# named resource and abort, wedging the slot red until the reaper sweeps
# it.
#
# Strategy: for the current SLOT, delete via gcloud every per-slot
# resource that EXISTS in GCP but is ABSENT from terraform state, in
# dependency-safe order, so the upcoming apply re-creates it cleanly.
# Resources that ARE in state are left untouched — terraform owns them.
# This is the gcloud-pre-clean pattern already used by preview-destroy.yml
# (#341).
#
# Scope:
#   - Only resources with DETERMINISTIC names can 409, so only those are
#     swept: agent VMs, the relay MIG + static IP + firewalls, the
#     per-slot intra-WG firewalls, the mapwake Pub/Sub topics, and the
#     Cloud Run services.
#   - Subnets stay on the IMPORT path (import_orphan_testnet_subnets in
#     tf-orphan-vpc.sh): import is non-destructive and avoids re-allocating
#     the IPv6 /64. Call this sweep BEFORE the subnet import so orphaned
#     VMs/firewalls referencing an orphan subnet are gone first.
#   - The relay instance template is NOT swept: it uses name_prefix, so
#     terraform appends a random suffix and every apply gets a unique
#     name — it can never 409. Leftover templates cost nothing and are
#     reconciled by terraform's create_before_destroy / full destroy.
#   - The bypass-CP Spanner DB (waired-test-${SLOT}) is NEVER touched:
#     its gcloud create is already idempotent (describe-then-create) and
#     its lifecycle is owned by the reaper (10-DB-per-instance cap).
#
# Best-effort: a transient gcloud failure must never make bring-up or
# teardown WORSE than today. retry_with_backoff absorbs the iamcredentials
# 503 (#433); on exhaustion it exits, but we run it in a subshell so the
# `|| warn` keeps the caller going. If a critical orphan truly can't be
# removed, the following terraform apply just 409s as it does today (no
# regression) and the reaper / cleanup workflow reconcile later.
#
# Ephemeral-only: persistent slots (local / ad-hoc developer labels) may
# legitimately have GCP resources this slot's state hasn't caught up to
# mid-iteration, so the sweep self-gates and skips them — identical to the
# EPHEMERAL test in testnet-down.sh and the reaper's slot regex.
#
# Required exported env: PROJECT_ID, SLOT. Optional: REGION (default
# asia-northeast1), ZONE (default asia-northeast1-a). The working
# directory must be infra/terraform/envs/dev-workload after
# `terraform init` (so `terraform state list` reads the slot's state).

_SWEEP_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# retry_with_backoff — absorbs the WIF iamcredentials 503 on gcloud calls.
# shellcheck source=retry-backoff.sh
source "${_SWEEP_LIB_DIR}/retry-backoff.sh"

# _sweep ADDR DESCRIBE_ARGS DELETE_ARGS
#   ADDR          exact terraform state address; if present in state the
#                 resource is terraform-managed and left alone.
#   DESCRIBE_ARGS gcloud args (no spaces within an arg) to test existence.
#   DELETE_ARGS   gcloud args to delete it (--quiet is appended).
# Deletes only when the resource exists in GCP AND is absent from state.
_sweep() {
  local addr="$1" describe="$2" delete="$3"
  # Not in GCP → nothing to do (idempotent).
  # shellcheck disable=SC2086
  gcloud --project="${PROJECT_ID}" $describe >/dev/null 2>&1 || return 0
  # In state → terraform owns it; never delete a tracked resource.
  if grep -qxF "${addr}" <<<"${__SWEEP_STATE}"; then
    return 0
  fi
  # shellcheck disable=SC2086
  _orphan_delete "${addr}" $delete
}

# _orphan_delete LABEL GCLOUD_DELETE_ARGS...
# Best-effort delete of a confirmed orphan. Never aborts the caller.
_orphan_delete() {
  local label="$1"; shift
  echo "==> orphan: ${label} in GCP but not in state — deleting" >&2
  # Subshell contains retry_with_backoff's exit-on-exhaustion; `|| warn`
  # makes the whole thing best-effort.
  ( retry_with_backoff "delete orphan ${label}" -- \
      gcloud --project="${PROJECT_ID}" "$@" --quiet ) \
    || echo "::warning::orphan delete failed for ${label}; continuing (terraform/reaper will reconcile)" >&2
}

sweep_orphan_testnet_resources() {
  : "${PROJECT_ID:?sweep_orphan_testnet_resources: PROJECT_ID required}"
  : "${SLOT:?sweep_orphan_testnet_resources: SLOT required}"
  local region="${REGION:-asia-northeast1}"
  local zone="${ZONE:-asia-northeast1-a}"

  # Ephemeral slots only. Persistent (local / ad-hoc) slots are never
  # auto-deleted — mirrors testnet-down.sh's EPHEMERAL gate.
  if ! [[ "${SLOT}" =~ ^(pr-[0-9]+|manual-[0-9]+)$ ]]; then
    echo "==> sweep: SLOT=${SLOT} is persistent — skipping orphan sweep" >&2
    return 0
  fi

  # One state read; `terraform state list` needs no -var and only reads
  # the backend state index. Empty (fresh slot) or failure → treat every
  # candidate as a potential orphan (the gcloud describe still gates).
  __SWEEP_STATE="$(terraform state list 2>/dev/null || true)"

  # --- 1. Agent VMs (zonal). Enumerate live, delete those not in state.
  local vm vmzone
  while IFS=, read -r vm vmzone; do
    [ -z "${vm}" ] && continue
    if grep -qF "\"${vm}\"" <<<"${__SWEEP_STATE}"; then
      continue
    fi
    _orphan_delete "instance ${vm}" \
      compute instances delete "${vm}" --zone="${vmzone}"
  done < <(gcloud --project="${PROJECT_ID}" compute instances list \
             --filter="name~^waired-dev-agent-.+-${SLOT}\$" \
             --format='csv[no-heading](name,zone.basename())' 2>/dev/null || true)

  # --- 2. Relay MIG (zonal). Deleting it also deletes the managed relay
  #        VM, which frees the static IP for step 3.
  _sweep "module.gce_relay[0].google_compute_instance_group_manager.mig" \
    "compute instance-groups managed describe waired-relay-tyo-${SLOT}-mig --zone=${zone}" \
    "compute instance-groups managed delete waired-relay-tyo-${SLOT}-mig --zone=${zone}"

  # --- 3. Relay static IP (regional). After the MIG so it isn't in-use.
  _sweep "module.gce_relay[0].google_compute_address.static_ip" \
    "compute addresses describe waired-relay-tyo-${SLOT}-ip --region=${region}" \
    "compute addresses delete waired-relay-tyo-${SLOT}-ip --region=${region}"

  # --- 4. Relay firewalls (global).
  _sweep "module.gce_relay[0].google_compute_firewall.iap_ssh" \
    "compute firewall-rules describe waired-relay-tyo-${SLOT}-iap-ssh" \
    "compute firewall-rules delete waired-relay-tyo-${SLOT}-iap-ssh"
  _sweep "module.gce_relay[0].google_compute_firewall.disco_udp[0]" \
    "compute firewall-rules describe waired-relay-tyo-${SLOT}-disco" \
    "compute firewall-rules delete waired-relay-tyo-${SLOT}-disco"
  _sweep "module.gce_relay[0].google_compute_firewall.disco_udp_v6[0]" \
    "compute firewall-rules describe waired-relay-tyo-${SLOT}-disco-v6" \
    "compute firewall-rules delete waired-relay-tyo-${SLOT}-disco-v6"

  # --- 5. Per-slot intra-WG firewalls (global), both VPC sides.
  local side
  for side in a b; do
    _sweep "module.testnet_${side}[0].google_compute_firewall.intra_wg" \
      "compute firewall-rules describe waired-dev-testnet-${side}-${SLOT}-allow-intra-wg" \
      "compute firewall-rules delete waired-dev-testnet-${side}-${SLOT}-allow-intra-wg"
    _sweep "module.testnet_${side}[0].google_compute_firewall.intra_wg_v6[0]" \
      "compute firewall-rules describe waired-dev-testnet-${side}-${SLOT}-allow-intra-wg-v6" \
      "compute firewall-rules delete waired-dev-testnet-${side}-${SLOT}-allow-intra-wg-v6"
  done

  # --- 6. Subnets: handled by import_orphan_testnet_subnets (caller runs
  #        it right after this sweep). Not deleted here.

  # --- 7. mapwake Pub/Sub topics (global).
  _sweep "google_pubsub_topic.mapwake_control[0]" \
    "pubsub topics describe waired-control-dev-${SLOT}-mapwake" \
    "pubsub topics delete waired-control-dev-${SLOT}-mapwake"
  _sweep "google_pubsub_topic.mapwake_bypass[0]" \
    "pubsub topics describe waired-control-bypass-dev-${SLOT}-mapwake" \
    "pubsub topics delete waired-control-bypass-dev-${SLOT}-mapwake"

  # --- 8. Cloud Run services (regional).
  _sweep "module.cloud_run_control[0].google_cloud_run_v2_service.control" \
    "run services describe waired-control-dev-${SLOT} --region=${region}" \
    "run services delete waired-control-dev-${SLOT} --region=${region}"
  _sweep "module.cloud_run_control_bypass[0].google_cloud_run_v2_service.control" \
    "run services describe waired-control-bypass-dev-${SLOT} --region=${region}" \
    "run services delete waired-control-bypass-dev-${SLOT} --region=${region}"
}
