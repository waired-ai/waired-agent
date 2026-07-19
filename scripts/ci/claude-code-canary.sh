#!/usr/bin/env bash
# claude-code-canary.sh — weekly invariant check against the LATEST Claude
# Code release (#771). Waired's Claude Code integration composes with three
# behaviors of the Claude Code binary that are contracts, not APIs:
#
#   1. CLAUDE_CODE_AUTO_COMPACT_WINDOW — the env override waired used to
#      write (pre-#771) and now only scrubs/strips. If the knob disappears,
#      the scrub/Remove paths in internal/integration/claudemanaged go dead
#      code and operator guidance changes.
#   2. CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY — managed settings still
#      write this so the /model picker lists waired's /v1/models. If it
#      disappears (or discovery starts consuming max_input_tokens for the
#      compaction window — see managedsettings.go), the integration posture
#      should be revisited.
#   3. The reactive-compaction trigger: Claude Code parses a 400 body with
#      /prompt is too long[^0-9]*(\d+)\s*tokens?\s*>\s*(\d+)/i and compacts +
#      retries. waired's synthetic overflow 400
#      (internal/gateway/anthropic.go) is worded to match; the Go side of the
#      contract is pinned by
#      gateway.TestAnthropicMessages_OverflowMessageMatchesClaudeCodeParser.
#   4. CLAUDE_CODE_MAX_CONTEXT_TOKENS — the per-session window override the
#      model-route-directives opt-in (#52) writes so the non-"claude-" local
#      /model id ("anthropic-waired-local") gets an honest ~256k window. It is
#      honoured only for ids NOT starting with "claude-". If the knob
#      disappears (or starts applying to "claude-*" ids), the directive
#      window mechanism in internal/integration/claudemanaged must be
#      re-verified. The "[1m]" 1M-window suffix is the one remaining #52
#      dependency with no automated probe (it needs a rendered picker); it
#      still relies on the on-device verification recorded in the PR.
#
# Part 1 greps the released binary for the strings those behaviors hang off.
# Part 2 (the discovery E2E) drives the REAL `claude` binary against a stub
# gateway (canary-models-stub.py) and inspects the model cache Claude Code
# writes at startup, to actively probe the /model picker's ^(claude|anthropic)
# id filter (#52) — the contract a grep cannot see: it asserts the reserved
# directive ids survive the filter and a non-matching junk id does not. The E2E
# hard-fails only on a clear drift signal (cache written but filter behavior
# changed); if it cannot exercise discovery at all (no credentials, cache path
# moved), it WARNs rather than reds, so the canary stays low-noise.
#
# A failure does NOT mean waired is broken — it means a Claude Code release
# changed a load-bearing assumption and a human should re-verify (see
# waired#771 / waired#623 for the analysis methodology).
#
# usage: claude-code-canary.sh [path-to-claude-binary]
#        (default: `claude` resolved from PATH; symlinks followed)
set -euo pipefail

bin="${1:-$(command -v claude || true)}"
if [[ -z "${bin}" ]]; then
  echo "FAIL: claude binary not found (install step broken?)" >&2
  exit 1
fi
bin="$(readlink -f "${bin}")"

version="$("${bin}" --version 2>/dev/null || echo "unknown")"
echo "claude binary: ${bin}"
echo "claude version: ${version}"

fail=0
check() {
  local label="$1" pattern="$2"
  if grep -aqF -- "${pattern}" "${bin}"; then
    echo "OK:   ${label} (\"${pattern}\" present)"
  else
    echo "FAIL: ${label} (\"${pattern}\" missing from ${version})" >&2
    fail=1
  fi
}

check "auto-compact env override"   "CLAUDE_CODE_AUTO_COMPACT_WINDOW"
check "gateway model discovery env" "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"
check "reactive-compact trigger"    "prompt is too long"
check "max-context-tokens override" "CLAUDE_CODE_MAX_CONTEXT_TOKENS"

# --- Part 2: discovery E2E — probe the /model picker id filter (#52) ----------
# Drive the real `claude` against a stub gateway and inspect the model cache it
# writes at startup. WARN (not FAIL) when discovery cannot be exercised.
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
stub="${here}/canary-models-stub.py"
# Directive ids that MUST survive Claude Code's ^(claude|anthropic) filter, and
# the junk id that MUST be filtered out. Keep in sync with canary-models-stub.py
# and internal/proxy/intercept (wired{Local,Cloud}Model).
want_local="anthropic-waired-local"
want_cloud="claude-waired-cloud[1m]"
junk_id="waired-junk-should-be-filtered"

discovery_e2e() {
  if ! command -v python3 >/dev/null 2>&1; then
    echo "WARN: python3 not available — skipping discovery E2E" >&2
    return 0
  fi
  if [[ ! -f "${stub}" ]]; then
    echo "WARN: stub ${stub} missing — skipping discovery E2E" >&2
    return 0
  fi

  local work portfile port cfg cache
  work="$(mktemp -d)"
  portfile="${work}/port"
  cfg="${work}/claude-config"
  mkdir -p "${cfg}"

  python3 "${stub}" "${portfile}" &
  local stub_pid=$!
  # shellcheck disable=SC2064
  trap "kill ${stub_pid} 2>/dev/null || true; rm -rf '${work}'" RETURN

  # Wait (≤5s) for the stub to publish its port and answer.
  local i
  for i in $(seq 1 50); do
    [[ -s "${portfile}" ]] && break
    sleep 0.1
  done
  if [[ ! -s "${portfile}" ]]; then
    echo "WARN: stub did not start — skipping discovery E2E" >&2
    return 0
  fi
  port="$(cat "${portfile}")"
  if ! curl -fsS "http://127.0.0.1:${port}/v1/models" >/dev/null 2>&1; then
    echo "WARN: stub not reachable on :${port} — skipping discovery E2E" >&2
    return 0
  fi

  # Real claude, startup discovery pointed at the stub. Dummy key + isolated
  # config dir; the turn may fail (auth) but discovery fires at startup first.
  ANTHROPIC_BASE_URL="http://127.0.0.1:${port}" \
  CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY="1" \
  ANTHROPIC_API_KEY="canary-dummy-not-a-real-key" \
  CLAUDE_CONFIG_DIR="${cfg}" \
    timeout 60 "${bin}" -p "ping" >/dev/null 2>&1 || true

  # Claude Code caches discovered models here (id + display_name, picker-filtered).
  cache=""
  for c in "${cfg}/cache/gateway-models.json" "${HOME}/.claude/cache/gateway-models.json"; do
    [[ -f "${c}" ]] && { cache="${c}"; break; }
  done
  if [[ -z "${cache}" ]]; then
    echo "WARN: gateway-models.json not written — discovery not exercised" >&2
    echo "      (no valid credentials in CI, or Claude Code moved the cache path)." >&2
    return 0
  fi

  echo "discovery cache: ${cache}"
  local e2e_fail=0
  if ! grep -qF -- "${want_local}" "${cache}"; then
    echo "FAIL: E2E — \"${want_local}\" absent from picker cache (^(claude|anthropic) filter tightened, or discovery dropped it)" >&2
    e2e_fail=1
  fi
  if ! grep -qF -- "${want_cloud}" "${cache}"; then
    echo "FAIL: E2E — \"${want_cloud}\" absent from picker cache (filter tightened, or discovery dropped it)" >&2
    e2e_fail=1
  fi
  if grep -qF -- "${junk_id}" "${cache}"; then
    echo "FAIL: E2E — junk id \"${junk_id}\" surfaced in picker cache (^(claude|anthropic) filter loosened/removed)" >&2
    e2e_fail=1
  fi
  if [[ "${e2e_fail}" -eq 0 ]]; then
    echo "OK:   discovery E2E — directive ids survive, junk id filtered"
  fi
  return "${e2e_fail}"
}

if ! discovery_e2e; then
  fail=1
fi

if [[ "${fail}" -ne 0 ]]; then
  echo "One or more Claude Code invariants waired depends on have changed." >&2
  echo "Re-verify the integration per waired#771 before the next release." >&2
  exit 1
fi
echo "All Claude Code invariants present in ${version}."
