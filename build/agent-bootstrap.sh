#!/bin/sh
# Bootstrap a waired-agent on a GCE VM (or any host with the GCE
# metadata server reachable). Reads the VM's internal IP and short
# hostname from the metadata server, runs `waired init --bypass-mode`
# to enroll into the test network, then execs waired-agent.
#
# Idempotent: if $WAIRED_STATE_DIR already holds an enrolled identity,
# enrollment is skipped and the agent reuses the existing device row.
#
# Required environment:
#   WAIRED_CONTROL_URL    base URL of the bypass CP Cloud Run service
#                         (waired-control-bypass-dev-${slot}, the IAM-gated
#                         `--bypass-idp` service for the slot under test)
#
# Optional environment:
#   WAIRED_STATE_DIR      identity dir (default: /var/lib/waired)
#   WAIRED_BYPASS_EMAIL   override mock email (default <hostname>@test.waired.local)
#   WAIRED_FORCE_RELAY    if non-empty, pass --force-relay to waired-agent
#   WAIRED_FALLBACK_AFTER if non-empty, pass --fallback-after <value>
#   WAIRED_LISTEN_PORT    UDP port for WireGuard (default 51820)
set -eu

if [ -z "${WAIRED_CONTROL_URL:-}" ]; then
  echo "agent-bootstrap: WAIRED_CONTROL_URL is required" >&2
  exit 1
fi

state_dir="${WAIRED_STATE_DIR:-/var/lib/waired}"
listen_port="${WAIRED_LISTEN_PORT:-51820}"

mdget() {
  curl -fsS -H "Metadata-Flavor: Google" "http://metadata.google.internal/computeMetadata/v1/$1"
}

# --- timeline milestone emitter ------------------------------------------------
# Emits a structured bring-up milestone to Cloud Logging (logName
# waired-bootstrap) so scripts/dev/testnet-timeline.sh can decompose the
# per-VM "VM created -> first agent stats" boot floor (OS boot + image pull +
# enroll + agent start). Keyed by labels.instance_name = GCE instance/name,
# which is the SAME label cmd/waired-agent/stats.go stamps on waired_agent_stats,
# so milestones join with stats per-VM.
#
# This script runs in BOTH the docker-mode alpine container (entrypoint; has
# curl + ca-certificates, NO gcloud / jq) and on the host in native mode. curl +
# the metadata SA token + the Cloud Logging REST entries:write API is the only
# path available in both, so we use it uniformly rather than gcloud. The VM SA
# (test_agent) carries roles/logging.logWriter. Best-effort: never fails boot.
_ms_iname=""
milestone() {
  ms_name="$1"
  [ -n "${_ms_iname}" ] || _ms_iname="$(mdget instance/name 2>/dev/null || echo unknown)"
  ms_proj="$(mdget project/project-id 2>/dev/null || echo '')"
  ms_tok="$(mdget instance/service-accounts/default/token 2>/dev/null \
    | sed -n 's/.*"access_token" *: *"\([^"]*\)".*/\1/p')"
  [ -n "${ms_proj}" ] && [ -n "${ms_tok}" ] || return 0
  curl -fsS -m 5 -X POST \
    -H "Authorization: Bearer ${ms_tok}" \
    -H "Content-Type: application/json" \
    "https://logging.googleapis.com/v2/entries:write" \
    -d "{\"logName\":\"projects/${ms_proj}/logs/waired-bootstrap\",\"resource\":{\"type\":\"gce_instance\"},\"labels\":{\"instance_name\":\"${_ms_iname}\",\"event\":\"${ms_name}\"},\"entries\":[{\"severity\":\"INFO\",\"jsonPayload\":{\"msg\":\"bootstrap_milestone\",\"milestone\":\"${ms_name}\",\"instance_name\":\"${_ms_iname}\",\"ts_unix\":$(date +%s)}}]}" \
    >/dev/null 2>&1 || true
  return 0
}

milestone boot_observed

ip="$(mdget instance/network-interfaces/0/ip)"
short_host="$(mdget instance/hostname | cut -d. -f1)"
bypass_email="${WAIRED_BYPASS_EMAIL:-${short_host}@test.waired.local}"

if [ ! -f "${state_dir}/identity.json" ]; then
  echo "agent-bootstrap: enrolling (control=${WAIRED_CONTROL_URL} ip=${ip} host=${short_host})"
  milestone enroll_start
  waired init \
    --state-dir "${state_dir}" \
    --control "${WAIRED_CONTROL_URL}" \
    --bypass-mode \
    --bypass-email "${bypass_email}" \
    --device-name "${short_host}" \
    --listen "0.0.0.0:${listen_port}" \
    --endpoint "udp4:${ip}:${listen_port}"
  milestone enroll_done
else
  echo "agent-bootstrap: identity already present at ${state_dir}, skipping init"
  milestone enroll_skipped
fi

set -- --bypass-cp-iam
if [ -n "${WAIRED_FORCE_RELAY:-}" ]; then
  set -- "$@" --force-relay
fi
if [ -n "${WAIRED_FALLBACK_AFTER:-}" ]; then
  set -- "$@" --fallback-after "${WAIRED_FALLBACK_AFTER}"
fi

echo "agent-bootstrap: exec waired-agent $*"
milestone agent_exec
exec waired-agent --state-dir "${state_dir}" "$@"
