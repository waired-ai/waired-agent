# shellcheck shell=bash
# installtest-daemon-engine.sh — Tier-2 leg for the waired#835 §9/§11
# daemon-path setup-executor engine install.
#
# Sourced by installtest-run.sh (--daemon-engine, Tier >=2), AFTER
# installtest-enroll.sh (this file reuses _it_dev_name / IT_BUNDLED_OLLAMA_BIN
# and relies on ok()/bad()/gx()/it_* from run.sh + common.sh).
#
# WHY A SEPARATE LEG (the coverage gap it closes)
# ------------------------------------------------
# The other installtest legs enroll hands-free with `waired init
# --google-sa-login` (oidc) or `--bypass-mode`. Both of those FORCE the
# standalone enroll path (cmd/waired/main.go: the daemon path is gated on
# `!bypassMode && !googleSALogin && !renewing && daemonReachable`). On the
# standalone path the engine, when installed at all, is installed by
# install.sh (--inference) or by the interactive `configureInference` hook —
# never by the setup executor. So the waired#835 §9/§11 executor engine
# install (`ensureDaemonPathEngine` / `runSetupEngineInstall`, the resident
# `sudo waired init` attaching a lease and installing the engine the browser
# wizard asked for) was covered only by unit tests, never end to end.
#
# Since #119 the installer starts the daemon BEFORE `waired init`, so a real
# fresh first-run now takes the DAEMON path — the exact path the standalone
# enroll here never exercises. This leg drives that real path hands-free.
#
# HOW IT DRIVES THE DAEMON PATH HANDS-FREE
# ----------------------------------------
# `waired init` on the daemon path proxies a Control-Plane login session it
# creates via POST /waired/v1/login/start, then polls the daemon until the
# session goes active. The CP's OIDC direct grant (POST
# {control}/v1/login/oidc-grant, internal/controlplane/api/oidc_grant.go)
# completes ANY waiting session by id — it does not care which client created
# it. So we:
#   1. run `waired init` in the FOREGROUND *without* --google-sa-login (so it
#      takes the daemon path) and with --non-interactive (so awaitBrowserSetup
#      returns at once and the resident executor runs ensureDaemonPathEngine),
#      inference on + a tiny bundled model so an engine-less host installs one;
#   2. from a background watcher, rejoin the in-flight session (POST
#      /login/start is single-flight → returns init's session id) and complete
#      it out-of-band with a host-minted SA id_token;
#   3. still in the watcher, poll GET /waired/v1/setup/state while init
#      installs the engine, recording whether the executor lease went live
#      (executor_attached) and claimed the ollama install (install_claimed) —
#      proof the resident executor, not install.sh, drove the install.
#
# oidc mode only (mirrors installtest-macos.sh): the token is minted on the
# HOST with gcloud and the completion POST is sent from the host to the public
# CP; the guest only exposes the login session on its loopback mgmt API.

# The tiny bundled model this leg pins (~0.4 GB), so the model pull that
# follows the engine install stays cheap. Honours an IT_BUNDLED_MODEL_ID
# override set by the caller (installtest-run.sh).
IT_DAEMON_ENGINE_MODEL="${IT_BUNDLED_MODEL_ID:-qwen2.5-coder-0.5b-instruct}"

# _it_daemon_mint_token — mint the SA id_token on the host (oidc mode only).
# Echoes the token on stdout, empty (return 1) on failure. Precondition
# checks (mode / SA / gcloud) live in the caller so their it_die fires in the
# script — an it_die inside this $()-captured function would only exit the
# subshell. Callers wrap the capture in `|| true` so a mint failure surfaces
# as an empty token, not a set -e abort.
_it_daemon_mint_token() {
  local aud="$IT_OIDC_AUDIENCE"
  if [ -z "$aud" ]; then
    aud="$(curl -fsS --max-time 15 "$IT_CONTROL_URL/v1/login/oidc-grant/audience" 2>/dev/null \
      | sed -n 's/.*"audience":"\([^"]*\)".*/\1/p')"
  fi
  [ -n "$aud" ] || return 1
  gcloud auth print-identity-token \
    --impersonate-service-account="$IT_IMPERSONATE_SA" \
    --audiences="$aud" --include-email 2>/dev/null
}

# _it_daemon_setup_watcher <guest> <initlog> <token> <flagfile> — the
# background half of the daemon-path enrol. Records single-line facts into
# <flagfile> (grep'd by assert_daemon_engine), so the foreground init and this
# watcher never share shell state.
_it_daemon_setup_watcher() {
  local guest="$1" initlog="$2" tok="$3" flag="$4" url="" sess="" st _ seen_exec="" seen_claim=""
  : > "$flag"

  # (1) Discover the login session by SCRAPING the URL `waired init` prints
  # ("Sign in using this link:\n  <url>", presentLoginURL) from the tee'd
  # transcript, and take its last path segment as the session id (the same
  # lastPathSegment the standalone path uses). This is a READ — deliberately
  # not a POST /login/start, which the #838 writeGuard refuses on the TCP port
  # (mgmt writes must use the local IPC socket / named pipe).
  for _ in $(seq 1 60); do
    url="$(grep -oE 'https?://[^[:space:]]+' "$initlog" 2>/dev/null | head -1 || true)"
    if [ -n "$url" ]; then sess="${url##*/}"; sess="${sess%%[?#]*}"; fi
    [ -n "$sess" ] && break
    sleep 1
  done
  if [ -z "$sess" ]; then echo "no-session" >> "$flag"; return; fi
  echo "session=$sess" >> "$flag"

  # (2) Complete it out-of-band at the CP (public; no writeGuard there — that
  # guard is on the agent's LOCAL mgmt API, not the control plane).
  if curl -fsS --max-time 20 -X POST -H 'Content-Type: application/json' \
      -d "{\"login_session_id\":\"$sess\",\"id_token\":\"$tok\"}" \
      "$IT_CONTROL_URL/v1/login/oidc-grant" >/dev/null 2>&1; then
    echo "completed=1" >> "$flag"
  else
    echo "complete-failed" >> "$flag"
    return
  fi

  # (3) Watch the executor lease while init installs the engine (~5 min
  # budget; the foreground kills us the moment init returns). Record each
  # fact once.
  for _ in $(seq 1 150); do
    st="$(gx "$guest" curl -fsS --max-time 5 \
      http://127.0.0.1:9476/waired/v1/setup/state 2>/dev/null || true)"
    if [ -z "$seen_exec" ] && \
       printf '%s' "$st" | grep -qE '"executor_attached"[[:space:]]*:[[:space:]]*true'; then
      echo "executor_attached=1" >> "$flag"; seen_exec=1
    fi
    if [ -z "$seen_claim" ] && \
       printf '%s' "$st" | grep -qE '"install_claimed"[[:space:]]*:[[:space:]]*"ollama"'; then
      echo "install_claimed=ollama" >> "$flag"; seen_claim=1
    fi
    sleep 2
  done
}

# it_enroll_daemon_path <guest> — daemon-path enrol that reaches the setup
# executor engine install. Unlike it_enroll_guest it does NOT stop the
# service: the running-but-unenrolled daemon is what makes `waired init` take
# the daemon path.
it_enroll_daemon_path() {
  local guest="$1" name initlog flag tok watcher_pid rc
  name="$(_it_dev_name "$guest")"
  mkdir -p "$IT_LOGDIR"
  initlog="$IT_LOGDIR/init-daemon-$name.log"
  flag="$IT_LOGDIR/daemon-engine-$name.flag"

  it_log "daemon-path enrol for $guest (service left running; executor engine install)"
  [ "$IT_ENROLL_MODE" = oidc ] || it_die \
    "--daemon-engine supports IT_ENROLL_MODE=oidc only (got '$IT_ENROLL_MODE')"
  [ -n "$IT_IMPERSONATE_SA" ] || it_die \
    "IT_ENROLL_MODE=oidc needs IT_IMPERSONATE_SA (the #339 test SA)"
  command -v gcloud >/dev/null 2>&1 || it_die \
    "oidc enrol mints the SA id_token on the host; gcloud not found on PATH."
  # `|| true`: a mint failure must surface as the it_die below (empty token),
  # not as a set -e abort on the command substitution.
  tok="$(_it_daemon_mint_token || true)"
  [ -n "$tok" ] || it_die \
    "failed to mint an SA id_token — is your identity in oidc_grant_token_creators \
on $IT_IMPERSONATE_SA? (roles/iam.serviceAccountTokenCreator, or the CP audience \
endpoint is unreachable / --enable-oidc-grant is off)"

  # Fresh log so the watcher can't scrape a stale login URL from a prior run
  # (the foreground `tee` below also truncates, but the watcher may read first).
  : > "$initlog"
  _it_daemon_setup_watcher "$guest" "$initlog" "$tok" "$flag" &
  watcher_pid=$!

  # Foreground daemon-path init: NO --google-sa-login (→ daemon path),
  # --non-interactive (→ awaitBrowserSetup returns at once → the resident
  # executor runs ensureDaemonPathEngine), inference on + tiny model so an
  # engine-less host installs one. Blocks through engine install + model pull
  # + benchmark. stdin from /dev/null: a rerouted terminal must not block on a
  # prompt. Teed for the daemon-path signature assert. `if` guards `set -e`
  # around a non-zero init; PIPESTATUS[0] is init's own exit (not tee's).
  it_log "running daemon-path 'waired init' (fg) in $guest (cp=$IT_CONTROL_URL, model=$IT_DAEMON_ENGINE_MODEL)"
  if gx "$guest" sh -c "WAIRED_NO_EMOJI=1 waired init \
        --control '$IT_CONTROL_URL' --device-name '$name' \
        --inference-enabled=true --inference-bundled-model-id='$IT_DAEMON_ENGINE_MODEL' \
        --non-interactive --skip-integration --state-dir /var/lib/waired </dev/null 2>&1" \
      | tee "$initlog"; then
    rc=0
  else
    rc="${PIPESTATUS[0]}"
  fi

  kill "$watcher_pid" 2>/dev/null || true
  wait "$watcher_pid" 2>/dev/null || true

  # No post-init restart (unlike it_enroll_guest): the daemon enrolled AND
  # installed the engine in place, so it is already serving the enrolled state
  # + the installed engine. A restart would only risk a transient no_engine
  # read while the inference subsystem re-profiles.
  if [ "$rc" -ne 0 ]; then
    it_warn "daemon-path 'waired init' exited $rc in $guest — asserts below will surface what landed"
  fi
}

# assert_daemon_engine <guest> — verify the daemon-path executor installed the
# engine on an engine-less host. The regression bar (item 5): pre-N3 an
# engine-less daemon-path first-run stayed engine-less and engine_install was
# red forever; N3 makes the resident executor install it.
assert_daemon_engine() {
  local guest="$1" name initlog flag out state claim
  name="$(_it_dev_name "$guest")"
  initlog="$IT_LOGDIR/init-daemon-$name.log"
  flag="$IT_LOGDIR/daemon-engine-$name.flag"

  # 1. The enrol took the DAEMON path — the only path with a setup executor.
  if grep -q "signing in via the daemon" "$initlog" 2>/dev/null; then
    ok "init took the daemon path (setup-executor-capable first-run)"
  else
    bad "init did NOT take the daemon path (executor engine install not exercised)"
    sed 's/^/    /' "$initlog" 2>/dev/null | tail -20 >&2 || true
  fi

  # 2. The out-of-band OIDC completion drove the daemon login to active.
  if grep -q '^completed=1' "$flag" 2>/dev/null; then
    ok "daemon login completed out-of-band via the OIDC grant"
  else
    bad "out-of-band OIDC completion did not report success (flag: $(tr '\n' ' ' < "$flag" 2>/dev/null))"
  fi

  # 3. The resident executor lease went live while init installed the engine.
  if grep -q '^executor_attached=1' "$flag" 2>/dev/null; then
    ok "setup executor lease was live during setup (executor_attached)"
  else
    bad "never observed executor_attached — the executor engine-install path was not reached"
  fi

  # 4. The lease claimed the ollama install (executor drove it, not install.sh).
  #    Non-fatal: the 2 s poll can miss a very short install window.
  if grep -q '^install_claimed=ollama' "$flag" 2>/dev/null; then
    ok "executor claimed the ollama install (install_claimed=ollama)"
  else
    it_warn "did not catch install_claimed=ollama in the 2 s poll (short install window?) — non-fatal"
  fi

  # 5. THE REGRESSION BAR: an engine-less daemon-path host ends up WITH an
  #    engine. install.sh ran with --skip-ollama, so only the executor could
  #    have put it here.
  if gx "$guest" test -x "$IT_BUNDLED_OLLAMA_BIN"; then
    ok "bundled ollama installed by the daemon-path executor ($IT_BUNDLED_OLLAMA_BIN)"
  else
    bad "no engine after a daemon-path first-run (executor install did not land — pre-N3 behaviour)"
    gx "$guest" journalctl -u waired-agent --no-pager -n 30 2>&1 | sed 's/^/    /' || true
  fi

  # 6. The inference subsystem left the no_engine state.
  out="$(gx "$guest" curl -fsS --max-time 5 http://127.0.0.1:9476/waired/v1/inference/status 2>/dev/null || true)"
  state="$(printf '%s' "$out" | grep -oE '"subsystem_state"[[:space:]]*:[[:space:]]*"[a-z_]+"' | head -1 | grep -oE '"[a-z_]+"$' | tr -d '"')"
  case "$state" in
    ""|no_engine) bad "inference subsystem still reports '${state:-unreachable}' (engine not installed)" ;;
    *) ok "inference subsystem left no_engine (state=$state)" ;;
  esac

  # 7. No stuck install claim after init (§9-4: no never-resolving spinner).
  claim="$(gx "$guest" curl -fsS --max-time 5 http://127.0.0.1:9476/waired/v1/setup/state 2>/dev/null \
    | sed -n 's/.*"install_claimed"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
  if [ -z "$claim" ]; then
    ok "no stuck executor install claim after init (install_claimed cleared)"
  else
    bad "executor install claim still set after init (install_claimed=$claim; stuck spinner)"
  fi
}
