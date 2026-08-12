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
#   authkey — fully automated against the REAL production-like CP
#     (app.dev.waired.net), and the mode CI uses. The host mints a
#     Google-signed service-account id_token exactly as before (gcloud
#     impersonation of IT_IMPERSONATE_SA, audience auto-discovered from the
#     CP), but now exchanges it ONCE for a reusable auth key at
#     POST {cp}/test/auth-key, and the gcloud-less guest runs
#     `waired init --auth-key <key>` with the daemon UP. Needs the minting
#     identity to hold roles/iam.serviceAccountTokenCreator on that SA
#     (CP-side oidc_grant_token_creators) — see
#     docs/runbooks/oidc-grant-login.md.
#
#     This replaces the old `oidc` mode, which drove
#     `waired init --google-sa-login` down the LOCAL enrolment path with the
#     daemon stopped. That path is what waired-agent#175 is removing: it
#     registers a device whose capabilities the control plane never learns.
#     The key is a credential the DAEMON redeems, so the leg now exercises
#     the same journey a real headless install takes.
#
#   interactive — manual one-off against the real OAuth CP
#     (app.dev.waired.net): `waired init --no-browser` prints a login URL you
#     open in a browser and sign in once per guest.
#
# All guests in one run share the same identity (IT_IMPERSONATE_SA) so
# they land in the same network
# (required for the Tier-3 ping); device-name distinguishes them. The
# authkey mode mints ONE reusable key per run and hands it to every guest,
# which is both the fleet case a real operator has and the cheapest way to
# guarantee one network.
#
# IT_INFERENCE_ENABLED (default false): when true, init force-enables local
# inference so the deploy phase pulls the bundled model and runs the
# end-of-init benchmark (Tier-2 --inference; CPU is fine, no GPU needed).

IT_CONTROL_URL="${IT_CONTROL_URL:-https://app.dev.waired.net}"
IT_ENROLL_MODE="${IT_ENROLL_MODE:-authkey}"
# Run-scoped auth key, minted lazily by _it_mint_auth_key on first use.
IT_AUTH_KEY="${IT_AUTH_KEY:-}"
IT_IMPERSONATE_SA="${IT_IMPERSONATE_SA:-}"
IT_OIDC_AUDIENCE="${IT_OIDC_AUDIENCE:-}"
IT_INFERENCE_ENABLED="${IT_INFERENCE_ENABLED:-false}"

_it_dev_name() { printf '%s' "${1#"$IT_PREFIX"-}"; }

# _it_mint_auth_key sets IT_AUTH_KEY once per run.
#
# CI has no browser, so it cannot use the console's key issuer. It mints a
# Google-signed SA id_token on the HOST (the guests have no gcloud) and
# exchanges it at the CP's dev-only issuer, which verifies the token with
# the same real Google verifier the browser callback uses and accepts only
# allowlisted identities (waired#976, authkey_dev.go). The key is reusable
# so every guest in the run lands in one network — the Tier-3 ping needs
# them to see each other.
_it_mint_auth_key() {
  [ -n "$IT_AUTH_KEY" ] && return 0

  [ -n "$IT_IMPERSONATE_SA" ] || it_die \
    "IT_ENROLL_MODE=authkey needs IT_IMPERSONATE_SA (the #339 test SA, e.g. \
waired-devtest-login@dev-waired.iam.gserviceaccount.com)."
  command -v gcloud >/dev/null 2>&1 || it_die \
    "authkey enrol mints the SA id_token on the host; gcloud not found on PATH."

  # `|| true` on the three substitutions below: this function is reached
  # through a BARE call (it_enroll_guest, from installtest-run.sh), so the
  # driver's `set -euo pipefail` applies to every assignment in it. Without
  # the guard an unreachable CP or a failed gcloud aborts the whole run at
  # the assignment — before the `it_die` on the next line ever gets to say
  # which step failed, and with no FAIL line and no summary (#215). The
  # guard changes only the substitution's exit status, never the captured
  # value, and each one is followed by a test on that value.
  #
  # Deliberately NOT guarded: the IT_AUTH_KEY extraction below is
  # `printf | sed`, neither of which can fail — a no-match `sed -n .../p`
  # exits 0. Adding one there would imply a hazard that does not exist.
  local aud tok resp
  aud="$IT_OIDC_AUDIENCE"
  if [ -z "$aud" ]; then
    aud="$(curl -fsS --max-time 15 "$IT_CONTROL_URL/v1/login/oidc-grant/audience" 2>/dev/null \
      | sed -n 's/.*"audience":"\([^"]*\)".*/\1/p' || true)"
  fi
  [ -n "$aud" ] || it_die \
    "could not resolve the OIDC audience from $IT_CONTROL_URL/v1/login/oidc-grant/audience \
(is --enable-oidc-grant live on the CP?)"

  it_log "minting SA id_token on host (sa=$IT_IMPERSONATE_SA)"
  # gcloud's stderr goes to a file rather than /dev/null: it carries the ONLY
  # statement of why impersonation failed (wrong audience, missing
  # tokenCreator, an IAM propagation delay, a 429), and swallowing it leaves
  # the guess below as the whole diagnosis. stdout — the token — is captured
  # separately and never printed.
  local gerr
  gerr="$(mktemp)"
  tok="$(gcloud auth print-identity-token \
    --impersonate-service-account="$IT_IMPERSONATE_SA" \
    --audiences="$aud" --include-email 2>"$gerr" || true)"
  if [ -z "$tok" ]; then
    printf '\033[1;31m[installtest]\033[0m gcloud said:\n' >&2
    sed 's/^/    /' "$gerr" >&2 || true
    rm -f "$gerr"
    it_die "failed to mint an SA id_token for $IT_IMPERSONATE_SA — see gcloud's \
own error above (a missing roles/iam.serviceAccountTokenCreator on that SA is \
the usual cause; see docs/runbooks/oidc-grant-login.md)"
  fi
  rm -f "$gerr"

  it_log "exchanging the id_token for a reusable auth key ($IT_CONTROL_URL/test/auth-key)"
  # --data @- keeps the token off the process's argv.
  resp="$(printf '{"id_token":"%s","reusable":true,"description":"installtest %s"}' \
    "$tok" "$IT_PREFIX" \
    | curl -fsS --max-time 30 -X POST "$IT_CONTROL_URL/test/auth-key" \
        -H 'Content-Type: application/json' --data @- 2>/dev/null || true)"
  IT_AUTH_KEY="$(printf '%s' "$resp" | sed -n 's/.*"auth_key":"\([^"]*\)".*/\1/p')"
  [ -n "$IT_AUTH_KEY" ] || it_die \
    "could not mint an auth key at $IT_CONTROL_URL/test/auth-key \
(is the CP new enough — waired#976 — and started with --enable-oidc-grant?)"
  # Never print the key: these logs are archived as CI artifacts.
  it_log "auth key minted (reusable, one per run)"
}

# it_enroll_guest <guest> — reproduce install.sh's real first-run enrol.
# The headless guest has no tty for install.sh's own maybe_init, so we
# drive it explicitly: init -> restart the daemon on the enrolled state.
#
# The daemon stays UP throughout. Since #175 `waired init` drives
# the running agent and fails loudly when nothing answers, so stopping the
# service would fail the run rather than quietly enrolling locally — and
# keeping it up is what a real install does anyway (install.sh brings the
# service up before maybe_init).
it_enroll_guest() {
  local guest name initlog inf_flag
  guest="$1"
  name="$(_it_dev_name "$guest")"
  inf_flag="--inference-enabled=${IT_INFERENCE_ENABLED}"
  # Optional bundled-model pin (routing sentinel pins the withheld 350M so the
  # deploy pulls ~0.7 GB, not the hardware-selected 7B). Expands to zero args
  # when unset.
  local -a pin_flag=()
  [ -n "${IT_BUNDLED_MODEL_ID:-}" ] && pin_flag=("--inference-bundled-model-id=${IT_BUNDLED_MODEL_ID}")
  mkdir -p "$IT_LOGDIR"
  initlog="$IT_LOGDIR/init-$name.log"

  it_log "leaving waired-agent running: $IT_ENROLL_MODE enrols through the daemon (#175)"

  # Coding-agent integration. A real install NEVER passes
  # --skip-integration, and this harness passed it on every leg — which is
  # how #294 survived: the flag suppresses the whole integration, Claude
  # routing included, so the e2e that drives the real installer could not
  # see that a real install finished unrouted. Default off, so the leg
  # exercises what an operator actually gets; IT_SKIP_INTEGRATION=1 opts
  # back out for a leg that wants nothing written outside the state dir.
  local -a integ_flag=()
  [ "${IT_SKIP_INTEGRATION:-0}" = 1 ] && integ_flag=(--skip-integration)

  # Build the `waired init` argv per mode; run it once through tee so the
  # init transcript (model pull progress + benchmark) is captured for
  # assert_inference while still streaming to the run's stdout.
  local -a initargs
  case "$IT_ENROLL_MODE" in
    authkey)
      _it_mint_auth_key
      it_log "enrolling $guest with an auth key through the daemon (cp=$IT_CONTROL_URL)"
      initargs=(waired init --control "$IT_CONTROL_URL"
        --auth-key "$IT_AUTH_KEY"
        --device-name "$name" --non-interactive "$inf_flag" "${pin_flag[@]}"
        "${integ_flag[@]}" --state-dir /var/lib/waired)
      ;;
    interactive)
      printf '\033[1;33m[installtest]\033[0m ===> %s needs a one-time Google sign-in.\n' "$guest" >&2
      printf '\033[1;33m[installtest]\033[0m ===> open the URL printed below (device: %s)\n' "$name" >&2
      initargs=(waired init --no-browser --control "$IT_CONTROL_URL"
        --device-name "$name" --non-interactive "$inf_flag" "${pin_flag[@]}"
        "${integ_flag[@]}" --state-dir /var/lib/waired)
      ;;
    *) it_die "unknown IT_ENROLL_MODE=$IT_ENROLL_MODE (want authkey|interactive)" ;;
  esac

  # `waired init` has three outcomes, not two (#310): 0 signed in, 3 signed
  # in but this guest has no local AI, anything else failed. Collapsing 3
  # into "failed" would fail a guest that enrolled perfectly — which is the
  # DEFAULT here, since IT_INFERENCE_ENABLED is false unless a tier asks for
  # inference. `set -o pipefail` is on, so this reads init's status, not tee's.
  init_rc=0
  gx "$guest" env WAIRED_NO_EMOJI=1 "${initargs[@]}" 2>&1 | tee "$initlog" || init_rc=$?
  case "$init_rc" in
    0) ;;
    3)
      # A tier that asked for local inference and did not get it IS a
      # failure: that is the thing that tier exists to verify.
      if [ "$IT_INFERENCE_ENABLED" = true ]; then
        # Say what the daemon measured before dying. This arm is where the
        # inference leg actually ends when local AI does not come up, and it
        # used to end with nothing but init's transcript — no daemon log, no
        # status payload — which is why "did the host-speed measurement
        # complete?" was unanswerable from a finished run (#579).
        it_hostspeed_evidence "$guest"
        it_die "waired init enrolled $guest but local AI is not running, and this tier asked for it — see $initlog"
      fi
      it_log "waired init enrolled $guest; local AI is not running there (expected: IT_INFERENCE_ENABLED=$IT_INFERENCE_ENABLED)"
      ;;
    *) it_die "waired init ($IT_ENROLL_MODE) failed in $guest with exit $init_rc — see $initlog" ;;
  esac

  # Restart the daemon on the freshly enrolled + chowned state: it enrolled
  # THROUGH the daemon, and the restart makes it re-read the state it just
  # wrote — the property the #335 assertions below are about.
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

  # The compatibility reads stay on TCP: this is the route production
  # install.sh, this suite, and the testnet fallback scripts all poll.
  gx "$guest" curl -fsS --max-time 5 http://127.0.0.1:9476/waired/v1/status >/dev/null 2>&1 \
    && ok "TCP :9476 still serves the compatibility reads" \
    || bad "TCP :9476 no longer serves /status (waired#836 allow-list)"

  # Everything else must have moved to the socket (waired#836).
  code=$(gx "$guest" curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    http://127.0.0.1:9476/waired/v1/identity 2>/dev/null || true)
  case "$code" in
    2*) bad "TCP :9476 served /identity (HTTP $code); readGuard not enforcing (waired#836)" ;;
    "") bad "TCP :9476 unlisted-read probe produced no status code" ;;
    *)  ok "TCP :9476 refuses reads outside the allow-list (HTTP $code)" ;;
  esac

  # ...and must still be readable over the socket, or the read simply broke
  # rather than moved. curl --unix-socket needs an authority; the socket
  # handler carries no browserGuard, so any dummy will do.
  gx "$guest" curl -fsS --max-time 5 --unix-socket /run/waired/mgmt.sock \
    http://waired-mgmt/waired/v1/identity >/dev/null 2>&1 \
    && ok "the management socket serves /identity" \
    || bad "the management socket does not serve /identity; the read moved nowhere (waired#836)"

  # The #836 browser hardening itself. Nothing else in this suite exercises
  # it: browserGuard is OFF by default in the unit tests, so flipping
  # --mgmt-hardening would leave every Go test green.
  code=$(gx "$guest" curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -H 'Host: evil.example' http://127.0.0.1:9476/waired/v1/status 2>/dev/null || true)
  case "$code" in
    2*) bad "TCP :9476 answered an attacker Host (HTTP $code); browserGuard not enforcing (waired#836)" ;;
    "") bad "TCP :9476 Host probe produced no status code" ;;
    *)  ok "TCP :9476 rejects a non-loopback Host (HTTP $code)" ;;
  esac

  # A cross-site POST that skips the CORS preflight must be refused on the
  # Content-Type, before the route's own method or body handling.
  code=$(gx "$guest" curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST \
    -H 'Content-Type: text/plain' --data '{"peer":"x"}' \
    http://127.0.0.1:9476/waired/v1/ping 2>/dev/null || true)
  if [ "$code" = "415" ]; then
    ok "TCP :9476 requires application/json on writes (HTTP 415)"
  else
    bad "POST /ping with text/plain returned HTTP ${code:-none}, want 415 (waired#836)"
  fi

  # Leave the daemon active regardless of which leg above failed, so a
  # failure here cannot cascade into unrelated asserts.
  gx "$guest" waired resume >/dev/null 2>&1 || true
}

# IT_CLAUDE_MANAGED_SETTINGS is the machine-wide Claude Code
# managed-settings file on Linux (claudemanaged.managedSettingsPath()), and
# IT_CLAUDE_GATEWAY the loopback base URL init writes into it (the default
# Inference.ClaudeGatewayPort). Kept as named constants so a port or path
# change shows up as one edit here rather than a silently-passing grep.
IT_CLAUDE_MANAGED_SETTINGS=/etc/claude-code/managed-settings.json
IT_CLAUDE_GATEWAY='http://127.0.0.1:9472'

# assert_claude_route <guest> — where a real install leaves Claude Code.
#
# The gap this closes: `waired init` is the single decider of routing (the
# installers deleted their own post-init `waired claude enable` and forward
# --skip-claude-route into init instead), but only the deleted standalone
# enrollment path ever wrote the file. Every real install takes the daemon
# path, so every real install finished with Claude Code still talking to
# the Anthropic API — and no e2e noticed, because the routing sentinel
# drives the gateway on :9472 directly rather than reading what Claude Code
# was actually configured to use (#294).
#
# Asserts in BOTH directions, and always exactly two asserts, so the
# assert-count floor in installtest-run.sh holds whichever way the leg was
# configured: routing must happen on a normal install, and --skip-integration
# must leave Claude Code alone.
assert_claude_route() {
  local guest="$1" body present=0 pointed=0 want=1 label='a normal install must route Claude'
  if [ "${IT_SKIP_INTEGRATION:-0}" = 1 ]; then
    want=0
    label='--skip-integration must leave Claude Code alone'
  fi

  gx "$guest" test -f "$IT_CLAUDE_MANAGED_SETTINGS" && present=1
  body="$(gx "$guest" cat "$IT_CLAUDE_MANAGED_SETTINGS" 2>/dev/null || true)"
  # The file existing is not the point — an ANTHROPIC_BASE_URL pointing
  # somewhere else (or absent) leaves Claude Code on the real API just as
  # surely as no file at all.
  printf '%s' "$body" | grep -q "$IT_CLAUDE_GATEWAY" && pointed=1

  if [ "$present" = "$want" ]; then
    ok "Claude Code managed settings present=$present ($label)"
  else
    bad "Claude Code managed settings present=$present, want $want — $label ($IT_CLAUDE_MANAGED_SETTINGS)"
  fi
  if [ "$pointed" = "$want" ]; then
    ok "ANTHROPIC_BASE_URL -> $IT_CLAUDE_GATEWAY: $pointed ($label)"
  else
    bad "ANTHROPIC_BASE_URL -> $IT_CLAUDE_GATEWAY: $pointed, want $want — $label"
    printf '%s\n' "$body" | sed 's/^/    managed-settings| /' >&2
  fi
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

# assert_reinit_resumes: `waired init` on a device that is already signed
# in must resume the setup, not fail (waired-agent#313).
#
# The leg is defined by what it does NOT pass: no --state-dir, the way an
# operator types it and the way NAVI prescribes it for a stuck setup. On
# Windows that combination failed on every enrolled device — the CLI
# resolved a per-user dir the daemon does not use, found no identity,
# asked for a plain login, and reported the daemon's idempotent no-op as
# "daemon did not return a login session id". Here it is the parity bar:
# root resolves the same /var/lib/waired the daemon reads, so this leg
# pins the resume contract rather than reproducing the Windows defect.
#
# The auth key is deliberately still passed: an already-signed-in device
# must not spend it (the `tailscale up` rule), and must say so.
# Exactly three asserts, always — the tier-2 floor counts on it.
assert_reinit_resumes() {
  local guest="$1" name log rc=0
  name="$(_it_dev_name "$guest")"
  mkdir -p "$IT_LOGDIR"
  log="$IT_LOGDIR/reinit-$name.log"

  # The same --inference-enabled the enrol used, and it is load-bearing
  # rather than tidy: this leg must leave the host's local-AI posture
  # exactly as it found it, in EITHER direction.
  #
  # Passing nothing leaves the toggle unset, which hands the decision to
  # install-flow step 6 — and GitHub-hosted runners are genuinely below
  # the recommended spec. Measured on the routing sentinel's runner:
  # 81.9 tok/s of prefill, a 256 s lower bound on one coding question
  # against a 45 s budget. Step 6 then turns local AI off, correctly, and
  # the "local inference is on" assert later in the same run fails for a
  # reason that has nothing to do with #313. That only began when init
  # learned to READ that bound (waired-agent#579 Stage 3c); before, the
  # figure was published and step 6 could not see it. The behaviour is
  # the fix, not the bug.
  #
  # Passing a bare `true` is equally wrong, and in the other direction: on
  # the lean tier-2 guest it would INSTALL an engine, and the two legs
  # after this one (assert_reinit_engine_optout, #551; and
  # assert_reinit_default_unfit, #590) both need a host that wants
  # inference and HAS NO ENGINE. Their preconditions are this leg's
  # postconditions.
  #
  # Mirroring the enrol satisfies both: --inference-enabled=true on a tier
  # that asked for inference, =false where it did not, and never the unset
  # toggle that makes the outcome depend on how fast a runner happens to
  # be. The below-spec default has its own leg (#590) which FORCES the
  # verdict rather than depending on which machine CI bought; this leg
  # must not become a second, accidental copy of it.
  local -a args=(waired init --control "$IT_CONTROL_URL" --device-name "$name"
    "--inference-enabled=${IT_INFERENCE_ENABLED}" --non-interactive --skip-integration)
  # Keyed on the key itself, not the mode: the daemon-path leg
  # (--daemon-engine) runs under authkey too but deliberately never mints
  # one, and an empty --auth-key is not the same argv.
  [ -n "${IT_AUTH_KEY:-}" ] && args+=(--auth-key "$IT_AUTH_KEY")

  it_log "re-running waired init in $guest with no --state-dir (waired-agent#313)"
  gx "$guest" env WAIRED_NO_EMOJI=1 "${args[@]}" >"$log" 2>&1 || rc=$?
  [ "$rc" = 0 ] && ok "re-init on an enrolled device exits 0 (no --state-dir)" \
    || bad "re-init exited $rc — see $log"
  grep -q 'resuming setup' "$log" \
    && ok "re-init resumes setup instead of starting a sign-in" \
    || bad "re-init did not resume — see $log"
  if [ -n "${IT_AUTH_KEY:-}" ]; then
    grep -q 'auth key was not used' "$log" \
      && ok "re-init says the auth key went unused" \
      || bad "re-init spent or silently dropped the auth key — see $log"
  else
    ok "no auth key to leave unused ($IT_ENROLL_MODE enrol)"
  fi
  [ "$rc" = 0 ] || tail -n 20 "$log" | sed 's/^/    /' >&2
}

# assert_reinit_engine_optout: on a host where engine installs are turned
# off, `waired init` must not report the operator's own instruction back to
# them as a failed install (waired-agent#551).
#
# The arm under test is installEngineAsExecutor's engineActionSkipOptOut. It
# needs three things true AT ONCE — the host wants inference, it has no
# engine, and WAIRED_NO_OLLAMA is set — and until #551 the only place in all
# of CI where that happened was the Windows daemon-path leg, by accident: a
# `$env:` assignment there outlives the command that made it, so the leg told
# the executor not to install the engine the leg exists to prove it installs.
# That is fixed in installtest-windows.ps1, which is why this exists: without
# it, fixing the leak would have deleted the last runtime coverage of an arm
# whose exit status this very issue changes.
#
# Here it is deliberate and cheap. The lean tier-2 guest is already enrolled
# with no engine, so the whole probe is one extra init that downloads nothing.
# --inference-enabled=true is what makes it non-vacuous: without it the daemon
# answers `disabled`, daemonWantsEngine returns false, and the executor never
# reaches the decision — the leg would go green having tested nothing, which
# is the #178/#215 shape. Assert 2 is what proves that did not happen.
#
# Exactly four asserts, always — the tier-2 floor counts on it.
assert_reinit_engine_optout() {
  local guest="$1" name log rc=0
  name="$(_it_dev_name "$guest")"
  mkdir -p "$IT_LOGDIR"
  log="$IT_LOGDIR/reinit-optout-$name.log"

  it_log "re-running waired init in $guest with engine installs turned off (waired-agent#551)"
  gx "$guest" env WAIRED_NO_EMOJI=1 WAIRED_NO_OLLAMA=1 waired init \
    --control "$IT_CONTROL_URL" --device-name "$name" \
    --inference-enabled=true --non-interactive --skip-integration \
    >"$log" 2>&1 || rc=$?

  [ "$rc" = 0 ] \
    && ok "re-init with engine installs turned off exits 0 (waired-agent#551)" \
    || bad "re-init exited $rc with WAIRED_NO_OLLAMA set — an opt-out the operator configured is not a failed install — see $log"
  grep -q "$IT_ENGINE_OPTOUT_RE" "$log" \
    && ok "the executor reached the opt-out arm and said so" \
    || bad "init never reported the engine install as skipped — the opt-out arm was not reached, so the asserts around it prove nothing — see $log"
  grep -q "$IT_INSTALL_FAILURE_BOX_RE" "$log" \
    && bad "init called the operator's own opt-out a failed install — see $log" \
    || ok "init does not report the opt-out as a failed install"
  gx "$guest" test -x "$IT_BUNDLED_OLLAMA_BIN" \
    && bad "an engine was installed at $IT_BUNDLED_OLLAMA_BIN despite WAIRED_NO_OLLAMA" \
    || ok "no engine was installed while the opt-out was set"

  [ "$rc" = 0 ] || tail -n 20 "$log" | sed 's/^/    /' >&2
  # Leave the guest as we found it. The asserts after this one were measured
  # against a host with inference off, and `waired inference off` is the only
  # way back: :9476 refuses mutating writes over TCP (waired#838).
  gx "$guest" waired inference off >/dev/null 2>&1 || \
    it_warn "could not turn inference back off in $guest after the #551 probe"
}

# assert_reinit_default_unfit: the OTHER half of the step-4 twin
# (waired-agent#590; the waired#1067 rule "explicit flag > non-interactive
# default > interactive ask", waired-agent#584). On a host below the
# recommended spec, a non-interactive init with NO inference flag must END
# with local AI off, exit 0, and the skip note — a choice, not a fault
# (waired-agent#551's exit discipline; distinct from the #569/#576 exit-3
# contract). The measured deduction (waired-agent#568) is what makes the
# host below-spec DETERMINISTICALLY: WAIRED_RAM_AVAILABLE_GB=1 makes the
# fit verdicts read "nothing fits" whatever the runner's real memory, so
# the probe never depends on which machine class CI happened to buy.
#
# Non-vacuous the same way the #551 probe above is: the flagless run only
# reaches the ask when the daemon still wants an engine, so the probe
# turns inference back on first (the #551 probe left it off). Cleanup
# restores both the seam and the toggle, so the asserts after this one
# meet the guest exactly as the #551 probe left it.
assert_reinit_default_unfit() {
  local guest="$1" name log rc=0
  name="$(_it_dev_name "$guest")"
  mkdir -p "$IT_LOGDIR"
  log="$IT_LOGDIR/reinit-default-unfit-$name.log"

  it_log "re-running waired init in $guest with no inference flag on a forced below-spec host (waired-agent#590)"
  # Arrange what can be arranged: desired enabled, and a below-spec
  # measurement. "No engine installed" is the third thing this probe needs
  # and the one nothing here establishes — see _it_force_below_spec.
  _it_force_below_spec "$guest" "#590 default"

  gx "$guest" env WAIRED_NO_EMOJI=1 waired init \
    --control "$IT_CONTROL_URL" --device-name "$name" \
    --non-interactive --skip-integration \
    >"$log" 2>&1 || rc=$?

  [ "$rc" = 0 ] \
    && ok "flagless init on a below-spec host exits 0 (a choice, not a fault — waired-agent#590)" \
    || bad "flagless init exited $rc on a below-spec host — the non-interactive default is skip-and-continue, never a failure — see $log"
  if grep -q "$IT_UNFIT_SKIP_RE" "$log"; then
    ok "the step-4 non-interactive default said what it did"
  else
    bad "init never printed the skip note — the step-4 default arm was not reached, so the asserts around it prove nothing$(_it_engine_present_note "$guest") — see $log"
    tail -n 20 "$log" | sed 's/^/    init| /' >&2
  fi
  grep -q "$IT_INSTALL_FAILURE_BOX_RE" "$log" \
    && bad "init reported the below-spec default as a failed install — see $log" \
    || ok "the default is not reported as a failed install"
  local desired
  desired="$(gx "$guest" curl -fsS --max-time 5 http://127.0.0.1:9476/waired/v1/inference/status 2>/dev/null \
    | grep -oE '"desired_state"[[:space:]]*:[[:space:]]*"[a-z]+"' \
    | grep -oE '"[a-z]+"$' | tr -d '"' || true)"
  [ "$desired" = disabled ] \
    && ok "the default landed as the persisted toggle (mgmt API desired_state=disabled)" \
    || bad "mgmt API desired_state=$desired after the flagless below-spec init, want disabled"

  [ "$rc" = 0 ] || tail -n 20 "$log" | sed 's/^/    /' >&2
  _it_restore_host_memory "$guest" "#590 default"
}

# assert_models_pull_confirm: the `waired models pull` half of the #590
# twin (waired-ai/waired#1067, 2026-08-08 owner decision — no surface
# refuses a model any more; the refusal rule it supersedes was
# waired-ai/waired#1056).
#
# The contract has two rows and they differ only by --force:
#
#   --yes          a fits=false model is NOT downloaded. --yes skips
#                  confirmations whose safe answer is yes; this one's is
#                  No, so a non-interactive run declines and says how to
#                  override. Exit 0 — a decline is a choice, not a fault.
#   --yes --force  the same model IS honoured: the gate does not stop it.
#
# Runs on the LEAN tier-2 guest, beside the other two step-4 probes, and
# that placement is what makes it free: this guest has no engine, so
# PullModel refuses at #307's admission check ("cannot download … yet")
# BEFORE writing a catalog row or opening a socket. The --force row
# therefore proves the gate was passed without a single byte being
# downloaded, which is why this can live on the per-PR gate at all —
# `installtest.yml` deliberately stops before anything fetches weights.
#
# Anti-vacuity has two parts. The host is made below-spec with the same
# #568 measurement seam the probe above uses, so EVERY family reads
# fits=false whatever the runner's real memory (without it the gate is
# never reached and all five asserts pass having tested nothing); and
# inference is turned on first, because /inference/catalog is what the
# CLI asks for the fit verdict and a fail-open lookup would silently
# skip the gate.
#
# Exactly five asserts, always — the tier-2 floor counts on it.
assert_models_pull_confirm() {
  local guest="$1" name log model rc=0

  name="$(_it_dev_name "$guest")"
  mkdir -p "$IT_LOGDIR"

  it_log "checking the models-pull confirmation on a forced below-spec host (waired-agent#590)"
  _it_force_below_spec "$guest" "#590 pull twin"

  # The model comes from the catalog, not from a literal: the bundled
  # set is retired and replaced on its own schedule (#577), and a
  # hardcoded id turns that into a red leg about the wrong thing. Any
  # family will do — under the seam they are all fits=false.
  model="$(gx "$guest" curl -fsS --max-time 5 http://127.0.0.1:9476/waired/v1/inference/catalog 2>/dev/null \
    | grep -oE '"model_id"[[:space:]]*:[[:space:]]*"[^"]+"' | head -1 \
    | grep -oE '"[^"]+"$' | tr -d '"' || true)"
  if [ -z "$model" ]; then
    bad "no model_id in the catalog response — the pull gate reads the same endpoint, so nothing below would be testing it"
    _it_restore_host_memory "$guest" "#590 pull twin"
    # Still five. A leg that reports four has a block that stopped
    # executing, and the floor is what says so.
    bad "skipped: --yes on a model that does not fit was not exercised"
    bad "skipped: the decline is not a failed command"
    bad "skipped: --yes alone did not reach the pull layer"
    bad "skipped: --yes --force honoured the choice"
    return
  fi

  # Row 1: --yes alone. stdin from /dev/null so stdinIsInteractive() is
  # unambiguously false — this row IS the non-interactive branch.
  log="$IT_LOGDIR/models-pull-yes-$name.log"
  gx "$guest" sh -c "WAIRED_NO_EMOJI=1 waired models pull '$model' --yes --wait=false </dev/null" \
    >"$log" 2>&1 || rc=$?

  [ "$rc" = 0 ] \
    && ok "declining an over-memory pull is not a failed command (exit 0)" \
    || bad "\`models pull --yes\` exited $rc on a model that does not fit — a decline is a choice, not a fault — see $log"
  grep -qF "$IT_PULL_DECLINE_RE" "$log" \
    && ok "--yes alone declines an over-memory pull and says how to override" \
    || bad "\`models pull --yes\` never printed the decline line — --yes must not auto-confirm a default-No question — see $log"
  grep -q "$IT_PULL_QUEUED_RE" "$log" \
    && bad "\`models pull --yes\` queued the download anyway — the gate did not stop it — see $log" \
    || ok "--yes alone dispatched nothing to the daemon"

  # Row 2: --yes --force. Same model, same host; only the flag differs.
  rc=0
  log="$IT_LOGDIR/models-pull-force-$name.log"
  gx "$guest" sh -c "WAIRED_NO_EMOJI=1 waired models pull '$model' --yes --force --wait=false </dev/null" \
    >"$log" 2>&1 || rc=$?

  grep -qF "$IT_PULL_DECLINE_RE" "$log" \
    && bad "\`models pull --yes --force\` still declined — the scripted consent is the pair, and it was not honoured — see $log" \
    || ok "--yes --force is not stopped by the over-memory gate"
  # …and it reached the daemon. On this engine-less guest that shows up
  # as PullModel's own admission refusal (#307), which is the point: the
  # gate handed the request on, and the daemon declined it for an
  # unrelated and much cheaper reason.
  grep -qE "$IT_PULL_REACHED_RE" "$log" \
    && ok "--yes --force handed the pull to the daemon" \
    || bad "\`models pull --yes --force\` neither queued nor reached the daemon's pull layer — see $log"

  [ "$rc" = 0 ] || tail -n 10 "$log" | sed 's/^/    /' >&2
  _it_restore_host_memory "$guest" "#590 pull twin"
}

# assert_engine_only_install: "the AI software is installed and no model
# was chosen" is a FINISHED install, not a stalled one (waired-agent#590;
# owner-ruled 2026-08-08, waired-ai/waired#1067 — the picker's
# "0) Don't download a model now" is a normal completed state, exit 0,
# and a model can be added later from the browser dashboard or
# `waired models pull`).
#
# Nothing exercised that end to end. The Go tests cover the picker's own
# branches (#600), but the claim here is about the WHOLE tail: that init
# finishes, that the closing box does not read the host as broken, that
# the daemon publishes the standing choice, and — the part only a real
# host can show — that the answer SURVIVES a restart without the #379
# boot pre-pull deciding to fetch something anyway.
#
# THE ONE INTERACTIVE INIT IN THE SUITE. Every other init here passes
# --non-interactive, which is exactly what makes them unable to reach
# this: runInitModelPicker returns immediately on that flag. Feeding it
# one line of stdin is the only way in, and one line is enough because
# --inference-enabled=true silences the two questions in front of the
# picker — step 4 takes the flag as its answer, and step 6 returns
# before it waits for a measurement. The picker itself re-prompts on
# unparseable input, so even a stray line ahead of the "0" would not
# desync it.
#
# Not on the per-PR gate: this installs a real engine from a release
# asset, which is the external state installtest.yml deliberately stops
# short of.
#
# Exactly six asserts, always — the floor counts on it.
assert_engine_only_install() {
  local guest="$1" name log rc=0 out
  name="$(_it_dev_name "$guest")"
  mkdir -p "$IT_LOGDIR"
  log="$IT_LOGDIR/engine-only-$name.log"

  it_log "installing an engine and answering the model picker with 0 (waired-agent#590)"
  # The daemon has to WANT an engine, the same arrangement the two probes
  # above make. The guest is already enrolled, so this init resumes
  # (#313) rather than signing in again.
  gx "$guest" waired inference on >/dev/null 2>&1 || \
    it_warn "could not turn inference on in $guest before the #590 engine-only probe"

  gx "$guest" sh -c "printf '0\n' | WAIRED_NO_EMOJI=1 waired init \
      --control '$IT_CONTROL_URL' --device-name '$name' \
      --inference-enabled=true --skip-integration" >"$log" 2>&1 || rc=$?

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
  gx "$guest" test -x "$IT_BUNDLED_OLLAMA_BIN" \
    && ok "the engine is installed ($IT_BUNDLED_OLLAMA_BIN) — this host runs AI, it just has no model yet" \
    || bad "no engine at $IT_BUNDLED_OLLAMA_BIN — the point of this state is that the software IS installed — see $log"

  _it_no_model_selected "$guest" \
    && ok "the daemon publishes the standing no-model choice (mgmt API no_model_selected=true)" \
    || bad "mgmt API does not report no_model_selected after the operator chose not to download a model"

  # The restart is the whole point of the sixth assert: an answer that
  # does not survive one is not a standing choice, and the #379 boot
  # pre-pull is what would otherwise fetch a model nobody asked for.
  gx "$guest" systemctl restart waired-agent || \
    it_warn "could not restart the daemon in $guest for the #590 engine-only probe"
  _it_wait_enrolled "$guest" >/dev/null || \
    it_warn "daemon did not report enrolled after the #590 engine-only restart"
  _it_no_model_selected "$guest" \
    && ok "the choice survives a restart — the boot pre-pull stands down (waired-agent#379)" \
    || bad "no_model_selected is gone after a restart — the boot pre-pull is about to download a model nobody asked for"

  if [ "$rc" != 0 ]; then
    tail -n 25 "$log" | sed 's/^/    /' >&2
    gx "$guest" sh -c 'curl -fsS --max-time 10 http://127.0.0.1:9476/waired/v1/inference/status || echo "(status unreachable)"' 2>&1 | sed 's/^/    status| /' || true
  fi

  # Leave the guest as we found it for anything after this one.
  gx "$guest" waired inference off >/dev/null 2>&1 || \
    it_warn "could not turn inference back off in $guest after the #590 engine-only probe"
}

# _it_no_model_selected — is the daemon publishing the standing "run
# without a model" choice? Reads the mgmt API's own field rather than
# inferring from an empty model list: an empty list is also what a host
# that has simply not downloaded anything yet looks like, and telling
# those two apart is the whole contract.
_it_no_model_selected() {
  gx "$1" curl -fsS --max-time 5 http://127.0.0.1:9476/waired/v1/inference/status 2>/dev/null \
    | grep -qE '"no_model_selected"[[:space:]]*:[[:space:]]*true'
}

# _it_force_below_spec / _it_restore_host_memory — arrange and undo for
# every probe that needs this host to read as below the recommended
# spec. Shared by assert_reinit_default_unfit and
# assert_models_pull_confirm so the two can never drift on the ONE thing
# that makes them non-vacuous.
#
# WAIRED_RAM_AVAILABLE_GB is the #568 measurement seam: the daemon reads
# it in place of the probe and the persisted record, so the fit verdicts
# read "nothing fits" whatever the runner's real memory, and the probes
# never depend on which machine class CI happened to buy. It is daemon
# env, so it takes a service restart to be read — and the restart drops
# the enrolled session for a few seconds, which is what _it_wait_enrolled
# is for. Inference is turned ON because both probes need the daemon to
# still want an engine (the #551 probe before them leaves it off).
_it_force_below_spec() {
  local guest="$1" who="$2"
  gx "$guest" waired inference on >/dev/null 2>&1 || \
    it_warn "could not turn inference on in $guest before the $who probe"
  # The third thing these probes need, and the one this cannot arrange: an
  # engine-less host. Both callers reach their arm only while the daemon
  # still WANTS an engine, and an installed one is exactly what stops that
  # being true. Nothing in the suite uninstalls an engine, so the state is
  # INHERITED from whatever ran before (assert_reinit_resumes' postcondition,
  # documented at its --inference-enabled comment). Say so here rather than
  # letting the probe fail downstream naming step 4, which is what sent
  # waired-agent#640's reader to the wrong code.
  if gx "$guest" test -x "$IT_BUNDLED_OLLAMA_BIN"; then
    it_warn "an engine is already installed at $IT_BUNDLED_OLLAMA_BIN in $guest before the $who probe — the daemon no longer wants one, so the arm under test will not be reached (waired-agent#640)"
  fi
  gx "$guest" sh -c "printf 'WAIRED_RAM_AVAILABLE_GB=1\n' >> /etc/waired/agent.env && systemctl restart waired-agent"
  _it_wait_enrolled "$guest" >/dev/null || \
    it_warn "daemon did not report enrolled after the $who seam restart"
}

# _it_engine_present_note <guest> — the likeliest reason the arm under test
# was not reached, when it is this one, as a clause to append to a failure
# message. Echoes nothing otherwise, so the caller appends it unconditionally.
#
# waired-agent#640: the missing-skip-note failure reported that step 4 was
# not reached and stopped there. An engine already on the host is the most
# likely why, and naming it here is what stops the next reader from going
# through the step-4 code and the below-spec seam, which are both fine.
_it_engine_present_note() {
  if gx "$1" test -x "$IT_BUNDLED_OLLAMA_BIN"; then
    printf ' — an engine is already installed at %s, so the daemon no longer wanted one (waired-agent#640)' \
      "$IT_BUNDLED_OLLAMA_BIN"
  fi
}

# Leave the guest as we found it: seam out, daemon restarted on real
# measurements, toggle back off — the state every assert after these
# probes was written against.
_it_restore_host_memory() {
  local guest="$1" who="$2"
  gx "$guest" sh -c "sed -i '/^WAIRED_RAM_AVAILABLE_GB=/d' /etc/waired/agent.env && systemctl restart waired-agent" || \
    it_warn "could not remove the RAM seam from $guest's agent.env"
  # The asserts after these probes talk to the daemon straight away (the
  # pause/resume pair was the first casualty): wait out the restart's
  # re-enrollment window before handing the guest back.
  _it_wait_enrolled "$guest" >/dev/null || \
    it_warn "daemon did not report enrolled after the $who cleanup restart"
  gx "$guest" waired inference off >/dev/null 2>&1 || \
    it_warn "could not turn inference back off in $guest after the $who probe"
}

# The step-4 non-interactive default's skip note (waired-agent#584/#590).
# A POSITIVE grep, and the anti-vacuity assert of the default probe: a
# host that did not read as below-spec never reaches this arm, and every
# other assert around it would pass having tested nothing.
IT_UNFIT_SKIP_RE='Non-interactive: skipping local AI'

# The model picker's "0) Don't download a model now" acknowledgement
# (waired-agent#586/#590). A POSITIVE grep, and the anti-vacuity assert
# of the engine-only probe: a host where the picker never ran satisfies
# every other assert there.
#
# The product line ends with an em dash clause ("— the AI software stays
# ready"); only the ASCII head is matched, so a leg on a console that
# mangles the dash still greps true. Registered in
# scripts/ci/harness-failure-strings-guard.sh now that the macOS and
# Windows engine-only twins exist — that guard requires three agreeing
# copies, and it was the reason this one waited.
IT_NO_MODEL_RE='No model selected'

# The three strings assert_models_pull_confirm greps for. Kept as named
# literals for the same reason IT_ENGINE_OPTOUT_RE is, and matched the
# same way: IT_PULL_DECLINE_RE is asserted both PRESENT (row 1) and
# ABSENT (row 2), so a rename would half-pass silently.
#
# Matched with grep -F (decline) / -E (reached), never with a pattern
# that would make `--yes --force` a regex operator set.
#
# IT_PULL_QUEUED_RE and IT_PULL_REACHED_RE are deliberately NOT in
# scripts/ci/harness-failure-strings-guard.sh: `queued pull:` is a
# format string with a `%s` after it and `cannot download` is a wrapped
# error, so a literal search of the product source cannot find either,
# and a guard entry that can never pass is worse than none. The two that
# ARE searchable — the skip note and the decline line — are registered.
IT_PULL_DECLINE_RE='Not downloading. Re-run with --yes --force to download it anyway.'
IT_PULL_QUEUED_RE='queued pull:'
IT_PULL_REACHED_RE='queued pull:|cannot download'

# Bundled engine path on Linux: waired's BUNDLED Ollama lives under the state
# dir (#567) — it is NOT a system ollama on PATH, and it serves on the
# waired-owned port :9475, never the upstream default :11434.
#
# Since #138 install.sh does not put it there: `waired init` does, through the
# daemon path (cmd/waired/login_client.go -> ensureDaemonPathEngine, or
# runSetupEngineInstall when a browser wizard is driving), into the state dir
# the daemon itself declared. Same binary, same path — what changed is that
# nothing downloads it before the operator has said this host runs models.
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

# The two strings the engine-opt-out probe greps for (waired-agent#551), under
# the same guard and for the same reason.
#
# IT_ENGINE_OPTOUT_RE is a POSITIVE grep, so a rename shows up as a red leg on
# its own. IT_INSTALL_FAILURE_BOX_RE is asserted ABSENT — init must not call
# the operator's own opt-out a failed install — and an absent-assert for
# wording the product stopped printing passes forever, which is #178 with the
# sign flipped.
#
# What actually catches a rename of that second string is
# TestRunInitViaDaemon_EngineInstallFailureSkipsTheWait, not this guard's
# "still in the product" half: that half searches *.go, and
# the test file's own copy satisfies it. Measured, not assumed — renaming the
# string leaves the guard green and turns the unit test red. The guard earns
# its entry on the OTHER check, that the three harnesses agree on one literal.
#
# These are matched as LITERALS, not patterns — grep without -E (parentheses
# are ordinary characters in BRE) and Select-String -SimpleMatch on Windows.
# The Windows half is not optional: -Pattern is a .NET regex, where
# `(WAIRED_NO_OLLAMA)` is a capture group, so the pattern would hunt for
# "skipped WAIRED_NO_OLLAMA" with no brackets and never match. Escaping the
# declaration instead would break the other two, since `\(` in BRE means the
# opposite. Sharing one literal is what keeps the three harnesses honest, so
# the matching side is what adapts — keep any new string here free of regex
# metacharacters for the same reason.
IT_ENGINE_OPTOUT_RE='Engine install skipped (WAIRED_NO_OLLAMA)'
IT_INSTALL_FAILURE_BOX_RE='The AI engine could not be installed on this device'

# Lines `waired init` prints when the benchmark did not run because the MODEL
# was not ready — not because anything is broken (#382). The benchmark assert
# below distinguishes these from a benchmark that ran and produced nothing, so
# the red names the download instead of sending every investigation to the
# engine.
#
# Each branch and where the product prints it:
#   Model not ready in time   cmd/waired/init_benchmark.go — the benchmark wait
#                             gave up while the pull was still in flight
#   Model download failed     cmd/waired/init_benchmark.go (pull_failed arm) and
#                             cmd/waired/init_pull.go (terminal pull failure)
#   Model still downloading   cmd/waired/init_pull.go — the foreground pull wait
#                             ran out of budget with the download progressing
#   No model was chosen for this computer
#                             cmd/waired/init_pull.go — the same wait, on a host
#                             where nothing was ever selected, so there was no
#                             download to run out of budget (waired-agent#736)
#
# The fourth branch has to be here or the leg blames the engine. It is a
# not-ready ending like the other three, and an ending this grep does not
# carry falls through to the "no benchmark THROUGHPUT figure" arm below,
# which dumps engine-side evidence for a fault that is not the engine's —
# the exact regression #382 added this set to stop.
#
# Deliberately not the IT_NO_MODEL_RE wording ('No model selected'): that
# one is asserted PRESENT on the engine-only legs, so reusing it here would
# make this line satisfy an assert on a different leg about a different
# thing. "was chosen" also matches the product's own register
# (cmd/waired/worker.go's "no model chosen on that peer").
#
# Same alternation-per-harness shape as IT_INSTALL_FAILURE_RE above, and
# checked by the same guard: mirror any change in installtest-macos.sh and
# installtest-windows.ps1.
IT_BENCH_NOT_READY_RE='Model not ready in time|Model download failed|Model still downloading|No model was chosen for this computer'

# The /waired/v1/inference/status field NAMES it_model_ready_state greps for.
# Not init wording — JSON keys, owned by
# internal/management/inference_handlers.go (ActiveSelection, HostSpeedStatus,
# and the no_model_selected flag) — but here for exactly the reason the
# alternations above are: this leg's model verdict is built by matching these
# literals out of a payload, so a field the daemon renamed reads as "nothing
# decided yet" and the leg goes red blaming the download for a rename.
#
#   no_model_selected  the operator's standing "no model now" choice (#586),
#                      which is terminal — nothing is coming, so stop polling
#   host_speed         the #496 measurement, and the object probe_model_id
#                      lives in
#   probe_model_id     which model the measurement pulled — what lets a red
#                      say "this host got a probe, not a pick" (#573)
#
# Checked, not asserted at runtime: all three are `omitempty`, so absence is a
# legitimate state (no measurement yet; no standing choice) and a presence
# assert would fail on a healthy host. The guard is the right place — it fails
# lint in the PR that renames the field, which is earlier than the leg could
# have told anyone anyway.
#
# Same mirror-and-guard rule as the alternations above: installtest-macos.sh
# and installtest-windows.ps1 carry the identical literal.
#
# shellcheck disable=SC2034  # read by scripts/ci/harness-failure-strings-guard.sh,
# not by this script: the literals themselves are inlined in the readers below,
# and this declaration is what lets the guard find and cross-check them.
IT_STATUS_FIELDS_RE='no_model_selected|host_speed|probe_model_id|turn_floor_seconds'

# The daemon-log lines the evidence dumps grep for when a model did not arrive
# in time. Two groups, and the second is the whole reason this is a named
# literal rather than three inlined copies:
#
#   boot pre-pull / bundled model    what the download was waiting for, what
#                                    released it, and which model was chosen
#   host speed / host cutoff /       what the #496 measurement was doing while
#   below the recommended spec /     the download waited, and whether it
#   measuring whether this host      reached a verdict at all (#579)
#
# The second group was missing everywhere, and that is not a detail. On run
# 31316731884 the macOS leg failed with the bundled model absent, and the same
# status payload carried `turn_seconds: 542.3` against a 45 s budget — the
# measurement had taken 7 min 12 s of a 10 min window and finished 53 seconds
# before init gave up. None of that was in the job log: the dump ran, matched
# `api/pull` alone, and printed one line. "The download was slow" and "the
# measurement was in front of the download" are different failures with
# different fixes, and the pattern could not tell them apart.
#
# `api/pull` is deliberately OUTSIDE this alternation and appended at each use
# site. It is ollama's own request log surfaced through `waired logs`, not a
# string the agent's Go source contains, so a guard entry for it could never
# pass — the same reason IT_PULL_QUEUED_RE is left out above.
#
# Same mirror-and-guard rule as the alternations above: installtest-macos.sh
# and installtest-windows.ps1 carry the identical literal.
# The last two branches are about the DUMP rather than the download, and
# they are what let a reader tell "the pull never reached the engine" from
# "the evidence never reached this log" (waired-agent#642):
#
#   engine log truncated at cap   the product dropped the tail itself —
#                                 cappedWriter keeps the START of engine.log
#                                 (internal/runtime), so a late pull line can
#                                 be gone before any harness sees it
#   no engine logs found          logdump collected no engine log at all, so
#                                 the absence of `/api/pull` says nothing
#                                 about whether a pull happened
IT_DAEMON_EVIDENCE_RE='boot pre-pull|bundled model|host speed|host cutoff|below the recommended spec|measuring whether this host|engine log truncated at cap|no engine logs found'

# --- reading the inference status --------------------------------------
#
# Four small readers instead of one jq call: jq is not on every guest image
# these legs run against, and the two objects being read are flat by
# construction, so a brace-free match is exact rather than approximate.
#
# it_json_object <json> <key> — the {...} value of an object key.
#
# Safe ONLY for objects whose own values contain no nested braces. The two
# read here qualify by construction: ActiveSelection is five strings and a
# string array, HostSpeedStatus is scalars only
# (internal/management/inference_handlers.go). The closing quote in the
# pattern is what stops "active" from matching "active_endpoints".
it_json_object() {
  printf '%s' "$1" | grep -oE "\"$2\"[[:space:]]*:[[:space:]]*\{[^}]*\}" | head -1 || true
}

# it_json_str <json> <field> — a string field's value. Scope it to an object
# first when the field name is not unique in the payload: "model_id" appears
# on active, on available_update and inside benchmark_recommendation.
it_json_str() {
  printf '%s' "$1" | grep -oE "\"$2\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" | head -1 |
    sed -E 's/.*:[[:space:]]*"(.*)"$/\1/' || true
}

# it_json_true <json> <field> — 0 when the field is literally true.
it_json_true() {
  printf '%s' "$1" | grep -qE "\"$2\"[[:space:]]*:[[:space:]]*true"
}

# it_models_ready <json> — the ids in models.ready, one per line. `sed 1d`
# drops the key itself, which is the first quoted token the slice matches.
it_models_ready() {
  printf '%s' "$1" | grep -oE '"ready"[[:space:]]*:[[:space:]]*\[[^]]*\]' | head -1 |
    grep -oE '"[^"]+"' | sed 1d | tr -d '"' || true
}

# it_model_ready_state <json> — what the daemon says about the model this leg
# is waiting for. Echoes exactly one of:
#
#   ready <id>   the daemon is COMMITTED to serving <id>, and <id> is on disk
#   none         the operator's standing "no model now" choice (#586)
#   probe <id>   the only weights on disk are #496's host-cutoff probe and
#                nothing was selected — a probe, not a pick (#573)
#   pending      nothing decided yet
#
# Keyed on `active` — "the engine + model the agent is committed to serving" —
# rather than on models.ready being non-empty. models.ready is "every model on
# disk and loadable", and the cutoff probe lands there like any other pull, so
# a probe and a pick are indistinguishable from that field alone. That is
# #573: a green `ok` naming the probe, on a host where selection declined and
# the 1 GB download was the measurement.
#
# Deliberately matches NO model id literal. The id comes from the daemon, so a
# catalog change needs no harness edit and this cannot silently start reading
# "not ready" the way a vendor-name match would (#322) — the rule that comment
# in installtest-windows.ps1 states, kept by asking the daemon which id it
# committed to instead of by knowing the answer.
it_model_ready_state() {
  local json="$1" id ready probe
  id="$(it_json_str "$(it_json_object "$json" active)" model_id)"
  ready="$(it_models_ready "$json")"
  if [ -n "$id" ] && printf '%s\n' "$ready" | grep -qxF "$id"; then
    printf 'ready %s\n' "$id"; return 0
  fi
  # subsystem_state "ready" is strictly stronger than the arm above — it needs
  # an active selection whose model is ready (cmd/waired-agent/inference.go,
  # subsystemState) — so it cannot be satisfied by the probe. Kept as a second
  # accepting arm rather than dropped: it is what the assert accepted before
  # #573, and removing an accepting arm is how a fix turns a legitimate green
  # red.
  if printf '%s' "$json" | grep -qE '"subsystem_state"[[:space:]]*:[[:space:]]*"ready"'; then
    printf 'ready %s\n' "${id:-(ready)}"; return 0
  fi
  if it_json_true "$json" no_model_selected; then
    printf 'none\n'; return 0
  fi
  # Only when NOTHING was selected. An active selection whose weights have not
  # landed yet is the ordinary state for most of this poll — the probe is ~1 GB
  # and the pick is 20-45 GB — and calling that "a probe, not a pick" would put
  # a false claim in the red every time a download simply ran long.
  probe="$(it_json_str "$(it_json_object "$json" host_speed)" probe_model_id)"
  if [ -z "$id" ] && [ -n "$probe" ] && [ "$ready" = "$probe" ]; then
    printf 'probe %s\n' "$probe"; return 0
  fi
  printf 'pending\n'
}

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
#
# "Ready" is it_model_ready_state's verdict, not "models.ready is non-empty":
# the #496 cutoff probe lands in models.ready like any other pull, so the old
# test returned 0 on a host that had a probe and no selection (#573).
_it_wait_inference_ready() {
  local guest="$1" _ out state
  for _ in $(seq 1 60); do          # ~5 min; CPU model pull is minutes-scale
    out="$(gx "$guest" curl -fsS --max-time 10 http://127.0.0.1:9476/waired/v1/inference/status 2>/dev/null || true)"
    case "$(it_model_ready_state "$out")" in
      ready\ *)
        printf '%s' "$out"; return 0 ;;
      none)
        # The operator's standing "no model now" choice (#586). Nothing is
        # coming, so waiting out the budget only delays the red.
        printf '%s' "$out"; return 1 ;;
    esac
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

# it_prepull_evidence — the daemon's own account of a model that never
# arrived (#540), for the not-ready arm of the benchmark assert.
#
# `waired logs` rather than a per-OS log command: it is the one surface that
# reads the service log AND the bundled engine logs on all three OSes, so the
# three harnesses ask this question the same way — and the Windows leg, which
# had no engine-log dump at all, gets one for free.
#
# The pattern is IT_DAEMON_EVIDENCE_RE plus `api/pull` — the facts that settle
# where the time went. The boot pre-pull's hold says what it is waiting for and
# what released it, `POST /api/pull` carries the download's real duration, and
# the host-speed lines say what was standing in front of it (#579). `grep .` is
# what makes an empty result say so — a bare grep would print nothing and read
# as "the dump did not run".
#
# _it_evidence_dump <guest> <bundle> — the shared body of both dumps, over a
# bundle `waired logs` has already written.
#
# TWO greps rather than one, and counted (waired-agent#642). Every `/api/pull`
# line comes from the engine's own request log, which logdump appends as the
# LAST section, so a single `tail -40` over the union shows the newest slice
# of a set the host-speed lines can dominate — and it printed no count, so one
# surviving pull line read the same whether the bundled download never reached
# the engine or the window cut the rest. Splitting the groups lets the pull
# group answer that on its own; the counts make a cut window visible instead
# of silent. Free space is here for the third hypothesis the old dump could
# not test at all: a multi-GB download that had nowhere to land.
_it_evidence_dump() {
  local guest="$1" bundle="$2"
  gx "$guest" sh -c "
    n=\$(grep -icE '$IT_DAEMON_EVIDENCE_RE' '$bundle' 2>/dev/null) || n=0
    echo \"daemon evidence: \$n line(s) matched, showing the last 40\"
    grep -iE '$IT_DAEMON_EVIDENCE_RE' '$bundle' 2>/dev/null | tail -40 |
      grep . || echo '(no pre-pull or host-speed lines in the daemon log)'
    p=\$(grep -icE 'api/pull' '$bundle' 2>/dev/null) || p=0
    echo \"engine pull requests: \$p line(s) matched, showing all\"
    grep -iE 'api/pull' '$bundle' 2>/dev/null |
      grep . || echo '(no api/pull lines in the daemon log)'
    d=\$(df -Ph /var/lib/waired 2>/dev/null | tail -1) || d=
    echo \"state-dir free space: \${d:-(state dir unreadable)}\"
  " 2>&1
}

# it_hostspeed_evidence — the daemon's own account of the #496 host-speed
# measurement and the selection it fed, for the arms that die on init's exit
# code.
#
# A sibling of it_prepull_evidence rather than part of it, because the failure
# that needs this one kills the run BEFORE assert_inference is reached: a leg
# that dies in it_enroll_guest never reaches any of the diagnostic dumps below,
# so on a finished run the state of the measurement — the thing #579 turns on —
# was simply unrecoverable. Observed on run 31311709284, where 611 lines of job
# log contained no daemon output at all and the question "did the measurement
# complete?" could not be answered even in principle.
#
# `waired logs` for the same reason it_prepull_evidence uses it: one surface
# that reads the service log and the bundled engine's log on all three OSes.
it_hostspeed_evidence() {
  local guest="$1"
  gx "$guest" sh -c "waired logs --since 30m --state-dir /var/lib/waired -o /tmp/it-hs.txt >/dev/null 2>&1" || true
  _it_evidence_dump "$guest" /tmp/it-hs.txt |
    sed 's/^/    agent| /' >&2 || true
  gx "$guest" sh -c 'curl -fsS --max-time 10 http://127.0.0.1:9476/waired/v1/inference/status || echo "(status unreachable)"' 2>&1 |
    sed 's/^/    status| /' >&2 || true
}

it_prepull_evidence() {
  local guest="$1"
  gx "$guest" sh -c "waired logs --since 30m --state-dir /var/lib/waired -o /tmp/it-logs.txt >/dev/null 2>&1" || true
  _it_evidence_dump "$guest" /tmp/it-logs.txt |
    sed 's/^/    agent| /' || true
}

# assert_serving_ollama <guest> <context> — the engine ANSWERING REQUESTS is
# waired's own, at the pinned version (#494, Phase 3 of #488).
#
# The assert beside this one stats a file. That is a different claim, and the
# gap between them is the whole of #139: a host can hold waired's binary at the
# right path and still be served by something else. Three facts close it, and
# they are deliberately three lines rather than one — a red then says WHICH
# layer broke without a second run.
#
#   1. the process listening on :9475 is that exact binary. Read from the
#      host (ss -> /proc/<pid>/exe), not from the agent, so it holds even if
#      the agent's own bookkeeping is what is wrong. It is also the only one
#      of the three a foreign engine AT THE PIN could not satisfy — which is
#      reachable: EnsureRunning adopts an exact-pin survivor on our port
#      (internal/runtime/ollama.go, EngineModeAdopted).
#   2. it reports the pinned version. Compared against the daemon's
#      pinned_version rather than a version literal kept here: that field IS
#      infruntime.OllamaPinnedVersion compiled into the build under test, so
#      a pin bump needs no harness edit and the two cannot drift.
#   3. waired spawned it. Distinguishes (1)-satisfied-by-our-own-child from
#      (1)-satisfied-by-an-orphan-of-a-previous-run.
#
# Mirror any change in installtest-macos.sh (assert_serving_ollama_macos) and
# installtest-windows.ps1 (Assert-ServingEngine). The wording is identical in
# all three down to the punctuation, and scripts/dev/installtest-serving-asserts.sh
# runs every branch of every copy per PR and fails on the first difference —
# these asserts otherwise execute only in the nightly legs, where a copy that
# had stopped being able to fail would sit green for a long time.
assert_serving_ollama() {
  local guest="$1" ctx="$2" _ body live st pinned mode pid exe

  # No engine on disk means nothing can be serving, and the 180 s poll below
  # would spend three minutes arriving at "installed but not answering" — a
  # sentence that is FALSE in that case and points the reader at the engine
  # instead of at the install. Observed on the Windows daemon-path leg, where
  # the executor never attached (#505) and no engine was ever installed.
  if ! gx "$guest" test -x "$IT_BUNDLED_OLLAMA_BIN"; then
    bad "nothing can be serving on :9475 ($ctx): no engine at $IT_BUNDLED_OLLAMA_BIN"
    return
  fi

  # The engine is normally up by the time we get here — the --inference leg is
  # past init's foreground model wait (#519). The daemon-engine leg can arrive
  # mid cold start, so give the port a bounded window rather than racing it.
  # The gate is a PARSED version, not a non-empty body: a reply we cannot read
  # a version out of is not an engine we can check, and calling that "serving"
  # would carry an empty $live into the comparison below.
  #
  # 180 s, deliberately LONGER than the agent's own first-readiness budget
  # (OllamaConfig.StartupReadyTimeout, 150 s by default): a harness window
  # shorter than the product's tolerance reds on a slow cold start the product
  # is still happy with. Keep it above whatever that constant becomes.
  for _ in $(seq 1 60); do            # ~180 s
    body="$(gx "$guest" curl -fsS --max-time 5 http://127.0.0.1:9475/api/version 2>/dev/null || true)"
    live="$(printf '%s' "$body" | sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
    [ -n "$live" ] && break
    sleep 3
  done
  if [ -z "$live" ]; then
    bad "nothing is serving on :9475 after 180 s ($ctx) — the engine is installed but not answering"
    gx "$guest" sh -c 'tail -n 40 /var/lib/waired/runtimes/ollama/logs/engine.log 2>/dev/null || echo "(no engine.log)"' 2>&1 \
      | sed 's/^/    engine.log| /' >&2 || true
    return
  fi

  st="$(gx "$guest" curl -fsS --max-time 5 http://127.0.0.1:9476/waired/v1/inference/status 2>/dev/null || true)"
  # pinned_version is emitted by the ollama runtime only, so it is unique in
  # the document. "mode" is not a unique key across the API, but within THIS
  # payload it is: runtimes is the second field of InferenceStatus, Go sorts
  # map keys so "ollama" precedes "vllm", and vllm sets no mode.
  pinned="$(printf '%s' "$st" | sed -n 's/.*"pinned_version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
  mode="$(printf '%s' "$st" | grep -oE '"mode"[[:space:]]*:[[:space:]]*"[a-z]+"' | head -1 \
    | sed 's/.*"\([a-z]*\)"$/\1/' || true)"

  # 1. the listener IS the state-dir binary. No separate "is ss installed"
  #    probe: a missing ss and a listener ss cannot see both come back as an
  #    empty pid, and "ss found no listening process" is the right report for
  #    either — one branch fewer, and the same sentence on all three OSes.
  pid="$(gx "$guest" sh -c 'ss -Hltpn "sport = :9475" 2>/dev/null | sed -n "s/.*pid=\([0-9][0-9]*\).*/\1/p" | head -1' 2>/dev/null || true)"
  # Plain readlink, not -f: a binary replaced under a running process leaves
  # the target as "<path> (deleted)", which should read as a mismatch with its
  # reason on show — `readlink -f` would resolve it to nothing at all.
  exe=""
  if [ -n "$pid" ]; then
    exe="$(gx "$guest" readlink "/proc/$pid/exe" 2>/dev/null || true)"
  fi
  if [ -z "$pid" ]; then
    # /api/version answered above, so something IS listening — an empty pid is
    # a lookup failure, not an absent engine, and must not be reported as the
    # wrong binary.
    bad "could not identify the process listening on :9475 ($ctx) — ss found no listening process"
  elif [ "$exe" = "$IT_BUNDLED_OLLAMA_BIN" ]; then
    ok "the process serving :9475 is the state-dir binary ($ctx; pid $pid)"
  else
    bad "the process serving :9475 is not waired's engine ($ctx): pid=$pid exe=${exe:-unreadable}, expected $IT_BUNDLED_OLLAMA_BIN"
    gx "$guest" sh -c 'ss -Hltpn "sport = :9475" 2>/dev/null' 2>&1 | sed 's/^/    ss| /' >&2 || true
  fi

  # 2. it reports the pin. An empty pinned_version is its own failure: two
  #    empty strings compare equal, which is the assert-that-cannot-fail shape
  #    #178/#215 already cost this repo five days of green CI.
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

# assert_inference — verify the install→...→model-download→benchmark tail of
# the journey ran on CPU (Tier-2 --inference). `waired init
# --inference-enabled=true` installed the bundled engine through the daemon
# path (#138 — install.sh no longer pre-installs one), then (via #519)
# foreground-waited while the agent pulled the bundled model into the :9475
# engine, and ran the end-of-init benchmark.
# Proof points: bundled engine present, the model READY in the waired store
# (mgmt API on :9476, NOT a bare `ollama list` on :11434 — #567), inference
# enabled in persisted config, and a benchmark figure in the init transcript.
assert_inference() {
  local guest="$1" name initlog tps notready out

  name="$(_it_dev_name "$guest")"
  initlog="$IT_LOGDIR/init-$name.log"

  # PRIMARY: init's own transcript. If it says the engine install failed, the
  # leg failed — no amount of on-disk evidence overrides the installer's own
  # verdict (#215/#178). Runs first so the reason is the first thing printed.
  #
  # Three arms, not two. `[ -f ] && grep -q` collapses "no failure line" and
  # "no transcript at all" into the same `ok`, so a leg where init produced no
  # output reported the installer's verdict as clean (#505). Windows has always
  # had the third arm (Assert-Inference in installtest-windows.ps1); this is
  # the Linux twin of it, worded the same.
  if [ ! -f "$initlog" ]; then
    bad "no init transcript to check for install failures ($initlog)"
  elif grep -qE "$IT_INSTALL_FAILURE_RE" "$initlog"; then
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
    || bad "bundled ollama not installed at $IT_BUNDLED_OLLAMA_BIN (\`waired init --inference-enabled=true\` should have, via the daemon-path engine install)"

  # …and it is what actually serves, at the pin (#494). See the function for
  # why "installed" and "serving" are two claims and not one.
  assert_serving_ollama "$guest" "waired init"

  # #567: the bundled engine is waired-owned on :9475 with its own store; the
  # agent pulls there, NOT into the upstream default :11434. Read readiness
  # from the mgmt API and poll until ready, never a bare `ollama list` (:11434,
  # always empty here, the original false negative).
  local state
  if out="$(_it_wait_inference_ready "$guest")"; then
    state="$(it_model_ready_state "$out")"
    ok "bundled model ready in waired store :9475 (${state#ready }; the daemon's active selection, via mgmt API)"
  else
    state="$(it_model_ready_state "$out")"
    case "$state" in
      probe\ *)
        # The #573 red. Reported by name because the alternative — "not ready"
        # — sends the reader to the download, and on this host the download
        # SUCCEEDED: it was the measurement's own 1 GB probe, and selection is
        # what declined. See #579 for the defect this arm exists to surface.
        bad "this host got a probe, not a pick: the only model in the waired store is the host-cutoff probe (${state#probe }), and the daemon committed to no selection (#573)" ;;
      none)
        bad "no model was selected on this host (mgmt API no_model_selected=true) — \`waired init --inference-enabled=true\` should have picked one" ;;
      *)
        bad "bundled model not ready via mgmt API (deploy/pull failed?)" ;;
    esac
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

  # #496/#579: the one-time host-speed measurement. Asserted on the inference
  # leg because it is what decides whether a model is downloaded at all — and
  # because it runs inside init's window, so a leg that ends with no
  # measurement published is the shape #579 is about. The figures go in the ok
  # line so the nightly's job summary carries them: the 9m29s that opened #579
  # was only recoverable by downloading engine.log from a failed run.
  #
  # Skipped when the daemon published nothing at all — the red above already
  # named an unreachable daemon, and a second red would spread one cause over
  # two lines.
  # POLLED, not read once. The measurement is asynchronous by design — it
  # must not block init, so it runs on the boot goroutine behind
  # awaitQuietEngine — and reading the status a single time asserts on a
  # race rather than on the daemon. Observed both ways on the routing
  # sentinel leg: on one run the figure landed one second before this
  # assert read it, on the next it had not landed at all.
  #
  # So the claim is "the daemon reaches a verdict within this window",
  # which is a statement about the product; "it had one at the instant I
  # looked" is a statement about scheduling. Returns as soon as a figure
  # appears, so a healthy leg pays nothing.
  local hs turn budget samples floor method figure
  local hs_deadline=$((SECONDS + 180))
  while [ -n "$out" ] && [ "$SECONDS" -lt "$hs_deadline" ]; do
    hs="$(it_json_object "$out" host_speed)"
    case "$(printf '%s' "$hs" | grep -oE '"turn(_floor)?_seconds"[[:space:]]*:[[:space:]]*[0-9.]+' | grep -oE '[0-9.]+$' | grep -vE '^0(\.0+)?$' | head -1)" in
      "") ;;
      *)  break ;;
    esac
    sleep 5
    out="$(gx "$guest" curl -fsS --max-time 5 http://127.0.0.1:9476/waired/v1/inference/status 2>/dev/null || true)"
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
    # BLOCKING since waired-agent#579 shipped. It was soft while that was
    # open, because the absent case was a real defect every PR would have
    # gone red for. Stage 3 closed it from both ends: a host too slow to
    # measure at full depth inside the install window now publishes a
    # prefill-only BOUND instead of nothing, and this assert takes either
    # figure. Proven on run 31330389679 before the flip.
    #
    # So "no figure at all" now means what it was always supposed to mean:
    # the daemon reached no conclusion about this host, and nothing decided
    # whether a model belonged here.
    case "$figure" in
      ""|0|0.0)
        bad "no host-speed measurement published (#496): the daemon never finished measuring this host inside init, so nothing decided whether a model belonged here (waired-agent#579)"
        # Say what the daemon got as far as. it_hostspeed_evidence was
        # attached only to the init-exit-3 arm, so THIS red — the one the
        # assert exists to produce — arrived with no daemon output at all:
        # run 31605659210's macos routing-sentinel job carried zero `agent|`
        # lines, and which of "measured nothing" and "measured and could not
        # confirm it" had happened was unanswerable from the job log
        # (waired-agent#735). Counts no assert, so the tier floors do not move.
        it_hostspeed_evidence "$guest"
        ;;
      *)        ok "host speed measured (${method:-?}: turn ${turn:-0}s, floor ${floor:-0}s, against a ${budget:-?}s budget; ${samples:-0} samples)" ;;
    esac
  fi

  # Asked of the DAEMON, not of agent.json — see the darwin twin in
  # installtest-macos.sh for why the config file is the wrong source
  # (waired-agent#552). desired_state is planInitialInference's answer:
  # install-time default, persisted toggle and flag, already folded.
  # One read, two facts, so they cannot disagree: `desired_state` says the
  # toggle is off, and `host_speed.turned_inference_off` says whether the
  # #496 cutoff is what turned it off. That flag is the cutoff's own claim
  # and stops being made the moment anything else moves the toggle
  # (HostSpeedStatus), so the pair names the culprit in the red's first
  # line instead of costing a second run.
  local desired_json desired by_cutoff
  desired_json="$(gx "$guest" curl -fsS --max-time 5 http://127.0.0.1:9476/waired/v1/inference/status 2>/dev/null || true)"
  desired="$(printf '%s' "$desired_json" | grep -oE '"desired_state"[[:space:]]*:[[:space:]]*"[a-z]+"' \
    | grep -oE '"[a-z]+"$' | tr -d '"' || true)"
  by_cutoff=false
  it_json_true "$(it_json_object "$desired_json" host_speed)" turned_inference_off && by_cutoff=true
  case "$desired" in
    enabled) ok "local inference is on (mgmt API desired_state=enabled)" ;;
    "")      bad "the daemon published no desired_state — cannot tell an enabled host from a disabled one" ;;
    *)       bad "local inference is off (mgmt API desired_state=$desired; the host-speed cutoff turned it off: $by_cutoff)" ;;
  esac

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
  # That paragraph describes a TWO-state model — a number, or a 503 — and
  # there is a third (#382): the benchmark is never attempted at all, because
  # the model is not ready, and `waired init` prints its ordinary success
  # epilogue anyway. So three arms:
  #
  #   the benchmark RAN and produced nothing — a 503, an engine that never came
  #   up, an unreachable daemon. The engine is the thing to look at, and the
  #   diagnostics below say so.
  #
  #   the benchmark NEVER RAN because the model was not ready in time. Nothing
  #   is broken in the engine; the download did not finish inside init's
  #   window. This was 6 of the last 9 routing-sentinel failures, three of them
  #   on plain `main` pushes, and every one of them reported as a missing
  #   throughput figure — which sent the investigation to the engine, where
  #   there was nothing to find.
  #
  # Both stay RED. The distinction is what the red SAYS, not whether it is red:
  # a leg that could not measure did not test what it was asked to test, and a
  # model-registry that is too slow to make the deadline is a condition this
  # suite should keep reporting. What it must not do is blame the engine for it.
  #
  # `|| true`: a no-match grep exits 1 and would trip `set -e` in the sourcing
  # driver; head-closing a multi-match grep (SIGPIPE 141) would too.
  tps=""; notready=""
  if [ -f "$initlog" ]; then
    tps="$(grep -ioE '[0-9]+(\.[0-9]+)? *(tok|tokens)/s' "$initlog" | head -1 || true)"
    notready="$(grep -oE "$IT_BENCH_NOT_READY_RE" "$initlog" | head -1 || true)"
  fi
  if [ -n "$tps" ]; then
    ok "benchmark ran during init ($tps)"
  elif [ -n "$notready" ]; then
    # Which not-ready ending it was decides what the red may claim. "the
    # download, not the engine" is true of the three endings that HAVE a
    # download; on the fourth there is nothing to download, and saying it
    # anyway is the same false claim init used to print (waired-agent#736).
    # One `bad` either way — the tier floors count asserts, not arms.
    case "$notready" in
      'No model was chosen for this computer')
        bad "no model was ever selected for this host, so init's benchmark window had nothing to measure — neither the download nor the engine (\"$notready\"; $initlog)" ;;
      *)
        bad "the model was not ready inside init's benchmark window, so nothing was measured — the download, not the engine (\"$notready\"; $initlog)" ;;
    esac
    grep -iE 'download|model|pull' "$initlog" 2>/dev/null | tail -20 | sed 's/^/    init| /' || true
    # Pull-side evidence only. engine.log and the boot-benchmark slog stay on
    # the arm below: printing them HERE is exactly what made every one of these
    # failures read as an engine problem.
    gx "$guest" sh -c "OLLAMA_HOST=127.0.0.1:9475 '$IT_BUNDLED_OLLAMA_BIN' list" 2>&1 | sed 's/^/    :9475 /' || true
    gx "$guest" sh -c 'curl -fsS --max-time 10 http://127.0.0.1:9476/waired/v1/inference/status || echo "(status unreachable)"' 2>&1 | sed 's/^/    status| /' || true
    it_prepull_evidence "$guest"
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
