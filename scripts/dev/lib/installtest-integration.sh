# shellcheck shell=bash
# installtest-integration.sh — Tier-2.5 coding-agent routing sentinel (#496).
#
# Rides the enrolled daemon + gateway that Tier 2 stood up. `--integration`
# enables local inference but PINS the withheld 350M model (IT_BUNDLED_MODEL_ID),
# so the deploy pulls ~0.7 GB instead of the bundled 7B — cheap enough for a
# per-PR Linux leg. This hook waits for the tiny model to be ready, then runs
# the Go routing harness (internal/e2e/integration, -tags integration) which,
# for each coding-agent leg (Claude proxy :9472 / OpenClaw no-token
# data plane :9479), drives one real inference request and asserts via the
# daemon's observability event ring that the completion was SERVED LOCALLY and
# did NOT fail open to real Anthropic.
#
# Depends on installtest-common.sh (it_log) and run.sh's ok()/bad() +
# installtest-enroll.sh's _it_wait_inference_ready(). In-place/native only:
# the harness reaches the daemon over host loopback, which the nested-LXD path
# does not expose (gated to IT_LOCAL=1 here; the macOS/Windows harnesses call
# the same `go test` directly).

: "${IT_TINY_ALIAS:=waired/tiny}"
: "${IT_MGMT_URL:=http://127.0.0.1:9476}"
# Defaults duplicated from installtest-enroll.sh so this file does not depend on
# source order (installtest-run.sh sources enroll first today, but the coupling
# is invisible and one reorder away from an empty path).
: "${IT_BUNDLED_OLLAMA_BIN:=/var/lib/waired/runtimes/ollama/bin/ollama}"
: "${IT_ENGINE_LOG:=/var/lib/waired/runtimes/ollama/logs/engine.log}"

# _it_integration_diag <guest> — the evidence the routing harness cannot reach.
#
# The Claude legs only ever see the intercept's fail-open 502, and the engine's
# own reason (a crashed llama-server, an OOM, a refused bind) lives ONLY in
# engine.log. waired-agent#29 went undiagnosed for a week because this hook had
# no failure branch at all: a segfaulted model runner presented as "waired proxy
# could not reach the upstream API".
#
# Note engine.log is O_TRUNCed on every engine respawn, so this runs as soon
# after the failure as possible — which is also why the Go harness now fails
# fast on a terminal engine error instead of retrying for three minutes first.
_it_integration_diag() {
  local guest="$1"
  gx "$guest" sh -c "OLLAMA_HOST=127.0.0.1:9475 '$IT_BUNDLED_OLLAMA_BIN' ps" 2>&1 \
    | sed 's/^/    :9475 ps| /' || true
  gx "$guest" sh -c "OLLAMA_HOST=127.0.0.1:9475 '$IT_BUNDLED_OLLAMA_BIN' list" 2>&1 \
    | sed 's/^/    :9475 list| /' || true
  gx "$guest" curl -fsS --max-time 5 "$IT_MGMT_URL/waired/v1/inference/status" 2>&1 \
    | sed 's/^/    status| /' || true
  gx "$guest" journalctl -u waired-agent --no-pager -n 120 2>&1 \
    | sed 's/^/    agent| /' || true
  gx "$guest" sh -c "tail -n 200 '$IT_ENGINE_LOG' 2>/dev/null || echo '(no engine.log)'" 2>&1 \
    | sed 's/^/    engine.log| /' || true
}

# assert_integration <guest> — run the routing sentinel against the enrolled
# daemon. Records ok()/bad(); never aborts the run itself.
assert_integration() {
  local guest="$1"

  if [ "${IT_LOCAL:-0}" != 1 ]; then
    it_log "routing sentinel needs the daemon on host loopback; skipping (not --local/native)"
    return 0
  fi
  if ! command -v go >/dev/null 2>&1; then
    bad "go toolchain not on PATH (needed to run the routing harness)"
    return 0
  fi

  # The tiny model was deployed as the pinned bundled model; confirm it is
  # ready before driving requests (idempotent — assert_inference already
  # waited, this just re-reads the mgmt API).
  if _it_wait_inference_ready "$guest" >/dev/null; then
    ok "tiny routing model ready in the enrolled daemon (:9475 via mgmt API)"
  else
    bad "tiny routing model never became ready; skipping the routing harness"
    return 0
  fi

  it_log "running the coding-agent routing sentinel (go test -tags integration)"
  if ( cd "$ROOT" && \
       WAIRED_MGMT_URL="$IT_MGMT_URL" \
       WAIRED_TINY_ALIAS="$IT_TINY_ALIAS" \
       WAIRED_STATE_DIR=/var/lib/waired \
       WAIRED_ANTHROPIC_BLACKHOLED="${IT_ANTHROPIC_BLACKHOLED:-0}" \
       go test -tags integration -count=1 -v -timeout 15m ./internal/e2e/integration/... ); then
    ok "coding-agent routing sentinel: every leg served locally (no fail-open)"
  else
    bad "coding-agent routing sentinel failed (see go test output above)"
    _it_integration_diag "$guest"
  fi
}
