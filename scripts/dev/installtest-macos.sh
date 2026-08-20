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
#   CPU — `waired init --inference-enabled=true` installs the bundled engine
#   (since #138 install.sh puts no engine on the host; --skip-ollama is how you
#   tell init not to), pulls the bundled model in its deploy phase and runs the
#   end-of-init benchmark. Asserts the engine is waired's own under the state dir
#   AND is what serves, at the pin (#494); the model READY in the waired store via
#   the mgmt API on :9476, never a bare `ollama list` against :11434 (#567);
#   inference enabled in the persisted config; and a benchmark figure in the init
#   transcript (the macOS analog of lib/installtest-enroll.sh's assert_inference).
#
# --engine-only (pairs with --tier 2; waired-agent#590): its own mode. Install
#   the AI software and answer the model picker with "don't download a model
#   now", then assert that state is a FINISHED install — exit 0, an engine on
#   disk, and a standing choice the daemon keeps across a restart. The darwin
#   twin of installtest-run.sh's --engine-only.
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
ENGINE_ONLY=0
while [ $# -gt 0 ]; do
  case "$1" in
    --tier) shift; TIER="${1:?--tier needs N}" ;;
    --tier=*) TIER="${1#--tier=}" ;;
    --inference) INFER=1 ;;
    --integration) INTEG=1; INFER=1 ;;   # routing sentinel rides the inference engine
    --daemon-engine) DAEMON_ENGINE=1 ;;  # waired#835 §9/§11 daemon-path executor engine install
    --engine-only) ENGINE_ONLY=1 ;;      # waired-agent#590 engine installed, no model chosen
    -h|--help) sed -n '2,36p' "$0"; exit 0 ;;
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
# Mirrors of lib/installtest-enroll.sh's engine-opt-out pair (waired-agent#551)
# — see the comment there. Same guard checks these three copies agree.
IT_ENGINE_OPTOUT_RE='Engine install skipped (WAIRED_NO_OLLAMA)'
IT_INSTALL_FAILURE_BOX_RE='The AI engine could not be installed on this device'
# Mirror of lib/installtest-enroll.sh's IT_BENCH_NOT_READY_RE — see the comment
# there (#382). Same guard checks these three copies agree.
IT_BENCH_NOT_READY_RE='Model not ready in time|Model download failed|Model still downloading|No model was chosen for this computer'
# Mirrors of lib/installtest-enroll.sh's step-4 default / models-pull pair
# (waired-agent#590) — see the comments there. Same guard checks these three
# copies agree and that the product still prints them.
IT_UNFIT_SKIP_RE='Non-interactive: skipping local AI'
IT_PULL_DECLINE_RE='Not downloading. Re-run with --yes --force to download it anyway.'
IT_PULL_QUEUED_RE='queued pull:'
IT_PULL_REACHED_RE='queued pull:|cannot download'
# Mirror of lib/installtest-enroll.sh's IT_NO_MODEL_RE (waired-agent#586/#590)
# — see the comment there, including why only the ASCII head of the line is
# matched. Same guard checks these three copies agree.
IT_NO_MODEL_RE='No model selected'
# Mirror of lib/installtest-enroll.sh's IT_STATUS_FIELDS_RE (waired-agent#573)
# — see the comment there. Same guard checks these three copies agree and that
# the product still publishes the fields.
# shellcheck disable=SC2034  # read by the guard, not by this script.
IT_STATUS_FIELDS_RE='no_model_selected|host_speed|probe_model_id|turn_floor_seconds'
# Mirror of lib/installtest-enroll.sh's IT_DAEMON_EVIDENCE_RE (waired-agent#579)
# — see the comment there for why the host-speed group belongs in a dump that
# was previously pull-side only, why `api/pull` is appended at the use site
# instead of living in the alternation, and what the last two branches are for
# (waired-agent#642).
IT_DAEMON_EVIDENCE_RE='boot pre-pull|bundled model|host speed|host cutoff|below the recommended spec|measuring whether this host|engine log truncated at cap|no engine logs found'
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

# evidence_dump <bundle> — the macOS twin of lib/installtest-enroll.sh's
# _it_evidence_dump. See that function for why the pull group is separate and
# untruncated, why both groups are counted, and why free space is here
# (waired-agent#642).
evidence_dump() {
  local bundle="$1" n p
  n="$(grep -icE "$IT_DAEMON_EVIDENCE_RE" "$bundle" 2>/dev/null)" || n=0
  echo "daemon evidence: $n line(s) matched, showing the last 40"
  grep -iE "$IT_DAEMON_EVIDENCE_RE" "$bundle" 2>/dev/null | tail -40 |
    grep . || echo "(no pre-pull or host-speed lines in the daemon log)"
  p="$(grep -icE 'api/pull' "$bundle" 2>/dev/null)" || p=0
  echo "engine pull requests: $p line(s) matched, showing all"
  grep -iE 'api/pull' "$bundle" 2>/dev/null |
    grep . || echo "(no api/pull lines in the daemon log)"
  local d
  d="$(df -Ph "$STATE_DIR" 2>/dev/null | tail -1)" || d=
  echo "state-dir free space: ${d:-(state dir unreadable)}"
  # What the engine says it has RESIDENT, verbatim. size_vram is the field
  # that separates a model actually on the GPU from one the engine loaded
  # into system memory while still reporting a GPU backend — the daemon's own
  # `backend` label cannot answer that, because on arm64 it is asserted
  # unconditionally (waired-agent#35). Raw body rather than a parsed field:
  # whether darwin populates size_vram at all is itself the open question.
  #
  # :9475 is waired's bundled engine, never the upstream default :11434.
  # Unloaded returns {"models":[]}, so this is only meaningful after
  # something has run — which every arm printing this dump has done.
  echo "engine /api/ps: $(curl -fsS --max-time 10 http://127.0.0.1:9475/api/ps 2>/dev/null || echo '(unreachable)')"
}

# hostspeed_evidence — the macOS twin of lib/installtest-enroll.sh's
# it_hostspeed_evidence (waired-agent#579), for the arms that end on init's
# exit code rather than on an assert.
#
# It is a separate function from the dump inside assert_inference_macos for the
# reason the linux comment gives: an init that exits 3 never reaches any
# assert, so on a finished run the state of the #496 measurement — the thing
# #579 turns on — was unrecoverable. This leg had NO daemon output at all on
# that path.
#
# Prints nothing this script can assert on; it exists so a red says why. Both
# streams go to stderr so the dump sits with the `bad` line it explains.
hostspeed_evidence() {
  sudo "$BINDIR/waired" logs --since 30m --state-dir "$STATE_DIR" -o /tmp/it-hs.txt >/dev/null 2>&1 || true
  evidence_dump /tmp/it-hs.txt 2>&1 |
    sed 's/^/    agent| /' >&2 || true
  # `|| echo` inside the pipe, not after it: a failed `curl -fsS` prints
  # nothing and the pipeline's status is sed's, so a trailing `|| true` would
  # never fire and an unreachable daemon would leave no line at all.
  { curl -fsS --max-time 10 http://127.0.0.1:9476/waired/v1/inference/status || echo "(status unreachable)"; } 2>&1 |
    sed 's/^/    status| /' >&2 || true
}

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
# tail of the journey ran (Tier-2 --inference). Config reads use sudo since init
# wrote the state dir root-owned.
#
# assert_bundled_ollama_macos: the engine is the one waired installed, at the
# one path the daemon will serve from.
#
# This replaces the bundle-integrity block that guarded the /Applications
# layout — codesign/spctl on Ollama.app, a search for stray waired files inside
# it, and the state-dir ownership record that stood in for the seal-breaking
# in-bundle marker (#329). #492 moved the engine under the state dir, so there
# is no bundle, no seal, and nothing to record: the path IS the ownership
# claim, and asserting it is what catches an install that quietly resolved to
# somebody else's Ollama (#139).
#
# Shared by the --inference and --daemon-engine legs: they install the engine by
# different routes (init's own install vs the setup executor's).
assert_bundled_ollama_macos() {
  local bin="$STATE_DIR/runtimes/ollama/bin/ollama"

  # 1. The engine waired installed is where waired installs it. Quoted
  #    everywhere because this path has a space in it — /Library/Application
  #    Support/waired — which the Linux twin never had to think about.
  if sudo test -x "$bin"; then
    ok "bundled ollama installed under the state dir ($bin)"
  else
    bad "no bundled ollama at $bin (init --inference-enabled=true should have installed one)"
    sudo ls -la "$STATE_DIR/runtimes/ollama" 2>&1 | sed 's/^/    /' >&2 || true
    return
  fi

  # 2. Its runners came out of the archive beside it. The macOS release is a
  #    FLAT tarball — ollama, llama-server, the dylibs and mlx_metal_v*/ are
  #    all siblings — so an extract that put the binary in the right place but
  #    scattered the rest would still fail to serve, and would do it at first
  #    inference rather than here.
  if sudo test -x "$STATE_DIR/runtimes/ollama/bin/llama-server"; then
    ok "the engine's runner is beside it (flat darwin archive extracted intact)"
  else
    bad "llama-server missing beside the engine — the darwin archive did not extract into bin/"
    sudo ls "$STATE_DIR/runtimes/ollama/bin" 2>&1 | head -20 | sed 's/^/    /' >&2 || true
  fi

  # 3. No ENGINE was installed into /Applications. Not a style point: that
  #    location is shared with the user's own Ollama, and putting ours there is
  #    what made #329 and #139 possible. Waired.app is a different matter and
  #    is asserted PRESENT above (waired-agent#833) -- nothing else on the
  #    machine puts a bundle at that path, so it carries no ownership
  #    ambiguity.
  if [ ! -d /Applications/Ollama.app ]; then
    ok "no Ollama.app in /Applications (waired installs nothing there since #492)"
  else
    # A pre-existing app is the user's and must survive untouched; only a
    # freshly created one would mean waired put it there. The runner is a
    # disposable VM, so anything here was created by this run.
    bad "/Applications/Ollama.app exists on a fresh runner — something still installs there"
  fi
}

# assert_serving_ollama_macos <context> — the engine ANSWERING REQUESTS is
# waired's own, at the pinned version (#494). Twin of
# lib/installtest-enroll.sh's assert_serving_ollama; see that function for the
# reasoning behind all three asserts. Mirror any change there and in
# installtest-windows.ps1 (Assert-ServingEngine).
#
# The macOS-only parts are the two host commands. There is no /proc here, so
# the listener is resolved with lsof and its executable read from `ps -o
# comm=`, which on macOS prints the full path (on Linux it prints the short
# name — the reason this cannot be one shared implementation). Both need sudo:
# the LaunchDaemon runs as root, so its child engine does too.
assert_serving_ollama_macos() {
  local ctx="$1" _ body live st pinned mode pid exe
  local bin="$STATE_DIR/runtimes/ollama/bin/ollama"

  # No engine on disk means nothing can be serving — see the Linux twin for
  # why this comes before the poll rather than after it.
  if ! sudo test -x "$bin"; then
    bad "nothing can be serving on :9475 ($ctx): no engine at $bin"
    return
  fi

  # The gate is a PARSED version, and 180 s outlasts the agent's own
  # first-readiness budget — see the Linux twin for both.
  for _ in $(seq 1 60); do            # ~180 s; the daemon-engine leg can arrive mid cold start
    body="$(curl -fsS --max-time 5 http://127.0.0.1:9475/api/version 2>/dev/null || true)"
    live="$(printf '%s' "$body" | sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
    [ -n "$live" ] && break
    sleep 3
  done
  if [ -z "$live" ]; then
    bad "nothing is serving on :9475 after 180 s ($ctx) — the engine is installed but not answering"
    sudo tail -n 40 "$STATE_DIR/runtimes/ollama/logs/engine.log" 2>/dev/null \
      | sed 's/^/    engine.log| /' >&2 || true
    return
  fi

  st="$(curl -fsS --max-time 5 http://127.0.0.1:9476/waired/v1/inference/status 2>/dev/null || true)"
  pinned="$(printf '%s' "$st" | sed -n 's/.*"pinned_version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
  mode="$(printf '%s' "$st" | grep -oE '"mode"[[:space:]]*:[[:space:]]*"[a-z]+"' | head -1 \
    | sed 's/.*"\([a-z]*\)"$/\1/' || true)"

  # 1. the listener IS the state-dir binary. Quoted throughout: this path has a
  #    space in it (/Library/Application Support/waired).
  pid="$(sudo lsof -nP -iTCP:9475 -sTCP:LISTEN -t 2>/dev/null | head -1 || true)"
  exe=""
  if [ -n "$pid" ]; then
    exe="$(sudo ps -p "$pid" -o comm= 2>/dev/null || true)"
  fi
  if [ -z "$pid" ]; then
    # /api/version answered above, so something IS listening — an empty pid is
    # a lookup failure, not an absent engine, and must not be reported as the
    # wrong binary.
    bad "could not identify the process listening on :9475 ($ctx) — lsof found no listening process"
  elif [ "$exe" = "$bin" ]; then
    ok "the process serving :9475 is the state-dir binary ($ctx; pid $pid)"
  else
    bad "the process serving :9475 is not waired's engine ($ctx): pid=$pid exe=${exe:-unreadable}, expected $bin"
    sudo lsof -nP -iTCP:9475 -sTCP:LISTEN 2>&1 | sed 's/^/    lsof| /' >&2 || true
  fi

  # 2. it reports the pin. An empty pinned_version is its own failure — two
  #    empty strings compare equal.
  if [ -z "$pinned" ]; then
    bad "the daemon published no pinned_version ($ctx) — the version comparison would be vacuous"
  elif [ "$live" = "$pinned" ]; then
    ok "the serving engine is the pinned release ($ctx; /api/version = $live)"
  else
    bad "the serving engine is not the pinned release ($ctx): /api/version = $live, pinned $pinned"
  fi

  # 3. waired spawned it, rather than adopting a survivor.
  case "$mode" in
    spawned) ok "waired spawned the serving engine ($ctx; mode=spawned)" ;;
    "")      bad "the daemon published no engine mode ($ctx) — cannot tell a spawned engine from an adopted one" ;;
    *)       bad "waired did not spawn the serving engine ($ctx; mode=$mode) — it adopted a process it does not supervise" ;;
  esac
}

# --- reading the inference status (mirror of lib/installtest-enroll.sh) -----
#
# Five functions duplicated rather than sourced: this script installs onto the
# host it runs on and cannot source the Linux library. See the comments there
# for what each one is safe for. scripts/dev/installtest-model-ready-asserts.sh
# drives all three copies through the same scenarios per PR and fails on the
# first disagreement — these run only in the dispatch-only inference leg, so a
# copy that had quietly stopped being able to fail would sit green for a long
# time (the shape #573 itself is).
it_json_object() {
  printf '%s' "$1" | grep -oE "\"$2\"[[:space:]]*:[[:space:]]*\{[^}]*\}" | head -1 || true
}

it_json_str() {
  printf '%s' "$1" | grep -oE "\"$2\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" | head -1 |
    sed -E 's/.*:[[:space:]]*"(.*)"$/\1/' || true
}

it_json_true() {
  printf '%s' "$1" | grep -qE "\"$2\"[[:space:]]*:[[:space:]]*true"
}

it_models_ready() {
  printf '%s' "$1" | grep -oE '"ready"[[:space:]]*:[[:space:]]*\[[^]]*\]' | head -1 |
    grep -oE '"[^"]+"' | sed 1d | tr -d '"' || true
}

it_model_ready_state() {
  local json="$1" id ready probe
  id="$(it_json_str "$(it_json_object "$json" active)" model_id)"
  ready="$(it_models_ready "$json")"
  if [ -n "$id" ] && printf '%s\n' "$ready" | grep -qxF "$id"; then
    printf 'ready %s\n' "$id"; return 0
  fi
  if printf '%s' "$json" | grep -qE '"subsystem_state"[[:space:]]*:[[:space:]]*"ready"'; then
    printf 'ready %s\n' "${id:-(ready)}"; return 0
  fi
  if it_json_true "$json" no_model_selected; then
    printf 'none\n'; return 0
  fi
  probe="$(it_json_str "$(it_json_object "$json" host_speed)" probe_model_id)"
  if [ -z "$id" ] && [ -n "$probe" ] && [ "$ready" = "$probe" ]; then
    printf 'probe %s\n' "$probe"; return 0
  fi
  printf 'pending\n'
}

assert_inference_macos() {
  local ollama_bin="$STATE_DIR/runtimes/ollama/bin/ollama" tps notready

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

  assert_bundled_ollama_macos
  # …and it is what actually serves, at the pin (#494). "Installed" and
  # "serving" are two claims; see assert_serving_ollama_macos for why.
  assert_serving_ollama_macos "waired init"

  # #567: the bundled engine is waired-owned on :9475 with its own store; the
  # agent (spawning the state-dir binary) pulls there, NOT into the
  # upstream default :11434. `waired init --inference-enabled=true` started the
  # LaunchDaemon and #519-foreground-waited for the pull, so readiness is read
  # from the mgmt API on :9476 — the same source init polls — never a bare
  # `ollama list` (which targets :11434 and is always empty here, the original
  # false negative). Poll briefly to absorb any residual async tail.
  local infurl="http://127.0.0.1:9476/waired/v1/inference/status"
  # "Ready" is it_model_ready_state's verdict, not "models.ready is non-empty":
  # the #496 cutoff probe lands in models.ready like any other pull, so the old
  # test broke here on a host that had a probe and no selection (#573).
  local out="" state verdict ready=0 _
  for _ in $(seq 1 60); do          # ~5 min; CPU model pull is minutes-scale
    out="$(curl -fsS --max-time 10 "$infurl" 2>/dev/null || true)"
    verdict="$(it_model_ready_state "$out")"
    case "$verdict" in
      ready\ *) ready=1; break ;;
      # The operator's standing "no model now" choice (#586) is terminal:
      # nothing is coming, so waiting out the budget only delays the red.
      none)     break ;;
    esac
    state="$(printf '%s' "$out" | grep -oE '"subsystem_state"[[:space:]]*:[[:space:]]*"[a-z_]+"' | head -1 | grep -oE '"[a-z_]+"$' | tr -d '"')"
    # engine_failed is terminal too (waired-agent#29): the engine crashed and
    # automatic recovery either is mid-flight (which shows as "starting") or
    # has given up. Either way, polling for "ready" will not fix it — this list
    # had drifted from the Linux one in lib/installtest-enroll.sh, so a crashed
    # engine burned the whole ~5 min budget here.
    case "$state" in pull_failed|disabled|stopped|engine_failed) break ;; esac
    sleep 5
  done
  verdict="$(it_model_ready_state "$out")"
  if [ "$ready" = 1 ]; then
    ok "bundled model ready in waired store :9475 (${verdict#ready }; the daemon's active selection, via mgmt API)"
  else
    case "$verdict" in
      probe\ *)
        bad "this host got a probe, not a pick: the only model in the waired store is the host-cutoff probe (${verdict#probe }), and the daemon committed to no selection (#573)" ;;
      none)
        bad "no model was selected on this host (mgmt API no_model_selected=true) — \`waired init --inference-enabled=true\` should have picked one" ;;
      *)
        bad "bundled model not ready via mgmt API (deploy/pull failed?)" ;;
    esac
    printf '%s\n' "$out" | sed 's/^/    /' >&2
    # Diagnostics from the RIGHT store (:9475), using the bundled binary.
    sudo test -x "$ollama_bin" \
      && sudo env OLLAMA_HOST=127.0.0.1:9475 "$ollama_bin" list 2>&1 | sed 's/^/    :9475 /' || true
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

  # #496/#579: the one-time host-speed measurement — see the Linux twin in
  # lib/installtest-enroll.sh for why this leg asserts it. This runner is the
  # one that measured 432 s per sample against a 45 s budget, so the figures in
  # the ok line are the early warning for a cap that has stopped fitting.
  # POLLED, not read once — see the Linux twin for why. The measurement is
  # asynchronous by design, so a single read asserts on scheduling rather
  # than on the daemon.
  local hs turn budget samples floor method figure
  local hs_deadline=$((SECONDS + 180))
  while [ -n "$out" ] && [ "$SECONDS" -lt "$hs_deadline" ]; do
    hs="$(it_json_object "$out" host_speed)"
    case "$(printf '%s' "$hs" | grep -oE '"turn(_floor)?_seconds"[[:space:]]*:[[:space:]]*[0-9.]+' | grep -oE '[0-9.]+$' | grep -vE '^0(\.0+)?$' | head -1)" in
      "") ;;
      *)  break ;;
    esac
    sleep 5
    out="$(curl -fsS --max-time 10 "$infurl" 2>/dev/null || true)"
  done

  if [ -z "$out" ]; then
    it_warn "no inference status payload — skipping the host-speed assert"
  else
    hs="$(it_json_object "$out" host_speed)"
    turn="$(printf '%s' "$hs" | grep -oE '"turn_seconds"[[:space:]]*:[[:space:]]*[0-9.]+' | grep -oE '[0-9.]+$' || true)"
    budget="$(printf '%s' "$hs" | grep -oE '"budget_seconds"[[:space:]]*:[[:space:]]*[0-9.]+' | grep -oE '[0-9.]+$' || true)"
    samples="$(printf '%s' "$hs" | grep -oE '"samples"[[:space:]]*:[[:space:]]*[0-9]+' | grep -oE '[0-9]+$' || true)"
    # A host far below the cutoff publishes a BOUND and no turn: turn_seconds
    # stays a measurement wherever it appears (owner ruling on waired-agent#620),
    # so the figure to assert on is whichever of the two the daemon set, and
    # `method` says which one that is (waired-agent#579 Stage 3).
    floor="$(printf '%s' "$hs" | grep -oE '"turn_floor_seconds"[[:space:]]*:[[:space:]]*[0-9.]+' | grep -oE '[0-9.]+$' || true)"
    method="$(printf '%s' "$hs" | sed -n 's/.*"method"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' || true)"
    figure="$turn"
    case "$figure" in ""|0|0.0) figure="$floor" ;; esac
    # BLOCKING since waired-agent#579 shipped — see the Linux twin.
    case "$figure" in
      ""|0|0.0)
        bad "no host-speed measurement published (#496): the daemon never finished measuring this host inside init, so nothing decided whether a model belonged here (waired-agent#579)"
        # The linux twin's comment applies here verbatim, and this is the
        # leg it was observed on: run 31605659210's routing sentinel (macos)
        # produced this red with zero daemon output (waired-agent#735).
        # Counts no assert, so the tier floors do not move.
        hostspeed_evidence
        ;;
      *)        ok "host speed measured (${method:-?}: turn ${turn:-0}s, floor ${floor:-0}s, against a ${budget:-?}s budget; ${samples:-0} samples)" ;;
    esac
  fi

  # Asked of the DAEMON, not of agent.json. The config file carries the
  # install-time DEFAULT (agentconfig.Inference.Enabled); the runtime answer is
  # planInitialInference's, folding that default together with the persisted
  # desired-inference toggle and any --inference-enabled flag. The two diverge
  # by design on a host the install-time selection declined and the operator
  # then turned on — which is exactly the routing sentinel, where this assert
  # failed while inference was demonstrably serving (waired-agent#552).
  #
  # desired_state is the field the tray reads for the same reason: it is the
  # operator's intent, independent of SubsystemState's engine health.
  # One read, two facts — see the Linux twin in lib/installtest-enroll.sh for
  # why turned_inference_off rides along.
  local desired_json desired by_cutoff
  desired_json="$(curl -fsS --max-time 5 http://127.0.0.1:9476/waired/v1/inference/status 2>/dev/null || true)"
  desired="$(printf '%s' "$desired_json" | grep -oE '"desired_state"[[:space:]]*:[[:space:]]*"[a-z]+"' \
    | grep -oE '"[a-z]+"$' | tr -d '"' || true)"
  by_cutoff=false
  it_json_true "$(it_json_object "$desired_json" host_speed)" turned_inference_off && by_cutoff=true
  case "$desired" in
    enabled) ok "local inference is on (mgmt API desired_state=enabled)" ;;
    "")      bad "the daemon published no desired_state — cannot tell an enabled host from a disabled one" ;;
    *)       bad "local inference is off (mgmt API desired_state=$desired; the host-speed cutoff turned it off: $by_cutoff)" ;;
  esac

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
  #
  # Three arms, not two (#382): a benchmark that RAN and produced nothing is an
  # engine problem, a benchmark that NEVER RAN because the model was not ready
  # in time is a download one. Both stay red — the distinction is what the red
  # says. Mirrors lib/installtest-enroll.sh; this leg is structurally MORE
  # exposed than the sentinel, because it pulls the real multi-GB bundled model
  # rather than a 350M fixture.
  tps=""; notready=""
  if [ -f "$INITLOG" ]; then
    tps="$(grep -ioE '[0-9]+(\.[0-9]+)? *(tok|tokens)/s' "$INITLOG" | head -1 || true)"
    notready="$(grep -oE "$IT_BENCH_NOT_READY_RE" "$INITLOG" | head -1 || true)"
  fi
  if [ -n "$tps" ]; then
    ok "benchmark ran during init ($tps)"
  elif [ -n "$notready" ]; then
    # See the linux twin: the fourth branch has no download, so it gets its
    # own red rather than one that names a transfer that never existed
    # (waired-agent#736). One `bad` either way.
    case "$notready" in
      'No model was chosen for this computer')
        bad "no model was ever selected for this host, so init's benchmark window had nothing to measure — neither the download nor the engine (\"$notready\"; $INITLOG)" ;;
      *)
        bad "the model was not ready inside init's benchmark window, so nothing was measured — the download, not the engine (\"$notready\"; $INITLOG)" ;;
    esac
    grep -iE 'download|model|pull' "$INITLOG" 2>/dev/null | tail -20 | sed 's/^/    init| /' >&2 || true
    # Pull-side evidence only; engine.log stays on the arm below (#382).
    #
    # sudo, like the sibling read further up: $ollama_bin lives under the state
    # dir, and this script's own Tier 1 asserts that dir is root-owned and mode
    # 700. An unelevated exec there cannot work by construction — it printed
    # `Permission denied` on every failed macOS leg instead of the model list,
    # which is exactly the line an investigation needs (#580). `sudo test -x`
    # rather than `[ -n ]` for the same reason: a mode-700 dir also hides the
    # binary from an unelevated test.
    sudo test -x "$ollama_bin" \
      && sudo env OLLAMA_HOST=127.0.0.1:9475 "$ollama_bin" list 2>&1 | sed 's/^/    :9475 /' >&2 || true
    # The `|| echo` is inside the pipe on purpose: a failed `curl -fsS` prints
    # nothing, and a trailing `|| true` would never fire because the pipeline's
    # status is sed's — so an unreachable daemon would leave no line at all.
    { curl -fsS --max-time 10 http://127.0.0.1:9476/waired/v1/inference/status || echo "(status unreachable)"; } 2>&1 |
      sed 's/^/    status| /' >&2 || true
    # The daemon's own account of a model that never arrived (#540). Mirrors
    # lib/installtest-enroll.sh's it_prepull_evidence: `waired logs` is the one
    # surface that reads the service log and the engine log on every OS, and
    # the pattern is the facts that settle where the time went — what the boot
    # pre-pull's hold was waiting for, what released it, `POST /api/pull` with
    # the download's real duration, and what the #496 measurement was doing
    # while all of that waited (#579).
    sudo "$BINDIR/waired" logs --since 30m --state-dir "$STATE_DIR" -o /tmp/it-logs.txt >/dev/null 2>&1 || true
    evidence_dump /tmp/it-logs.txt 2>&1 |
      sed 's/^/    agent| /' >&2 || true
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
# assert_reinit_engine_optout_macos: the darwin twin of
# lib/installtest-enroll.sh's assert_reinit_engine_optout — on a host where
# engine installs are turned off, `waired init` must not report the
# operator's own instruction back to them as a failed install
# (waired-agent#551). See that function for why the probe exists and why
# --inference-enabled=true is what keeps it from being vacuous.
#
# macOS is where this leg is worth the most: it has no #313 re-init assert at
# all, so until now nothing here re-ran init on an enrolled device.
#
# Exactly four asserts, always — the tier-2 floor counts on it.
assert_reinit_engine_optout_macos() {
  local log="$WORK/reinit-optout.log" rc=0

  # `| tee`, not `>`: the enrol block above uses the same shape for the same
  # two reasons — sudo does not own the redirect, and pipefail collapses the
  # pipeline's status so init's own exit code has to come from PIPESTATUS[0].
  it_log "re-running waired init with engine installs turned off (waired-agent#551)"
  if sudo env WAIRED_NO_EMOJI=1 WAIRED_NO_OLLAMA=1 "$BINDIR/waired" init \
        --control "$IT_CONTROL_URL" --device-name "$device" \
        --inference-enabled=true --non-interactive --skip-integration \
        --state-dir "$STATE_DIR" 2>&1 | tee "$log"; then
    rc=0
  else
    rc="${PIPESTATUS[0]}"
  fi

  [ "$rc" = 0 ] \
    && ok "re-init with engine installs turned off exits 0 (waired-agent#551)" \
    || bad "re-init exited $rc with WAIRED_NO_OLLAMA set — an opt-out the operator configured is not a failed install — see $log"
  grep -q "$IT_ENGINE_OPTOUT_RE" "$log" \
    && ok "the executor reached the opt-out arm and said so" \
    || bad "init never reported the engine install as skipped — the opt-out arm was not reached, so the asserts around it prove nothing — see $log"
  grep -q "$IT_INSTALL_FAILURE_BOX_RE" "$log" \
    && bad "init called the operator's own opt-out a failed install — see $log" \
    || ok "init does not report the opt-out as a failed install"
  sudo test -x "$STATE_DIR/runtimes/ollama/bin/ollama" \
    && bad "an engine was installed under $STATE_DIR despite WAIRED_NO_OLLAMA" \
    || ok "no engine was installed while the opt-out was set"

  [ "$rc" = 0 ] || tail -n 20 "$log" | sed 's/^/    /' >&2
  # Leave the host as we found it: the asserts after this one were measured
  # against inference off, and :9476 refuses mutating writes over TCP
  # (waired#838), so the CLI is the only way back.
  sudo "$BINDIR/waired" inference off --state-dir "$STATE_DIR" >/dev/null 2>&1 || \
    it_warn "could not turn inference back off after the #551 probe"
}

# _it_wait_enrolled_macos — poll the system daemon until it reports an
# identity. The darwin twin of lib/installtest-enroll.sh's
# _it_wait_enrolled, factored out of the Tier-2 readback below so the
# probes that restart the service can reuse it: a restart drops the
# enrolled session for a few seconds, and on linux the assert right
# after a restart was the first casualty of not waiting (#605).
_it_wait_enrolled_macos() {
  local _ out=""
  for _ in $(seq 1 40); do
    out="$(curl -fsS --max-time 5 "$MGMT" 2>/dev/null || true)"
    if printf '%s' "$out" | grep -qE '"device_id"[[:space:]]*:[[:space:]]*"dev_'; then
      printf '%s' "$out"; return 0
    fi
    sleep 1
  done
  printf '%s' "$out"; return 1
}

# The #568 install-time memory measurement, as the daemon persists it.
HOSTMEM_JSON="$STATE_DIR/runtime/host-memory.json"

# _it_force_below_spec_macos / _it_restore_host_memory_macos — make this
# host read as below the recommended spec for the probes that need it,
# and put it back.
#
# Linux arranges this with WAIRED_RAM_AVAILABLE_GB in the systemd
# EnvironmentFile. On darwin the plist route would need a
# bootout/bootstrap cycle (launchd does not re-read a plist on
# `kickstart`), so this uses `launchctl setenv` on the system domain,
# which the daemon inherits when kickstart relaunches it, and patches the
# persisted record as well — the OTHER end of the same read, since
# hostMemoryGB() takes the env seam first and the record otherwise.
#
# The record patch USED to be the whole arrangement, on the strength of
# the daemon reusing a record whose agent_version matched its own build
# (waired-agent#568). waired-agent#835 revised that: the daemon
# re-measures at every start and keeps the HIGHER of the reading and the
# record, so a patched-down record is raised straight back by the restart
# below. _it_check_below_spec_seam_macos reports which way it went.
#
# On the GitHub macOS runner these probes would be reached anyway — the
# runner has about 7 GB and is genuinely below spec — so a seam that
# quietly stopped working would leave the leg passing for the wrong
# reason. That is what the check exists to prevent.
_it_force_below_spec_macos() {
  local who="$1"
  sudo "$BINDIR/waired" inference on --state-dir "$STATE_DIR" >/dev/null 2>&1 || \
    it_warn "could not turn inference on before the $who probe"
  # The third thing these probes need, and the one this cannot arrange: an
  # engine-less host. See lib/installtest-enroll.sh's _it_force_below_spec
  # for the whole story (waired-agent#640) — the state is inherited from
  # whatever ran before, so the only useful thing to do is say when it does
  # not hold.
  if sudo test -x "$STATE_DIR/runtimes/ollama/bin/ollama"; then
    it_warn "an engine is already installed under $STATE_DIR before the $who probe — the daemon no longer wants one, so the arm under test will not be reached (waired-agent#640)"
  fi
  sudo launchctl setenv WAIRED_RAM_AVAILABLE_GB 1 2>/dev/null || \
    it_warn "could not set the WAIRED_RAM_AVAILABLE_GB seam for the $who probe"
  sudo cp "$HOSTMEM_JSON" "$WORK/host-memory.json.bak" 2>/dev/null || \
    it_warn "no host-memory record at $HOSTMEM_JSON — the $who probe cannot force a below-spec verdict"
  sudo /usr/bin/sed -i '' 's/"available_gb": *[0-9]*/"available_gb": 1/' "$HOSTMEM_JSON" 2>/dev/null || true
  # `1` followed by a NON-digit: a plain '"available_gb": 1' would also
  # match the 16 or 121 a failed sed would have left behind, which is a
  # verification that cannot fail.
  sudo grep -qE '"available_gb": 1([^0-9]|$)' "$HOSTMEM_JSON" 2>/dev/null || \
    it_warn "the #568 measurement seam did not take on $HOSTMEM_JSON; the $who probe's own asserts are what will say so"
  sudo launchctl kickstart -k "system/$LABEL" >/dev/null 2>&1 || \
    it_warn "could not restart the daemon for the $who probe"
  _it_wait_enrolled_macos >/dev/null || \
    it_warn "daemon did not report enrolled after the $who seam restart"
  _it_check_below_spec_seam_macos "$who"
}

# _it_check_below_spec_seam_macos — did the arrangement hold?
#
# The record is the witness. With the env seam in force the daemon
# persists nothing (WAIRED_RAM_AVAILABLE_GB short-circuits
# ensureHostMemoryMeasured), so the patched 1 is still on disk after the
# restart. If the seam did not reach the daemon, it re-measured and
# rewrote the record — and on a runner that happens to be below spec on
# its own, that difference is invisible in the asserts that follow.
# A warning rather than a failure for exactly that reason: the arm is
# still reached here, and the leg is still meaningful; what is lost is
# the guarantee that it would be reached on a larger runner.
_it_check_below_spec_seam_macos() {
  local who="$1"
  if sudo grep -qE '"available_gb": 1([^0-9]|$)' "$HOSTMEM_JSON" 2>/dev/null; then
    return 0
  fi
  it_warn "the $who below-spec seam did not hold — the daemon re-measured and rewrote $HOSTMEM_JSON. This runner is below spec on its own, so the asserts below still run, but they are no longer arranged (waired-agent#835)"
}

# _it_engine_present_note_macos — the darwin twin of
# lib/installtest-enroll.sh's _it_engine_present_note (waired-agent#640):
# the likeliest reason the arm under test was not reached, as a clause to
# append to a failure message. Echoes nothing when it does not apply.
_it_engine_present_note_macos() {
  if sudo test -x "$STATE_DIR/runtimes/ollama/bin/ollama"; then
    printf ' — an engine is already installed at %s, so the daemon no longer wanted one (waired-agent#640)' \
      "$STATE_DIR/runtimes/ollama/bin/ollama"
  fi
}

# Leave the host as we found it: the real measurement back, the daemon
# restarted on it, and inference off — the state every assert after these
# probes was written against.
_it_restore_host_memory_macos() {
  local who="$1"
  sudo launchctl unsetenv WAIRED_RAM_AVAILABLE_GB 2>/dev/null || true
  if [ -f "$WORK/host-memory.json.bak" ]; then
    sudo cp "$WORK/host-memory.json.bak" "$HOSTMEM_JSON" || \
      it_warn "could not restore the host-memory record after the $who probe"
  else
    # Nothing to restore: removing it makes the daemon measure again on
    # its next clean boot, which is the state we would have left anyway.
    sudo rm -f "$HOSTMEM_JSON" || true
  fi
  sudo launchctl kickstart -k "system/$LABEL" >/dev/null 2>&1 || \
    it_warn "could not restart the daemon after the $who probe"
  _it_wait_enrolled_macos >/dev/null || \
    it_warn "daemon did not report enrolled after the $who cleanup restart"
  sudo "$BINDIR/waired" inference off --state-dir "$STATE_DIR" >/dev/null 2>&1 || \
    it_warn "could not turn inference back off after the $who probe"
}

# assert_reinit_default_unfit_macos: the darwin twin of
# lib/installtest-enroll.sh's assert_reinit_default_unfit
# (waired-agent#590). On a host below the recommended spec, a
# non-interactive init with NO inference flag must end with local AI off,
# exit 0, and the skip note — a choice, not a fault (the #551 exit
# discipline; distinct from the #569/#576 exit-3 contract).
#
# macOS is where this twin is worth the most: the macos-14 runner has
# 7 GB, so this is the machine class where the measured deduction
# (waired-agent#568) flips the default in the field. The seam still runs,
# because a probe that only fires on small runners is a probe that stops
# firing the day CI buys a bigger one.
#
# Exactly four asserts, always — the tier-2 floor counts on it.
assert_reinit_default_unfit_macos() {
  local log="$WORK/reinit-default-unfit.log" rc=0

  it_log "re-running waired init with no inference flag on a forced below-spec host (waired-agent#590)"
  _it_force_below_spec_macos "#590 default"

  # `| tee`, not `>`, and PIPESTATUS[0] — same two reasons as the #551
  # probe above.
  if sudo env WAIRED_NO_EMOJI=1 "$BINDIR/waired" init \
        --control "$IT_CONTROL_URL" --device-name "$device" \
        --non-interactive --skip-integration \
        --state-dir "$STATE_DIR" 2>&1 | tee "$log"; then
    rc=0
  else
    rc="${PIPESTATUS[0]}"
  fi

  [ "$rc" = 0 ] \
    && ok "flagless init on a below-spec host exits 0 (a choice, not a fault — waired-agent#590)" \
    || bad "flagless init exited $rc on a below-spec host — the non-interactive default is skip-and-continue, never a failure — see $log"
  if grep -q "$IT_UNFIT_SKIP_RE" "$log"; then
    ok "the step-4 non-interactive default said what it did"
  else
    bad "init never printed the skip note — the step-4 default arm was not reached, so the asserts around it prove nothing$(_it_engine_present_note_macos) — see $log"
    tail -n 20 "$log" | sed 's/^/    init| /' >&2
  fi
  grep -q "$IT_INSTALL_FAILURE_BOX_RE" "$log" \
    && bad "init reported the below-spec default as a failed install — see $log" \
    || ok "the default is not reported as a failed install"
  local desired
  desired="$(curl -fsS --max-time 5 http://127.0.0.1:9476/waired/v1/inference/status 2>/dev/null \
    | grep -oE '"desired_state"[[:space:]]*:[[:space:]]*"[a-z]+"' \
    | grep -oE '"[a-z]+"$' | tr -d '"' || true)"
  [ "$desired" = disabled ] \
    && ok "the default landed as the persisted toggle (mgmt API desired_state=disabled)" \
    || bad "mgmt API desired_state=$desired after the flagless below-spec init, want disabled"

  [ "$rc" = 0 ] || tail -n 20 "$log" | sed 's/^/    /' >&2
  _it_restore_host_memory_macos "#590 default"
}

# assert_models_pull_confirm_macos: the darwin twin of
# lib/installtest-enroll.sh's assert_models_pull_confirm
# (waired-agent#590). See that function for the contract and for why an
# engine-less host makes the honoured row free — the daemon refuses the
# handed-on pull at #307's admission check instead of fetching weights.
#
# Exactly five asserts, always — the tier-2 floor counts on it.
assert_models_pull_confirm_macos() {
  local log model rc=0

  it_log "checking the models-pull confirmation on a forced below-spec host (waired-agent#590)"
  _it_force_below_spec_macos "#590 pull twin"

  model="$(curl -fsS --max-time 5 http://127.0.0.1:9476/waired/v1/inference/catalog 2>/dev/null \
    | grep -oE '"model_id"[[:space:]]*:[[:space:]]*"[^"]+"' | head -1 \
    | grep -oE '"[^"]+"$' | tr -d '"' || true)"
  if [ -z "$model" ]; then
    bad "no model_id in the catalog response — the pull gate reads the same endpoint, so nothing below would be testing it"
    _it_restore_host_memory_macos "#590 pull twin"
    # Still five: a leg that reports four has a block that stopped
    # executing, and the floor is what says so.
    bad "skipped: --yes on a model that does not fit was not exercised"
    bad "skipped: the decline is not a failed command"
    bad "skipped: --yes alone did not reach the pull layer"
    bad "skipped: --yes --force honoured the choice"
    return
  fi

  # `| tee`, not `>`: sudo does not own the redirect (shellcheck SC2024),
  # and pipefail collapses the pipeline's status, so the CLI's own exit
  # code has to come from PIPESTATUS[0] — the same shape the #551 probe
  # above uses.
  log="$WORK/models-pull-yes.log"
  if sudo env WAIRED_NO_EMOJI=1 "$BINDIR/waired" models pull "$model" --yes --wait=false \
        </dev/null 2>&1 | tee "$log"; then
    rc=0
  else
    rc="${PIPESTATUS[0]}"
  fi

  [ "$rc" = 0 ] \
    && ok "declining an over-memory pull is not a failed command (exit 0)" \
    || bad "\`models pull --yes\` exited $rc on a model that does not fit — a decline is a choice, not a fault — see $log"
  grep -qF "$IT_PULL_DECLINE_RE" "$log" \
    && ok "--yes alone declines an over-memory pull and says how to override" \
    || bad "\`models pull --yes\` never printed the decline line — --yes must not auto-confirm a default-No question — see $log"
  grep -q "$IT_PULL_QUEUED_RE" "$log" \
    && bad "\`models pull --yes\` queued the download anyway — the gate did not stop it — see $log" \
    || ok "--yes alone dispatched nothing to the daemon"

  rc=0
  log="$WORK/models-pull-force.log"
  if sudo env WAIRED_NO_EMOJI=1 "$BINDIR/waired" models pull "$model" --yes --force --wait=false \
        </dev/null 2>&1 | tee "$log"; then
    rc=0
  else
    rc="${PIPESTATUS[0]}"
  fi

  grep -qF "$IT_PULL_DECLINE_RE" "$log" \
    && bad "\`models pull --yes --force\` still declined — the scripted consent is the pair, and it was not honoured — see $log" \
    || ok "--yes --force is not stopped by the over-memory gate"
  grep -qE "$IT_PULL_REACHED_RE" "$log" \
    && ok "--yes --force handed the pull to the daemon" \
    || bad "\`models pull --yes --force\` neither queued nor reached the daemon's pull layer — see $log"

  [ "$rc" = 0 ] || tail -n 10 "$log" | sed 's/^/    /' >&2
  _it_restore_host_memory_macos "#590 pull twin"
}

# assert_engine_only_install_macos: the darwin twin of
# lib/installtest-enroll.sh's assert_engine_only_install
# (waired-agent#590). See that function for the contract — "the AI
# software is installed and no model was chosen" is a FINISHED install,
# and the restart is what makes the answer a standing choice rather than
# a transient one.
#
# THE ONE INTERACTIVE INIT ON THIS HARNESS, for the same reason it is on
# the Linux one: every other init here passes --non-interactive, and
# runInitModelPicker returns on that flag before it asks anything. One
# line of stdin is the only way in, and one line is enough because
# --inference-enabled=true silences the two questions in front of the
# picker.
#
# Exactly six asserts, always — the floor counts on it.
assert_engine_only_install_macos() {
  local log="$WORK/engine-only.log" rc=0
  local bin="$STATE_DIR/runtimes/ollama/bin/ollama"

  it_log "installing an engine and answering the model picker with 0 (waired-agent#590)"
  # The daemon has to WANT an engine — the two #590 probes above leave the
  # toggle off, and the #551 probe before them turned it off too.
  sudo "$BINDIR/waired" inference on --state-dir "$STATE_DIR" >/dev/null 2>&1 || \
    it_warn "could not turn inference on before the #590 engine-only probe"

  # `| tee`, not `>`, and PIPESTATUS — same two reasons as the #551 probe
  # above. The index is [1], not [0]: printf is the head of this pipeline,
  # so the CLI is the SECOND element. sudo hands its own stdin to the
  # command it runs, and this host has passwordless sudo (asserted at
  # startup), so nothing upstream consumes the line.
  if printf '0\n' | sudo env WAIRED_NO_EMOJI=1 "$BINDIR/waired" init \
        --control "$IT_CONTROL_URL" --device-name "$device" \
        --inference-enabled=true --skip-integration \
        --state-dir "$STATE_DIR" 2>&1 | tee "$log"; then
    rc=0
  else
    rc="${PIPESTATUS[1]}"
  fi

  [ "$rc" = 0 ] \
    && ok "an install that ends with no model chosen exits 0 (waired-agent#590)" \
    || bad "init exited $rc after the operator chose not to download a model — that is a finished install, not a failure — see $log"
  # Anti-vacuity, and the load-bearing one: without it every assert here
  # would pass on a host where the picker never ran and the daemon's own
  # auto-selection quietly applied instead (which is what #607 was).
  grep -qF "$IT_NO_MODEL_RE" "$log" \
    && ok "the picker asked and recorded the no-model answer" \
    || bad "init never printed the no-model line — the picker did not run, so the asserts around it prove nothing — see $log"
  grep -q "$IT_INSTALL_FAILURE_BOX_RE" "$log" \
    && bad "init reported an engine-only install as a failed install — see $log" \
    || ok "an engine-only install is not reported as a failed install"
  sudo test -x "$bin" \
    && ok "the engine is installed ($bin) — this host runs AI, it just has no model yet" \
    || bad "no engine at $bin — the point of this state is that the software IS installed — see $log"

  _it_no_model_selected_macos \
    && ok "the daemon publishes the standing no-model choice (mgmt API no_model_selected=true)" \
    || bad "mgmt API does not report no_model_selected after the operator chose not to download a model"

  # The restart is the whole point of the sixth assert: an answer that
  # does not survive one is not a standing choice, and the #379 boot
  # pre-pull is what would otherwise fetch a model nobody asked for.
  sudo launchctl kickstart -k "system/$LABEL" >/dev/null 2>&1 || \
    it_warn "could not restart the daemon for the #590 engine-only probe"
  _it_wait_enrolled_macos >/dev/null || \
    it_warn "daemon did not report enrolled after the #590 engine-only restart"
  _it_no_model_selected_macos \
    && ok "the choice survives a restart — the boot pre-pull stands down (waired-agent#379)" \
    || bad "no_model_selected is gone after a restart — the boot pre-pull is about to download a model nobody asked for"

  if [ "$rc" != 0 ]; then
    tail -n 25 "$log" | sed 's/^/    /' >&2
    curl -fsS --max-time 10 http://127.0.0.1:9476/waired/v1/inference/status 2>&1 | sed 's/^/    status| /' || true
  fi

  # Leave the host as we found it for anything after this one.
  sudo "$BINDIR/waired" inference off --state-dir "$STATE_DIR" >/dev/null 2>&1 || \
    it_warn "could not turn inference back off after the #590 engine-only probe"
}

# _it_no_model_selected_macos — is the daemon publishing the standing "run
# without a model" choice? Twin of lib/installtest-enroll.sh's
# _it_no_model_selected; see that one for why it reads the mgmt API's own
# field rather than inferring from an empty model list.
_it_no_model_selected_macos() {
  curl -fsS --max-time 5 http://127.0.0.1:9476/waired/v1/inference/status 2>/dev/null \
    | grep -qE '"no_model_selected"[[:space:]]*:[[:space:]]*true'
}

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

  # The compatibility reads stay on TCP (waired#836 allow-list).
  curl -fsS --max-time 5 "$MGMT" >/dev/null 2>&1 \
    && ok "TCP :9476 still serves the compatibility reads" \
    || bad "TCP :9476 no longer serves /status (waired#836 allow-list)"

  # Everything else moved to the socket.
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    http://127.0.0.1:9476/waired/v1/identity 2>/dev/null || true)
  case "$code" in
    2*) bad "TCP :9476 served /identity (HTTP $code); readGuard not enforcing (waired#836)" ;;
    "") bad "TCP :9476 unlisted-read probe produced no status code" ;;
    *)  ok "TCP :9476 refuses reads outside the allow-list (HTTP $code)" ;;
  esac

  curl -fsS --max-time 5 --unix-socket /var/run/waired/mgmt.sock \
    http://waired-mgmt/waired/v1/identity >/dev/null 2>&1 \
    && ok "the management socket serves /identity" \
    || bad "the management socket does not serve /identity; the read moved nowhere (waired#836)"

  # The #836 browser hardening itself — browserGuard is OFF by default in
  # the unit tests, so nothing else would notice --mgmt-hardening flipping.
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -H 'Host: evil.example' http://127.0.0.1:9476/waired/v1/status 2>/dev/null || true)
  case "$code" in
    2*) bad "TCP :9476 answered an attacker Host (HTTP $code); browserGuard not enforcing (waired#836)" ;;
    "") bad "TCP :9476 Host probe produced no status code" ;;
    *)  ok "TCP :9476 rejects a non-loopback Host (HTTP $code)" ;;
  esac

  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST \
    -H 'Content-Type: text/plain' --data '{"peer":"x"}' \
    http://127.0.0.1:9476/waired/v1/ping 2>/dev/null || true)
  if [ "$code" = "415" ]; then
    ok "TCP :9476 requires application/json on writes (HTTP 415)"
  else
    bad "POST /ping with text/plain returned HTTP ${code:-none}, want 415 (waired#836)"
  fi

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
  local out state setup_state desired_engine installed claim
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
  # THE REGRESSION BAR: install.sh ran with --skip-ollama, so only the
  # executor could have put an engine here — and since #492 "here" is one
  # path, not "anywhere download.ResolveBinary can see" (#139).
  assert_bundled_ollama_macos
  # …and that binary is the one serving, at the pin (#494). The assert above
  # proves the executor put something on disk; this proves the host is not
  # being served by something else, which is the half #139 was about.
  assert_serving_ollama_macos "daemon-path executor"
  out="$(curl -fsS --max-time 5 http://127.0.0.1:9476/waired/v1/inference/status 2>/dev/null || true)"
  state="$(printf '%s' "$out" | grep -oE '"subsystem_state"[[:space:]]*:[[:space:]]*"[a-z_]+"' | head -1 | grep -oE '"[a-z_]+"$' | tr -d '"')"
  # ready, and nothing else — see the twin in lib/installtest-daemon-engine.sh
  # for why (#748). Not-no_engine accepted 10 of the 11 declared states,
  # disabled among them. Measured ready on this leg in run 31605659210.
  case "$state" in
    ready) ok "inference subsystem is serving (state=ready)" ;;
    *) bad "inference subsystem reports '${state:-unreachable}', want ready (the executor's engine is not serving)" ;;
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

# --engine-only (waired-agent#590) is its own mode for the same reason, plus
# one of its own: its single init is INTERACTIVE, which every other mode's
# --non-interactive would make unreachable.
if [ "$ENGINE_ONLY" = 1 ]; then
  { [ "$INFER" = 1 ] || [ "$INTEG" = 1 ] || [ "$DAEMON_ENGINE" = 1 ]; } && it_die \
    "--engine-only is its own mode; not with --inference/--integration/--daemon-engine"
  [ "$TIER" -ge 2 ] || it_die "--engine-only needs --tier 2 (it enrols before it asks about models)"
fi

# --- build the darwin tarball install.sh will consume -----------------------
arch="$(uname -m)"; [ "$arch" = "x86_64" ] && arch=amd64   # arm64 stays arm64
tarball="waired-darwin-${arch}.tar.gz"
ver="$(git -C "$ROOT" rev-parse --short HEAD)"
# Version and BuildSHA are DIFFERENT strings, as they are in a real build.
# Stamping the bare SHA into both is the shape of #631, and it is what this
# harness used to do — so it could never have caught it. $semver is the same
# dev version already written to the tarball's VERSION file below.
semver="0.0.0-$ver"
ldf="-s -w -X github.com/waired-ai/waired-agent/internal/buildinfo.Version=$semver -X github.com/waired-ai/waired-agent/internal/buildinfo.BuildSHA=$ver"

it_step "building waired + waired-agent + waired-tray (darwin/$arch) and packing $tarball"
mkdir -p "$WORK/stage" "$DIST"
( cd "$ROOT"
  GOOS=darwin GOARCH="$arch" CGO_ENABLED=0 go build -trimpath -ldflags="$ldf" -o "$WORK/stage/waired"       ./cmd/waired
  GOOS=darwin GOARCH="$arch" CGO_ENABLED=0 go build -trimpath -ldflags="$ldf" -o "$WORK/stage/waired-agent" ./cmd/waired-agent
  # CGO=1 for the tray: its systray backend is Cocoa, exactly as
  # `make build-tray-darwin` does it. The tarball has to carry a REAL
  # waired-tray now -- install.sh builds /Applications/Waired.app around it
  # (waired-agent#833), and a stub would leave "the shipped binary is what
  # lands in the bundle" untested on the only leg that can check it.
  CGO_ENABLED=1 GOOS=darwin GOARCH="$arch" go build -trimpath -ldflags="$ldf" -o "$WORK/stage/waired-tray" ./cmd/waired-tray
) || it_die "go build (darwin/$arch) failed"
printf '%s' "$semver" > "$WORK/stage/VERSION"
# Guard the pack + checksum explicitly: this script runs `set -uo pipefail`
# (NOT -e — the inference poll below relies on no-match greps in command
# substitutions that -e would abort). Without a guard a failed pack would let
# the run barrel into install.sh against a missing tarball and report a
# confusing "install.sh exited N" instead of dying at the real cause.
tar czf "$DIST/$tarball" -C "$WORK/stage" waired waired-agent waired-tray VERSION \
  || it_die "packing $tarball failed"
( cd "$DIST" && shasum -a 256 "$tarball" > "$tarball.sha256" ) \
  || it_die "checksumming $tarball failed"

# --- Tier 1: run install.sh's darwin path + assert --------------------------
# Ollama: install.sh no longer pre-installs the engine — `waired init` owns
# the decision + install, so the Tier-2 `--inference-enabled=true` init below
# downloads the bundled engine itself (#514 journey preserved, ordering
# fixed; #492 moved it under the state dir). The
# default path opts out explicitly (--skip-ollama -> WAIRED_NO_OLLAMA for
# init) — Tier 1/2 only need the installer + enroll.
#
# --log-level debug is passed on purpose (waired-agent#801): it is the
# configuration whose runtime override used to be silently undone by every
# restart, so a host installed WITHOUT it cannot exercise the regression at
# all. It also makes the log-rotation assert further down read the debug cap
# unless it re-pins the level, which it does.
#
# WAIRED_NO_TRAY is deliberately NOT set any more (waired-agent#833). It used
# to be, which meant the macOS leg never executed a single line of the tray's
# install path -- the "launch it once; it then returns at every login" claim in
# the banner was describing a mechanism that did not exist, and no test on any
# OS could have noticed. The runner is disposable, so building the real
# /Applications/Waired.app on it is what a real install does.
inst_args=(--no-init --log-level debug)
inst_env=(WAIRED_INSTALL_BASE_URL="file://$DIST" WAIRED_NO_EMOJI=1)
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

# The Waired app (waired-agent#833). A bare Mach-O in /usr/local/bin is
# invisible to Spotlight, Launchpad and Login Items, which is why the owner's
# rc10 review could not find the tray anywhere and was told to run a terminal
# one-liner instead. The bundle is what makes it reachable at all.
WAIRED_APP="${WAIRED_DARWIN_APPDIR:-/Applications}/Waired.app"
[ -x "$WAIRED_APP/Contents/MacOS/waired-tray" ] \
  && ok "Waired.app installed ($WAIRED_APP)" \
  || bad "no executable at $WAIRED_APP/Contents/MacOS/waired-tray"
# LSUIElement is what makes it a menu-bar accessory instead of a Dock app, and
# the bundle id is what Login Items and launchd key off.
if grep -q 'LSUIElement' "$WAIRED_APP/Contents/Info.plist" 2>/dev/null \
   && grep -q 'ai.waired.tray' "$WAIRED_APP/Contents/Info.plist" 2>/dev/null; then
  ok "Waired.app declares LSUIElement + the bundle id"
else
  bad "Waired.app Info.plist is missing LSUIElement / ai.waired.tray"
fi
# One binary under two names, not two copies that can drift apart.
[ -L "$BINDIR/waired-tray" ] \
  && ok "$BINDIR/waired-tray is a symlink into the bundle" \
  || bad "$BINDIR/waired-tray is not a symlink (a second copy would drift)"
# Same Gatekeeper reasoning as the CLI above, on the file that actually gets
# double-clicked. Measured on macOS 26.6.2 (2026-08-21): an ad-hoc signed
# bundle with no quarantine xattr launches, the same bundle with the xattr set
# is refused. A quarantine xattr appearing here would mean the app cannot be
# opened at all.
if xattr -p com.apple.quarantine "$WAIRED_APP" >/dev/null 2>&1; then
  bad "Waired.app has com.apple.quarantine (would be Gatekeeper-blocked)"
else
  ok "Waired.app has no Gatekeeper quarantine xattr"
fi
# The banner has to describe what happened. A hosted runner has no Aqua
# session, so the honest outcome here is "not started", and the sentence this
# replaced ("launch it once; it then returns at every login") must be gone.
if grep -qF 'it then returns at every login' "$INSTALLLOG"; then
  bad "the banner still tells the user to launch the tray by hand (#833)"
else
  ok "the banner no longer claims a first-launch mechanism that does not exist"
fi

sudo test -f "$PLIST"         && ok "system LaunchDaemon plist written ($LABEL)" || bad "LaunchDaemon plist missing ($PLIST)"
sudo test -d "$STATE_DIR"     && ok "system state dir present"                   || bad "state dir missing ($STATE_DIR)"

# waired-agent#801: --log-level must NOT reach the plist's ProgramArguments.
# An agent flag there outranks agent.json at every boot, which is what made a
# runtime `waired config log-level` revert on every restart. The install-time
# level is a persisted setting now, so the three asserts here are "it did not
# land in the definition", "it did land in the daemon", and — the regression
# bar — "a runtime change survives a restart".
if sudo grep -q -- '--log-level' "$PLIST" 2>/dev/null; then
  bad "the LaunchDaemon plist still pins a log level; a runtime change will revert on the next restart (waired-agent#801)"
else
  ok "the LaunchDaemon plist pins no log level (waired-agent#801)"
fi

# it_wait_log_level echoes the daemon's answer once it is answering over its
# IPC socket, or nothing if it never does. `waired config log-level` prints
# "(persisted; waired-agent not running)" when it fell back to reading
# agent.json, which is the case that must not be mistaken for a live read.
it_wait_log_level() {
  _n=0
  while [ "$_n" -lt 30 ]; do
    _out="$(sudo "$BINDIR/waired" config log-level 2>/dev/null || true)"
    case "$_out" in
      *"not running"*) : ;;
      "Log level: "*) printf '%s' "$_out"; return 0 ;;
    esac
    _n=$((_n + 1))
    sleep 1
  done
  return 1
}

lvl_now="$(it_wait_log_level || true)"
case "$lvl_now" in
  "Log level: debug") ok "--log-level debug reached the daemon as the persisted level" ;;
  "") bad "the daemon never answered a log-level read, so the install-time level could not be checked" ;;
  *) bad "--log-level debug did not become the persisted level: [$lvl_now]" ;;
esac

# Use a third value — not the installed debug, not the built-in info — so
# neither "nothing changed" nor "fell back to the default" can pass.
sudo "$BINDIR/waired" config log-level warn >/dev/null 2>&1 || true
sudo launchctl kickstart -k "system/$LABEL" >/dev/null 2>&1 || true
lvl_after="$(it_wait_log_level || true)"
if [ "$lvl_after" = "Log level: warn" ]; then
  ok "a runtime log-level choice survives a service restart (waired-agent#801)"
else
  bad "a runtime log-level choice did not survive a restart: [$lvl_after] (waired-agent#801)"
fi
# Leave the host at the level the rest of the suite was written against.
sudo "$BINDIR/waired" config log-level debug >/dev/null 2>&1 || true


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
# The reinstall mirrors install.sh's darwin_register_agent exactly — that
# function has one branch now (waired-agent#801 removed the --log-level one),
# so this re-runs the real registration, not a lookalike.
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
      #
      # The other arm is `ok`, not `it_log`: it IS an assertion — that init
      # honours #310's exit-code contract on a host that never asked for an
      # engine. Counting it also keeps the assert-count floor stable, since
      # it_log moves no counter.
      if [ "$INFER" = 1 ]; then
        bad "waired init (authkey) enrolled but local AI is not running, and this tier asked for it — see $INITLOG"
        # Say what the daemon measured before init gave up. Without this the
        # arm ends with init's transcript and nothing else, which is how the
        # linux twin's #579 failures were unreadable (see the linux arm).
        hostspeed_evidence
      else
        ok "waired init (authkey) enrolled; local AI is not running here (expected: this tier did not ask for it)"
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
  out="$(_it_wait_enrolled_macos)" && enrolled=1
  if [ "$enrolled" = 1 ]; then
    ok "system daemon read the enrolled state and reports an identity"
  else
    bad "system daemon did not report enrolled"
    it_log "recent waired-agent log:"
    sudo log show --predicate 'process == "waired-agent"' --last 2m 2>/dev/null | tail -40 >&2 || true
    [ -f /Library/Logs/waired-agent.err.log ] && sudo tail -40 /Library/Logs/waired-agent.err.log >&2 || true
  fi

  # An opt-out is not a failed install (waired-agent#551). Lean leg only:
  # --inference and --daemon-engine leave an engine on the host, so
  # daemonWantsEngine would answer false and the probe would pass without
  # reaching the arm it exists to test.
  if [ "$INFER" != 1 ] && [ "$DAEMON_ENGINE" != 1 ]; then
    it_step "engine opt-out asserts (waired-agent#551)"
    assert_reinit_engine_optout_macos
    # The step-4 twin's other half and the models-pull twin
    # (waired-agent#590). Here for the same reason the opt-out probe is:
    # this host still has no engine, so every arm they test is reachable
    # — and for the pull twin, an engine-less host is what keeps the
    # honoured row from downloading anything.
    it_step "below-spec default asserts (waired-agent#590)"
    assert_reinit_default_unfit_macos
    it_step "models-pull confirmation asserts (waired-agent#590)"
    assert_models_pull_confirm_macos
  fi

  # Cheap and fast, so it runs before the minutes-long inference asserts.
  it_step "management write socket asserts (waired#838)"
  assert_mgmt_socket_macos

  # LAST of the engine-less probes, because it is the one that ends this
  # host's engine-less life: it installs one (waired-agent#590).
  if [ "$ENGINE_ONLY" = 1 ]; then
    it_step "engine installed, no model chosen (waired-agent#590)"
    assert_engine_only_install_macos
  fi

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

  # Pin the level first, and this line is load-bearing: the cap depends on it
  # since #658 (logrotate.PolicyForLevel: 32 MB at info, 128 MB at debug),
  # and this suite installs with --log-level debug (waired-agent#801), so the
  # host IS at the 128 MB cap until this re-pins it. Without this the filler
  # below would never cross the threshold and the assert would pass vacuously.
  waired config log-level info >/dev/null 2>&1 || true

  # Known archive state, then push the live file past the info cap the agent
  # rotates at (internal/platform/logrotate.DefaultPolicy = 32 MB).
  sudo rm -f "$err".* 2>/dev/null || true
  yes 'waired installtest rotation filler' | head -c 34000000 | sudo tee -a "$err" >/dev/null

  # The rotation ticker is 60s, and gzipping 32 MB takes a moment on top;
  # give it a margin.
  for _ in $(seq 1 120); do
    sudo test -f "$err.0.gz" && break
    sleep 1
  done
  if ! sudo test -f "$err.0.gz"; then
    bad "the daemon did not rotate $err within 120s of it passing its 32 MB cap (#331)"
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

# --- #680: --clean takes the Keychain-stored identity with it ---------------
# Runs LAST, and only at tier 2, because it is terminal (it removes the
# binaries and the state dir) and because the item it checks only exists once
# something has enrolled — LoadOrCreateMachineKey is what writes it, and tier 1
# installs with --no-init.
#
# The bug: on macOS the state dir is not the whole identity. securestore
# mirrors the Machine Key into the Keychain (the System keychain, since the
# daemon enrolls as root) and reads it back first, so `--clean` deleting the
# files left the host able to prove it was the same device. The control plane
# matches enrollment on the Machine Key, so a wiped and reinstalled host
# re-enrolled onto its old device row instead of a new one. Linux and Windows
# have no Keychain backend, so they never had the divergence — which is
# exactly why a suite that only ever ran `waired-agent uninstall` could not
# see it.
#
# Nothing here re-implements the deletion: the assert is on the observable
# end state of `uninstall.sh --clean`, so it stays true whichever way the
# script gets there.
if [ "$TIER" -ge 2 ]; then
  it_step "#680 --clean removes the Keychain-stored machine key"
  kc_before=0
  sudo security find-generic-password -a waired -s machine-key \
    /Library/Keychains/System.keychain >/dev/null 2>&1 && kc_before=1
  it_log "System keychain held the machine key before --clean: $kc_before"

  clean_rc=0
  sudo -E bash "$ROOT/packaging/install/uninstall.sh" --clean --yes >/dev/null 2>&1 || clean_rc=$?
  [ "$clean_rc" -eq 0 ] && ok "uninstall.sh --clean exited 0" \
    || bad "uninstall.sh --clean exited $clean_rc"

  # Proves the --clean arm actually ran, so the Keychain assert below cannot
  # pass by the script having done nothing.
  sudo test -d "$STATE_DIR" \
    && bad "state dir survived uninstall.sh --clean ($STATE_DIR)" \
    || ok "uninstall.sh --clean removed the state dir"

  # THE REGRESSION BAR.
  if sudo security find-generic-password -a waired -s machine-key \
       /Library/Keychains/System.keychain >/dev/null 2>&1; then
    bad "the machine key survived --clean in the System keychain (#680) — this host will re-enroll as the same device"
  else
    ok "no machine key left in the System keychain after --clean (#680)"
  fi
fi

echo
it_step "Tier $TIER summary: $PASS passed, $FAIL failed, $SKIP skipped"

# Assert-count floor (#215) — see the same block in installtest-run.sh for
# the rationale. Floors MEASURED from a green run of the leanest config:
# tier 1 = the binaries / Gatekeeper / plist / state-dir / launchd asserts
# plus the uninstall->reinstall round trip, tier 2 = those plus enrol and
# the mgmt-socket set. Raised by 3 in #331: the next-steps banner assert and
# the two log-rotation asserts.
#
# "Options only ever add asserts" stopped being true at waired-agent#551:
# assert_reinit_engine_optout_macos runs ONLY on the lean leg, because
# --inference and --daemon-engine leave an engine on the host and the probe
# would then pass without reaching the arm it tests. The floor still holds —
# it is measured from the leanest config, which is the one that runs the
# probe, and both richer options add far more than the 4 it contributes.
#
# waired-agent#590 makes that lean-only block 13 asserts rather than 4, which
# is more than a richer leg's own tail is guaranteed to make up. So the tier-2
# floor is PER CONFIGURATION now, the way installtest-windows.ps1 has kept its
# since #215: 35 was measured on the lean config and stays as the floor for
# the richer ones, and lean adds the 9 the two new probes contribute.
#
# --engine-only is a THIRD configuration: it does not set INFER, so it keeps
# the whole lean engine-less block above and adds its own six on top of it.
# 50 was derived that way (44 + 6) and then CONFIRMED — run 31316424716's
# --engine-only leg executed exactly 50, 0 failed. The derivation is sound
# only because assert_engine_only_install_macos contributes a fixed six
# whichever way each one lands (no early return, no conditional assert), so
# re-measure the moment that stops being true.
#
# #680 adds 3 to every tier-2 configuration (and none to tier 1): the
# --clean Keychain block runs unconditionally inside `[ "$TIER" -ge 2 ]`
# and contributes a fixed three whichever way each assert lands, so the
# derivation stays additive the way the --engine-only note above requires.
case "$TIER" in
  1) floor=24 ;;
  # 31 shared + the lean-only engine-less block:
  #   +4  assert_reinit_engine_optout_macos  (waired-agent#551)
  #   +4  assert_reinit_default_unfit_macos  (waired-agent#590)
  #   +5  assert_models_pull_confirm_macos   (waired-agent#590)
  # waired-agent#573's host-speed assert does NOT move these — it is soft while
  # waired-agent#579 is open, so it contributes 0 on the leg that hits that
  # case. See the Linux twin in installtest-run.sh.
  *) if [ "$INFER" = 1 ] || [ "$DAEMON_ENGINE" = 1 ]; then floor=38   # 35 + #680's 3
     elif [ "$ENGINE_ONLY" = 1 ]; then floor=53   # 47 + assert_engine_only_install_macos's 6
     else floor=47; fi ;;                          # 44 + #680's 3
esac
executed=$((PASS + FAIL))
if [ "$executed" -lt "$floor" ]; then
  printf '\033[1;31m[installtest] FAIL\033[0m only %d asserts ran at tier %s; at least %d must (a block stopped executing — see the assert-count floor in %s)\n' \
    "$executed" "$TIER" "$floor" "$(basename "$0")" >&2
  exit 1
fi

[ "$FAIL" -eq 0 ] || exit 1
