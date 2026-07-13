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
#
# This script greps the released binary for the strings those behaviors hang
# off. A failure does NOT mean waired is broken — it means a Claude Code
# release changed a load-bearing assumption and a human should re-verify
# (see waired#771 / waired#623 for the analysis methodology).
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

if [[ "${fail}" -ne 0 ]]; then
  echo "One or more Claude Code invariants waired depends on have changed." >&2
  echo "Re-verify the integration per waired#771 before the next release." >&2
  exit 1
fi
echo "All Claude Code invariants present in ${version}."
