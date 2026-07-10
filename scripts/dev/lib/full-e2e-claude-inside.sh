#!/usr/bin/env bash
# Inside-container half of scripts/dev/full-e2e-claude.sh. The runner
# script bind-mounts the waired source tree at /work and invokes us
# via `bash /work/scripts/dev/lib/full-e2e-claude-inside.sh`.
#
# This script never reaches outside /tmp + /root + /work, so the host
# is unaffected even if it crashes mid-stage.
set -euo pipefail

step() { printf '\n[stage] %s\n' "$1"; }
fail() { printf '\n[FAIL] %s\n' "$1" >&2; exit 1; }

WAIRED=/work/bin/waired
[ -x "$WAIRED" ] || fail "waired binary missing at $WAIRED"

export WAIRED_STATE_DIR=/tmp/waired-state
rm -rf "$WAIRED_STATE_DIR"
mkdir -p "$WAIRED_STATE_DIR"

# Plant a user-content shell rc so we can later assert sentinel-block
# insertion + clean removal.
mkdir -p /root
echo "# pre-existing user content" > /root/.bashrc

# -------------------------------------------------------------------
step "1: waired link --dry-run prints a coherent plan"
# -------------------------------------------------------------------
PLAN=$("$WAIRED" link --dry-run)
echo "$PLAN" | grep -qE 'apply coding-agent integration' || fail "dry-run summary missing"
echo "$PLAN" | grep -q 'http://127.0.0.1:9473' || fail "dry-run missing gateway URL"

# -------------------------------------------------------------------
step "2: waired link all writes env.sh + sentinel + skills"
# -------------------------------------------------------------------
"$WAIRED" link all

[ -f "$WAIRED_STATE_DIR/integrations/env.sh" ] || fail "env.sh not written"
[ -f "$WAIRED_STATE_DIR/integrations/env.fish" ] || fail "env.fish not written"
[ -f "$WAIRED_STATE_DIR/secrets/gateway-token" ] || fail "gateway-token not generated"
[ -f /root/.claude/skills/waired-status/SKILL.md ] || fail "claude-code waired-status skill missing"
[ -f /root/.claude/skills/waired-doctor/SKILL.md ] || fail "claude-code waired-doctor skill missing"
grep -q 'waired managed' /root/.bashrc || fail "sentinel block missing from .bashrc"
grep -q '# pre-existing user content' /root/.bashrc || fail "user content overwritten"

# -------------------------------------------------------------------
step "3: source env.sh produces the documented variables"
# -------------------------------------------------------------------
# shellcheck disable=SC1091
. "$WAIRED_STATE_DIR/integrations/env.sh"

[ "$ANTHROPIC_BASE_URL" = "http://127.0.0.1:9473/anthropic" ] || fail "ANTHROPIC_BASE_URL=$ANTHROPIC_BASE_URL"
[ "${#ANTHROPIC_AUTH_TOKEN}" -eq 64 ] || fail "ANTHROPIC_AUTH_TOKEN length=${#ANTHROPIC_AUTH_TOKEN}"
[ "$ANTHROPIC_MODEL" = "waired-coding-auto" ] || fail "ANTHROPIC_MODEL=$ANTHROPIC_MODEL"
[ "$OPENAI_BASE_URL" = "http://127.0.0.1:9473/v1" ] || fail "OPENAI_BASE_URL=$OPENAI_BASE_URL"
[ "$OPENAI_API_KEY" = "$ANTHROPIC_AUTH_TOKEN" ] || fail "OPENAI_API_KEY mismatch"

GATEWAY_TOKEN=$(cat "$WAIRED_STATE_DIR/secrets/gateway-token")
[ "$ANTHROPIC_AUTH_TOKEN" = "$GATEWAY_TOKEN" ] || fail "env.sh / gateway-token mismatch"

# -------------------------------------------------------------------
step "4: idempotent re-apply changes nothing on disk"
# -------------------------------------------------------------------
SUM_BEFORE=$(sha256sum /root/.bashrc "$WAIRED_STATE_DIR/integrations/env.sh" "$WAIRED_STATE_DIR/secrets/gateway-token")
"$WAIRED" link all >/dev/null
SUM_AFTER=$(sha256sum /root/.bashrc "$WAIRED_STATE_DIR/integrations/env.sh" "$WAIRED_STATE_DIR/secrets/gateway-token")
[ "$SUM_BEFORE" = "$SUM_AFTER" ] || fail "second apply mutated state"

# -------------------------------------------------------------------
step "5: stub gateway captures claude's Authorization header"
# -------------------------------------------------------------------
GATEWAY_LOG=/tmp/gateway-headers.log
: > "$GATEWAY_LOG"
python3 /work/scripts/dev/lib/stub-gateway.py 9473 "$GATEWAY_LOG" &
STUB_PID=$!
sleep 1

# Whether claude exits 0 or non-zero is uninteresting; we only care
# that it issued a POST with our token. timeout caps the wall clock
# at 30s in case claude streams or buffers oddly inside the container.
( timeout 30s bash -c '. "$WAIRED_STATE_DIR/integrations/env.sh"; claude --print "say ok"' ) || true

kill "$STUB_PID" 2>/dev/null || true
wait 2>/dev/null || true

grep -q "Authorization: Bearer $ANTHROPIC_AUTH_TOKEN" "$GATEWAY_LOG" \
    || { echo "----"; cat "$GATEWAY_LOG"; echo "----"; fail "claude did not present the gateway token"; }

# -------------------------------------------------------------------
step "6: waired unlink removes everything cleanly"
# -------------------------------------------------------------------
"$WAIRED" unlink

[ ! -f "$WAIRED_STATE_DIR/integrations/env.sh" ] || fail "env.sh survived uninstall"
[ ! -f "$WAIRED_STATE_DIR/integrations/env.fish" ] || fail "env.fish survived uninstall"
[ ! -f /root/.claude/skills/waired-status/SKILL.md ] || fail "claude-code skill survived"
[ ! -f /root/.claude/skills/waired-doctor/SKILL.md ] || fail "claude-code doctor skill survived"
! grep -q 'waired managed' /root/.bashrc || fail "sentinel survived"
grep -q '# pre-existing user content' /root/.bashrc || fail "user rc content lost during uninstall"

# Gateway token is intentionally left in place by uninstall (it is
# state, not integration) — re-applying gets the same token back.
# That's a feature, not a bug.

echo
echo "[ok] full-e2e-claude inside-container suite passed"
