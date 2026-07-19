#!/usr/bin/env bash
# installtest-run.sh — run the *working-tree* installer end-to-end inside
# an ephemeral LXD guest (real systemd as PID 1) and assert the result.
#
# Tier 1 (default): install.sh -> apt -> systemd. Asserts the package
#   installs, the waired user/state-dir/permissions are right, the unit
#   is enabled+active, the control URL lands in agent.env, and a second
#   install.sh run (update path, real repo) is a clean no-op — the #328
#   FLAG_YES path that the dash matrix can only approximate.
# Tier 2: + headless enroll against a Control Plane, asserting the #335
#   state-dir/ownership/daemon chain (identity under /var/lib/waired owned
#   by waired, daemon serves its mgmt API). Enrol mode = IT_ENROLL_MODE
#   (oidc|bypass|interactive); see lib/installtest-enroll.sh.
# Tier 3: + WireGuard data plane on a real kernel (LXD VM): two guests
#   enroll through the full installer and ping over the overlay.
#
# --inference: exercise the full first-run journey on CPU — install.sh
#   installs Ollama (no --skip-ollama), `waired init` force-enables
#   inference so its deploy phase pulls the bundled model and runs the
#   end-of-init benchmark. Pairs with Tier 2 (IT_ENROLL_MODE=oidc against
#   the real app.dev.waired.net is the no-human path). GPU not required.
#
# --integration (--local/native only; #496): implies --inference but PINS the
#   tiny 0.5B as the bundled model (deploy pulls ~0.4 GB, not the 7B), then runs
#   the coding-agent routing sentinel (internal/e2e/integration, -tags
#   integration) — each leg drives the gateway surface its tool config targets
#   and asserts via the observability event ring that the completion was served
#   LOCALLY and did not fail open to real Anthropic.
#
# A system container is used for Tier 1/2 (fast); Tier 3 forces a VM.
#
# Usage:
#   bash scripts/dev/installtest-run.sh                 # Tier 1, container
#   bash scripts/dev/installtest-run.sh --tier 2        # + headless enroll
#   bash scripts/dev/installtest-run.sh --tier 2 --inference   # + Ollama/model/benchmark (CPU)
#   bash scripts/dev/installtest-run.sh --tier 2 --integration --local  # + routing sentinel (0.5B)
#   bash scripts/dev/installtest-run.sh --tier 3        # + data plane (2 VMs)
#   bash scripts/dev/installtest-run.sh --keep          # don't delete the guest
#   bash scripts/dev/installtest-run.sh --name foo --image ubuntu:22.04
set -euo pipefail

ROOT="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)"
# shellcheck source=scripts/dev/lib/installtest-common.sh
source "$ROOT/scripts/dev/lib/installtest-common.sh"

TIER=1
KEEP=0
WITH_TRAY=0
USE_VM=0
INFER=0
INTEG=0
NAME="g1"
while [ $# -gt 0 ]; do
  case "$1" in
    --tier) shift; TIER="${1:?--tier needs N}" ;;
    --tier=*) TIER="${1#--tier=}" ;;
    --keep) KEEP=1 ;;
    --with-tray) WITH_TRAY=1 ;;
    --inference) INFER=1 ;;
    --integration) INTEG=1; INFER=1 ;;   # routing sentinel rides the inference engine
    --vm) USE_VM=1 ;;
    --local) IT_LOCAL=1 ;;
    --name) shift; NAME="${1:?--name needs a value}" ;;
    --image) shift; IT_IMAGE="${1:?--image needs a value}" ;;
    -h|--help) sed -n '2,33p' "$0"; exit 0 ;;
    *) it_die "unknown argument: $1 (try --help)" ;;
  esac
  shift
done
[ "$TIER" -ge 3 ] && USE_VM=1   # data plane needs a real kernel
# Force-enable inference for the enrol step (read by lib/installtest-enroll.sh).
[ "$INFER" = 1 ] && export IT_INFERENCE_ENABLED=true
# The routing sentinel (#496) pins the tiny 0.5B as the bundled model so the
# deploy pulls ~0.4 GB, not the 7B — cheap enough for a per-PR Linux leg.
[ "$INTEG" = 1 ] && export IT_BUNDLED_MODEL_ID="${IT_BUNDLED_MODEL_ID:-qwen2.5-coder-0.5b-instruct}"

# --local installs waired ON THIS HOST as root (apt + systemd + a service
# user + a running daemon). Safe only on a disposable machine — guard so a
# developer can't nuke their workstation by accident; CI opts in explicitly.
if [ "$IT_LOCAL" = 1 ]; then
  [ "${IT_ALLOW_LOCAL_DESTRUCTIVE:-0}" = 1 ] || it_die \
    "--local root-installs waired on THIS host. Set IT_ALLOW_LOCAL_DESTRUCTIVE=1 to confirm \
(CI does); use the default LXD path on a workstation."
  [ "$TIER" -le 2 ] || it_die "--local supports Tier 1-2 only (Tier 3 needs two guests; use the LXD path)."
fi

it_require curl
[ "$IT_LOCAL" = 1 ] || it_require lxc
export PATH="$HOME/go/bin:$PATH"

# Pull host-side knobs (WAIRED_APT_*, IT_GW, IT_CP_URL) written by host-up.
[ -f "$IT_WORKDIR/env" ] || it_die "no $IT_WORKDIR/env — run installtest-host-up.sh first"
# shellcheck disable=SC1091
source "$IT_WORKDIR/env"
: "${IT_GW:?env missing IT_GW — re-run installtest-host-up.sh}"
[ "$IT_LOCAL" = 1 ] || it_ensure_bridge
it_wait_url "$WAIRED_APT_BASE_URL/key.asc" 10 \
  || it_die "apt repo not reachable at $WAIRED_APT_BASE_URL — run installtest-host-up.sh"

# Enrol target: the real dogfood CP by default (set IT_CONTROL_URL to
# override, e.g. a PR preview). Used both as install.sh's --dev URL
# (written to agent.env) and as the Tier-2/3 enrol --control.
CONTROL_URL="${IT_CONTROL_URL:-https://app.dev.waired.net}"

PASS=0; FAIL=0
ok()   { printf '\033[1;32m[installtest]  ok \033[0m %s\n' "$*"; PASS=$((PASS+1)); }
bad()  { printf '\033[1;31m[installtest] FAIL\033[0m %s\n' "$*" >&2; FAIL=$((FAIL+1)); }
# gx <guest> <cmd...> — run a privileged command in the test environment.
# LXD: `lxc exec` (root in the guest). --local: `sudo` on this host (the LXD
# guest's root maps to host root). The <guest> arg is ignored in --local.
gx() {
  if [ "$IT_LOCAL" = 1 ]; then sudo "${@:2}"; else lxc exec "$1" -- "${@:2}"; fi
}

# Launch a clean guest on the harness bridge and wait until systemd +
# outbound DNS are ready. Echoes nothing; dies on failure.
#
# For --inference we cap the guest's memory (IT_GUEST_MEMORY, default 16GiB):
# LXD's lxcfs virtualizes /proc/meminfo, so without a cap the guest sees the
# *host's* total RAM and waired's model-selection pre-caches an oversized
# model (e.g. a 120B variant on a 120GB host) — unrealistic for a CPU host.
# Set IT_GUEST_MEMORY= (empty) to disable the cap.
launch_guest() {
  local guest="$1" extra=()
  if [ "$IT_LOCAL" = 1 ]; then
    it_log "local mode: installing in place on $(uname -n) (no guest launch)"
    return 0
  fi
  [ "$USE_VM" = 1 ] && extra+=(--vm)
  if [ "$INFER" = 1 ] && [ -n "${IT_GUEST_MEMORY-16GiB}" ]; then
    # Applied at LAUNCH, not via a post-launch `lxc config set`: a VM's
    # memory is fixed at boot, so the old post-launch set was silently
    # ineffective (error swallowed by `|| true`) and a VM guest ran the
    # whole inference journey inside LXD's 1GiB default — lxd-agent was
    # the OOM victim and every in-flight `lxc exec` died with 129
    # mid-download. Containers apply the cap live either way.
    extra+=(-c "limits.memory=${IT_GUEST_MEMORY:-16GiB}")
    [ "$USE_VM" = 1 ] && extra+=(-c "limits.cpu=${IT_GUEST_CPU:-4}")
  fi
  lxc delete --force "$guest" >/dev/null 2>&1 || true
  it_log "launching $guest ($IT_IMAGE$([ "$USE_VM" = 1 ] && echo ', vm'))"
  lxc launch "$IT_IMAGE" "$guest" --network "$IT_BRIDGE" "${extra[@]}" >/dev/null
  it_wait_guest_ready "$guest" || it_die "$guest never became ready"
}

# Run the working-tree install.sh inside the guest, as root, against the
# local apt repo. We exercise the canonical dogfood one-liner shape
# (`--dev`, resolving to CONTROL_URL via WAIRED_DEV_CONTROL_URL). Tier 1
# uses --no-init (enrol is a separate Tier-2 step). Ollama is skipped by
# default; --inference drops --skip-ollama so the real engine installs and
# a later `waired init --inference-enabled=true` can pull + benchmark (CPU).
run_install() {
  local guest="$1"; shift
  local notray=WAIRED_NO_TRAY=1
  [ "$WITH_TRAY" = 1 ] && notray=
  local ollama_arg=--skip-ollama
  [ "$INFER" = 1 ] && ollama_arg=   # install Ollama for the CPU-inference journey
  it_log "running install.sh${IT_LOCAL:+ (local)} in $guest (--dev -> $CONTROL_URL${ollama_arg:+, }${ollama_arg:-, +ollama})"
  # The env knobs point install.sh at the harness-built signed apt repo; it
  # then runs its real `apt-get install waired` path (#328 update path on a
  # re-run). --local runs it straight on the host as root; LXD pushes + execs.
  if [ "$IT_LOCAL" = 1 ]; then
    # shellcheck disable=SC2086  # $notray/$ollama_arg are word-split on purpose
    sudo env \
      WAIRED_APT_BASE_URL="$WAIRED_APT_BASE_URL" \
      WAIRED_APT_KEY_URL="$WAIRED_APT_KEY_URL" \
      WAIRED_APT_SUITE="$WAIRED_APT_SUITE" \
      WAIRED_APT_COMPONENT="$WAIRED_APT_COMPONENT" \
      WAIRED_DEV_CONTROL_URL="$CONTROL_URL" \
      $notray \
      sh "$ROOT/packaging/install/install.sh" --no-init $ollama_arg --dev "$@"
    return
  fi
  lxc file push "$ROOT/packaging/install/install.sh" "$guest/root/install.sh" >/dev/null
  # shellcheck disable=SC2086  # $notray/$ollama_arg are word-split on purpose
  lxc exec "$guest" -- env \
    WAIRED_APT_BASE_URL="$WAIRED_APT_BASE_URL" \
    WAIRED_APT_KEY_URL="$WAIRED_APT_KEY_URL" \
    WAIRED_APT_SUITE="$WAIRED_APT_SUITE" \
    WAIRED_APT_COMPONENT="$WAIRED_APT_COMPONENT" \
    WAIRED_DEV_CONTROL_URL="$CONTROL_URL" \
    $notray \
    sh /root/install.sh --no-init $ollama_arg --dev "$@"
}

# Poll systemctl is-active up to ~15s (the daemon may take a beat to
# settle after `enable --now`).
wait_service_active() {
  local guest="$1" _ st
  for _ in $(seq 1 15); do
    st=$(gx "$guest" systemctl is-active waired-agent 2>/dev/null || true)
    [ "$st" = active ] && return 0
    sleep 1
  done
  return 1
}

assert_tier1() {
  local guest="$1" v
  gx "$guest" dpkg -s waired >/dev/null 2>&1 && ok "package waired installed" || bad "package waired NOT installed"
  gx "$guest" test -f /lib/systemd/system/waired-agent.service && ok "systemd unit present" || bad "systemd unit missing"
  v=$(gx "$guest" systemctl is-enabled waired-agent 2>/dev/null || true)
  [ "$v" = enabled ] && ok "service enabled (is-enabled=$v)" || bad "service not enabled (is-enabled=$v)"
  if wait_service_active "$guest"; then ok "service active"; else
    bad "service not active"; gx "$guest" systemctl --no-pager -l status waired-agent 2>&1 | sed 's/^/    /' || true
    gx "$guest" journalctl -u waired-agent --no-pager -n 30 2>&1 | sed 's/^/    /' || true
  fi
  gx "$guest" id waired >/dev/null 2>&1 && ok "service user 'waired' exists" || bad "service user 'waired' missing"
  v=$(gx "$guest" stat -c '%U:%G' /var/lib/waired 2>/dev/null || true)
  [ "$v" = "waired:waired" ] && ok "state dir owned by waired:waired" || bad "state dir owner = $v (want waired:waired)"
  # postinst creates 0750; the daemon tightens the tree to 0700 at boot
  # (secrets.go). Either is fine — the invariant is "no world access".
  v=$(gx "$guest" stat -c '%a' /var/lib/waired 2>/dev/null || true)
  case "$v" in
    700|750) ok "state dir not world-accessible (mode 0$v)" ;;
    *) bad "state dir mode 0$v exposes world bits (want owner-only: 0700/0750)" ;;
  esac
  if gx "$guest" grep -q "^WAIRED_CONTROL_URL=$CONTROL_URL\$" /etc/waired/agent.env 2>/dev/null; then
    ok "control URL written to agent.env"
  else
    bad "control URL not in agent.env"; gx "$guest" cat /etc/waired/agent.env 2>&1 | sed 's/^/    /' || true
  fi
}

# Re-run install.sh: with waired already installed and the repo candidate
# equal to installed, this takes the update path and must be a clean
# no-op ("already the latest"). This is the real-flow #328 FLAG_YES path.
assert_idempotent() {
  local guest="$1"
  it_log "re-running install.sh in $guest (idempotency / update path)"
  if run_install "$guest" >/tmp/it-reinstall.log 2>&1; then
    ok "second install.sh run is a clean no-op (exit 0)"
  else
    bad "second install.sh run failed (exit $?)"; sed 's/^/    /' /tmp/it-reinstall.log >&2 || true
  fi
}

# Simulate the #335 breakage (a pre-#340 root `waired init` left the
# identity/secrets root-owned, crash-looping the User=waired daemon) and
# assert that re-running postinst configure — what an `apt upgrade` does —
# reclaims the whole tree for the service user.
assert_postinst_selfheal() {
  local guest="$1" stray
  it_log "simulating root-owned state tree in $guest (#335), re-running postinst"
  gx "$guest" install -d -m 0700 /var/lib/waired/secrets
  gx "$guest" sh -c 'echo tok > /var/lib/waired/secrets/access_token && chmod 0600 /var/lib/waired/secrets/access_token'
  gx "$guest" chown -R root:root /var/lib/waired
  if ! gx "$guest" dpkg-reconfigure -fnoninteractive waired >/dev/null 2>&1; then
    bad "dpkg-reconfigure waired failed"; return
  fi
  stray=$(gx "$guest" find /var/lib/waired ! -user waired 2>/dev/null || true)
  if [ -z "$stray" ]; then
    ok "postinst re-run reclaims root-owned state tree (self-heal)"
  else
    bad "root-owned paths survive postinst re-run:"
    printf '%s\n' "$stray" | sed 's/^/    /' >&2
  fi
}

# --- drive the requested tier -----------------------------------------
GUEST="$IT_PREFIX-$NAME"

cleanup() {
  if [ "$KEEP" = 1 ]; then
    it_warn "leaving guest(s) up (--keep): $GUEST${TIER_GUESTS:+ $TIER_GUESTS}"
  elif [ "$IT_LOCAL" = 1 ]; then
    # The runner is disposable, so we don't uninstall; just best-effort drop
    # the device's identity so it doesn't linger on the CP.
    command -v it_logout_guest >/dev/null 2>&1 && it_logout_guest "$GUEST"
  else
    for g in "$GUEST" ${TIER_GUESTS:-}; do
      # Best-effort deregister so disposable guests don't pile up on the CP.
      command -v it_logout_guest >/dev/null 2>&1 && it_logout_guest "$g"
      lxc delete --force "$g" >/dev/null 2>&1 || true
    done
  fi
}
TIER_GUESTS=""
trap cleanup EXIT

it_step "Tier $TIER run (guest=$GUEST)"

if [ "$TIER" -le 2 ]; then
  launch_guest "$GUEST"
  run_install "$GUEST"
  assert_tier1 "$GUEST"
  assert_idempotent "$GUEST"
  assert_postinst_selfheal "$GUEST"
  if [ "$TIER" -ge 2 ]; then
    # shellcheck source=scripts/dev/lib/installtest-enroll.sh
    source "$ROOT/scripts/dev/lib/installtest-enroll.sh"
    it_enroll_guest "$GUEST"   # enrol (IT_ENROLL_MODE) against the Control Plane
    assert_tier2 "$GUEST"
    [ "$INFER" = 1 ] && assert_inference "$GUEST"
    if [ "$INTEG" = 1 ]; then
      # shellcheck source=scripts/dev/lib/installtest-integration.sh
      source "$ROOT/scripts/dev/lib/installtest-integration.sh"
      assert_integration "$GUEST"
    fi
    # Last: it toggles pause/resume, so keep it clear of the asserts above.
    assert_mgmt_socket "$GUEST"
  fi
else
  # Tier 3: two VMs, full installer + enrol on each, then overlay ping.
  # shellcheck source=scripts/dev/lib/installtest-enroll.sh
  source "$ROOT/scripts/dev/lib/installtest-enroll.sh"
  A="$IT_PREFIX-${NAME}-a"; B="$IT_PREFIX-${NAME}-b"
  TIER_GUESTS="$A $B"
  for g in "$A" "$B"; do
    launch_guest "$g"; run_install "$g"; assert_tier1 "$g"; it_enroll_guest "$g"; assert_tier2 "$g"
  done
  assert_tier3_ping "$A" "$B"
fi

echo
it_step "Tier $TIER summary: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] || exit 1
