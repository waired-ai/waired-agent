# shellcheck shell=bash
# installtest-enroll.sh — Tier 2/3 helpers: enrol a guest into a Waired
# network and assert the result (the #335 state-dir chain, then a real
# overlay ping for Tier 3).
#
# Sourced by installtest-run.sh (--tier >=2). Relies on
# installtest-common.sh and on ok()/bad()/gx() defined by run.sh.
#
# Enrol modes (IT_ENROLL_MODE):
#
#   oidc — fully automated against the REAL production-like CP
#     (app.dev.waired.net) via the #339 SA-OIDC direct grant. The host mints a
#     Google-signed service-account id_token (gcloud impersonation of
#     IT_IMPERSONATE_SA, audience auto-discovered from the CP) and injects
#     it into the gcloud-less guest as `waired init --google-sa-login
#     --oidc-id-token <tok>`. Needs the minting identity to hold
#     roles/iam.serviceAccountTokenCreator on that SA (CP-side
#     oidc_grant_token_creators) — see docs/runbooks/oidc-grant-login.md.
#     This is the no-human path for real-CP inference verification.
#
#   bypass — fully automated, no human. Runs
#     `waired init --bypass-mode --bypass-email "$IT_BYPASS_EMAIL"`
#     against a --bypass-idp test Control Plane (IT_CONTROL_URL). Works
#     from this off-GCE LXD guest only if that endpoint is callable without
#     a GCE identity token (gcptoken injects a Bearer only on GCE — see
#     internal/gcptoken/transport.go). The deployed dev bypass CP is
#     IAM-gated (403 off-GCE), so prefer `oidc` for app.dev.waired.net.
#
#   interactive — manual one-off against the real OAuth CP
#     (app.dev.waired.net): `waired init --no-browser` prints a login URL you
#     open in a browser and sign in once per guest.
#
# All guests in one run share the same identity (IT_BYPASS_EMAIL for
# bypass, IT_IMPERSONATE_SA for oidc) so they land in the same network
# (required for the Tier-3 ping); device-name distinguishes them.
#
# IT_INFERENCE_ENABLED (default false): when true, init force-enables local
# inference so the deploy phase pulls the bundled model and runs the
# end-of-init benchmark (Tier-2 --inference; CPU is fine, no GPU needed).

IT_CONTROL_URL="${IT_CONTROL_URL:-https://app.dev.waired.net}"
IT_ENROLL_MODE="${IT_ENROLL_MODE:-bypass}"
IT_BYPASS_EMAIL="${IT_BYPASS_EMAIL:-}"
IT_IMPERSONATE_SA="${IT_IMPERSONATE_SA:-}"
IT_OIDC_AUDIENCE="${IT_OIDC_AUDIENCE:-}"
IT_INFERENCE_ENABLED="${IT_INFERENCE_ENABLED:-false}"

_it_dev_name() { printf '%s' "${1#"$IT_PREFIX"-}"; }

# it_enroll_guest <guest> — reproduce install.sh's real first-run enrol:
# `waired init` running as root *before* the daemon owns the identity, so
# it takes the local-enrolment path that writes identity as root and chowns
# the state dir to the service user (FixStateOwnership — the #335 chain).
# bypass-mode never auto-starts the agent and the headless guest has no
# tty for install.sh's own maybe_init, so we drive it explicitly:
# stop daemon -> init -> (re)start daemon on the enrolled, chowned state.
#
# Only for the two modes that select local enrolment explicitly (bypass /
# oidc). Since #175 `waired init` no longer infers that path from a failed
# daemon probe: with a systemd unit registered and the daemon stopped it
# fails with "the background service is installed but isn't responding"
# instead of enrolling locally. The interactive mode carries no such flag,
# so it keeps the daemon up and takes the daemon-driven journey — which is
# what a real interactive install does anyway (install.sh brings the
# service up before maybe_init).
it_enroll_guest() {
  local guest name initlog inf_flag
  guest="$1"
  name="$(_it_dev_name "$guest")"
  inf_flag="--inference-enabled=${IT_INFERENCE_ENABLED}"
  # Optional bundled-model pin (routing sentinel pins the tiny 0.5B so the
  # deploy pulls ~0.4 GB, not the hardware-selected 7B). Expands to zero args
  # when unset.
  local -a pin_flag=()
  [ -n "${IT_BUNDLED_MODEL_ID:-}" ] && pin_flag=("--inference-bundled-model-id=${IT_BUNDLED_MODEL_ID}")
  mkdir -p "$IT_LOGDIR"
  initlog="$IT_LOGDIR/init-$name.log"

  if [ "$IT_ENROLL_MODE" != interactive ]; then
    it_log "stopping waired-agent so init takes the local root-enrol path (#335)"
    gx "$guest" systemctl stop waired-agent 2>/dev/null || true
  else
    it_log "leaving waired-agent running: interactive enrol takes the daemon path (#175)"
  fi

  # Build the `waired init` argv per mode; run it once through tee so the
  # init transcript (model pull progress + benchmark) is captured for
  # assert_inference while still streaming to the run's stdout.
  local -a initargs
  case "$IT_ENROLL_MODE" in
    oidc)
      [ -n "$IT_IMPERSONATE_SA" ] || it_die \
        "IT_ENROLL_MODE=oidc needs IT_IMPERSONATE_SA (the #339 test SA, e.g. \
waired-devtest-login@dev-waired.iam.gserviceaccount.com)."
      command -v gcloud >/dev/null 2>&1 || it_die \
        "oidc enrol mints the SA id_token on the host; gcloud not found on PATH."
      local aud tok
      aud="$IT_OIDC_AUDIENCE"
      if [ -z "$aud" ]; then
        aud="$(curl -fsS --max-time 15 "$IT_CONTROL_URL/v1/login/oidc-grant/audience" 2>/dev/null \
          | sed -n 's/.*"audience":"\([^"]*\)".*/\1/p' || true)"
      fi
      [ -n "$aud" ] || it_die \
        "could not resolve the OIDC audience from $IT_CONTROL_URL/v1/login/oidc-grant/audience \
(is --enable-oidc-grant live on the CP?)"
      it_log "minting SA id_token on host (sa=$IT_IMPERSONATE_SA)"
      tok="$(gcloud auth print-identity-token \
        --impersonate-service-account="$IT_IMPERSONATE_SA" \
        --audiences="$aud" --include-email 2>/dev/null || true)"
      [ -n "$tok" ] || it_die \
        "failed to mint an SA id_token — is your identity in oidc_grant_token_creators \
on $IT_IMPERSONATE_SA? (roles/iam.serviceAccountTokenCreator)"
      it_log "enrolling $guest via OIDC grant (google-sa-login, host-minted token, cp=$IT_CONTROL_URL)"
      initargs=(waired init --control "$IT_CONTROL_URL"
        --google-sa-login --oidc-id-token "$tok"
        --device-name "$name" --non-interactive "$inf_flag" "${pin_flag[@]}"
        --skip-integration --state-dir /var/lib/waired)
      ;;
    bypass)
      [ -n "$IT_BYPASS_EMAIL" ] || it_die \
        "IT_ENROLL_MODE=bypass needs IT_BYPASS_EMAIL (the dev test account). \
Set IT_BYPASS_EMAIL + IT_CONTROL_URL to the --bypass-idp test endpoint, \
or use IT_ENROLL_MODE=oidc (real app.dev.waired.net) / interactive."
      it_log "enrolling $guest via bypass-idp (email=$IT_BYPASS_EMAIL, cp=$IT_CONTROL_URL)"
      initargs=(waired init --control "$IT_CONTROL_URL"
        --bypass-mode --bypass-email "$IT_BYPASS_EMAIL"
        --device-name "$name" --non-interactive "$inf_flag" "${pin_flag[@]}"
        --skip-integration --state-dir /var/lib/waired)
      ;;
    interactive)
      printf '\033[1;33m[installtest]\033[0m ===> %s needs a one-time Google sign-in.\n' "$guest" >&2
      printf '\033[1;33m[installtest]\033[0m ===> open the URL printed below (device: %s)\n' "$name" >&2
      initargs=(waired init --no-browser --control "$IT_CONTROL_URL"
        --device-name "$name" --non-interactive "$inf_flag" "${pin_flag[@]}"
        --skip-integration --state-dir /var/lib/waired)
      ;;
    *) it_die "unknown IT_ENROLL_MODE=$IT_ENROLL_MODE (want oidc|bypass|interactive)" ;;
  esac

  if ! gx "$guest" env WAIRED_NO_EMOJI=1 "${initargs[@]}" 2>&1 | tee "$initlog"; then
    it_die "waired init ($IT_ENROLL_MODE) failed in $guest — see $initlog"
  fi

  # Boot the daemon on the freshly enrolled + chowned state. bypass/oidc
  # enrolled with it stopped and never auto-start it; interactive enrolled
  # THROUGH it, and the restart re-reads the state it just wrote — which is
  # the property the #335 assertions below are about either way.
  gx "$guest" systemctl restart waired-agent
}

# Best-effort: revoke the device server-side so disposable CI guests don't
# pile up on the shared test account. `--revoke` is required: a plain logout
# leaves the device `reauth_required`, which still counts toward the #659
# per-account device cap (only `revoked` frees a slot). `--state-dir
# /var/lib/waired` targets the service identity (a bare invocation reads the
# per-user dir and no-ops), and `--yes` avoids the interactive prompt in the
# non-interactive guest. Call before deleting the guest.
it_logout_guest() {
  gx "$1" waired logout --revoke --yes --state-dir /var/lib/waired >/dev/null 2>&1 || true
}

# Poll the daemon's Management API until it reports an identity — proving
# the daemon read the enrolled, chowned state dir. We hit /waired/v1/status
# directly (the source of truth) rather than `waired status`, whose CLI
# state-dir resolution is a separate axis: a root `waired status` without
# --state-dir would read the per-user dir, not the service's /var/lib/waired,
# and falsely print "Not enrolled" even on an enrolled daemon.
_it_wait_enrolled() {
  local guest="$1" _ out
  for _ in $(seq 1 25); do
    out="$(gx "$guest" curl -fsS --max-time 5 http://127.0.0.1:9476/waired/v1/status 2>/dev/null || true)"
    if printf '%s' "$out" | grep -qE '"device_id"[[:space:]]*:[[:space:]]*"dev_'; then
      printf '%s' "$out"; return 0
    fi
    sleep 1
  done
  printf '%s' "$out"; return 1
}

# assert_mgmt_socket verifies the waired#838 write path end to end: mutating
# requests must travel over the local IPC socket and must NOT be accepted on
# the loopback TCP port, while reads stay on TCP.
#
# This assert is load-bearing because writeGuard fails OPEN when the socket
# is not bound — without it, a socket that never binds would silently
# degrade to the old TCP-write behaviour and nobody would notice.
assert_mgmt_socket() {
  local guest="$1" code out wbin
  gx "$guest" test -S /run/waired/mgmt.sock \
    && ok "management write socket present at /run/waired/mgmt.sock" \
    || bad "management write socket missing at /run/waired/mgmt.sock (RuntimeDirectory / bind failure)"

  # Positive: the CLI drives a mutating verb, which can only reach the
  # daemon over the socket. Resume restores the pre-assert phase.
  #
  # The EXIT CODE alone proves nothing: runPhaseTransition (cmd/waired/main.go)
  # treats an unreachable daemon as the documented fallback — it persists the
  # desired phase locally and returns 0 — and its isConnectionRefused() even
  # matches "no such file or directory", i.e. a MISSING socket. So assert on
  # stdout: "pause ok." is printed only on the daemon round-trip.
  # `|| true` because the driver runs `set -euo pipefail` and this is a BARE
  # assignment, so the shell's errexit applies to it: a non-zero `waired pause`
  # would abort the whole run mid-assert, with no summary line, no bad line,
  # and nothing naming which check was in flight (#215). The exit code is not
  # the assert here — the stdout below is, deliberately, because
  # runPhaseTransition returns 0 on the offline fallback path.
  #
  # Where this guard is NOT needed, and deliberately absent: errexit is
  # suppressed inside a function called from a CONDITION, so the assignments
  # in _it_wait_inference_ready (called as `if _it_wait_inference_ready ...`)
  # and in assert_inference (called as `[ "$INFER" = 1 ] && assert_inference`)
  # cannot abort anything. Adding `|| true` there would imply a hazard that
  # does not exist. Check the CALL SITE before adding one — and note that
  # moving such a function to a bare call re-arms the hazard for every bare
  # assignment inside it.
  #
  # `|| true` never changes the captured VALUE, only the substitution's exit
  # status, and every one of these is followed by a test on the value — so no
  # assert loses information; the ones above gain a reported failure where
  # they used to get a silent exit.
  out="$(gx "$guest" waired pause 2>&1 || true)"
  if printf '%s' "$out" | grep -q 'not running'; then
    bad "waired pause fell back to the offline desired-phase path (socket unreachable): $out"
  elif printf '%s' "$out" | grep -q 'pause ok\.'; then
    ok "waired pause reached the daemon over the local IPC socket"
  else
    bad "waired pause produced no daemon acknowledgement: $out"
  fi

  # The #838 premise itself: the daemon runs as the `waired` service user and
  # the socket is 0666 inside a 0755 runtime dir, so ANY local user must be
  # able to drive it — that is what a system-wide install requires, and it is
  # the reason peer-uid authorization was rejected. runuser (util-linux) is
  # used over sudo so this does not depend on a sudoers policy in the guest.
  # Bare assignment of a PIPELINE, so `set -o pipefail` makes a failing `gx`
  # abort the run even though `tr` succeeded — and "waired not on PATH" is a
  # perfectly ordinary answer here, handled by the `-n` test below.
  wbin="$(gx "$guest" sh -c 'command -v waired' 2>/dev/null | tr -d '\r' || true)"
  if [ -n "$wbin" ]; then
    # Same errexit hazard as the pause above, and more likely to fire: this
    # one is EXPECTED to fail on a host where the socket is unreachable, which
    # is precisely the regression it exists to report.
    out="$(gx "$guest" runuser -u nobody -- "$wbin" resume 2>&1 || true)"
    if printf '%s' "$out" | grep -q 'resume ok\.'; then
      ok "an unprivileged local user reaches the service-user daemon's socket (#838 premise)"
    else
      bad "unprivileged 'waired resume' did not reach the daemon: $out"
    fi
  else
    bad "could not locate the waired binary in the guest (command -v waired)"
  fi

  # Negative: the same mutating verb must be refused on the TCP port.
  code=$(gx "$guest" curl -s -o /dev/null -w '%{http_code}' -X POST \
    -H 'Content-Type: application/json' \
    http://127.0.0.1:9476/waired/v1/pause 2>/dev/null || true)
  case "$code" in
    2*) bad "TCP :9476 accepted a mutating write (HTTP $code); writeGuard not enforcing (waired#838)" ;;
    "") bad "TCP :9476 mutating-write probe produced no status code" ;;
    *)  ok "TCP :9476 refuses mutating writes (HTTP $code)" ;;
  esac

  # Reads deliberately stay on TCP.
  gx "$guest" curl -fsS --max-time 5 http://127.0.0.1:9476/waired/v1/status >/dev/null 2>&1 \
    && ok "TCP :9476 still serves reads" \
    || bad "TCP :9476 no longer serves reads"

  # Leave the daemon active regardless of which leg above failed, so a
  # failure here cannot cascade into unrelated asserts.
  gx "$guest" waired resume >/dev/null 2>&1 || true
}

assert_tier2() {
  local guest="$1" v out
  gx "$guest" test -f /var/lib/waired/identity.json \
    && ok "identity.json written under /var/lib/waired" \
    || bad "identity.json missing under /var/lib/waired"
  v=$(gx "$guest" stat -c '%U:%G' /var/lib/waired/identity.json 2>/dev/null || true)
  [ "$v" = "waired:waired" ] && ok "identity.json owned by waired:waired (#335 chain)" \
    || bad "identity.json owner = $v (want waired:waired — #335 regression)"
  if out="$(_it_wait_enrolled "$guest")"; then
    ok "daemon read the enrolled state and reports an identity"
  else
    bad "daemon did not report enrolled (can't read chowned state dir?)"
    printf '%s\n' "$out" | sed 's/^/    /' >&2
    gx "$guest" journalctl -u waired-agent --no-pager -n 20 2>&1 | sed 's/^/    /' || true
  fi
}

# Bundled engine path on Linux: install.sh now provisions waired's BUNDLED
# Ollama (`waired runtimes install ollama`, #567) under the state dir — it
# is NOT a system ollama on PATH, and it serves on the waired-owned port
# :9475, never the upstream default :11434.
IT_BUNDLED_OLLAMA_BIN=/var/lib/waired/runtimes/ollama/bin/ollama

# Failure lines `waired init` prints when an engine install did not succeed.
# A leg whose transcript contains one of these FAILED, whatever else it can
# still find on disk — #178 printed the exact reason into CI logs for five
# straight days while the leg said `ok  ollama engine installed`, because the
# primary assert stat'd an ollama binary that a half-finished install had
# already unpacked before signature verification failed.
#
# Kept as one alternation per harness rather than inline so
# scripts/ci/harness-failure-strings-guard.sh can check every branch of it
# still exists in the product source. Mirror any change in
# installtest-macos.sh and installtest-windows.ps1.
IT_INSTALL_FAILURE_RE='Engine install failed:|vLLM install failed:'

# _it_wait_inference_ready — poll the agent mgmt API's inference status
# until the bundled model is ready in the waired-owned engine, proving the
# install -> enroll -> engine-spawn -> model-pull tail ran. Mirrors
# _it_wait_enrolled. Since #364 the bundled engine pulls into its own :9475
# store (NOT the upstream default :11434), so readiness is read from
# /waired/v1/inference/status — the SAME source `waired init`'s #519
# foreground wait polls — never a bare `ollama list` against :11434 (always
# empty here; the original false negative, #564/#567). `waired init` already
# blocks until ready, so this is normally satisfied on the first probe; the
# budget absorbs the harness's post-init `systemctl restart` re-check tail.
# Echoes the last status JSON; returns 0 when ready, 1 on timeout / a
# terminal failure state.
_it_wait_inference_ready() {
  local guest="$1" _ out state
  for _ in $(seq 1 60); do          # ~5 min; CPU model pull is minutes-scale
    out="$(gx "$guest" curl -fsS --max-time 10 http://127.0.0.1:9476/waired/v1/inference/status 2>/dev/null || true)"
    if printf '%s' "$out" | grep -qE '"subsystem_state"[[:space:]]*:[[:space:]]*"ready"'; then
      printf '%s' "$out"; return 0
    fi
    # models.ready lists a loaded qwen/coder model (the only json:"ready" key).
    if printf '%s' "$out" | grep -oE '"ready"[[:space:]]*:[[:space:]]*\[[^]]*\]' | grep -qiE 'qwen|coder'; then
      printf '%s' "$out"; return 0
    fi
    # Bail early on a terminal state instead of burning the whole budget.
    # (no_engine/initializing are transient during engine cold start.)
    state="$(printf '%s' "$out" | grep -oE '"subsystem_state"[[:space:]]*:[[:space:]]*"[a-z_]+"' | head -1 | grep -oE '"[a-z_]+"$' | tr -d '"')"
    # engine_failed is terminal too (waired-agent#29): the engine crashed and
    # automatic recovery either is mid-flight (which shows as "starting") or has
    # given up. Either way, polling for "ready" will not fix it.
    case "$state" in pull_failed|disabled|stopped|engine_failed) printf '%s' "$out"; return 1 ;; esac
    sleep 5
  done
  printf '%s' "$out"; return 1
}

# assert_inference — verify the install→...→model-download→benchmark tail of
# the journey ran on CPU (Tier-2 --inference). install.sh provisioned the
# bundled engine (no --skip-ollama), `waired init --inference-enabled=true`
# started the agent and (via #519) foreground-waited while it pulled the
# bundled model into the :9475 engine, then ran the end-of-init benchmark.
# Proof points: bundled engine present, the model READY in the waired store
# (mgmt API on :9476, NOT a bare `ollama list` on :11434 — #567), inference
# enabled in persisted config, and a benchmark figure in the init transcript.
assert_inference() {
  local guest="$1" name initlog tps out model

  name="$(_it_dev_name "$guest")"
  initlog="$IT_LOGDIR/init-$name.log"

  # PRIMARY: init's own transcript. If it says the engine install failed, the
  # leg failed — no amount of on-disk evidence overrides the installer's own
  # verdict (#215/#178). Runs first so the reason is the first thing printed.
  if [ -f "$initlog" ] && grep -qE "$IT_INSTALL_FAILURE_RE" "$initlog"; then
    bad "init transcript reports an engine install failure ($initlog)"
    grep -nE "$IT_INSTALL_FAILURE_RE" "$initlog" | sed 's/^/    /' >&2 || true
  else
    ok "init transcript reports no engine install failure"
  fi

  # SECONDARY: the binary exists. Deliberately worded as presence, not as
  # success: a half-finished install leaves an unpacked binary behind, so this
  # line saying `ok` was exactly the #178 false positive.
  gx "$guest" test -x "$IT_BUNDLED_OLLAMA_BIN" \
    && ok "bundled ollama binary present ($IT_BUNDLED_OLLAMA_BIN, CPU)" \
    || bad "bundled ollama not installed at $IT_BUNDLED_OLLAMA_BIN (install.sh should have, without --skip-ollama)"

  # #567: the bundled engine is waired-owned on :9475 with its own store; the
  # agent pulls there, NOT into the upstream default :11434. Read readiness
  # from the mgmt API and poll until ready, never a bare `ollama list` (:11434,
  # always empty here, the original false negative).
  if out="$(_it_wait_inference_ready "$guest")"; then
    model="$(printf '%s' "$out" | grep -oE '"ready"[[:space:]]*:[[:space:]]*\[[^]]*\]' | grep -oiE '(qwen|coder)[^",]*' | head -1)"
    ok "bundled model ready in waired store :9475 (${model:-ready}; via mgmt API)"
  else
    bad "bundled model not ready via mgmt API (deploy/pull failed?)"
    printf '%s\n' "$out" | sed 's/^/    /' >&2
    # Diagnostics from the RIGHT store (:9475), using the bundled binary.
    gx "$guest" sh -c "OLLAMA_HOST=127.0.0.1:9475 '$IT_BUNDLED_OLLAMA_BIN' list" 2>&1 | sed 's/^/    :9475 /' || true
    gx "$guest" journalctl -u waired-agent --no-pager -n 30 2>&1 | sed 's/^/    /' || true
    # #22: the agent captures `ollama serve`'s own stdout+stderr here, so an
    # engine startup crash leaves its REAL reason in this log — journalctl
    # only shows the agent's "not ready" wrapper. (gx runs as root in the
    # guest, same as the journalctl read above.)
    gx "$guest" sh -c 'tail -n 60 /var/lib/waired/runtimes/ollama/logs/engine.log 2>/dev/null || echo "(no engine.log)"' 2>&1 | sed 's/^/    engine.log| /' || true
  fi

  if gx "$guest" sh -c 'grep -hqsE "\"enabled\" *: *true" /var/lib/waired/*.json' 2>/dev/null; then
    ok "inference enabled in persisted agent config"
  else
    bad "inference not enabled in persisted config"
  fi

  # The end-of-init benchmark (offerBenchmark, non-bypass) must report a
  # THROUGHPUT NUMBER.
  #
  # This assert used to also accept the bare "Local inference works" line, on
  # the theory that a host too slow to measure a stable rate reports
  # MeasuredTokps=0 yet still ran a real generation. But a benchmark whose
  # warm-up got an engine 500 printed exactly that same line, so this assert
  # passed 13 seconds before the routing sentinel found a dead engine
  # (waired-agent#29). A current daemon now 503s a failed run and the CLI
  # prints no success line at all, so requiring the number is both correct and
  # achievable: with at least one valid sample inside the measurement budget
  # the median is always a number, and total failure is a 503.
  #
  # `|| true`: a no-match grep exits 1 and would trip `set -e` in the sourcing
  # driver; head-closing a multi-match grep (SIGPIPE 141) would too.
  tps=""
  [ -f "$initlog" ] && tps="$(grep -ioE '[0-9]+(\.[0-9]+)? *(tok|tokens)/s' "$initlog" | head -1 || true)"
  if [ -n "$tps" ]; then
    ok "benchmark ran during init ($tps)"
  else
    bad "no benchmark THROUGHPUT figure in init transcript ($initlog)"
    grep -iE 'benchmark|inference|engine' "$initlog" 2>/dev/null | tail -20 | sed 's/^/    init| /' || true
    # Surface the daemon's own boot benchmark slog and the engine's log — a
    # failed benchmark is usually the engine's fault, and engine.log is where
    # it says so.
    gx "$guest" sh -c 'journalctl -u waired-agent --no-pager -n 200 | grep -iE "boot benchmark|benchmark" | tail -15' 2>&1 | sed 's/^/    agent| /' || true
    gx "$guest" sh -c 'tail -n 60 /var/lib/waired/runtimes/ollama/logs/engine.log 2>/dev/null || echo "(no engine.log)"' 2>&1 | sed 's/^/    engine.log| /' || true
  fi
}

# Tier 3: both guests enrolled to the same account, on real kernels — ping
# over the real overlay each way (mirrors full-e2e.sh's ping but through
# the full installer + real relay/NAT traversal).
assert_tier3_ping() {
  local a="$1" b="$2" na nb _
  na="$(_it_dev_name "$a")"; nb="$(_it_dev_name "$b")"
  it_log "waiting for the network map to list both peers"
  for _ in $(seq 1 30); do
    lxc exec "$a" -- waired status 2>/dev/null | grep -qi "$nb" && break
    sleep 2
  done
  if lxc exec "$a" -- waired ping "$nb" >/dev/null 2>&1; then
    ok "overlay ping $na -> $nb"
  else
    bad "overlay ping $na -> $nb failed"
    lxc exec "$a" -- waired status 2>&1 | sed 's/^/    /' || true
  fi
  if lxc exec "$b" -- waired ping "$na" >/dev/null 2>&1; then
    ok "overlay ping $nb -> $na"
  else
    bad "overlay ping $nb -> $na failed"
  fi
}
