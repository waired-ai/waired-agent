#!/usr/bin/env bash
# installtest-macos.sh — run the working-tree macOS installer end-to-end on
# THIS host (a disposable runner) and assert the result. The macOS analog of
# installtest-run.sh's Linux path (#497).
#
# Tier 1: build waired + waired-agent (darwin), tar them, point install.sh's
#   darwin path at the local tarball via WAIRED_INSTALL_BASE_URL (file://), run
#   it, and assert the binaries land in /usr/local/bin, the system LaunchDaemon
#   plist is written + loaded, and the system state dir exists root-owned.
# Tier 2 (--tier 2): + hands-free OIDC enroll (#339) — gcloud (WIF) mints the SA
#   id_token, then `sudo waired init --google-sa-login --oidc-id-token`. Asserts
#   the identity lands under /Library/Application Support/waired and the REAL
#   system daemon reports it on the mgmt API.
#
# --inference (pairs with --tier 2; #514): exercise the full first-run journey on
#   CPU — install.sh installs Ollama (no --skip-ollama) and `waired init
#   --inference-enabled=true` pulls the bundled model in its deploy phase and runs
#   the end-of-init benchmark. Asserts Ollama present, the model in `ollama list`,
#   inference enabled in the persisted config, and a benchmark figure in the init
#   transcript (the macOS analog of lib/installtest-enroll.sh's assert_inference).
#
# Since #520 the agent is a system LaunchDaemon (root, /Library/LaunchDaemons,
# system/ launchctl domain) — boot-time and login-independent, exactly like the
# Linux systemd unit and Windows SCM service. That removes the old per-user
# `gui/<uid>` GUI-session caveat entirely: `launchctl bootstrap system/` works
# on a headless runner, so this test asserts the same real service on all three
# OSes (no subprocess fallback). It needs passwordless sudo (GH macos runners
# have it); install.sh sudo's the privileged steps itself.
set -uo pipefail

ROOT="$(git rev-parse --show-toplevel)"
TIER=1
INFER=0
INTEG=0
while [ $# -gt 0 ]; do
  case "$1" in
    --tier) shift; TIER="${1:?--tier needs N}" ;;
    --tier=*) TIER="${1#--tier=}" ;;
    --inference) INFER=1 ;;
    --integration) INTEG=1; INFER=1 ;;   # routing sentinel rides the inference engine
    -h|--help) sed -n '2,30p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 1 ;;
  esac
  shift
done

# --- enroll knobs (mirror lib/installtest-enroll.sh) ------------------------
IT_CONTROL_URL="${IT_CONTROL_URL:-https://app.dev.waired.net}"
IT_ENROLL_MODE="${IT_ENROLL_MODE:-oidc}"
IT_IMPERSONATE_SA="${IT_IMPERSONATE_SA:-}"

BINDIR="${WAIRED_DARWIN_BINDIR:-/usr/local/bin}"
STATE_DIR="/Library/Application Support/waired"
LABEL="com.waired.agent"
PLIST="/Library/LaunchDaemons/$LABEL.plist"
MGMT="http://127.0.0.1:9476/waired/v1/status"
WORK="$(mktemp -d)"
DIST="$WORK/dist"
INITLOG="$WORK/init.log"   # waired init transcript (model pull + benchmark, --inference)

# --- logging / counters -----------------------------------------------------
PASS=0; FAIL=0
it_step() { printf '\033[1;32m[installtest]\033[0m ==> %s\n' "$*"; }
it_log()  { printf '\033[1;36m[installtest]\033[0m %s\n' "$*"; }
it_warn() { printf '\033[1;33m[installtest]\033[0m %s\n' "$*" >&2; }
ok()   { printf '\033[1;32m[installtest]  ok \033[0m %s\n' "$*"; PASS=$((PASS+1)); }
bad()  { printf '\033[1;31m[installtest] FAIL\033[0m %s\n' "$*" >&2; FAIL=$((FAIL+1)); }
skip() { printf '\033[1;33m[installtest] SKIP\033[0m %s\n' "$*"; }
it_die() { printf '\033[1;31m[installtest]\033[0m %s\n' "$*" >&2; cleanup; exit 1; }

cleanup() {
  # Best-effort teardown: deauth, then unregister the system LaunchDaemon.
  if [ -x "$BINDIR/waired" ]; then
    sudo "$BINDIR/waired" logout --yes --local --state-dir "$STATE_DIR" >/dev/null 2>&1 || true
  fi
  [ -x "$BINDIR/waired-agent" ] && sudo "$BINDIR/waired-agent" uninstall >/dev/null 2>&1 || true
  rm -rf "$WORK" 2>/dev/null || true
}
trap cleanup EXIT

# assert_inference_macos — macOS analog of lib/installtest-enroll.sh's
# assert_inference: prove the Ollama-install -> bundled-model-pull -> benchmark
# tail of the journey ran (Tier-2 --inference). Paths are darwin-specific
# (Ollama.app, the system state dir); config reads use sudo since init wrote the
# state dir root-owned.
assert_inference_macos() {
  local ollama_bin="" cand tps
  for cand in \
      "$(command -v ollama 2>/dev/null || true)" \
      /Applications/Ollama.app/Contents/Resources/ollama \
      /usr/local/bin/ollama /opt/homebrew/bin/ollama; do
    if [ -n "$cand" ] && [ -x "$cand" ]; then ollama_bin="$cand"; break; fi
  done
  if [ -n "$ollama_bin" ]; then
    ok "ollama engine installed (install.sh, $ollama_bin)"
  else
    bad "ollama engine not installed (install.sh should have, without --skip-ollama)"
  fi

  # #567: the bundled engine is waired-owned on :9475 with its own store; the
  # agent (PATH-resolving the Ollama.app binary) pulls there, NOT into the
  # upstream default :11434. `waired init --inference-enabled=true` started the
  # LaunchDaemon and #519-foreground-waited for the pull, so readiness is read
  # from the mgmt API on :9476 — the same source init polls — never a bare
  # `ollama list` (which targets :11434 and is always empty here, the original
  # false negative). Poll briefly to absorb any residual async tail.
  local infurl="http://127.0.0.1:9476/waired/v1/inference/status"
  local out="" state model ready=0 _
  for _ in $(seq 1 60); do          # ~5 min; CPU model pull is minutes-scale
    out="$(curl -fsS --max-time 10 "$infurl" 2>/dev/null || true)"
    if printf '%s' "$out" | grep -qE '"subsystem_state"[[:space:]]*:[[:space:]]*"ready"'; then ready=1; break; fi
    if printf '%s' "$out" | grep -oE '"ready"[[:space:]]*:[[:space:]]*\[[^]]*\]' | grep -qiE 'qwen|coder'; then ready=1; break; fi
    state="$(printf '%s' "$out" | grep -oE '"subsystem_state"[[:space:]]*:[[:space:]]*"[a-z_]+"' | head -1 | grep -oE '"[a-z_]+"$' | tr -d '"')"
    case "$state" in pull_failed|disabled|stopped) break ;; esac
    sleep 5
  done
  if [ "$ready" = 1 ]; then
    model="$(printf '%s' "$out" | grep -oE '"ready"[[:space:]]*:[[:space:]]*\[[^]]*\]' | grep -oiE '(qwen|coder)[^",]*' | head -1)"
    ok "bundled model ready in waired store :9475 (${model:-ready}; via mgmt API)"
  else
    bad "bundled model not ready via mgmt API (deploy/pull failed?)"
    printf '%s\n' "$out" | sed 's/^/    /' >&2
    # Diagnostics from the RIGHT store (:9475), using the resolved binary.
    [ -n "$ollama_bin" ] && OLLAMA_HOST=127.0.0.1:9475 "$ollama_bin" list 2>&1 | sed 's/^/    :9475 /' || true
    # #22: the agent captures `ollama serve`'s own stdout+stderr here, so a
    # startup crash (state="failed", last_error="...exit status 1") leaves
    # its REAL reason in this log — but nothing else surfaces it. State dir
    # is root-owned, hence sudo (as elsewhere in this script).
    local englog="/Library/Application Support/waired/runtimes/ollama/logs/engine.log"
    if sudo test -f "$englog"; then
      echo "    --- ollama engine.log (tail) ---" >&2
      sudo tail -n 60 "$englog" 2>&1 | sed 's/^/    engine.log| /' >&2 || true
    else
      echo "    (no ollama engine.log at $englog)" >&2
    fi
  fi

  if sudo sh -c 'grep -hqsE "\"enabled\" *: *true" "/Library/Application Support/waired"/*.json' 2>/dev/null; then
    ok "inference enabled in persisted agent config"
  else
    bad "inference not enabled in persisted config"
  fi

  # The end-of-init benchmark (offerBenchmark) proves the inference tail ran.
  # Accept either a throughput number (tok/s|tokens/s|throughput) OR the
  # "Local inference works" smoke line: on a host too slow/constrained to
  # measure a stable rate the boot benchmark exhausts its time budget and
  # reports MeasuredTokps=0 ("…interactive performance looks good"), yet a real
  # generation still ran (the 200 that emits the smoke line). Both are printed
  # ONLY after a benchmark ran — never the "run `waired runtimes benchmark`
  # later" tip — so neither is the #564 false positive.
  if [ -f "$INITLOG" ] && grep -qiE 'tok/s|tokens/s|throughput|Local inference works' "$INITLOG"; then
    tps="$(grep -ioE '[0-9]+(\.[0-9]+)? *(tok|tokens)/s' "$INITLOG" | head -1)"
    ok "benchmark ran during init${tps:+ (}${tps}${tps:+)}"
  else
    bad "no benchmark output captured in init transcript ($INITLOG)"
    # Genuine miss (the benchmark never ran) — surface the daemon's own boot
    # benchmark slog for the reason (endpoint 404, engine not ready, …).
    sudo grep -iE 'boot benchmark|benchmark' /Library/Logs/waired-agent.err.log 2>/dev/null | tail -15 | sed 's/^/    agent.err| /' >&2 || true
  fi
}

# Passwordless sudo is a hard requirement now that the agent is a system
# daemon (install.sh sudo's the register/init steps; we sudo the asserts).
sudo -n true 2>/dev/null || it_die "passwordless sudo required (system LaunchDaemon install needs root)"

# --- build the darwin tarball install.sh will consume -----------------------
arch="$(uname -m)"; [ "$arch" = "x86_64" ] && arch=amd64   # arm64 stays arm64
tarball="waired-darwin-${arch}.tar.gz"
ver="$(git -C "$ROOT" rev-parse --short HEAD)"
ldf="-s -w -X github.com/waired-ai/waired-agent/internal/buildinfo.Version=$ver -X github.com/waired-ai/waired-agent/internal/buildinfo.BuildSHA=$ver"

it_step "building waired + waired-agent (darwin/$arch) and packing $tarball"
mkdir -p "$WORK/stage" "$DIST"
( cd "$ROOT"
  GOOS=darwin GOARCH="$arch" CGO_ENABLED=0 go build -trimpath -ldflags="$ldf" -o "$WORK/stage/waired"       ./cmd/waired
  GOOS=darwin GOARCH="$arch" CGO_ENABLED=0 go build -trimpath -ldflags="$ldf" -o "$WORK/stage/waired-agent" ./cmd/waired-agent
) || it_die "go build (darwin/$arch) failed"
printf '0.0.0-%s' "$ver" > "$WORK/stage/VERSION"
tar czf "$DIST/$tarball" -C "$WORK/stage" waired waired-agent VERSION
( cd "$DIST" && shasum -a 256 "$tarball" > "$tarball.sha256" )

# --- Tier 1: run install.sh's darwin path + assert --------------------------
# --inference drops --skip-ollama / WAIRED_NO_OLLAMA so install.sh installs
# Ollama.app (the engine for the Tier-2 model pull + benchmark, #514). The
# default path keeps Ollama out — Tier 1/2 only need the installer + enroll.
inst_args=(--no-init)
inst_env=(WAIRED_INSTALL_BASE_URL="file://$DIST" WAIRED_NO_TRAY=1 WAIRED_NO_EMOJI=1)
if [ "$INFER" = 1 ]; then
  it_step "running install.sh (darwin, --no-init; Ollama enabled for inference)"
else
  inst_args+=(--skip-ollama); inst_env+=(WAIRED_NO_OLLAMA=1)
  it_step "running install.sh (darwin, --no-init --skip-ollama)"
fi
install_rc=0
env "${inst_env[@]}" bash "$ROOT/packaging/install/install.sh" "${inst_args[@]}" || install_rc=$?

it_step "Tier 1 asserts"
[ "$install_rc" -eq 0 ]       && ok "install.sh exited 0"                        || bad "install.sh exited $install_rc"
[ -x "$BINDIR/waired" ]       && ok "waired installed ($BINDIR/waired)"          || bad "waired missing in $BINDIR"
[ -x "$BINDIR/waired-agent" ] && ok "waired-agent installed"                     || bad "waired-agent missing in $BINDIR"
sudo test -f "$PLIST"         && ok "system LaunchDaemon plist written ($LABEL)" || bad "LaunchDaemon plist missing ($PLIST)"
sudo test -d "$STATE_DIR"     && ok "system state dir present"                   || bad "state dir missing ($STATE_DIR)"
owner="$(sudo stat -f '%Su' "$STATE_DIR" 2>/dev/null || true)"
[ "$owner" = "root" ] && ok "state dir owned by root ($owner)" || bad "state dir owner = $owner (want root)"

# The whole point of #520: the system domain loads on a headless runner with no
# GUI (Aqua) session — no per-user gui/<uid> probe, no subprocess fallback.
if sudo launchctl print "system/$LABEL" >/dev/null 2>&1; then
  ok "LaunchDaemon loaded in the system domain"
else
  bad "LaunchDaemon not loaded in system/ (headless system daemon must load without a GUI session)"
fi

# --- Tier 2: hands-free enroll + assert -------------------------------------
if [ "$TIER" -ge 2 ]; then
  [ "$IT_ENROLL_MODE" = oidc ] || it_die "installtest-macos.sh supports IT_ENROLL_MODE=oidc only (got '$IT_ENROLL_MODE')"
  [ -n "$IT_IMPERSONATE_SA" ]  || it_die "IT_ENROLL_MODE=oidc needs IT_IMPERSONATE_SA (the #339 test SA)"
  command -v gcloud >/dev/null 2>&1 || it_die "oidc enroll mints the SA id_token on the host; gcloud not found"

  it_step "enrolling via OIDC grant (google-sa-login, host-minted token)"
  aud="$(curl -fsS --max-time 15 "$IT_CONTROL_URL/v1/login/oidc-grant/audience" 2>/dev/null \
    | sed -n 's/.*"audience":"\([^"]*\)".*/\1/p')"
  [ -n "$aud" ] || it_die "could not resolve the OIDC audience from $IT_CONTROL_URL"
  it_log "minting SA id_token (sa=$IT_IMPERSONATE_SA)"
  tok="$(gcloud auth print-identity-token --impersonate-service-account="$IT_IMPERSONATE_SA" \
    --audiences="$aud" --include-email 2>/dev/null)"
  [ -n "$tok" ] || it_die "failed to mint an SA id_token (CI principal in oidc_grant_token_creators?)"

  device="mac-ci-${GITHUB_RUN_ID:-$(date +%Y%m%d%H%M%S)}"
  inf_flag="--inference-enabled=$([ "$INFER" = 1 ] && echo true || echo false)"
  # Routing sentinel pins the tiny 0.5B so the deploy pulls ~0.4 GB (fits the
  # 4 GB macOS runner; dodges the #573 7B OOM). Zero args when not --integration.
  pin_flag=()
  [ "$INTEG" = 1 ] && pin_flag=(--inference-bundled-model-id=qwen2.5-coder-0.5b-instruct)
  # init runs as root: it writes identity to the system state dir and, since the
  # LaunchDaemon is already registered (Tier 1), (re)starts it in the system/
  # domain so the real daemon re-reads the freshly-enrolled state. With
  # --inference the deploy phase foreground-pulls the bundled model and runs the
  # end-of-init benchmark (#519); tee the transcript for assert_inference_macos.
  # pipefail (set -o, line ~22) makes the `if !` see init's exit, not tee's.
  # ${pin_flag[@]+...} is the unset-safe expansion: an empty array must expand
  # to zero args even under `set -u` on macOS's system bash 3.2.
  if ! sudo env WAIRED_NO_EMOJI=1 "$BINDIR/waired" init --control "$IT_CONTROL_URL" \
        --google-sa-login --oidc-id-token "$tok" \
        --device-name "$device" --non-interactive "$inf_flag" ${pin_flag[@]+"${pin_flag[@]}"} \
        --skip-integration --state-dir "$STATE_DIR" 2>&1 | tee "$INITLOG"; then
    bad "waired init (oidc) failed"
  fi

  it_step "Tier 2 asserts"
  sudo test -f "$STATE_DIR/identity.json" && ok "identity.json written under state dir" \
    || bad "identity.json missing under state dir"

  # Read the enrolled state back through the REAL system daemon's mgmt API —
  # no subprocess. The Keychain machine-key round-trip is fixed (#512) and the
  # daemon now reads the System keychain as root (#520), so a readback failure
  # here is a real regression.
  enrolled=0
  for _ in $(seq 1 40); do
    out="$(curl -fsS --max-time 5 "$MGMT" 2>/dev/null || true)"
    if printf '%s' "$out" | grep -qE '"device_id"[[:space:]]*:[[:space:]]*"dev_'; then enrolled=1; break; fi
    sleep 1
  done
  if [ "$enrolled" = 1 ]; then
    ok "system daemon read the enrolled state and reports an identity"
  else
    bad "system daemon did not report enrolled"
    it_log "recent waired-agent log:"
    sudo log show --predicate 'process == "waired-agent"' --last 2m 2>/dev/null | tail -40 >&2 || true
    [ -f /Library/Logs/waired-agent.err.log ] && sudo tail -40 /Library/Logs/waired-agent.err.log >&2 || true
  fi

  if [ "$INFER" = 1 ]; then
    it_step "inference asserts (--inference)"
    assert_inference_macos
  fi

  if [ "$INTEG" = 1 ]; then
    it_step "coding-agent routing sentinel (--integration)"
    if command -v go >/dev/null 2>&1; then
      # The Go harness (internal/e2e/integration, -tags integration) drives each
      # coding-agent leg at the real gateway surface and asserts via the event
      # ring that the completion was served locally (no fail-open). It pulls +
      # retries the tiny model itself, so it tolerates a still-warming engine.
      if ( cd "$ROOT" && \
           WAIRED_MGMT_URL="http://127.0.0.1:9476" \
           WAIRED_TINY_ALIAS="waired/tiny" \
           WAIRED_STATE_DIR="$STATE_DIR" \
           go test -tags integration -count=1 -v ./internal/e2e/integration/... ); then
        ok "coding-agent routing sentinel: every leg served locally (no fail-open)"
      else
        bad "coding-agent routing sentinel failed (see go test output above)"
      fi
    else
      bad "go toolchain not on PATH (needed to run the routing harness)"
    fi
  fi
fi

echo
it_step "Tier $TIER summary: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] || exit 1
