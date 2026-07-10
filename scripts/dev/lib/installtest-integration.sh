# shellcheck shell=bash
# installtest-integration.sh — Tier-2.5 coding-agent routing sentinel (#496).
#
# Rides the enrolled daemon + gateway that Tier 2 stood up. `--integration`
# enables local inference but PINS the tiny 0.5B model (IT_BUNDLED_MODEL_ID),
# so the deploy pulls ~0.4 GB instead of the bundled 7B — cheap enough for a
# per-PR Linux leg. This hook waits for the tiny model to be ready, then runs
# the Go routing harness (internal/e2e/integration, -tags integration) which,
# for each coding-agent leg (Claude proxy :9472 / OpenCode + OpenClaw no-token
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
       go test -tags integration -count=1 -v ./internal/e2e/integration/... ); then
    ok "coding-agent routing sentinel: every leg served locally (no fail-open)"
  else
    bad "coding-agent routing sentinel failed (see go test output above)"
  fi
}
