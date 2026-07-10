#!/usr/bin/env bash
# macos-installtest-run.sh — drive the *published* macOS installer
# end-to-end inside a throwaway tart macOS VM and assert the transparent
# Claude-proxy data path. The macOS analog of installtest-run.sh (LXD).
#
# Why a VM: `waired claude enable` writes host-wide system managed settings
# (/Library/Application Support/ClaudeCode/managed-settings.json). It must NOT
# run on a dev box that is also running the user's real Claude Code. tart
# (Virtualization.framework) gives a disposable, non-invasive macOS guest.
#
# Flow (mirrors the Linux Tier-1 installer e2e; #488 managed-settings model):
#   1. clone+run a fresh macos-sequoia-base VM, seed an ssh key
#   2. curl install.sh | sh --<channel> --no-init   (real download + SHA
#      verify + /usr/local/bin placement + per-user LaunchAgent)
#   3. sudo waired claude enable
#   4. assert: managed-settings.json written with ANTHROPIC_BASE_URL ->
#      127.0.0.1:9472 and NO credential (subscription preserved); the loopback
#      gateway LISTENs on :9472; and NONE of the retired MITM artifacts appear
#      (no /etc/hosts redirect, no :443 listener, no System-keychain CA, no
#      /etc/zshenv NODE_EXTRA_CA_CERTS). A passthrough curl to the loopback
#      gateway reaches real Anthropic (plain HTTP, no MITM).
#   5. sudo waired claude disable removes the managed ANTHROPIC_BASE_URL.
#   6. tear the VM down (the base OCI image stays cached).
#
# The install is intentionally --no-init (enrollment is interactive
# OAuth): the proxy fails open, which is exactly what proves the
# intercept path. Ollama is skipped by default (irrelevant to fail-open).
#
# Usage:
#   bash scripts/dev/macos-installtest-run.sh                 # edge channel
#   CHANNEL=stable bash scripts/dev/macos-installtest-run.sh  # latest GA
#   KEEP=1 bash scripts/dev/macos-installtest-run.sh          # leave VM up
#
# Env:
#   CHANNEL   edge (default) | stable
#   VM        VM name (default: waired-installtest)
#   IMAGE     base image (default: ghcr.io/cirruslabs/macos-sequoia-base:latest)
#   KEEP      if set, do not delete the VM at the end (debug)
#
# Requires (host, macOS/Apple Silicon): tart, expect, ssh, scp.

set -euo pipefail

CHANNEL="${CHANNEL:-edge}"
VM="${VM:-waired-installtest}"
IMAGE="${IMAGE:-ghcr.io/cirruslabs/macos-sequoia-base:latest}"
KEEP="${KEEP:-}"

case "$CHANNEL" in
  edge)   INSTALL_SH_URL="https://github.com/waired-ai/waired-install/releases/download/edge/install.sh"; INSTALL_ARGS="--edge --no-init" ;;
  stable) INSTALL_SH_URL="https://github.com/waired-ai/waired-install/releases/latest/download/install.sh"; INSTALL_ARGS="--no-init" ;;
  *) echo "unknown CHANNEL=$CHANNEL (want edge|stable)" >&2; exit 2 ;;
esac

for bin in tart expect ssh scp; do
  command -v "$bin" >/dev/null 2>&1 || { echo "missing required tool: $bin" >&2; exit 2; }
done

WORK="$(mktemp -d)"
KEY="$WORK/vmkey"
trap cleanup EXIT

cleanup() {
  if [ -z "$KEEP" ]; then
    echo "+++ tearing down VM $VM"
    tart stop "$VM" >/dev/null 2>&1 || true
    tart delete "$VM" >/dev/null 2>&1 || true
  else
    echo "+++ KEEP set — leaving VM $VM up (ip: $(tart ip "$VM" 2>/dev/null || echo '?')); key: $KEY"
    trap - EXIT   # keep $WORK (holds the ssh key) so the dev can ssh in
    return 0
  fi
  rm -rf "$WORK"
}

log() { printf '\n=== %s ===\n' "$*"; }

# --- 1. VM up + ssh ---------------------------------------------------------
log "clone + run VM ($VM from $IMAGE)"
tart clone "$IMAGE" "$VM"
tart run "$VM" --no-graphics &
IP="$(tart ip "$VM" --wait 120)"
echo "VM ip: $IP"

ssh-keygen -t ed25519 -N '' -f "$KEY" -q
PUB="$(cat "$KEY.pub")"
log "seed ssh key (creds admin/admin via expect)"
/usr/bin/expect <<EOF
log_user 0
set timeout 40
spawn ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null admin@$IP "mkdir -p ~/.ssh && chmod 700 ~/.ssh && printf '%s\n' '$PUB' >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys && echo SEEDED"
expect {
  -re "(yes/no|fingerprint)" { send "yes\r"; exp_continue }
  -re "(P|p)assword:"        { send "admin\r"; exp_continue }
  "SEEDED" {}
  timeout { puts "seed TIMEOUT"; exit 1 }
}
expect eof
EOF

# convenience: run a command in the VM over ssh (flags inlined — zsh on the
# guest does not word-split an unquoted "$SSH" variable).
runvm() { ssh -i "$KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=20 "admin@$IP" "$@"; }

# shellcheck disable=SC2016  # single-quoted on purpose: the guest shell evaluates this, not the host
runvm 'echo "guest: $(sw_vers -productVersion) $(uname -m), sudo:$(sudo -n true 2>/dev/null && echo nopasswd || echo passwd)"'

# --- 2. installer -----------------------------------------------------------
log "real installer: $INSTALL_SH_URL ($INSTALL_ARGS, WAIRED_NO_OLLAMA=1)"
runvm "curl -fsSL '$INSTALL_SH_URL' | WAIRED_NO_OLLAMA=1 sh -s -- $INSTALL_ARGS"

# --- 3-6. proxy e2e (assertions run guest-side) -----------------------------
REMOTE="$WORK/remote-e2e.sh"
cat > "$REMOTE" <<'RSH'
#!/bin/bash
set +e
SD="$HOME/Library/Application Support/waired"
pass=0; fail=0
chk()   { if [ "$2" = 0 ]; then echo "  ok  : $1"; pass=$((pass+1)); else echo "  FAIL: $1"; fail=$((fail+1)); fi; }
chkno() { if [ "$2" != 0 ]; then echo "  ok  : $1"; pass=$((pass+1)); else echo "  FAIL: $1"; fail=$((fail+1)); fi; }
wait_hosts() { for _ in $(seq 1 12); do grep -q api.anthropic.com /etc/hosts && c=present || c=absent; [ "$c" = "$1" ] && return 0; sleep 1; done; return 1; }

echo "--- post-install: binaries runnable, agent up ---"
test -x /usr/local/bin/waired && test -x /usr/local/bin/waired-agent; chk "binaries installed + executable" $?
xattr -l /usr/local/bin/waired 2>/dev/null | grep -qi quarantine; chkno "no Gatekeeper quarantine xattr" $?
/usr/local/bin/waired version --json >/dev/null 2>&1; chk "waired version runs (not Gatekeeper-blocked)" $?
echo "    version: $(/usr/local/bin/waired version --json 2>/dev/null)"
launchctl print "gui/$(id -u)/com.waired.agent" 2>/dev/null | grep -q "state = running"; chk "agent LaunchAgent running (state dir created)" $?
curl -fsS -m 5 http://127.0.0.1:9476/waired/v1/status >/dev/null 2>&1; chk "agent mgmt API responding" $?

echo "--- claude enable (managed settings, #488) ---"
MS="/Library/Application Support/ClaudeCode/managed-settings.json"
sudo /usr/local/bin/waired claude enable; echo "(enable rc=$?)"; sleep 2
test -f "$MS"; chk "managed-settings.json written" $?
grep -q '127.0.0.1:9472' "$MS" 2>/dev/null; chk "ANTHROPIC_BASE_URL -> loopback gateway :9472" $?
! grep -qiE 'ANTHROPIC_AUTH_TOKEN|ANTHROPIC_API_KEY|apiKeyHelper' "$MS"; chk "no credential in managed settings (subscription preserved)" $?
nc -z 127.0.0.1 9472 >/dev/null 2>&1; chk "Claude loopback gateway LISTEN on 127.0.0.1:9472" $?
# None of the retired MITM artifacts must appear.
grep -q api.anthropic.com /etc/hosts; chkno "no /etc/hosts redirect" $?
sudo lsof -nP -iTCP:443 -sTCP:LISTEN 2>/dev/null | grep -q waired; chkno "no waired listener on :443" $?
sudo security find-certificate -c "waired Claude proxy CA" /Library/Keychains/System.keychain >/dev/null 2>&1; chkno "no MITM CA in System keychain" $?
grep -q NODE_EXTRA_CA_CERTS /etc/zshenv 2>/dev/null; chkno "/etc/zshenv has no NODE_EXTRA_CA_CERTS" $?

echo "--- passthrough (non-message path -> real Anthropic, plain HTTP loopback) ---"
code=$(curl -sS -o /tmp/cout -w '%{http_code}' http://127.0.0.1:9472/v1/models --max-time 25 2>/dev/null)
{ [ -n "$code" ] && [ "$code" != "000" ]; }; chk "loopback gateway reachable + passthrough to real Anthropic (http=$code)" $?

echo "--- claude disable ---"
sudo /usr/local/bin/waired claude disable >/dev/null 2>&1; sleep 1
{ ! test -f "$MS" || ! grep -q '127.0.0.1:9472' "$MS"; }; chk "managed-settings ANTHROPIC_BASE_URL removed" $?

echo
echo "RESULT pass=$pass fail=$fail"
[ "$fail" = 0 ]
RSH
scp -i "$KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "$REMOTE" "admin@$IP:/tmp/remote-e2e.sh" >/dev/null
log "proxy install + e2e assertions"
runvm 'bash /tmp/remote-e2e.sh'

echo
echo "macos-installtest: PASS ($CHANNEL channel)"
