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
#      /model id ("anthropic-waired-local") gets its real local window. It is
#      honoured only for ids NOT starting with "claude-". If the knob
#      disappears (or starts applying to "claude-*" ids), the directive
#      window mechanism in internal/integration/claudemanaged must be
#      re-verified.
#   5. The discovery CREDENTIAL GATE and the picker's fallback-to-cache read
#      (#332/#407). Claude Code skips the /v1/models fetch entirely unless
#      ANTHROPIC_AUTH_TOKEN or a resolved API key is configured, but the
#      picker reads whatever cache already exists. waired holds NO credential
#      by design (#488), so the agent writes ~/.claude/cache/gateway-models.json
#      itself. That makes three upstream details load-bearing: the cache
#      FILENAME, its relocation under CLAUDE_CONFIG_DIR, and the gate itself
#      (if upstream lifts it, discovery starts overwriting our file and the
#      posture should be revisited).
#   6. CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC — the picker's cache read
#      shares discovery's enable gate, so setting this hides the Waired entries
#      even with a valid cache (anthropics/claude-code#61112). It is documented
#      as a troubleshooting cause in docs-site; if the knob goes, so does that
#      guidance.
#
# Part 1 greps the released binary for the strings those behaviors hang off.
# Part 2 drives the REAL `claude` binary against a stub gateway
# (canary-models-stub.py) in two legs:
#   (a) with a dummy API key — discovery fires, so we can probe the /model
#       picker's ^(claude|anthropic) id filter (#52) and the on-disk schema of
#       gateway-models.json (#407 writes that exact shape). Contracts a grep
#       cannot see.
#   (b) with NO credential at all — the configuration waired's dogfooding
#       actually runs. Discovery must NOT fire and the cache must NOT be
#       written. This is the leg PR #77 never built (it exported a dummy key
#       and classified the credential-less case as unexercisable), which is why
#       #332 went unseen for six weeks.
# Leg (a) hard-fails only on a clear drift signal and WARNs when it cannot
# exercise discovery at all, so the canary stays low-noise. Leg (b) hard-fails
# when the cache IS written: that is the unambiguous signal that the gate moved.
#
# NOT probed here, and deliberately so: "an unknown claude-* id defaults to a
# 200k window", "a [1m] suffix outranks CLAUDE_CODE_MAX_CONTEXT_TOKENS", and
# "that env applies to non-claude- ids only". All three are window-RESOLUTION
# behaviours with no headless surface — observing them needs a rendered picker
# or a multi-hundred-k-token turn. They were established by binary analysis in
# #332 and stay owner-verified on device; the strings the resolution hangs off
# (the env names above, the "[1m]" suffix convention) are what Part 1 can hold.
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
# #407: the agent writes this file, so its name and its CLAUDE_CONFIG_DIR
# relocation are waired's contract now, not just Claude Code's. A rename lands
# as a silent no-op — the writer keeps succeeding against a path nothing reads.
check "picker cache filename"       "gateway-models.json"
check "picker cache config dir"     "CLAUDE_CONFIG_DIR"
# #332: the first term of the discovery credential gate. Its disappearance
# means the gate was restructured — re-read the fetch guard before trusting
# leg (b) below to still be testing what it claims.
check "discovery credential gate"   "ANTHROPIC_AUTH_TOKEN"
# #407 troubleshooting: hides the Waired entries even with a valid cache.
check "nonessential-traffic knob"   "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"
# #52: the "[1m]" suffix convention "claude-waired-cloud[1m]" rides on. This is
# a code string, not an env name — the window RESOLUTION it drives is still
# owner-verified on device (see the header).
check "1m-window id suffix"         'endsWith("[1m]")'

# --- Part 2: discovery E2E — the picker id filter, the cache schema, the gate --
# Drive the real `claude` against a stub gateway and inspect the model cache.
# Leg (a) uses a dummy API key so discovery fires; leg (b) uses no credential at
# all, which is the configuration waired ships.
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
stub="${here}/canary-models-stub.py"
schema_probe="${here}/canary-cache-schema.py"
# Directive ids that MUST survive Claude Code's ^(claude|anthropic) filter, and
# the junk id that MUST be filtered out. Keep in sync with canary-models-stub.py
# and internal/proxy/intercept (wired{Local,Cloud}Model).
want_auto="anthropic-waired-auto"
want_local="anthropic-waired-local"
want_cloud="claude-waired-cloud[1m]"
junk_id="waired-junk-should-be-filtered"

# The cache path Claude Code writes and the picker reads. The agent writes this
# same path (#407), so both legs assert against the ISOLATED CLAUDE_CONFIG_DIR
# copy only — never $HOME's. Reading $HOME here would let a developer's own
# Claude Code state decide the verdict, and leg (b) would call a pre-existing
# personal cache "the gate is gone".
cache_rel="cache/gateway-models.json"

# stub_pid/stub_port are set by start_models_stub for the calling leg to use.
stub_pid=""
stub_port=""

# start_models_stub <workdir> — boot canary-models-stub.py on a free port and
# wait for it to answer. Returns non-zero when the stub cannot be exercised;
# callers treat that as "skip with a WARN", never as a drift signal.
start_models_stub() {
  local work="$1" portfile="$1/port"
  python3 "${stub}" "${portfile}" &
  stub_pid=$!
  # Wait (≤5s) for the stub to publish its port and answer.
  for _ in $(seq 1 50); do
    [[ -s "${portfile}" ]] && break
    sleep 0.1
  done
  [[ -s "${portfile}" ]] || return 1
  stub_port="$(cat "${portfile}")"
  curl -fsS "http://127.0.0.1:${stub_port}/v1/models" >/dev/null 2>&1 || return 1
  return 0
}

stop_models_stub() {
  if [[ -n "${stub_pid}" ]]; then
    kill "${stub_pid}" 2>/dev/null || true
    stub_pid=""
  fi
}

# e2e_prereqs <leg-label> — the shared "can we run a real-client leg at all?"
# gate. Missing tooling is a WARN, not a drift signal.
e2e_prereqs() {
  if ! command -v python3 >/dev/null 2>&1; then
    echo "WARN: python3 not available — skipping $1" >&2
    return 1
  fi
  if [[ ! -f "${stub}" ]]; then
    echo "WARN: stub ${stub} missing — skipping $1" >&2
    return 1
  fi
  return 0
}

# --- leg (a): discovery fires (dummy key) — id filter + on-disk schema --------
discovery_e2e() {
  e2e_prereqs "discovery E2E" || return 0

  local work cfg cache
  work="$(mktemp -d)"
  cfg="${work}/claude-config"
  mkdir -p "${cfg}"
  # shellcheck disable=SC2064
  trap "stop_models_stub; rm -rf '${work}'" RETURN

  if ! start_models_stub "${work}"; then
    echo "WARN: stub did not start / not reachable — skipping discovery E2E" >&2
    return 0
  fi

  # Real claude, startup discovery pointed at the stub. Dummy key + isolated
  # config dir; the turn may fail (auth) but discovery fires at startup first.
  ANTHROPIC_BASE_URL="http://127.0.0.1:${stub_port}" \
  CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY="1" \
  ANTHROPIC_API_KEY="canary-dummy-not-a-real-key" \
  CLAUDE_CONFIG_DIR="${cfg}" \
    timeout 60 "${bin}" -p "ping" >/dev/null 2>&1 || true

  cache="${cfg}/${cache_rel}"
  if [[ ! -f "${cache}" ]]; then
    echo "WARN: gateway-models.json not written — discovery not exercised" >&2
    echo "      (dummy key rejected before the fetch, or Claude Code moved the cache path)." >&2
    return 0
  fi

  echo "discovery cache: ${cache}"
  local e2e_fail=0
  if ! grep -qF -- "${want_auto}" "${cache}"; then
    echo "FAIL: E2E — \"${want_auto}\" absent from picker cache (^(claude|anthropic) filter tightened, or discovery dropped it)" >&2
    e2e_fail=1
  fi
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

  # #407: the agent writes this same file by hand, so its shape is a contract
  # waired now depends on. Assert only the fields the writer must produce —
  # upstream ADDING a field is not drift (the reader strips unknown keys), a
  # field disappearing or changing type is.
  if [[ -f "${schema_probe}" ]]; then
    if ! python3 "${schema_probe}" "${cache}" "http://127.0.0.1:${stub_port}"; then
      echo "FAIL: E2E — gateway-models.json schema drifted; the agent-side writer" >&2
      echo "      (internal/integration/claudecode) must be re-verified before it" >&2
      echo "      keeps producing the old shape." >&2
      e2e_fail=1
    fi
  else
    echo "WARN: ${schema_probe} missing — skipping cache schema assert" >&2
  fi

  if [[ "${e2e_fail}" -eq 0 ]]; then
    echo "OK:   discovery E2E — directive ids survive, junk id filtered, schema intact"
  fi
  return "${e2e_fail}"
}

# --- leg (b): no credential — discovery must NOT fire -------------------------
# waired writes ANTHROPIC_BASE_URL with NO credential so the claude.ai
# subscription stays the active auth (#488), and Claude Code's discovery fetch
# is gated on ANTHROPIC_AUTH_TOKEN or a resolved API key (#332). That is why the
# agent writes the cache itself. If this leg ever finds a cache, the gate moved:
# discovery would start overwriting the agent-written file, and the whole #407
# approach needs re-examining.
credential_gate_probe() {
  e2e_prereqs "credential-gate probe" || return 0

  local work cfg cache
  work="$(mktemp -d)"
  cfg="${work}/claude-config"
  mkdir -p "${cfg}"
  # shellcheck disable=SC2064
  trap "stop_models_stub; rm -rf '${work}'" RETURN

  if ! start_models_stub "${work}"; then
    echo "WARN: stub did not start / not reachable — skipping credential-gate probe" >&2
    return 0
  fi

  # Same invocation as leg (a) minus every credential. `env -u` matters: the
  # canary may run on a developer box that exports one. The isolated
  # CLAUDE_CONFIG_DIR also keeps a real subscription login (and any
  # apiKeyHelper in the user's settings) out of the resolution — a subscription
  # OAuth token does not satisfy the gate anyway, which is the whole finding.
  env -u ANTHROPIC_API_KEY -u ANTHROPIC_AUTH_TOKEN \
    ANTHROPIC_BASE_URL="http://127.0.0.1:${stub_port}" \
    CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY="1" \
    CLAUDE_CONFIG_DIR="${cfg}" \
    timeout 60 "${bin}" -p "ping" >/dev/null 2>&1 || true

  cache="${cfg}/${cache_rel}"
  if [[ -f "${cache}" ]]; then
    echo "FAIL: credential-gate probe — gateway-models.json WAS written with no" >&2
    echo "      credential configured. Either Claude Code lifted the discovery" >&2
    echo "      credential gate (the #332 premise, and the reason the agent writes" >&2
    echo "      that file itself — see internal/integration/claudecode), or this" >&2
    echo "      host supplied a credential the probe did not scrub." >&2
    echo "      cache: ${cache}" >&2
    return 1
  fi
  echo "OK:   credential-gate probe — no credential, no discovery fetch, no cache"
  return 0
}

if ! discovery_e2e; then
  fail=1
fi

if ! credential_gate_probe; then
  fail=1
fi

# --- Part 3: agent-grade fixture weight (#322) --------------------------------
# The agent-grade probe measures models against a fixture whose SHAPE is meant
# to match what a real coding agent sends: many complex tool schemas plus a
# large system prompt. That weight is the whole point — a model that emits
# clean tool calls on a small request can still hand a coding agent raw JSON
# under load, which is the defect the probe exists to catch.
#
# So the fixture's weight is a dependency on somebody else's client, and it can
# drift out from under us silently: Claude Code adds tools, the real request
# doubles, and the fixture keeps grading models against last year's load while
# still reporting "pass".
#
# WARN, never FAIL. A heavier upstream request does not mean anything is broken
# here — it means the fixture floors should be revisited and the catalog's
# verdicts re-measured, which is a human decision. Same low-noise stance as the
# discovery E2E above.
agentgrade_fixture_drift() {
  local measure="${here}/../dev/measure-agent-request.py"
  if ! command -v python3 >/dev/null 2>&1; then
    echo "WARN: python3 not available — skipping agent-grade fixture check" >&2
    return 0
  fi
  if [[ ! -f "${measure}" ]]; then
    echo "WARN: ${measure} missing — skipping agent-grade fixture check" >&2
    return 0
  fi

  local shape work
  work="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "rm -rf '${work}'" RETURN

  if ! shape="$(python3 "${measure}" --timeout 120 -- "${bin}" -p hello 2>"${work}/err")"; then
    echo "WARN: could not measure the real request shape — skipping" >&2
    sed -e 's/^/      /' "${work}/err" >&2 || true
    return 0
  fi

  local tools req_bytes
  tools="$(printf '%s' "${shape}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["tools"])' 2>/dev/null || echo 0)"
  req_bytes="$(printf '%s' "${shape}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["whole_request_bytes"])' 2>/dev/null || echo 0)"
  if [[ "${req_bytes}" -eq 0 ]]; then
    echo "WARN: measured shape had no request size — skipping" >&2
    return 0
  fi

  echo "measured real request: ${tools} tools, ${req_bytes} B"

  # The fixture's own numbers, from the Go side, so this can never disagree
  # with what the probe actually sends.
  local fixture_bytes
  fixture_bytes="$(cd "${here}/../.." && go run ./cmd/catalog-tool agentgrade --fixture-bytes 2>/dev/null || echo 0)"
  if [[ "${fixture_bytes}" -eq 0 ]]; then
    echo "WARN: could not read the fixture's own size — skipping comparison" >&2
    return 0
  fi
  echo "agent-grade fixture:   ${fixture_bytes} B"

  # 2x is the threshold, not equality: the fixture is deliberately lighter than
  # the reference (see internal/agentgrade/fixture.go on why padding to match
  # buys a number rather than pressure). Twice the weight is the point at which
  # "lighter on purpose" stops being a fair description.
  if (( req_bytes > fixture_bytes * 2 )); then
    echo "WARN: the real coding-agent request is now ${req_bytes} B against a" >&2
    echo "      ${fixture_bytes} B fixture — more than 2x. Models are being graded" >&2
    echo "      against a materially easier request than they face in use." >&2
    echo "      Revisit the fixtureMin* floors in internal/agentgrade/fixture.go," >&2
    echo "      grow the fixture, and re-measure the catalog (the fixture revision" >&2
    echo "      changes, so stale verdicts surface as coverage gaps)." >&2
  else
    echo "OK:   agent-grade fixture weight is within 2x of the real request"
  fi
  return 0
}

agentgrade_fixture_drift || true

if [[ "${fail}" -ne 0 ]]; then
  echo "One or more Claude Code invariants waired depends on have changed." >&2
  echo "Re-verify the integration per waired#771 before the next release." >&2
  exit 1
fi
echo "All Claude Code invariants present in ${version}."
