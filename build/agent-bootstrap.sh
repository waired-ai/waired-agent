#!/bin/sh
# Bootstrap a waired-agent on a GCE VM (or any host with the GCE
# metadata server reachable). Starts waired-agent, waits for it to answer
# its management API, and enrolls THROUGH it with an auth key.
#
# The order used to be the other way round: `waired init --bypass-mode`
# first, then `exec waired-agent`. That drove the local enrolment
# implementation waired-agent#175 removes — it registers a device whose
# capabilities the control plane never learns. The agent now comes up
# first and owns enrolment, which is the same order the OS installers
# already use (install.sh linux_service_up -> linux_maybe_init).
#
# Consequence of starting the agent first: this script can no longer
# `exec`, so it stays alive as PID 1 and forwards signals.
#
# Idempotent: if $WAIRED_STATE_DIR already holds an enrolled identity,
# enrollment is skipped and the agent reuses the existing device row.
#
# Required environment:
#   WAIRED_CONTROL_URL    base URL of the bypass CP Cloud Run service
#                         (waired-control-bypass-dev-${slot}, the IAM-gated
#                         `--bypass-idp` service for the slot under test)
#   WAIRED_AUTH_KEY       auth key for this slot's account, minted at
#                         bring-up by scripts/dev/testnet-up.sh (waired#976)
#                         and delivered via the VM's agent.env. Only needed
#                         when the state dir holds no identity yet.
#
# Optional environment:
#   WAIRED_STATE_DIR      identity dir (default: /var/lib/waired)
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

# --login-listen is the endpoint the daemon ADVERTISES at enrolment; it is
# not a bind address (the local-candidate loop corrects it once the engine
# binds). Pointing it at the VM's internal IP is what the old init's
# --endpoint udp4:${ip}:${port} used to do — without it the daemon would
# advertise 127.0.0.1 and no peer could reach this VM.
set -- --bypass-cp-iam --login-listen "${ip}:${listen_port}"
if [ -n "${WAIRED_FORCE_RELAY:-}" ]; then
  set -- "$@" --force-relay
fi
if [ -n "${WAIRED_FALLBACK_AFTER:-}" ]; then
  set -- "$@" --fallback-after "${WAIRED_FALLBACK_AFTER}"
fi

echo "agent-bootstrap: starting waired-agent $*"
milestone agent_exec
waired-agent --state-dir "${state_dir}" "$@" &
agent_pid=$!

# PID 1 must pass container stop / systemd stop through to the agent, which
# `exec` used to do for free.
forward_signal() {
  kill -TERM "${agent_pid}" 2>/dev/null || true
}
trap forward_signal TERM INT

# Wait for the management API before enrolling: `waired init` now drives the
# daemon and fails loudly if nothing answers (#175), so racing it would turn
# a slow start into a hard failure.
i=0
while [ "$i" -lt 60 ]; do
  if curl -fsS -m 2 http://127.0.0.1:9476/waired/v1/status >/dev/null 2>&1; then
    break
  fi
  i=$((i + 1))
  sleep 1
done

if [ ! -f "${state_dir}/identity.json" ]; then
  if [ -z "${WAIRED_AUTH_KEY:-}" ]; then
    echo "agent-bootstrap: WAIRED_AUTH_KEY is required to enroll (none in the environment)" >&2
    kill -TERM "${agent_pid}" 2>/dev/null || true
    wait "${agent_pid}" 2>/dev/null || true
    exit 1
  fi
  echo "agent-bootstrap: enrolling via the daemon (control=${WAIRED_CONTROL_URL} ip=${ip} host=${short_host})"
  milestone enroll_start
  # --inference-enabled=false is explicit rather than left to the
  # hardware-derived default: these VMs exist to exercise NAT traversal, and
  # an accidental "yes" would have the daemon pull gigabytes of model.
  # --skip-integration because there is no user home to wire a coding tool into.
  if ! waired init \
    --state-dir "${state_dir}" \
    --control "${WAIRED_CONTROL_URL}" \
    --auth-key "${WAIRED_AUTH_KEY}" \
    --device-name "${short_host}" \
    --non-interactive \
    --inference-enabled=false \
    --skip-integration; then
    echo "agent-bootstrap: waired init failed" >&2
    kill -TERM "${agent_pid}" 2>/dev/null || true
    wait "${agent_pid}" 2>/dev/null || true
    exit 1
  fi
  milestone enroll_done
else
  echo "agent-bootstrap: identity already present at ${state_dir}, skipping init"
  milestone enroll_skipped
fi

wait "${agent_pid}"
