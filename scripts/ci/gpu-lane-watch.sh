#!/usr/bin/env bash
# Watch one GPU lane VM to a verdict, collect its evidence, and check that the
# evidence is about the thing we asked for (waired-ai/waired#590).
#
# The VM has no inbound channel. It publishes control state through guest
# attributes and its logs through GCS; this reads both. Everything here is a
# poll of state the VM pushed — there is no SSH, and no IAM on this side that
# would allow one (roles/compute.osAdminLogin and roles/iap.tunnelResourceAccessor
# were removed from github-ci in 2026-05, see the private repo's
# docs/records/20260508.md).
#
# Exits with the lane's own exit status, or non-zero for any way the lane could
# have reported success without having measured anything.
#
# Env:
#   INSTANCE, ZONE, ARTIFACT_URI   from gpu-lane-up.sh's outputs
#   GPU_LANE                       vllm | agentgrade
#   GPU_LANE_SHA                   the commit the VM was told to check out
#   GPU_LANE_TARGETS               what we asked the vllm lane to run
set -uo pipefail

: "${INSTANCE:?INSTANCE is required}"
: "${ZONE:?ZONE is required}"
: "${ARTIFACT_URI:?ARTIFACT_URI is required}"
: "${GPU_LANE:?GPU_LANE is required}"
: "${GPU_LANE_SHA:?GPU_LANE_SHA is required}"

readonly POLL_INTERVAL="${POLL_INTERVAL:-20}"        # 3/min, against a 10/min/VM cap
readonly READY_TIMEOUT="${READY_TIMEOUT:-900}"       # boot + disk + clone
readonly LANE_TIMEOUT="${LANE_TIMEOUT:-5400}"        # 90 min, under the job timeout
readonly HEARTBEAT_STALE="${HEARTBEAT_STALE:-240}"   # 4 missed 60 s beats

attrs=""            # the whole waired namespace, refreshed once per poll
fetch_attrs() {
  local json
  # ONE API call per poll. Guest attributes are capped at 10 queries/minute per
  # VM instance, so a read per key would spend the budget and start failing.
  json="$(gcloud compute instances get-guest-attributes "${INSTANCE}" \
            --zone="${ZONE}" --format=json 2>/dev/null)" || return 1
  attrs="$(printf '%s' "${json}" \
    | jq -c '[.[] | select(.namespace=="waired")] | map({(.key): .value}) | add // {}' 2>/dev/null)" || return 1
  [ -n "${attrs}" ]
}
attr() { printf '%s' "${attrs}" | jq -r --arg k "$1" '.[$k] // ""'; }

fail() { echo "::error::$*" >&2; collect; exit 1; }

collect() {
  echo "::group::collecting evidence from ${ARTIFACT_URI}"
  mkdir -p gpu-lane-artifacts
  gcloud storage cp "${ARTIFACT_URI}/*" gpu-lane-artifacts/ --quiet 2>&1 | tail -5 || true
  ls -l gpu-lane-artifacts/ 2>/dev/null || echo "(nothing arrived)"
  # Flatten into the workspace under the names the workflow uploads.
  for f in gpu-lane-artifacts/*; do
    [ -f "${f}" ] && cp -f "${f}" "./$(basename "${f}")"
  done
  echo "::endgroup::"
}

echo "watching ${INSTANCE} in ${ZONE} (lane=${GPU_LANE})"
start="${SECONDS}"
last_progress=""
ready=0

while :; do
  if ! fetch_attrs; then
    # A VM that was deleted under us, or a transient API error. Distinguish.
    if ! gcloud compute instances describe "${INSTANCE}" --zone="${ZONE}" \
           --format='value(status)' >/dev/null 2>&1; then
      fail "${INSTANCE} no longer exists; it was deleted while the lane was running"
    fi
    sleep "${POLL_INTERVAL}"; continue
  fi

  status="$(attr lane-status)"
  progress="$(attr lane-progress)"
  if [ -n "${progress}" ] && [ "${progress}" != "${last_progress}" ]; then
    printf '[%5ds] %s\n' "$(( SECONDS - start ))" "${progress}"
    last_progress="${progress}"
  fi

  case "${status}" in
    pass|fail) break ;;
  esac

  if [ "${ready}" = "0" ] && [ -n "$(attr lane-ready)" ]; then
    ready=1
    echo "lane started after $(( SECONDS - start ))s"
  fi

  if [ "${ready}" = "0" ] && [ $(( SECONDS - start )) -gt "${READY_TIMEOUT}" ]; then
    echo "::group::serial console"
    gcloud compute instances get-serial-port-output "${INSTANCE}" --zone="${ZONE}" 2>/dev/null | tail -60 || true
    echo "::endgroup::"
    fail "${INSTANCE} never started its lane within ${READY_TIMEOUT}s"
  fi

  # The heartbeat comes from the supervisor, not from lane output, so a stale
  # one means the VM is gone rather than that a kernel compile went quiet.
  hb="$(attr lane-heartbeat)"
  if [ "${ready}" = "1" ] && [ -n "${hb}" ]; then
    age=$(( $(date +%s) - hb ))
    if [ "${age}" -gt "${HEARTBEAT_STALE}" ]; then
      fail "${INSTANCE} stopped reporting ${age}s ago; the lane did not finish"
    fi
  fi

  if [ $(( SECONDS - start )) -gt "${LANE_TIMEOUT}" ]; then
    fail "${INSTANCE} did not finish within ${LANE_TIMEOUT}s"
  fi
  sleep "${POLL_INTERVAL}"
done

echo "lane reported '${status}' after $(( SECONDS - start ))s"
collect

exit_code="$(attr lane-exit)"
[ -n "${exit_code}" ] || fail "the lane never reported an exit status"

# A lane that failed has already answered the question. The checks below exist
# to stop a FALSE PASS, so running them first would replace a real error
# ("go build failed") with a confusing consequence of it ("you asked for five
# targets and it ran none").
if [ "${exit_code}" != "0" ]; then
  echo "the lane failed with exit status ${exit_code}"
  [ -n "$(attr lane-artifacts)" ] \
    || echo "::warning::the lane also uploaded no evidence, so the logs above may be all there is"
  exit "${exit_code}"
fi

# --- the lane claims it passed; check it passed at the thing we asked for ---
#
# Each of these is a way this lane could report success having measured
# nothing. That is not hypothetical here: this lane spent a quarter reporting
# nothing at all while the nightly stayed green (waired-ai/waired#1229).

got_sha="$(attr lane-sha)"
[ -n "${got_sha}" ] || fail "the lane never reported which commit it checked out"
[ "${got_sha}" = "${GPU_LANE_SHA}" ] \
  || fail "the lane ran ${got_sha}, not the requested ${GPU_LANE_SHA}"

[ "$(attr lane-gpu-ok)" = "1" ] \
  || fail "the lane never confirmed a working GPU (nvidia-smi did not pass)"

image="$(attr lane-image)"
[ -n "${image}" ] && [ "${image}" != unknown ] \
  || fail "the lane could not say which image it booted from"
boot_image="$(gcloud compute disks describe "${INSTANCE}" --zone="${ZONE}" --format='value(sourceImage)' 2>/dev/null)"
case "${boot_image##*/}" in
  *"${image}") ;;
  *) fail "image says '${image}' but the boot disk is '${boot_image##*/}'" ;;
esac
echo "image: ${boot_image##*/}"

if [ "${GPU_LANE}" = vllm ]; then
  # Set comparison, not string comparison: order is not part of the request.
  want="$(printf '%s' "${GPU_LANE_TARGETS}" | tr ' ' '\n' | sed '/^$/d' | sort -u)"
  got="$(attr lane-targets | tr ' ' '\n' | sed '/^$/d' | sort -u)"
  if [ "${want}" != "${got}" ]; then
    fail "asked for [$(printf '%s' "${want}" | tr '\n' ' ')] but the lane ran [$(printf '%s' "${got}" | tr '\n' ' ')]"
  fi
  echo "targets run: $(printf '%s' "${got}" | tr '\n' ' ')"
fi

# Every file the VM says it uploaded must be here, and be the file it hashed.
manifest="$(attr lane-artifacts)"
[ -n "${manifest}" ] || fail "the lane uploaded no evidence at all"
while IFS=: read -r name want_sha; do
  [ -n "${name}" ] || continue
  [ -f "${name}" ] || fail "the lane reported ${name} but it did not arrive"
  got_sha256="$(sha256sum "${name}" | cut -d' ' -f1)"
  [ "${got_sha256}" = "${want_sha}" ] \
    || fail "${name} arrived corrupted (${got_sha256} != ${want_sha})"
done <<< "${manifest}"
echo "evidence verified:"; printf '%s\n' "${manifest}" | sed '/^$/d;s/^/  /'

echo "lane passed"
exit 0
