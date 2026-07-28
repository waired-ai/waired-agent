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
#
# milestone <name> [detail] — the optional detail carries a failure reason.
# It is JSON-escaped rather than interpolated raw: an error message holds
# quotes, backslashes and newlines, and a malformed body would make the
# entry vanish exactly when it is the only evidence there is.
_ms_iname=""
_ms_json_escape() {
  # Order matters: backslashes first, or the escapes we add get re-escaped.
  printf '%s' "$1" \
    | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/\t/\\t/g' \
    | awk 'NR>1{printf "\\n"} {printf "%s", $0}'
}
milestone() {
  ms_name="$1"
  ms_detail="${2:-}"
  [ -n "${_ms_iname}" ] || _ms_iname="$(mdget instance/name 2>/dev/null || echo unknown)"
  ms_proj="$(mdget project/project-id 2>/dev/null || echo '')"
  ms_tok="$(mdget instance/service-accounts/default/token 2>/dev/null \
    | sed -n 's/.*"access_token" *: *"\([^"]*\)".*/\1/p')"
  [ -n "${ms_proj}" ] && [ -n "${ms_tok}" ] || return 0
  ms_sev="INFO"
  ms_extra=""
  if [ -n "${ms_detail}" ]; then
    ms_sev="ERROR"
    ms_extra=",\"detail\":\"$(_ms_json_escape "${ms_detail}")\""
  fi
  curl -fsS -m 5 -X POST \
    -H "Authorization: Bearer ${ms_tok}" \
    -H "Content-Type: application/json" \
    "https://logging.googleapis.com/v2/entries:write" \
    -d "{\"logName\":\"projects/${ms_proj}/logs/waired-bootstrap\",\"resource\":{\"type\":\"gce_instance\"},\"labels\":{\"instance_name\":\"${_ms_iname}\",\"event\":\"${ms_name}\"},\"entries\":[{\"severity\":\"${ms_sev}\",\"jsonPayload\":{\"msg\":\"bootstrap_milestone\",\"milestone\":\"${ms_name}\",\"instance_name\":\"${_ms_iname}\",\"ts_unix\":$(date +%s)${ms_extra}}}]}" \
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
#
# Wait for the WRITE socket, not just the TCP read port. `waired init` POSTs
# /login/start, and every mutating route is confined to the local IPC socket
# (waired#838) — the loopback port answers such a POST with 403. So a probe
# of /waired/v1/status proves only that reads work: it went green while the
# socket was missing entirely, and the leg failed twenty minutes later with
# nothing but "did not appear in recent stats" to go on.
#
# The daemon binds the socket fail-open (a bind failure logs and the agent
# keeps serving reads), which is right for a desktop but means this script
# is the only thing that can turn it into a loud failure here.
mgmt_sock="${WAIRED_MGMT_SOCKET:-/run/waired/mgmt.sock}"
i=0
while [ "$i" -lt 60 ]; do
  if curl -fsS -m 2 --unix-socket "${mgmt_sock}" \
      http://waired-mgmt/waired/v1/status >/dev/null 2>&1; then
    break
  fi
  i=$((i + 1))
  sleep 1
done
if ! curl -fsS -m 2 --unix-socket "${mgmt_sock}" \
    http://waired-mgmt/waired/v1/status >/dev/null 2>&1; then
  echo "agent-bootstrap: the management write socket never came up at ${mgmt_sock};" >&2
  echo "  enrolment needs it (waired#838). On a host systemd's RuntimeDirectory=waired" >&2
  echo "  creates the dir; in a container the image must (build/Dockerfile.waired-agent)." >&2
  milestone mgmt_socket_missing
  kill -TERM "${agent_pid}" 2>/dev/null || true
  wait "${agent_pid}" 2>/dev/null || true
  exit 1
fi

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
  # Captured to a file rather than piped: `sh` has no pipefail, so
  # `waired init ... | tee` would report TEE's exit status and this branch
  # could never fire — the same shape as the readiness probe that could
  # never go red.
  init_log=/tmp/waired-init.log
  set +e
  waired init \
    --state-dir "${state_dir}" \
    --control "${WAIRED_CONTROL_URL}" \
    --auth-key "${WAIRED_AUTH_KEY}" \
    --device-name "${short_host}" \
    --non-interactive \
    --inference-enabled=false \
    --skip-integration >"${init_log}" 2>&1
  init_rc=$?
  set -e
  cat "${init_log}"
  if [ "${init_rc}" -ne 0 ]; then
    # init's own output is the only statement of WHY, and it went to
    # container stdout, which nothing collects — so a crash-looping agent
    # was visible only as a device that never appeared. Carry the tail into
    # Cloud Logging, where the bring-up is already looking.
    echo "agent-bootstrap: waired init failed (rc=${init_rc})" >&2
    milestone enroll_failed "$(tail -c 400 "${init_log}" 2>/dev/null || echo unknown)"
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
