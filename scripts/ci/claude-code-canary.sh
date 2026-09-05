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
#   3. The reactive-compaction trigger. Claude Code classifies a 400 two
#      ways: by the upstream's own wording
#      (/prompt is too long[^0-9]*(\d+)\s*tokens?\s*>\s*(\d+)/i) or by the
#      documented gateway token "capability_rejected: prompt_too_long".
#      waired's synthetic overflow 400 (internal/gateway/anthropic.go)
#      carries the token since waired-agent#1187; its OpenAI surface still
#      answers OpenAI clients in the wording. Both are watched: the wording
#      is the other arm of the same classifier, so losing it means the
#      classifier was restructured and the token needs re-measuring. The Go
#      side of the contract is pinned by
#      gateway.TestAnthropicMessages_OverflowMessageCarriesTheDocumentedToken.
#   4. CLAUDE_CODE_MAX_CONTEXT_TOKENS — the per-session window override the
#      model-route-directives opt-in (#52) writes so the non-"claude-" local
#      /model id ("anthropic-waired-local") gets its real local window. It is
#      honoured only for ids NOT starting with "claude-". If the knob
#      disappears (or starts applying to "claude-*" ids), the directive
#      window mechanism in internal/integration/claudemanaged must be
#      re-verified.
#   5. The `modelPicker` setting, which is how the Waired /model rows are
#      published since waired-agent#1185, and the discovery CREDENTIAL GATE
#      (#332/#488) that is the reason they are not published by discovery:
#      Claude Code skips the /v1/models fetch entirely unless
#      ANTHROPIC_AUTH_TOKEN or a resolved API key is configured, and waired
#      holds no credential by design. If upstream lifts that gate, discovery
#      becomes a second source of rows and the posture should be revisited.
#
# Part 1 greps the released binary for the strings those behaviors hang off.
# Part 2 drives the REAL `claude` binary against a stub gateway
# (canary-models-stub.py) in two legs, both of them contracts a grep cannot
# see:
#   (a) the `modelPicker` lineup waired writes must PARSE. A row Claude Code
#       cannot read is dropped and the rest kept — silently, from the picker's
#       point of view — so a settings warning is the only signal.
#   (b) CLAUDE_CODE_MAX_CONTEXT_TOKENS must still apply to a non-"claude-" id.
#       That predicate is why every reserved id is spelled `waired…`
#       (waired-agent#1185); if it moves, every Waired row silently runs in an
#       assumed window.
# Both WARN rather than fail when they cannot be exercised at all, so the
# canary stays low-noise.
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
# waired-agent#1187: the token the Anthropic-compatible surface emits for a
# context overflow, and the class name it carries. Claude Code matches the
# whole string exactly, so either half disappearing means the recovery is
# gone and an over-window turn fails instead of compacting.
check "capability-rejected token"   "capability_rejected: "
check "prompt-too-long class"       "prompt_too_long"
# Still watched although waired no longer emits it: it is the other arm of
# the same client-side classifier, so a build that drops it has restructured
# the classifier and the token above needs re-measuring. waired's OpenAI
# surface also still answers OpenAI clients in this wording.
check "reactive-compact trigger"    "prompt is too long"
check "max-context-tokens override" "CLAUDE_CODE_MAX_CONTEXT_TOKENS"
# waired-agent#1185: the setting the Waired /model rows are published through,
# and the field that would hide Anthropic's own lineup if waired ever set it
# (waired never does — leaving it unset is what appends rather than replaces).
check "model picker setting"        "modelPicker"
check "picker replace flag"         "replaceBuiltInOptions"
# waired writes ~/.claude/settings.json, which moves with this variable.
check "picker settings config dir"  "CLAUDE_CONFIG_DIR"
# #332: the first term of the discovery credential gate. waired no longer sets
# the discovery flag, and this is why: the fetch never fires on a
# subscription-OAuth host. Its disappearance would mean discovery started
# running on hosts waired configures, which would put a second, competing
# source of rows in the picker.
check "discovery credential gate"   "ANTHROPIC_AUTH_TOKEN"
# #52: the "[1m]" suffix convention "claude-waired-cloud[1m]" rides on. This is
# a code string, not an env name — the window RESOLUTION it drives is still
# owner-verified on device (see the header).
check "1m-window id suffix"         'endsWith("[1m]")'

# --- Part 2: the /model rows E2E — the setting, and the id predicate ---------
# Drive the real `claude` against a stub gateway.
#
# This used to drive Claude Code's DISCOVERY and inspect the private picker
# cache waired wrote by hand. Both are gone with waired-agent#1185: the rows
# come from the documented `modelPicker` setting, so the two things worth
# measuring against a real binary are that the setting is still read the way
# waired writes it, and that the id predicate the row set rests on still holds.
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
stub="${here}/canary-models-stub.py"

stub_pid=""
stub_port=""

# start_models_stub <workdir> — boot canary-models-stub.py on a free port and
# wait for it to answer. Returns non-zero when the stub cannot be exercised;
# callers treat that as "skip with a WARN", never as a drift signal.
start_models_stub() {
  local work="$1" portfile="$1/port"
  python3 "${stub}" "${portfile}" &
  stub_pid=$!
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

# seed_config <cfgdir> — the smallest config that lets `claude -p` run without
# the first-run prompts. Matches what the binary's own sandbox seeds.
seed_config() {
  mkdir -p "$1"
  cat > "$1/.claude.json" <<'JSON'
{"hasCompletedOnboarding":true,"autoUpdates":false,"bypassPermissionsModeAccepted":false}
JSON
}

# --- leg (a): the modelPicker lineup waired writes is accepted ----------------
# A row Claude Code cannot parse is DROPPED and the rest kept, and an
# unparseable lineup is ignored whole — both silently, from the picker's point
# of view. The only trace is a settings warning, so that warning is the signal.
model_picker_accepted() {
  e2e_prereqs "modelPicker E2E" || return 0

  local work cfg out
  work="$(mktemp -d)"
  cfg="${work}/claude-config"
  seed_config "${cfg}"
  # shellcheck disable=SC2064
  trap "stop_models_stub; rm -rf '${work}'" RETURN

  if ! start_models_stub "${work}"; then
    echo "WARN: stub did not start / not reachable — skipping modelPicker E2E" >&2
    return 0
  fi

  # The shape internal/integration/claudecode/modelpicker.go writes.
  cat > "${cfg}/settings.json" <<'JSON'
{
  "modelPicker": {
    "options": [
      { "model": "waired", "label": "Waired", "description": "Any of your computers" },
      { "model": "waired[1m]", "label": "Waired (1M context)", "description": "Any of your computers" },
      { "model": "waired/peer-canary", "label": "Waired peer: canary", "description": "a-model" }
    ]
  }
}
JSON

  out="${work}/out.txt"
  ANTHROPIC_BASE_URL="http://127.0.0.1:${stub_port}" \
  ANTHROPIC_AUTH_TOKEN="canary-dummy-not-a-real-key" \
  CLAUDE_CONFIG_DIR="${cfg}" \
    timeout 90 "${bin}" -p "ping" </dev/null >"${out}" 2>&1 || true

  local rc=0
  if grep -qiE 'invalid modelPicker row|"modelPicker" must be an object' "${out}"; then
    echo "FAIL: E2E — Claude Code rejected the lineup waired writes:" >&2
    grep -iE 'modelPicker' "${out}" | sed -e 's/^/      /' >&2
    rc=1
  else
    echo "OK:   E2E — the modelPicker lineup waired writes parses"
  fi
  return "${rc}"
}

# --- leg (b): the id predicate the row set rests on ---------------------------
# CLAUDE_CODE_MAX_CONTEXT_TOKENS is honoured only for ids that do NOT start
# with "claude-" (measured on 2.1.261, 2026-09-06; the predicate in the bundle
# is `!id.toLowerCase().startsWith("claude-")`). That is why every reserved id
# is spelled `waired…` since waired-agent#1185 — under the old "claude-"
# heads the variable was ignored and every row silently ran in Claude Code's
# assumed 200k session, with an on-screen notice saying the id "isn't
# described by this version's model catalog".
#
# Asymmetric on purpose. A notice on OUR spelling is a hard failure: the
# variable stopped applying and the rows are mis-sized. No notice on the
# "claude-" control is only a WARN: upstream may have dropped the enforcement
# entirely, which costs waired nothing but means this predicate needs
# re-measuring before anyone leans on it again.
id_window_predicate() {
  e2e_prereqs "id window predicate" || return 0

  local work cfg
  work="$(mktemp -d)"
  cfg="${work}/claude-config"
  seed_config "${cfg}"
  # shellcheck disable=SC2064
  trap "stop_models_stub; rm -rf '${work}'" RETURN

  if ! start_models_stub "${work}"; then
    echo "WARN: stub did not start / not reachable — skipping id window predicate" >&2
    return 0
  fi

  # probe <model-id> <outfile>
  probe() {
    ANTHROPIC_BASE_URL="http://127.0.0.1:${stub_port}" \
    ANTHROPIC_AUTH_TOKEN="canary-dummy-not-a-real-key" \
    CLAUDE_CONFIG_DIR="${cfg}" \
    CLAUDE_CODE_MAX_CONTEXT_TOKENS="131072" \
      timeout 90 "${bin}" --debug -p "ping" --model "$1" </dev/null >"$2" 2>&1 || true
  }

  local ours control rc=0
  ours="${work}/ours.txt"
  control="${work}/control.txt"
  probe "waired/canary-probe" "${ours}"
  probe "claude-waired-canary-probe" "${control}"

  if grep -qF "model catalog" "${ours}"; then
    echo "FAIL: id window predicate — CLAUDE_CODE_MAX_CONTEXT_TOKENS no longer applies to" >&2
    echo "      a non-\"claude-\" id, so every Waired /model row is running in an assumed" >&2
    echo "      window. Re-measure the predicate and revisit the id scheme (#1185)." >&2
    rc=1
  else
    echo "OK:   id window predicate — the window variable still applies to waired ids"
  fi

  if ! grep -qF "model catalog" "${control}"; then
    echo "WARN: the \"claude-\"-headed control produced no catalog notice either — upstream" >&2
    echo "      may have dropped unknown-model window enforcement. Not a defect here; the" >&2
    echo "      predicate above is no longer being exercised by its control." >&2
  fi
  return "${rc}"
}

if ! model_picker_accepted; then
  fail=1
fi

if ! id_window_predicate; then
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
