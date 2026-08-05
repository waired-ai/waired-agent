#!/usr/bin/env bash
# installtest-macos.sh — run the working-tree macOS installer end-to-end on
# THIS host (a disposable runner) and assert the result. The macOS analog of
# installtest-run.sh's Linux path (#497).
#
# Tier 1: build waired + waired-agent (darwin), tar them, point install.sh's
#   darwin path at the local tarball via WAIRED_INSTALL_BASE_URL (file://), run
#   it, and assert the binaries land in /usr/local/bin, the system LaunchDaemon
#   plist is written + loaded, and the system state dir exists root-owned.
# Tier 2 (--tier 2): + hands-free enroll — gcloud (WIF) mints the SA id_token
#   (#339), exchanges it for a reusable auth key at the CP's dev issuer
#   (waired#976), then `sudo waired init --auth-key` enrols THROUGH the running
#   LaunchDaemon (#175). Asserts the identity lands under
#   /Library/Application Support/waired and the REAL system daemon reports it
#   on the mgmt API.
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
DAEMON_ENGINE=0
while [ $# -gt 0 ]; do
  case "$1" in
    --tier) shift; TIER="${1:?--tier needs N}" ;;
    --tier=*) TIER="${1#--tier=}" ;;
    --inference) INFER=1 ;;
    --integration) INTEG=1; INFER=1 ;;   # routing sentinel rides the inference engine
    --daemon-engine) DAEMON_ENGINE=1 ;;  # waired#835 §9/§11 daemon-path executor engine install
    -h|--help) sed -n '2,30p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 1 ;;
  esac
  shift
done

# --- enroll knobs (mirror lib/installtest-enroll.sh) ------------------------
IT_CONTROL_URL="${IT_CONTROL_URL:-https://app.dev.waired.net}"
IT_ENROLL_MODE="${IT_ENROLL_MODE:-authkey}"
IT_IMPERSONATE_SA="${IT_IMPERSONATE_SA:-}"

BINDIR="${WAIRED_DARWIN_BINDIR:-/usr/local/bin}"
STATE_DIR="/Library/Application Support/waired"
LABEL="com.waired.agent"
PLIST="/Library/LaunchDaemons/$LABEL.plist"
NEWSYSLOG_CONF="/etc/newsyslog.d/waired-agent.conf"
MGMT="http://127.0.0.1:9476/waired/v1/status"
# Mirror of lib/installtest-enroll.sh's IT_INSTALL_FAILURE_RE — see the comment
# there. scripts/ci/harness-failure-strings-guard.sh checks the three copies
# agree and that every branch still exists in the product source.
IT_INSTALL_FAILURE_RE='Engine install failed:|vLLM install failed:'
WORK="$(mktemp -d)"
DIST="$WORK/dist"
INITLOG="$WORK/init.log"   # waired init transcript (model pull + benchmark, --inference)
INSTALLLOG="$WORK/install.log"  # install.sh transcript (asserts it reached its last step)

# --- logging / counters -----------------------------------------------------
PASS=0; FAIL=0; SKIP=0
it_step() { printf '\033[1;32m[installtest]\033[0m ==> %s\n' "$*"; }
it_log()  { printf '\033[1;36m[installtest]\033[0m %s\n' "$*"; }
it_warn() { printf '\033[1;33m[installtest]\033[0m %s\n' "$*" >&2; }
ok()   { printf '\033[1;32m[installtest]  ok \033[0m %s\n' "$*"; PASS=$((PASS+1)); }
bad()  { printf '\033[1;31m[installtest] FAIL\033[0m %s\n' "$*" >&2; FAIL=$((FAIL+1)); }
# Counted, and printed in the summary: a skip nobody can see is how a leg
# quietly stops testing anything (#215).
skip() { printf '\033[1;33m[installtest] SKIP\033[0m %s\n' "$*"; SKIP=$((SKIP+1)); }
# A die counts as a failure and prints the tally before tearing down. Without
# that it printed one uncoloured line and exited straight past the summary, so
# a leg that died mid-run and a leg that failed an assert produced the same red
# job with none of the same evidence (#505). FAIL-prefixed so a die lands in
# the same grep as every other failure. The assert-count floor is deliberately
# NOT run here: its question is already answered by the die's own reason.
it_die() {
  printf '\033[1;31m[installtest] FAIL\033[0m %s\n' "$*" >&2
  FAIL=$((FAIL+1))
  echo >&2
  it_step "Tier $TIER summary (died before finishing): $PASS passed, $FAIL failed, $SKIP skipped"
  cleanup
  exit 1
}

cleanup() {
  # Best-effort teardown: deauth, then unregister the system LaunchDaemon.
  if [ -x "$BINDIR/waired" ]; then
    sudo "$BINDIR/waired" logout --yes --local --state-dir "$STATE_DIR" >/dev/null 2>&1 || true
  fi
  [ -x "$BINDIR/waired-agent" ] && sudo "$BINDIR/waired-agent" uninstall >/dev/null 2>&1 || true
  rm -rf "$WORK" 2>/dev/null || true
}
trap cleanup EXIT

# assert_launchd_healthy <context> — the three things that have to be true of
# the registered system LaunchDaemon. Factored out of the Tier-1 block because
# it is asserted TWICE: once on the fresh install, and again after the
# uninstall -> reinstall round trip below (#195/#176).
assert_launchd_healthy() {
  local ctx="$1" disabled running=0

  # The whole point of #520: the system domain loads on a headless runner with
  # no GUI (Aqua) session — no per-user gui/<uid> probe, no subprocess fallback.
  if sudo launchctl print "system/$LABEL" >/dev/null 2>&1; then
    ok "LaunchDaemon loaded in the system domain ($ctx)"
  else
    bad "LaunchDaemon not loaded in system/ ($ctx; headless system daemon must load without a GUI session)"
  fi

  # Enabled bit: `launchctl enable system/<label>` (run by `waired-agent
  # install`) is what makes the daemon return after a reboot — distinct from
  # "loaded this boot". A stale disabled override would pass the loaded check
  # above yet leave the host agent-less post-reboot. Absent from print-disabled
  # (or "=> false"/"=> enabled") means enabled; only "=> true"/"=> disabled" is
  # a real miss. Mirrors Linux's `systemctl is-enabled` assert.
  disabled="$(sudo launchctl print-disabled system 2>/dev/null | grep -F "\"$LABEL\"" || true)"
  if printf '%s' "$disabled" | grep -qiE '=>[[:space:]]*(true|disabled)'; then
    bad "LaunchDaemon disabled in launchd's DB ($ctx; won't return after reboot): $disabled"
  else
    ok "LaunchDaemon enabled ($ctx; survives reboot)"
  fi

  # Liveness: RunAtLoad + KeepAlive should have the job actually running, not
  # just loaded — and a crash-loop (e.g. an unreadable state dir) would show
  # here instead of a silent pass. Poll briefly; launchd spawns it
  # asynchronously after bootstrap.
  for _ in $(seq 1 15); do
    if sudo launchctl print "system/$LABEL" 2>/dev/null | grep -qE 'state[[:space:]]*=[[:space:]]*running'; then running=1; break; fi
    sleep 1
  done
  [ "$running" = 1 ] && ok "LaunchDaemon is running ($ctx; state = running)" \
    || bad "LaunchDaemon not running after bootstrap ($ctx; RunAtLoad/KeepAlive; state dir unreadable?)"
}

# assert_inference_macos — macOS analog of lib/installtest-enroll.sh's
# assert_inference: prove the Ollama-install -> bundled-model-pull -> benchmark
# tail of the journey ran (Tier-2 --inference). Paths are darwin-specific
# (Ollama.app, the system state dir); config reads use sudo since init wrote the
# state dir root-owned.
# assert_ollama_bundle_integrity_macos: the engine's app bundle must still be a
# valid, Gatekeeper-acceptable signed bundle after waired installed it (#329).
#
# These asserts INVERT the old "waired-managed marker present (Ollama.app)"
# check. That assert positively required the file whose presence was the bug:
# anything added to a signed bundle's root invalidates its v2 resource seal, so
# codesign reports "unsealed contents present in the bundle root", spctl
# rejects, and on Apple Silicon every exec of the engine is killed. Ownership is
# now recorded in the state dir instead, which is what the last check below
# looks for.
#
# Shared by the --inference and --daemon-engine legs: they install the engine by
# different routes (init's own install vs the setup executor's) and only the
# first had any bundle assert at all.
assert_ollama_bundle_integrity_macos() {
  local app=/Applications/Ollama.app
  local record="$STATE_DIR/runtimes/ollama/darwin-managed.json"
  local stray

  if [ ! -d "$app" ]; then
    # Homebrew / a bare CLI install has no bundle to check. Counted, so a leg
    # that silently stops testing this is visible in the summary (#215).
    skip "no $app on this host; bundle-integrity asserts not applicable"
    return
  fi

  # 1. Nothing of ours anywhere inside the bundle. Checked as a search rather
  #    than a single path so a future helper cannot reintroduce the class one
  #    directory deeper.
  stray="$(find "$app" -name '.waired-managed.json' 2>/dev/null | head -5)"
  if [ -z "$stray" ]; then
    ok "no waired file inside the Ollama.app bundle (signature seal intact)"
  else
    bad "waired wrote into the signed bundle — this breaks its signature (#329):"
    printf '%s\n' "$stray" | sed 's/^/    /' >&2
  fi

  # 2. The seal itself. This is the assert that would have caught #329 on day
  #    one: it fails on exactly the corruption the marker caused.
  if codesign --verify --deep --strict "$app" 2>/dev/null; then
    ok "codesign --verify --deep --strict passes on Ollama.app"
  else
    bad "Ollama.app fails codesign (macOS will refuse to run the engine):"
    codesign --verify --deep --strict "$app" 2>&1 | sed 's/^/    /' >&2 || true
  fi

  # 3. Gatekeeper's own verdict on executing it.
  if spctl --assess --type execute "$app" 2>/dev/null; then
    ok "spctl accepts Ollama.app for execution"
  else
    bad "spctl rejects Ollama.app (Gatekeeper will block the engine):"
    spctl --assess --type execute "$app" 2>&1 | sed 's/^/    /' >&2 || true
  fi

  # 4. Ownership is still recorded — outside the bundle, where it belongs.
  if sudo test -f "$record"; then
    ok "waired-managed record present outside the bundle ($record)"
  else
    bad "waired-managed record missing ($record) — waired will not recognise its own install"
  fi
}

assert_inference_macos() {
  local ollama_bin="" cand tps

  # PRIMARY: init's own transcript. See IT_INSTALL_FAILURE_RE's comment in
  # lib/installtest-enroll.sh — the installer's verdict outranks anything
  # still findable on disk (#215/#178). First, so the reason leads.
  #
  # Three arms: a missing transcript is its own failure, not a pass. See the
  # Linux twin in lib/installtest-enroll.sh for why (#505).
  if [ ! -f "$INITLOG" ]; then
    bad "no init transcript to check for install failures ($INITLOG)"
  elif grep -qE "$IT_INSTALL_FAILURE_RE" "$INITLOG"; then
    bad "init transcript reports an engine install failure ($INITLOG)"
    grep -nE "$IT_INSTALL_FAILURE_RE" "$INITLOG" | sed 's/^/    /' >&2 || true
  else
    ok "init transcript reports no engine install failure"
  fi

  for cand in \
      "$(command -v ollama 2>/dev/null || true)" \
      /Applications/Ollama.app/Contents/Resources/ollama \
      /usr/local/bin/ollama /opt/homebrew/bin/ollama; do
    if [ -n "$cand" ] && [ -x "$cand" ]; then ollama_bin="$cand"; break; fi
  done
  # SECONDARY, and worded as presence rather than success: a half-finished
  # install leaves an unpacked binary behind (#178).
  if [ -n "$ollama_bin" ]; then
    ok "ollama binary present ($ollama_bin)"
  else
    bad "ollama engine not installed (waired init --inference-enabled=true should have installed Ollama.app)"
  fi
  assert_ollama_bundle_integrity_macos

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
    if printf '%s' "$out" | grep -oE '"ready"[[:space:]]*:[[:space:]]*\[[[:space:]]*"[^"]' >/dev/null; then ready=1; break; fi
    state="$(printf '%s' "$out" | grep -oE '"subsystem_state"[[:space:]]*:[[:space:]]*"[a-z_]+"' | head -1 | grep -oE '"[a-z_]+"$' | tr -d '"')"
    # engine_failed is terminal too (waired-agent#29): the engine crashed and
    # automatic recovery either is mid-flight (which shows as "starting") or
    # has given up. Either way, polling for "ready" will not fix it — this list
    # had drifted from the Linux one in lib/installtest-enroll.sh, so a crashed
    # engine burned the whole ~5 min budget here.
    case "$state" in pull_failed|disabled|stopped|engine_failed) break ;; esac
    sleep 5
  done
  if [ "$ready" = 1 ]; then
    model="$(printf '%s' "$out" | grep -oE '"ready"[[:space:]]*:[[:space:]]*\[[^]]*\]' | grep -oE '"[^"]+"' | sed -n 2p | tr -d '"' || true)"
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

  # The end-of-init benchmark (offerBenchmark) must report a THROUGHPUT NUMBER.
  # Mirrors installtest-enroll.sh's assert (cross-OS parity): the bare "Local
  # inference works" line used to be accepted for a host too slow to measure a
  # rate, but a benchmark whose warm-up got an engine 500 printed exactly that
  # line too — so the assert passed while the engine was dead
  # (waired-agent#29). A current daemon 503s a failed run and the CLI then
  # prints no success line at all.
  #
  # `|| true`: a no-match / multi-match(SIGPIPE) grep must not trip a `set -e`
  # driver even though this script itself runs set -uo only.
  tps=""
  [ -f "$INITLOG" ] && tps="$(grep -ioE '[0-9]+(\.[0-9]+)? *(tok|tokens)/s' "$INITLOG" | head -1 || true)"
  if [ -n "$tps" ]; then
    ok "benchmark ran during init ($tps)"
  else
    bad "no benchmark THROUGHPUT figure in init transcript ($INITLOG)"
    grep -iE 'benchmark|inference|engine' "$INITLOG" 2>/dev/null | tail -20 | sed 's/^/    init| /' >&2 || true
    # Surface the daemon's own boot benchmark slog and the engine log — a
    # failed benchmark is usually the engine's fault.
    sudo grep -iE 'boot benchmark|benchmark' /Library/Logs/waired-agent.err.log 2>/dev/null | tail -15 | sed 's/^/    agent.err| /' >&2 || true
    sudo tail -n 60 "/Library/Application Support/waired/runtimes/ollama/logs/engine.log" 2>/dev/null | sed 's/^/    engine.log| /' >&2 || true
  fi
}

# assert_mgmt_socket_macos — darwin analog of lib/installtest-enroll.sh's
# assert_mgmt_socket (waired#838/#80). Mutating management requests must travel
# over the local IPC socket and must NOT be accepted on the loopback TCP port,
# while reads stay on TCP.
#
# Load-bearing because writeGuard fails OPEN: if the socket never binds, writes
# silently fall back to the old TCP behaviour and NOTHING else goes red. On
# Linux this assert is what caught the missing systemd RuntimeDirectory.
#
# Deliberately unelevated: the daemon is a root LaunchDaemon and the socket is
# 0666 inside a 0755 /var/run/waired, so the desktop user must be able to drive
# it. That cross-user reachability IS the #838 design premise (peer-uid
# authorization was rejected because it contradicts a system-wide install), and
# macOS is the only leg where the daemon and the CLI genuinely run as different
# users.
assert_mgmt_socket_macos() {
  local sock=/var/run/waired/mgmt.sock out code

  [ -S "$sock" ] && ok "management write socket present at $sock" \
    || bad "management write socket missing at $sock (MkdirAll / bind failure)"

  # The exit code alone proves nothing: runPhaseTransition treats an
  # unreachable daemon as the documented offline fallback (persist the desired
  # phase, return 0) and its isConnectionRefused() even matches "no such file
  # or directory" — i.e. a MISSING socket. Assert on stdout instead; "pause
  # ok." is printed only after a real daemon round-trip.
  out="$("$BINDIR/waired" pause 2>&1)"
  if printf '%s' "$out" | grep -q 'not running'; then
    bad "waired pause fell back to the offline desired-phase path (socket unreachable): $out"
  elif printf '%s' "$out" | grep -q 'pause ok\.'; then
    ok "unelevated 'waired pause' reached the root daemon over $sock (#838 premise)"
  else
    bad "waired pause produced no daemon acknowledgement: $out"
  fi

  out="$("$BINDIR/waired" resume 2>&1)"
  printf '%s' "$out" | grep -q 'resume ok\.' \
    && ok "unelevated 'waired resume' reached the daemon over the socket" \
    || bad "waired resume did not reach the daemon: $out"

  # Negative: the same mutating verb must be refused on the TCP port.
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
    -H 'Content-Type: application/json' \
    http://127.0.0.1:9476/waired/v1/pause 2>/dev/null || true)
  case "$code" in
    2*) bad "TCP :9476 accepted a mutating write (HTTP $code); writeGuard not enforcing (waired#838)" ;;
    "") bad "TCP :9476 mutating-write probe produced no status code" ;;
    *)  ok "TCP :9476 refuses mutating writes (HTTP $code)" ;;
  esac

  # Reads deliberately stay on TCP.
  curl -fsS --max-time 5 "$MGMT" >/dev/null 2>&1 \
    && ok "TCP :9476 still serves reads" \
    || bad "TCP :9476 no longer serves reads"

  # Leave the daemon active whichever leg above failed.
  "$BINDIR/waired" resume >/dev/null 2>&1 || true
}

# --- daemon-path setup-executor engine install (waired#835 §9/§11) ----------
# macOS analog of lib/installtest-daemon-engine.sh. The system LaunchDaemon is
# already running (Tier 1), so `waired init` takes the DAEMON path and its
# resident executor installs the engine. This leg deliberately does NOT use an
# auth key: a key authorizes the session inside the create call, leaving the
# executor lease with nothing to observe. Instead we let init open an ordinary
# login session and complete it out-of-band via the OIDC grant (the CP flips
# any waiting session, internal/controlplane/api/oidc_grant.go), so the
# in-flight window the executor works in actually exists. Then assert the
# engine landed via the executor, not install.sh (which ran --skip-ollama).
DAEMON_ENGINE_MODEL="granite4-350m"
DAEMON_ENGINE_FLAG="$WORK/daemon-engine.flag"

# _daemon_setup_watcher <token> — background half: scrape the login session id
# from init's transcript (a READ — POST /login/start is refused on TCP by the
# #838 writeGuard), complete it out-of-band at the CP, then watch the executor
# lease while init installs the engine. Records facts into the flag.
_daemon_setup_watcher() {
  local tok="$1" url="" sess="" st _ seen_exec="" seen_claim=""
  : > "$DAEMON_ENGINE_FLAG"
  for _ in $(seq 1 60); do
    url="$(grep -oE 'https?://[^[:space:]]+' "$INITLOG" 2>/dev/null | head -1)"
    if [ -n "$url" ]; then sess="${url##*/}"; sess="${sess%%[?#]*}"; fi
    [ -n "$sess" ] && break
    sleep 1
  done
  if [ -z "$sess" ]; then echo "no-session" >> "$DAEMON_ENGINE_FLAG"; return; fi
  echo "session=$sess" >> "$DAEMON_ENGINE_FLAG"
  if curl -fsS --max-time 20 -X POST -H 'Content-Type: application/json' \
      -d "{\"login_session_id\":\"$sess\",\"id_token\":\"$tok\"}" \
      "$IT_CONTROL_URL/v1/login/oidc-grant" >/dev/null 2>&1; then
    echo "completed=1" >> "$DAEMON_ENGINE_FLAG"
  else
    echo "complete-failed" >> "$DAEMON_ENGINE_FLAG"; return
  fi
  for _ in $(seq 1 150); do
    st="$(curl -fsS --max-time 5 http://127.0.0.1:9476/waired/v1/setup/state 2>/dev/null || true)"
    if [ -z "$seen_exec" ] && printf '%s' "$st" | grep -qE '"executor_attached"[[:space:]]*:[[:space:]]*true'; then
      echo "executor_attached=1" >> "$DAEMON_ENGINE_FLAG"; seen_exec=1
    fi
    if [ -z "$seen_claim" ] && printf '%s' "$st" | grep -qE '"install_claimed"[[:space:]]*:[[:space:]]*"ollama"'; then
      echo "install_claimed=ollama" >> "$DAEMON_ENGINE_FLAG"; seen_claim=1
    fi
    sleep 2
  done
}

# daemon_path_enroll_macos <token> <device> — foreground daemon-path init while
# the watcher completes login and observes the lease. init runs as root (sudo);
# the already-running LaunchDaemon makes it take the daemon path.
daemon_path_enroll_macos() {
  local tok="$1" device="$2" watcher_pid rc
  : > "$INITLOG"   # fresh: the watcher must not scrape a stale login URL
  _daemon_setup_watcher "$tok" &
  watcher_pid=$!
  it_step "daemon-path 'waired init' (fg, no credential flag → daemon path)"
  # inference on + tiny model so an engine-less host installs one; --non-interactive
  # so the resident executor runs ensureDaemonPathEngine; stdin from /dev/null.
  # pipefail makes the `if` see init's exit; PIPESTATUS[0] is init's, not tee's.
  if sudo env WAIRED_NO_EMOJI=1 "$BINDIR/waired" init --control "$IT_CONTROL_URL" \
        --device-name "$device" --inference-enabled=true \
        --inference-bundled-model-id="$DAEMON_ENGINE_MODEL" \
        --non-interactive --skip-integration --state-dir "$STATE_DIR" \
        </dev/null 2>&1 | tee "$INITLOG"; then
    rc=0
  else
    rc="${PIPESTATUS[0]}"
  fi
  kill "$watcher_pid" 2>/dev/null || true
  wait "$watcher_pid" 2>/dev/null || true
  [ "$rc" -eq 0 ] || it_warn "daemon-path init exited $rc — asserts will surface what landed"
}

# assert_daemon_engine_macos — the executor engine-install asserts (analog of
# lib/installtest-daemon-engine.sh's assert_daemon_engine). Regression bar: an
# engine-less daemon-path first-run ends up WITH an engine (pre-N3 it stayed
# engine-less and engine_install was red forever).
assert_daemon_engine_macos() {
  local out state setup_state desired_engine installed claim ollama_bin="" cand
  grep -q "signing in via the daemon" "$INITLOG" 2>/dev/null \
    && ok "init took the daemon path (setup-executor-capable first-run)" \
    || bad "init did NOT take the daemon path (executor engine install not exercised)"
  grep -q '^completed=1' "$DAEMON_ENGINE_FLAG" 2>/dev/null \
    && ok "daemon login completed out-of-band via the OIDC grant" \
    || bad "out-of-band OIDC completion did not report success"
  grep -q '^executor_attached=1' "$DAEMON_ENGINE_FLAG" 2>/dev/null \
    && ok "setup executor lease was live during setup (executor_attached)" \
    || bad "never observed executor_attached — executor engine-install path not reached"
  grep -q '^install_claimed=ollama' "$DAEMON_ENGINE_FLAG" 2>/dev/null \
    && ok "executor claimed the ollama install (install_claimed=ollama)" \
    || it_warn "did not catch install_claimed=ollama in the 2 s poll — non-fatal"
  for cand in \
      "$(command -v ollama 2>/dev/null || true)" \
      /Applications/Ollama.app/Contents/Resources/ollama \
      /usr/local/bin/ollama /opt/homebrew/bin/ollama; do
    if [ -n "$cand" ] && [ -x "$cand" ]; then ollama_bin="$cand"; break; fi
  done
  [ -n "$ollama_bin" ] \
    && ok "ollama engine installed by the daemon-path executor ($ollama_bin)" \
    || bad "no engine after a daemon-path first-run (executor install did not land — pre-N3 behaviour)"
  # #329: this leg installs through the setup executor, the path the browser
  # wizard uses, and had no bundle assert at all until now.
  assert_ollama_bundle_integrity_macos
  out="$(curl -fsS --max-time 5 http://127.0.0.1:9476/waired/v1/inference/status 2>/dev/null || true)"
  state="$(printf '%s' "$out" | grep -oE '"subsystem_state"[[:space:]]*:[[:space:]]*"[a-z_]+"' | head -1 | grep -oE '"[a-z_]+"$' | tr -d '"')"
  case "$state" in
    ""|no_engine) bad "inference subsystem still reports '${state:-unreachable}' (engine not installed)" ;;
    *) ok "inference subsystem left no_engine (state=$state)" ;;
  esac
  setup_state="$(curl -fsS --max-time 5 http://127.0.0.1:9476/waired/v1/setup/state 2>/dev/null || true)"
  # engine_installed — what the SETUP WIZARD reads (#195/#179). The checks
  # above look at the host and at the inference subsystem; neither is the value
  # the daemon reports to the UI, and the two have disagreed (#179: an engine
  # on disk but not on PATH, so the wizard kept offering to install it).
  # desired_engine is read first because SetupState computes engine_installed
  # only when one is set — see lib/installtest-daemon-engine.sh's item 7.
  desired_engine="$(printf '%s' "$setup_state" \
    | sed -n 's/.*"desired_engine"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
  installed="$(printf '%s' "$setup_state" \
    | grep -oE '"engine_installed"[[:space:]]*:[[:space:]]*(true|false)' | head -1 \
    | grep -oE '(true|false)$')"
  if [ -z "$setup_state" ]; then
    bad "could not read /setup/state (daemon unreachable) — engine_installed unverifiable"
  elif [ -z "$desired_engine" ]; then
    it_warn "no desired_engine at the end of the leg, so engine_installed is false by definition — not a #179 signal: $setup_state"
  elif [ "$installed" = true ]; then
    ok "daemon reports engine_installed=true for desired_engine=$desired_engine (setup wizard sees the engine)"
  else
    bad "engine is on the host but the daemon reports engine_installed=false for desired_engine=$desired_engine (#179 class)"
  fi
  claim="$(printf '%s' "$setup_state" \
    | sed -n 's/.*"install_claimed"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
  [ -z "$claim" ] \
    && ok "no stuck executor install claim after init (install_claimed cleared)" \
    || bad "executor install claim still set after init (install_claimed=$claim; stuck)"
}

# Passwordless sudo is a hard requirement now that the agent is a system
# daemon (install.sh sudo's the register/init steps; we sudo the asserts).
sudo -n true 2>/dev/null || it_die "passwordless sudo required (system LaunchDaemon install needs root)"

# --daemon-engine (waired#835 §9/§11) is its own mode: install.sh keeps
# --skip-ollama (engine ABSENT), enrol goes daemon-path via out-of-band OIDC.
if [ "$DAEMON_ENGINE" = 1 ]; then
  { [ "$INFER" = 1 ] || [ "$INTEG" = 1 ]; } && it_die "--daemon-engine is its own mode; not with --inference/--integration"
  [ "$TIER" -ge 2 ] || it_die "--daemon-engine needs --tier 2 (it enrols to reach the executor)"
fi

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
# Guard the pack + checksum explicitly: this script runs `set -uo pipefail`
# (NOT -e — the inference poll below relies on no-match greps in command
# substitutions that -e would abort). Without a guard a failed pack would let
# the run barrel into install.sh against a missing tarball and report a
# confusing "install.sh exited N" instead of dying at the real cause.
tar czf "$DIST/$tarball" -C "$WORK/stage" waired waired-agent VERSION \
  || it_die "packing $tarball failed"
( cd "$DIST" && shasum -a 256 "$tarball" > "$tarball.sha256" ) \
  || it_die "checksumming $tarball failed"

# --- Tier 1: run install.sh's darwin path + assert --------------------------
# Ollama: install.sh no longer pre-installs the engine — `waired init` owns
# the decision + install, so the Tier-2 `--inference-enabled=true` init below
# downloads Ollama.app itself (#514 journey preserved, ordering fixed). The
# default path opts out explicitly (--skip-ollama -> WAIRED_NO_OLLAMA for
# init) — Tier 1/2 only need the installer + enroll.
inst_args=(--no-init)
inst_env=(WAIRED_INSTALL_BASE_URL="file://$DIST" WAIRED_NO_TRAY=1 WAIRED_NO_EMOJI=1)
if [ "$INFER" = 1 ]; then
  it_step "running install.sh (darwin, --no-init; Ollama enabled for inference)"
else
  inst_args+=(--skip-ollama); inst_env+=(WAIRED_NO_OLLAMA=1)
  it_step "running install.sh (darwin, --no-init --skip-ollama)"
fi
install_rc=0
env "${inst_env[@]}" bash "$ROOT/packaging/install/install.sh" "${inst_args[@]}" 2>&1 | tee "$INSTALLLOG"
install_rc=${PIPESTATUS[0]}

it_step "Tier 1 asserts"
[ "$install_rc" -eq 0 ]       && ok "install.sh exited 0"                        || bad "install.sh exited $install_rc"
# The next-steps banner is the LAST thing a complete darwin run prints, so its
# absence is the signature of an installer that stopped part-way — the #193
# class of failure, where an unguarded `set -eu` abort in darwin_register_agent
# silently skipped the rest of the run. This assert replaces the newsyslog
# drop-in's role as the "did the run reach its end" sentinel (#331 retired the
# drop-in), and it is a strictly later marker than that file ever was.
grep -qF 'Waired is installed (macOS' "$INSTALLLOG" \
  && ok "install.sh reached its final step (next-steps banner printed)" \
  || bad "install.sh printed no next-steps banner — did it abort mid-run? (see $INSTALLLOG)"
[ -x "$BINDIR/waired" ]       && ok "waired installed ($BINDIR/waired)"          || bad "waired missing in $BINDIR"
[ -x "$BINDIR/waired-agent" ] && ok "waired-agent installed"                     || bad "waired-agent missing in $BINDIR"

# Gatekeeper: curl-downloaded binaries carry no com.apple.quarantine xattr, so
# the unsigned ad-hoc binary execs — including as a root LaunchDaemon. Assert
# both; a quarantined or non-execing binary fails opaquely at launchd spawn
# (ported from scripts/dev/macos-installtest-run.sh; the CI driver lacked it).
if xattr -p com.apple.quarantine "$BINDIR/waired" >/dev/null 2>&1; then
  bad "waired has com.apple.quarantine (would be Gatekeeper-blocked)"
else
  ok "waired has no Gatekeeper quarantine xattr"
fi
"$BINDIR/waired" version >/dev/null 2>&1 && ok "waired binary execs (version)" \
  || bad "waired binary does not exec (ad-hoc signature / arch mismatch?)"

sudo test -f "$PLIST"         && ok "system LaunchDaemon plist written ($LABEL)" || bad "LaunchDaemon plist missing ($PLIST)"
sudo test -d "$STATE_DIR"     && ok "system state dir present"                   || bad "state dir missing ($STATE_DIR)"

# #331 inverted this assert. It used to require the newsyslog drop-in to be
# PRESENT; the drop-in is now retired, because launchd owns the descriptor the
# daemon writes through and newsyslog's rename never reached it — the daemon
# kept writing into the renamed inode and lost every line until a restart. The
# agent rotates its own logs instead (internal/platform/logrotate), so the
# converged state is the drop-in being gone, on fresh installs and on updated
# hosts alike.
sudo test -e "$NEWSYSLOG_CONF" \
  && bad "legacy newsyslog drop-in still present ($NEWSYSLOG_CONF) — it races the agent's own rotation (#331)" \
  || ok "legacy newsyslog rotation retired ($NEWSYSLOG_CONF absent)"
owner="$(sudo stat -f '%Su' "$STATE_DIR" 2>/dev/null || true)"
[ "$owner" = "root" ] && ok "state dir owned by root ($owner)" || bad "state dir owner = $owner (want root)"

# State-dir must not be world-accessible (parity with Linux's SecureDir 0700 /
# Windows' restrictive DACL): agent.json + identity.json live here. Validates
# the darwin Install hardening (secrets.SecureDir); a regression to 0755 would
# expose them to every local user.
mode="$(sudo stat -f '%Lp' "$STATE_DIR" 2>/dev/null || true)"
if [ -n "$mode" ] && [ "$(( 8#$mode & 0007 ))" -eq 0 ]; then
  ok "state dir not world-accessible (mode $mode)"
else
  bad "state dir world-accessible (mode ${mode:-?}; want no world rwx)"
fi

assert_launchd_healthy "fresh install"

# --- uninstall -> reinstall round trip (#195; the #176 regression bar) -------
# A fresh VM per run means a poisoned host NEVER arrives from the outside, so
# the enabled-bit assert above can only ever see a clean launchd DB. The bug
# #176 actually described — `waired-agent uninstall` leaving a persistent
# `launchctl disable` override, so the NEXT install produces a host that is
# loaded now and agent-less after a reboot — is therefore invisible to a
# single-install suite. It only shows up if the same host uninstalls and
# reinstalls, which is what a real user does on every repair or channel
# switch.
#
# Deliberately NOT a defensive `launchctl enable` at suite start: on both the
# hosted and the self-hosted macOS pools the VM is disposable, so a defensive
# enable could not fix a real problem and would instead mask the one this leg
# exists to catch.
#
# Runs before Tier 2 for two reasons: the enrol that follows then happens on a
# REINSTALLED host ("a repaired install can still enrol" comes free), and
# `waired-agent uninstall` calls deregisterOnUninstall() — harmless here
# because nothing has enrolled yet, but it would drop the device out of the
# control plane mid-suite if this ran after Tier 2.
#
# The reinstall mirrors install.sh's darwin_register_agent exactly (its
# no-LOG_LEVEL branch; the harness sets none), so this re-runs the real
# registration, not a lookalike.
it_step "uninstall -> reinstall round trip (#176)"
uninstall_rc=0
sudo "$BINDIR/waired-agent" uninstall >/dev/null 2>&1 || uninstall_rc=$?
[ "$uninstall_rc" -eq 0 ] && ok "waired-agent uninstall exited 0" \
  || bad "waired-agent uninstall exited $uninstall_rc"
sudo test -f "$PLIST" && bad "LaunchDaemon plist survived uninstall ($PLIST)" \
  || ok "LaunchDaemon plist removed by uninstall"
sudo launchctl print "system/$LABEL" >/dev/null 2>&1 \
  && bad "LaunchDaemon still loaded after uninstall (bootout did not run)" \
  || ok "LaunchDaemon booted out of the system domain"

# THE REGRESSION BAR: uninstall must leave launchd's disabled DB clean. An
# override here is what made the post-#176 reinstall come back dead on reboot.
disabled_after_uninstall="$(sudo launchctl print-disabled system 2>/dev/null | grep -F "\"$LABEL\"" || true)"
if printf '%s' "$disabled_after_uninstall" | grep -qiE '=>[[:space:]]*(true|disabled)'; then
  bad "uninstall left a launchd disable override (#176): $disabled_after_uninstall"
else
  ok "uninstall left no launchd disable override (#176)"
fi

reinstall_rc=0
sudo "$BINDIR/waired-agent" install --state-dir "$STATE_DIR" >/dev/null 2>&1 || reinstall_rc=$?
[ "$reinstall_rc" -eq 0 ] && ok "waired-agent install (reinstall) exited 0" \
  || bad "waired-agent install (reinstall) exited $reinstall_rc"
assert_launchd_healthy "reinstall"

# --- Tier 2: hands-free enroll + assert -------------------------------------
if [ "$TIER" -ge 2 ]; then
  [ "$IT_ENROLL_MODE" = authkey ] || it_die "installtest-macos.sh supports IT_ENROLL_MODE=authkey only (got '$IT_ENROLL_MODE')"
  [ -n "$IT_IMPERSONATE_SA" ]  || it_die "IT_ENROLL_MODE=authkey needs IT_IMPERSONATE_SA (the #339 test SA)"
  command -v gcloud >/dev/null 2>&1 || it_die "authkey enroll mints the SA id_token on the host; gcloud not found"

  it_step "minting an auth key (host-minted SA token -> CP dev issuer)"
  aud="$(curl -fsS --max-time 15 "$IT_CONTROL_URL/v1/login/oidc-grant/audience" 2>/dev/null \
    | sed -n 's/.*"audience":"\([^"]*\)".*/\1/p')"
  [ -n "$aud" ] || it_die "could not resolve the OIDC audience from $IT_CONTROL_URL"
  it_log "minting SA id_token (sa=$IT_IMPERSONATE_SA)"
  tok="$(gcloud auth print-identity-token --impersonate-service-account="$IT_IMPERSONATE_SA" \
    --audiences="$aud" --include-email 2>/dev/null)"
  [ -n "$tok" ] || it_die "failed to mint an SA id_token (CI principal in oidc_grant_token_creators?)"

  # Exchange the token for a reusable auth key. The daemon-engine leg below
  # still completes its login out-of-band with the raw token, so the token
  # itself is kept. --data @- keeps it off the process's argv.
  authkey="$(printf '{"id_token":"%s","reusable":true,"description":"installtest macos"}' "$tok" \
    | curl -fsS --max-time 30 -X POST "$IT_CONTROL_URL/test/auth-key" \
        -H 'Content-Type: application/json' --data @- 2>/dev/null \
    | sed -n 's/.*"auth_key":"\([^"]*\)".*/\1/p')"
  [ -n "$authkey" ] || it_die "could not mint an auth key at $IT_CONTROL_URL/test/auth-key (CP new enough — waired#976?)"

  device="mac-ci-${GITHUB_RUN_ID:-$(date +%Y%m%d%H%M%S)}"
  if [ "$DAEMON_ENGINE" = 1 ]; then
    # Daemon-path enrol with an OUT-OF-BAND completion: the point of this
    # leg is to watch the resident executor install the engine
    # (waired#835 §9/§11) while init is mid-flight, which needs a login the
    # watcher completes at the CP. An auth key would authorize the session
    # in the create call and leave nothing to observe, so this leg keeps
    # the interactive-shaped login and the raw SA token.
    daemon_path_enroll_macos "$tok" "$device"
  else
  inf_flag="--inference-enabled=$([ "$INFER" = 1 ] && echo true || echo false)"
  # Routing sentinel pins the withheld 350M so the deploy pulls ~0.7 GB (fits the
  # 4 GB macOS runner; dodges the #573 7B OOM). Zero args when not --integration.
  pin_flag=()
  [ "$INTEG" = 1 ] && pin_flag=(--inference-bundled-model-id=granite4-350m)
  # init runs as root: it writes identity to the system state dir and, since the
  # LaunchDaemon is already registered (Tier 1), (re)starts it in the system/
  # domain so the real daemon re-reads the freshly-enrolled state. With
  # --inference the deploy phase foreground-pulls the bundled model and runs the
  # end-of-init benchmark (#519); tee the transcript for assert_inference_macos.
  # ${pin_flag[@]+...} is the unset-safe expansion: an empty array must expand
  # to zero args even under `set -u` on macOS's system bash 3.2.
  #
  # Three outcomes, not two (#310): 0 signed in, 3 signed in but this host has
  # no local AI, anything else failed. Only lib/installtest-enroll.sh learned
  # that; this leg kept reading 3 as an outright failure, which would fail a
  # host that enrolled perfectly on every non-inference tier (#505). The exit
  # code has to come from PIPESTATUS[0] rather than the `if` — pipefail (set
  # -o, line ~22) collapses the pipeline to a single non-zero, which cannot
  # tell 3 from anything else. Same idiom as daemon_path_enroll_macos.
  if sudo env WAIRED_NO_EMOJI=1 "$BINDIR/waired" init --control "$IT_CONTROL_URL" \
        --auth-key "$authkey" \
        --device-name "$device" --non-interactive "$inf_flag" ${pin_flag[@]+"${pin_flag[@]}"} \
        --skip-integration --state-dir "$STATE_DIR" 2>&1 | tee "$INITLOG"; then
    init_rc=0
  else
    init_rc="${PIPESTATUS[0]}"
  fi
  case "$init_rc" in
    0) ;;
    3)
      # A tier that asked for local inference and did not get it IS a
      # failure: that is the thing that tier exists to verify.
      if [ "$INFER" = 1 ]; then
        bad "waired init (authkey) enrolled but local AI is not running, and this tier asked for it — see $INITLOG"
      else
        it_log "waired init (authkey) enrolled; local AI is not running here (expected: this tier did not ask for it)"
      fi
      ;;
    *) bad "waired init (authkey) failed with exit $init_rc — see $INITLOG" ;;
  esac
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

  # Cheap and fast, so it runs before the minutes-long inference asserts.
  it_step "management write socket asserts (waired#838)"
  assert_mgmt_socket_macos

  if [ "$DAEMON_ENGINE" = 1 ]; then
    it_step "daemon-path executor engine-install asserts (waired#835 §9/§11)"
    assert_daemon_engine_macos
  elif [ "$INFER" = 1 ]; then
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
           go test -tags integration -count=1 -v -timeout 15m ./internal/e2e/integration/... ); then
        ok "coding-agent routing sentinel: every leg served locally (no fail-open)"
      else
        bad "coding-agent routing sentinel failed (see go test output above)"
      fi
    else
      bad "go toolchain not on PATH (needed to run the routing harness)"
    fi
  fi
fi

# --- #331: the daemon rotates its own launchd log, losing nothing -----------
# Runs LAST because it deliberately grows and then rotates the live err.log;
# every assert that reads that file (the inference benchmark grep, the failure
# tails) has already had its look by here.
#
# The bug this pins: newsyslog renamed the file, launchd's descriptor stayed on
# the renamed inode, and every line the daemon wrote afterwards went to a file
# that was then gzipped and deleted. A host wedged for 12 hours logged nothing
# at all. The daemon now rotates from inside the process holding the
# descriptor, so the assert is not "did an archive appear" alone — it is "and
# is the daemon's stderr now on the NEW file", which is the part that was
# broken.
it_step "#331 launchd log rotation"
assert_log_rotation() {
  local err="/Library/Logs/waired-agent.err.log" pid fd_ino live_ino

  pid="$(sudo launchctl print "system/$LABEL" 2>/dev/null \
    | sed -n 's/^[[:space:]]*pid = \([0-9][0-9]*\).*/\1/p' | head -1)"
  if [ -z "$pid" ]; then
    bad "could not read the daemon pid from launchctl — cannot check log rotation (#331)"
    return
  fi

  # Known archive state, then push the live file past the 1 MB cap the agent
  # rotates at (internal/platform/logrotate.DefaultPolicy).
  sudo rm -f "$err".* 2>/dev/null || true
  yes 'waired installtest rotation filler' | head -c 1200000 | sudo tee -a "$err" >/dev/null

  # The rotation ticker is 60s; give it a margin.
  for _ in $(seq 1 90); do
    sudo test -f "$err.0.gz" && break
    sleep 1
  done
  if ! sudo test -f "$err.0.gz"; then
    bad "the daemon did not rotate $err within 90s of it passing 1 MB (#331)"
    return
  fi
  ok "the daemon rotated its own launchd log ($err.0.gz)"

  # The fix itself. Comparing inodes rather than lsof's path string because a
  # renamed-but-open file still reports a plausible-looking name.
  fd_ino="$(sudo lsof -p "$pid" -a -d 2 -F i 2>/dev/null | sed -n 's/^i//p' | head -1)"
  live_ino="$(sudo stat -f '%i' "$err" 2>/dev/null || true)"
  if [ -n "$fd_ino" ] && [ "$fd_ino" = "$live_ino" ]; then
    ok "the daemon's stderr descriptor followed the rotation (inode $fd_ino)"
  else
    bad "the daemon's stderr is still on the pre-rotation inode (fd=${fd_ino:-?}, live=${live_ino:-?}) — every line after a rotation is lost (#331)"
  fi
}
assert_log_rotation

echo
it_step "Tier $TIER summary: $PASS passed, $FAIL failed, $SKIP skipped"

# Assert-count floor (#215) — see the same block in installtest-run.sh for
# the rationale. Floors MEASURED from a green run of the leanest config:
# tier 1 = the binaries / Gatekeeper / plist / state-dir / launchd asserts
# plus the uninstall->reinstall round trip, tier 2 = those plus enrol and
# the mgmt-socket set. Options only ever add asserts. Raised by 3 in #331:
# the next-steps banner assert and the two log-rotation asserts.
case "$TIER" in
  1) floor=24 ;;
  *) floor=31 ;;
esac
executed=$((PASS + FAIL))
if [ "$executed" -lt "$floor" ]; then
  printf '\033[1;31m[installtest] FAIL\033[0m only %d asserts ran at tier %s; at least %d must (a block stopped executing — see the assert-count floor in %s)\n' \
    "$executed" "$TIER" "$floor" "$(basename "$0")" >&2
  exit 1
fi

[ "$FAIL" -eq 0 ] || exit 1
