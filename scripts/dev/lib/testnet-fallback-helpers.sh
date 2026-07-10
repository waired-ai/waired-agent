#!/usr/bin/env bash
# testnet-fallback-helpers.sh — shared helpers for the
# testnet-fallback-* suite. Sourced from each scenario script; never
# executed standalone.
#
# Provides:
#   - VM list constants (matching scripts/dev/testnet-punch-verify.sh)
#   - require_testnet_up: aborts if the testnet isn't running
#   - vm_ssh / vm_status: convenience wrappers around gcloud compute ssh
#   - block_direct / unblock_direct: iptables-based direct-UDP isolation
#     between two VMs (idempotent; cleanup trap-friendly)
#   - wait_until_path: poll status.peers[<peer>].current_path until it
#     matches an expected value within a timeout
#   - print_path_matrix: dump current per-VM per-peer state for the
#     scenario log

# Bash strict mode, but only when sourced from a strict caller — leave
# this comment here as a reminder; scenario scripts set their own.

# --- topology ----------------------------------------------------------------

PROJECT_ID="${PROJECT_ID:-dev-waired}"
ZONE="${ZONE:-asia-northeast1-a}"
WG_PORT="${WG_PORT:-51820}"
MGMT_URL="${MGMT_URL:-http://127.0.0.1:9476/waired/v1/status}"

# Slot isolation (matches scripts/dev/testnet-{up,down,status,punch-verify}.sh).
# CI uses SLOT=ci, local devs use SLOT=local (default).
SLOT="${SLOT:-local}"
if ! [[ "$SLOT" =~ ^[a-z][a-z0-9-]{0,15}$ ]]; then
  echo "ERROR: SLOT='${SLOT}' must match ^[a-z][a-z0-9-]{0,15}$" >&2
  exit 2
fi
export SLOT

# Two VPCs, 6 VMs total. Same naming as testnet-punch-verify.sh
# (slot-suffixed since infra+ci slot-isolation landed on main).
VPC_A_VMS=(
  "waired-dev-agent-a1-docker-${SLOT}"
  "waired-dev-agent-a1-native-${SLOT}"
  "waired-dev-agent-a2-docker-${SLOT}"
  "waired-dev-agent-a2-native-${SLOT}"
)
VPC_B_VMS=(
  "waired-dev-agent-b1-docker-${SLOT}"
  "waired-dev-agent-b1-native-${SLOT}"
)
ALL_VMS=( "${VPC_A_VMS[@]}" "${VPC_B_VMS[@]}" )

# Default scenario peer pairing: two same-VPC VMs in VPC A. Direct UDP
# is permitted by the intra_wg firewall in that VPC, so the path
# starts on direct and we have something to break.
DEFAULT_VM_A="${DEFAULT_VM_A:-waired-dev-agent-a1-native-${SLOT}}"
DEFAULT_VM_B="${DEFAULT_VM_B:-waired-dev-agent-a2-native-${SLOT}}"

# Cross-VPC pairing for the asymmetric scenario (where direct is
# expected to fail naturally without intervention).
CROSS_VM_A="${CROSS_VM_A:-waired-dev-agent-a1-native-${SLOT}}"
CROSS_VM_B="${CROSS_VM_B:-waired-dev-agent-b1-native-${SLOT}}"

# Iptables chain we install per scenario; keeps rules grouped so
# cleanup is `iptables -F WAIRED_FALLBACK_TEST -X WAIRED_FALLBACK_TEST`.
IPT_CHAIN="WAIRED_FALLBACK_TEST"
IPT6_CHAIN="WAIRED_FALLBACK_TEST6"

# --- utility -----------------------------------------------------------------

step() { printf '\n==> %s\n' "$*" >&2; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
pass() { printf 'PASS: %s\n' "$*" >&2; }

require_testnet_up() {
  # testnet-status.sh has inverted exit semantics on purpose:
  #   exit 0 = no VMs (testnet down — safe to walk away, no cost)
  #   exit 1 = VMs present (testnet up — must testnet-down later)
  # We need exit-1 here.
  set +e
  bash "$(dirname "${BASH_SOURCE[0]}")/../testnet-status.sh" >/dev/null 2>&1
  local rc=$?
  set -e
  if [[ "$rc" -ne 1 ]]; then
    fail "testnet-status.sh reports not-up (exit $rc); run 'bash scripts/dev/testnet-up.sh' first"
  fi
}

# vm_ssh <vm> <command...> — runs the command on the VM via IAP tunnel.
# Returns the command's exit code; stdout/stderr passed through.
#
# Uses `gcloud beta compute ssh` because plain `gcloud compute ssh`
# errors out under WIF-impersonated principals (the CI path) with
# "SSH using federated workforce identities is not yet generally
# available". `beta` is the documented escape hatch and is fine for
# local-dev Google accounts too.
vm_ssh() {
  local vm="$1"; shift
  gcloud beta compute ssh "$vm" \
    --zone="$ZONE" --project="$PROJECT_ID" --tunnel-through-iap \
    --ssh-flag="-o ConnectTimeout=10" \
    --command="$*"
}

# vm_status <vm> — fetches /waired/v1/status JSON (or empty on
# unreachable). Caller parses with python/jq.
vm_status() {
  local vm="$1"
  vm_ssh "$vm" "curl -s --max-time 4 ${MGMT_URL} 2>/dev/null" 2>/dev/null || true
}

# vm_internal_ip <vm> — returns the VM's primary internal IP.
vm_internal_ip() {
  local vm="$1"
  gcloud compute instances describe "$vm" \
    --zone="$ZONE" --project="$PROJECT_ID" \
    --format='value(networkInterfaces[0].networkIP)' 2>/dev/null
}

# vm_external_ip <vm> — returns the VM's primary external IP (or empty
# if NAT-only). Used for cross-VPC scenarios.
vm_external_ip() {
  local vm="$1"
  gcloud compute instances describe "$vm" \
    --zone="$ZONE" --project="$PROJECT_ID" \
    --format='value(networkInterfaces[0].accessConfigs[0].natIP)' 2>/dev/null
}

# vm_device_id <vm> — returns the CP-assigned device_id (e.g.,
# dev_<hex>) for that VM. Reads /var/lib/waired/identity.json on the
# VM via SSH each call (no caching).
#
# Earlier versions cached the result in /tmp to avoid the ~2s IAP-SSH
# overhead per call, but that bit hard after `testnet-down` +
# `testnet-up` recycled the VMs and re-enrolled them with new
# device_ids: the cached value silently kept the prior identity, the
# scenario polled `status.peers[<old_id>]` which now belonged to a
# stale Spanner peer (current_path=relay, last_switch_reason=
# safety_net, zero samples), and the upgrade trigger that DID fire on
# the actually-live new peer was invisible. Each scenario only calls
# this twice (PEER_A + PEER_B) at start so re-SSH cost is negligible.
vm_device_id() {
  local vm="$1"
  local out
  out="$(vm_ssh "$vm" "sudo python3 -c 'import json; print(json.load(open(\"/var/lib/waired/identity.json\"))[\"device_id\"])' 2>/dev/null" 2>/dev/null | tr -d '\r' | tail -1)"
  if [[ -z "$out" ]]; then
    fail "could not resolve device_id for $vm"
  fi
  printf '%s' "$out"
}

# --- direct UDP isolation ----------------------------------------------------

# install_chain <vm> — creates the IPT_CHAIN if absent and inserts a
# jump from INPUT/OUTPUT to it. Idempotent.
install_chain() {
  local vm="$1"
  vm_ssh "$vm" "sudo bash -c '
    iptables -nL ${IPT_CHAIN} >/dev/null 2>&1 || iptables -N ${IPT_CHAIN}
    iptables -C INPUT -j ${IPT_CHAIN} 2>/dev/null || iptables -I INPUT 1 -j ${IPT_CHAIN}
    iptables -C OUTPUT -j ${IPT_CHAIN} 2>/dev/null || iptables -I OUTPUT 1 -j ${IPT_CHAIN}
  '" >/dev/null
}

# block_direct <vm_local> <vm_peer> [direction=both] — drop UDP/${WG_PORT}
# packets between vm_local and vm_peer in `direction` (both | inbound
# | outbound). Blocks BOTH the intra-VPC IP (from `gcloud compute
# instances describe`) AND the disco-observed public IP (from vm_peer's
# /waired/v1/status.observed_addr) so the rule catches the path the
# agent is actually using — once disco confirms a peer's public addr
# (directHinted=true), the agent prefers it over the intra-VPC
# candidate from the network map, and an intra-VPC-only block is a
# silent no-op.
block_direct() {
  local vm_local="$1"; local vm_peer="$2"; local dir="${3:-both}"
  local peer_internal; peer_internal="$(vm_internal_ip "$vm_peer")"
  if [[ -z "$peer_internal" ]]; then
    fail "could not resolve internal IP for $vm_peer"
  fi
  # Best-effort public addr lookup: SSH to vm_peer, read its
  # observed_addr from the management API (set after the relay STUN
  # observation completes; may be empty for a still-coldstart agent).
  local peer_observed
  peer_observed="$(vm_ssh "$vm_peer" "curl -s --max-time 4 ${MGMT_URL} 2>/dev/null" 2>/dev/null \
    | python3 -c '
import json, sys
try:
    d = json.loads(sys.stdin.read())
except Exception:
    sys.exit(0)
addr = d.get("observed_addr","")
# strip ":port" suffix; iptables wants bare host
if ":" in addr:
    addr = addr.rsplit(":", 1)[0]
print(addr)
' 2>/dev/null | tr -d '\r' | tail -1)"
  install_chain "$vm_local"
  for ip in "$peer_internal" "$peer_observed"; do
    [[ -z "$ip" ]] && continue
    case "$dir" in
      inbound|both)
        vm_ssh "$vm_local" "sudo iptables -A ${IPT_CHAIN} -p udp --dport ${WG_PORT} -s ${ip} -j DROP" >/dev/null
        ;;
    esac
    case "$dir" in
      outbound|both)
        vm_ssh "$vm_local" "sudo iptables -A ${IPT_CHAIN} -p udp --dport ${WG_PORT} -d ${ip} -j DROP" >/dev/null
        ;;
    esac
  done
  step "blocked direct UDP/${WG_PORT} ${dir} between ${vm_local} and ${vm_peer} (internal=${peer_internal}, observed=${peer_observed:-none})"
}

# unblock_direct <vm> — flush + delete the test chain on vm. Safe to
# call even if install_chain was never run.
unblock_direct() {
  local vm="$1"
  vm_ssh "$vm" "sudo bash -c '
    iptables -D INPUT -j ${IPT_CHAIN} 2>/dev/null || true
    iptables -D OUTPUT -j ${IPT_CHAIN} 2>/dev/null || true
    iptables -F ${IPT_CHAIN} 2>/dev/null || true
    iptables -X ${IPT_CHAIN} 2>/dev/null || true
  '" >/dev/null 2>&1 || true
  step "unblocked direct UDP on ${vm}"
}

# --- IPv6 variant ------------------------------------------------------------
#
# Mirrors install_chain / block_direct / unblock_direct using ip6tables
# and IPT6_CHAIN. The ipv6-direct scenario runs WITHOUT installing any
# block — it asserts that the agent's natural state converges on direct
# IPv6. The block_direct_v6 helper is provided for an optional follow-on
# ipv6-block scenario.

install_chain_v6() {
  local vm="$1"
  vm_ssh "$vm" "sudo bash -c '
    ip6tables -nL ${IPT6_CHAIN} >/dev/null 2>&1 || ip6tables -N ${IPT6_CHAIN}
    ip6tables -C INPUT -j ${IPT6_CHAIN} 2>/dev/null || ip6tables -I INPUT 1 -j ${IPT6_CHAIN}
    ip6tables -C OUTPUT -j ${IPT6_CHAIN} 2>/dev/null || ip6tables -I OUTPUT 1 -j ${IPT6_CHAIN}
  '" >/dev/null
}

# vm_external_ipv6 <vm> — gcloud-side lookup of the VM's external /96.
# Empty when the VM is v4-only.
vm_external_ipv6() {
  local vm="$1"
  gcloud compute instances describe "$vm" \
    --zone "${ZONE}" \
    --project "${PROJECT_ID}" \
    --format='value(networkInterfaces[0].ipv6AccessConfigs[0].externalIpv6)' \
    2>/dev/null | tr -d '\r' | tail -1
}

# block_direct_v6 <vm_local> <vm_peer> [direction=both] — drop UDP/WG_PORT
# between vm_local and vm_peer over IPv6. Mirrors block_direct but uses
# ip6tables and the peer's external /96 (no equivalent of NAT for v6 in
# the testnet — VMs route directly via their /96, so no relay-STUN
# observed-addr lookup is needed; the gcloud-side external addr is
# authoritative).
block_direct_v6() {
  local vm_local="$1"; local vm_peer="$2"; local dir="${3:-both}"
  local peer_v6; peer_v6="$(vm_external_ipv6 "$vm_peer")"
  if [[ -z "$peer_v6" ]]; then
    fail "could not resolve external IPv6 for $vm_peer (testnet_enable_ipv6 not set?)"
  fi
  install_chain_v6 "$vm_local"
  case "$dir" in
    inbound|both)
      vm_ssh "$vm_local" "sudo ip6tables -A ${IPT6_CHAIN} -p udp --dport ${WG_PORT} -s ${peer_v6} -j DROP" >/dev/null
      ;;
  esac
  case "$dir" in
    outbound|both)
      vm_ssh "$vm_local" "sudo ip6tables -A ${IPT6_CHAIN} -p udp --dport ${WG_PORT} -d ${peer_v6} -j DROP" >/dev/null
      ;;
  esac
  step "blocked direct UDP/${WG_PORT} ${dir} v6 between ${vm_local} and ${vm_peer} (peer_v6=${peer_v6})"
}

unblock_direct_v6() {
  local vm="$1"
  vm_ssh "$vm" "sudo bash -c '
    ip6tables -D INPUT -j ${IPT6_CHAIN} 2>/dev/null || true
    ip6tables -D OUTPUT -j ${IPT6_CHAIN} 2>/dev/null || true
    ip6tables -F ${IPT6_CHAIN} 2>/dev/null || true
    ip6tables -X ${IPT6_CHAIN} 2>/dev/null || true
  '" >/dev/null 2>&1 || true
  step "unblocked direct UDP v6 on ${vm}"
}

# unblock_all — remove the v4 + v6 chains on every VM. Trap-friendly:
# tolerates already-clean state on either family.
unblock_all() {
  for vm in "${ALL_VMS[@]}"; do
    unblock_direct "$vm" || true
    unblock_direct_v6 "$vm" || true
  done
}

# --- status polling ----------------------------------------------------------

# peer_field <vm> <peer_device_id> <field> — extract one nested field
# from `status.peers[<peer>].<field>`. Empty on unreachable / missing.
#
# We pass the JSON body through stdin AND the Python script via
# /dev/stdin substitution would race; instead we shovel the JSON
# through a here-string from a captured body and pass the python
# program as a -c arg. Single-quote the python source so bash leaves
# all the quotes inside alone.
peer_field() {
  local vm="$1"; local peer="$2"; local field="$3"
  local body
  body="$(vm_status "$vm")"
  [[ -z "$body" ]] && return 0
  python3 -c '
import json, sys
peer, field = sys.argv[1], sys.argv[2]
try:
    data = json.loads(sys.stdin.read())
except Exception:
    sys.exit(0)
for p in data.get("peers", []) or []:
    if p.get("device_id") == peer:
        v = p.get(field)
        if isinstance(v, (str, int, float)):
            print(v)
        elif v is None:
            pass
        else:
            print(json.dumps(v))
        sys.exit(0)
' "$peer" "$field" <<<"$body"
}

# wait_until_path <vm> <peer> <expected_path> [timeout=120] [interval=5]
# Polls peer_field until current_path matches expected_path or timeout.
# Exits 0 on match, 1 on timeout.
wait_until_path() {
  local vm="$1"; local peer="$2"; local want="$3"
  local timeout="${4:-120}"; local interval="${5:-5}"
  local start; start=$(date +%s)
  while :; do
    local got; got="$(peer_field "$vm" "$peer" current_path)"
    printf '  [%s -> %s] current_path=%s want=%s\n' "$vm" "$peer" "${got:-?}" "$want" >&2
    if [[ "$got" == "$want" ]]; then
      return 0
    fi
    if (( $(date +%s) - start > timeout )); then
      return 1
    fi
    sleep "$interval"
  done
}

# wait_until_paths <vm_a> <vm_b> <peer_a_id> <peer_b_id> <want> [timeout]
# Bidirectional version: both VMs must converge on the same path.
wait_until_paths() {
  local va="$1"; local vb="$2"; local pa="$3"; local pb="$4"
  local want="$5"; local timeout="${6:-120}"
  local start; start=$(date +%s)
  while :; do
    local ga gb
    ga="$(peer_field "$va" "$pb" current_path)"
    gb="$(peer_field "$vb" "$pa" current_path)"
    printf '  [%s -> %s] %s | [%s -> %s] %s | want=%s\n' \
      "$va" "$pb" "${ga:-?}" "$vb" "$pa" "${gb:-?}" "$want" >&2
    if [[ "$ga" == "$want" && "$gb" == "$want" ]]; then
      return 0
    fi
    if (( $(date +%s) - start > timeout )); then
      return 1
    fi
    sleep 5
  done
}

# count_switches <vm> <peer> — returns the number of recorded
# last_switch_at values seen across multiple status polls (used by the
# flap-suppression scenario as a coarse "did it flip more than once?").
# Caller should buffer with a separate counter; this helper returns the
# CURRENT switch timestamp only.
last_switch() {
  peer_field "$1" "$2" last_switch_at
}

last_switch_reason() {
  peer_field "$1" "$2" last_switch_reason
}

# print_path_matrix — dump per-VM per-peer current_path / RTT / reason.
print_path_matrix() {
  printf '\n%-35s %-25s %-8s %-12s %-12s %s\n' \
    "VM" "Peer" "Path" "Direct RTT" "Relay RTT" "Last Switch Reason"
  printf '%-35s %-25s %-8s %-12s %-12s %s\n' "--" "----" "----" "----------" "---------" "------------------"
  for vm in "${ALL_VMS[@]}"; do
    body="$(vm_status "$vm" 2>/dev/null)"
    [[ -z "$body" ]] && { printf '%-35s (unreachable)\n' "$vm"; continue; }
    python3 -c '
import json, sys
vm = sys.argv[1]
try:
    d = json.loads(sys.stdin.read())
except Exception:
    print(vm + ": (bad-json)")
    sys.exit(0)
for p in d.get("peers", []) or []:
    did = p.get("device_id", "")
    cp  = p.get("current_path", "")
    drt = p.get("direct_rtt_ms", 0) or 0
    rrt = p.get("relay_rtt_ms", 0) or 0
    rsn = p.get("last_switch_reason", "")
    print("%-35s %-35s %-8s %-12.2f %-12.2f %s" % (vm, did, cp, drt, rrt, rsn))
' "$vm" <<<"$body"
  done
}
