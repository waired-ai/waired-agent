#!/usr/bin/env bash
# installtest-run.sh — run the *working-tree* installer end-to-end inside
# an ephemeral LXD guest (real systemd as PID 1) and assert the result.
#
# Tier 1 (default): install.sh -> apt -> systemd. Asserts the package
#   installs, the waired user/state-dir/permissions are right, the unit
#   is enabled+active, the control URL lands in agent.env, and a second
#   install.sh run (update path, real repo) is a clean no-op — the #328
#   FLAG_YES path that the dash matrix can only approximate.
# Tier 2: + headless enroll against a Control Plane, asserting the #335
#   state-dir/ownership/daemon chain (identity under /var/lib/waired owned
#   by waired, daemon serves its mgmt API). Enrol mode = IT_ENROLL_MODE
#   (authkey|interactive); see lib/installtest-enroll.sh.
# Tier 3: + WireGuard data plane on a real kernel (LXD VM): two guests
#   enroll through the full installer and ping over the overlay.
#
# --inference: exercise the full first-run journey on CPU — `waired init`
#   force-enables inference, so it installs the engine on the daemon path
#   (#138: install.sh pre-installs nothing on any OS now) and its deploy
#   phase pulls the bundled model and runs the end-of-init benchmark. Pairs
#   with Tier 2 (IT_ENROLL_MODE=authkey against the real app.dev.waired.net
#   is the no-human path). GPU not required.
#
# --integration (--local/native only; #496): implies --inference but PINS the
#   withheld 350M as the bundled model (deploy pulls ~0.7 GB, not the 7B), then runs
#   the coding-agent routing sentinel (internal/e2e/integration, -tags
#   integration) — each leg drives the gateway surface its tool config targets
#   and asserts via the observability event ring that the completion was served
#   LOCALLY and did not fail open to real Anthropic.
#
# --daemon-engine (waired#835 §9/§11): drive the DAEMON-path first-run so the
#   resident `waired init` executor installs the engine on an engine-less host
#   under a browser wizard — runSetupEngineInstall rather than --inference's
#   wizard-less ensureDaemonPathEngine. install.sh keeps
#   --skip-ollama (engine ABSENT), enrol completes the daemon's login session
#   out-of-band via the OIDC grant (lib/installtest-daemon-engine.sh), and a
#   tiny bundled model keeps the trailing pull cheap. Pairs with Tier 2; its
#   own mode (not combinable with --inference/--integration).
#
# --engine-only (waired-agent#590): the operator installs the AI software and
#   chooses NOT to download a model. Enrols exactly like the lean leg, then
#   runs ONE interactive init (--inference-enabled=true, "0" on stdin) so the
#   #586 model picker is reached and answered with "don't download a model
#   now". Asserts that state is a FINISHED install — exit 0, no failure box,
#   an engine on disk, and a standing choice the daemon keeps across a
#   restart. Its own mode, and nightly: it installs a real engine from a
#   release asset. Mirrored by installtest-macos.sh --engine-only and
#   installtest-windows.ps1 -EngineOnly; all three run in the same nightly job.
#
# A system container is used for Tier 1/2 (fast); Tier 3 forces a VM.
#
# Usage:
#   bash scripts/dev/installtest-run.sh                 # Tier 1, container
#   bash scripts/dev/installtest-run.sh --tier 2        # + headless enroll
#   bash scripts/dev/installtest-run.sh --tier 2 --inference   # + Ollama/model/benchmark (CPU)
#   bash scripts/dev/installtest-run.sh --tier 2 --integration --local  # + routing sentinel (350M)
#   bash scripts/dev/installtest-run.sh --tier 2 --daemon-engine  # + daemon-path executor engine install (waired#835)
#   bash scripts/dev/installtest-run.sh --tier 2 --engine-only    # + engine installed, no model chosen (waired-agent#590)
#   bash scripts/dev/installtest-run.sh --tier 3        # + data plane (2 VMs)
#   bash scripts/dev/installtest-run.sh --keep          # don't delete the guest
#   bash scripts/dev/installtest-run.sh --name foo --image ubuntu:22.04
set -euo pipefail

ROOT="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)"
# shellcheck source=scripts/dev/lib/installtest-common.sh
source "$ROOT/scripts/dev/lib/installtest-common.sh"

TIER=1
KEEP=0
WITH_TRAY=0
USE_VM=0
INFER=0
INTEG=0
DAEMON_ENGINE=0
ENGINE_ONLY=0
NAME="g1"
while [ $# -gt 0 ]; do
  case "$1" in
    --tier) shift; TIER="${1:?--tier needs N}" ;;
    --tier=*) TIER="${1#--tier=}" ;;
    --keep) KEEP=1 ;;
    --with-tray) WITH_TRAY=1 ;;
    --inference) INFER=1 ;;
    --integration) INTEG=1; INFER=1 ;;   # routing sentinel rides the inference engine
    --daemon-engine) DAEMON_ENGINE=1 ;;  # waired#835 §9/§11 daemon-path executor engine install
    --engine-only) ENGINE_ONLY=1 ;;      # waired-agent#590 engine installed, no model chosen
    --vm) USE_VM=1 ;;
    --local) IT_LOCAL=1 ;;
    --name) shift; NAME="${1:?--name needs a value}" ;;
    --image) shift; IT_IMAGE="${1:?--image needs a value}" ;;
    -h|--help) sed -n '2,48p' "$0"; exit 0 ;;
    *) it_die "unknown argument: $1 (try --help)" ;;
  esac
  shift
done
[ "$TIER" -ge 3 ] && USE_VM=1   # data plane needs a real kernel
# Force-enable inference for the enrol step (read by lib/installtest-enroll.sh).
[ "$INFER" = 1 ] && export IT_INFERENCE_ENABLED=true
# The routing sentinel (#496) pins the withheld 350M as the bundled model so
# the deploy pulls ~0.7 GB, not the 7B — cheap enough for a per-PR Linux leg.
# It is marked internal_only in the catalog (waired-ai/waired-agent#322): a real
# catalog entry, so the daemon resolves the pin, but withheld from every picker
# and catalog view so it is never offered to anyone.
[ "$INTEG" = 1 ] && export IT_BUNDLED_MODEL_ID="${IT_BUNDLED_MODEL_ID:-granite4-350m}"

# --daemon-engine (waired#835 §9/§11) is its own mode: unlike --inference it
# keeps install.sh's --skip-ollama (engine ABSENT), so only the daemon-path
# executor can install one. It pins the same tiny model for a cheap trailing
# pull. Not combinable with --inference/--integration (different enrol paths).
if [ "$DAEMON_ENGINE" = 1 ]; then
  { [ "$INFER" = 1 ] || [ "$INTEG" = 1 ]; } && it_die \
    "--daemon-engine is its own mode; do not combine it with --inference/--integration"
  [ "$TIER" -ge 2 ] || it_die "--daemon-engine needs --tier 2 (it enrols to reach the executor)"
  export IT_BUNDLED_MODEL_ID="${IT_BUNDLED_MODEL_ID:-granite4-350m}"
  # This leg's enrol asks for local inference: it_enroll_daemon_path passes
  # --inference-enabled=true directly (lib/installtest-daemon-engine.sh),
  # because installing an engine is the whole point of the leg. That value
  # never reached IT_INFERENCE_ENABLED, which only --inference/--integration
  # set — and assert_reinit_resumes builds its argv from THAT variable, so the
  # re-init told the same host to turn local inference back off, after the engine was
  # installed and before assert_daemon_engine read the subsystem state
  # (#748). Measured on run 31605659210: the Linux leg ended `disabled` and
  # still passed, because the assert accepted anything that was not
  # `no_engine`.
  #
  # Setting it here restores the rule assert_reinit_resumes states for
  # itself — "the same --inference-enabled the enrol used ... leave the
  # host's local-AI posture exactly as it found it, in EITHER direction" —
  # rather than adding an exception to it. Windows reached the same place
  # from the other side in #744.
  #
  # Only the re-init argv moves: the variable's three other readers are all
  # inside it_enroll_guest(), which this leg does not call (it uses
  # it_enroll_daemon_path).
  export IT_INFERENCE_ENABLED=true
fi

# --engine-only (waired-agent#590) is its own mode for the same reason: it
# keeps install.sh's --skip-ollama so the engine arrives through init, and its
# one init is INTERACTIVE, which every other mode's --non-interactive would
# make unreachable.
if [ "$ENGINE_ONLY" = 1 ]; then
  { [ "$INFER" = 1 ] || [ "$INTEG" = 1 ] || [ "$DAEMON_ENGINE" = 1 ]; } && it_die \
    "--engine-only is its own mode; do not combine it with --inference/--integration/--daemon-engine"
  [ "$TIER" -ge 2 ] || it_die "--engine-only needs --tier 2 (it enrols before it asks about models)"
fi

# --local installs waired ON THIS HOST as root (apt + systemd + a service
# user + a running daemon). Safe only on a disposable machine — guard so a
# developer can't nuke their workstation by accident; CI opts in explicitly.
if [ "$IT_LOCAL" = 1 ]; then
  [ "${IT_ALLOW_LOCAL_DESTRUCTIVE:-0}" = 1 ] || it_die \
    "--local root-installs waired on THIS host. Set IT_ALLOW_LOCAL_DESTRUCTIVE=1 to confirm \
(CI does); use the default LXD path on a workstation."
  [ "$TIER" -le 2 ] || it_die "--local supports Tier 1-2 only (Tier 3 needs two guests; use the LXD path)."
  # install.sh now starts UN-rooted here (see run_install), so sudo is what
  # raises the privilege rather than the caller. Say so up front: without a
  # passwordless sudoers entry the first $SUDO call blocks on a password
  # prompt, and common_daemon_owns_log_level would sit on thirty of them.
  # installtest-macos.sh:1267 has asserted the same precondition since the
  # macOS leg started un-rooted; the Linux leg never needed it until now.
  sudo -n true 2>/dev/null || it_die \
    "passwordless sudo required (--local starts install.sh un-rooted and lets it elevate)"
fi

it_require curl
[ "$IT_LOCAL" = 1 ] || it_require lxc
export PATH="$HOME/go/bin:$PATH"

# Pull host-side knobs (WAIRED_APT_*, IT_GW, IT_CP_URL) written by host-up.
[ -f "$IT_WORKDIR/env" ] || it_die "no $IT_WORKDIR/env — run installtest-host-up.sh first"
# shellcheck disable=SC1091
source "$IT_WORKDIR/env"
: "${IT_GW:?env missing IT_GW — re-run installtest-host-up.sh}"
[ "$IT_LOCAL" = 1 ] || it_ensure_bridge
it_wait_url "$WAIRED_APT_BASE_URL/key.asc" 10 \
  || it_die "apt repo not reachable at $WAIRED_APT_BASE_URL — run installtest-host-up.sh"

# Enrol target: the real dogfood CP by default (set IT_CONTROL_URL to
# override, e.g. a PR preview). Used both as install.sh's --dev URL
# (written to agent.env) and as the Tier-2/3 enrol --control.
CONTROL_URL="${IT_CONTROL_URL:-https://app.dev.waired.net}"

# Where run_install tees the installer's own output. Overwritten by each
# install in the leg, so an assert reads the run it belongs to and nothing
# older — the same reason installtest-macos.sh keeps INSTALLLOG.
INSTALL_LOG="$IT_WORKDIR/install.log"

PASS=0; FAIL=0; SKIP=0
ok()   { printf '\033[1;32m[installtest]  ok \033[0m %s\n' "$*"; PASS=$((PASS+1)); }
bad()  { printf '\033[1;31m[installtest] FAIL\033[0m %s\n' "$*" >&2; FAIL=$((FAIL+1)); }
# skip <reason> — an assert deliberately not run. Counted and printed in the
# summary: a skip nobody can see is how a leg quietly stops testing anything
# (#215).
skip() { printf '\033[1;33m[installtest] SKIP\033[0m %s\n' "$*"; SKIP=$((SKIP+1)); }
# it_die_hook (declared in lib/installtest-common.sh) — count the die as a
# failure and print the tally, so a leg that dies mid-run still reports what it
# got through. Deliberately does NOT run the assert-count floor: the floor's
# question ("did a block stop executing without saying so?") is already
# answered by the die's own reason, and a second FAIL line would bury it.
#
# Defined here, not earlier, because it reads the counters above. The argument
# and preflight it_die calls further up therefore keep the library's no-op
# hook — correctly: no assert has run at that point, so there is no tally to
# print, and the FAIL prefix they inherit is the whole of what they need.
it_die_hook() {
  FAIL=$((FAIL+1))
  echo >&2
  it_step "Tier ${TIER:-?} summary (died before finishing): $PASS passed, $FAIL failed, $SKIP skipped"
}
# gx <guest> <cmd...> — run a privileged command in the test environment.
# LXD: `lxc exec` (root in the guest). --local: `sudo` on this host (the LXD
# guest's root maps to host root). The <guest> arg is ignored in --local.
gx() {
  if [ "$IT_LOCAL" = 1 ]; then sudo "${@:2}"; else lxc exec "$1" -- "${@:2}"; fi
}

# Launch a clean guest on the harness bridge and wait until systemd +
# outbound DNS are ready. Echoes nothing; dies on failure.
#
# For --inference we cap the guest's memory (IT_GUEST_MEMORY, default 16GiB):
# LXD's lxcfs virtualizes /proc/meminfo, so without a cap the guest sees the
# *host's* total RAM and waired's model-selection pre-caches an oversized
# model (e.g. a 120B variant on a 120GB host) — unrealistic for a CPU host.
# Set IT_GUEST_MEMORY= (empty) to disable the cap.
launch_guest() {
  local guest="$1" extra=()
  if [ "$IT_LOCAL" = 1 ]; then
    it_log "local mode: installing in place on $(uname -n) (no guest launch)"
    return 0
  fi
  [ "$USE_VM" = 1 ] && extra+=(--vm)
  if { [ "$INFER" = 1 ] || [ "$DAEMON_ENGINE" = 1 ] || [ "$ENGINE_ONLY" = 1 ]; } && [ -n "${IT_GUEST_MEMORY-16GiB}" ]; then
    # Applied at LAUNCH, not via a post-launch `lxc config set`: a VM's
    # memory is fixed at boot, so the old post-launch set was silently
    # ineffective (error swallowed by `|| true`) and a VM guest ran the
    # whole inference journey inside LXD's 1GiB default — lxd-agent was
    # the OOM victim and every in-flight `lxc exec` died with 129
    # mid-download. Containers apply the cap live either way.
    extra+=(-c "limits.memory=${IT_GUEST_MEMORY:-16GiB}")
    [ "$USE_VM" = 1 ] && extra+=(-c "limits.cpu=${IT_GUEST_CPU:-4}")
  fi
  lxc delete --force "$guest" >/dev/null 2>&1 || true
  it_log "launching $guest ($IT_IMAGE$([ "$USE_VM" = 1 ] && echo ', vm'))"
  lxc launch "$IT_IMAGE" "$guest" --network "$IT_BRIDGE" "${extra[@]}" >/dev/null
  it_wait_guest_ready "$guest" || it_die "$guest never became ready"
}

# IT_AGENT_ENV_EXTRA — newline- or semicolon-separated KEY=VALUE pairs appended
# to /etc/waired/agent.env (the systemd EnvironmentFile install.sh already
# writes), then the daemon is restarted so it picks them up.
#
# The reason this exists: a nightly leg needs to pin an agent-side knob the
# auto path would not choose on that host. Concretely (waired-agent#29), the
# serve tuning now requests a quantized KV cache + flash attention only where
# it buys context, so CPU-only hosts stop exercising that combination — and
# every CI leg that runs ollama at all is CPU-only (the GPU runner is
# vLLM-only). One nightly leg pins WAIRED_OLLAMA_KV_CACHE_TYPE=q8_0 so the
# combination keeps a real-engine exercise deliberately, rather than by
# accident as before.
#
# No-op when unset, which is every per-PR run.
apply_agent_env_extra() {
  local guest="$1"
  [ -n "${IT_AGENT_ENV_EXTRA:-}" ] || return 0
  it_log "appending IT_AGENT_ENV_EXTRA to /etc/waired/agent.env"
  printf '%s\n' "$IT_AGENT_ENV_EXTRA" | tr ';' '\n' | while IFS= read -r kv; do
    [ -n "$kv" ] || continue
    it_log "  agent.env += $kv"
    gx "$guest" sh -c "printf '%s\n' '$kv' >> /etc/waired/agent.env"
  done
  gx "$guest" systemctl restart waired-agent
}

# Run the working-tree install.sh inside the guest, as root, against the
# local apt repo. We exercise the canonical dogfood one-liner shape
# (`--dev`, resolving to CONTROL_URL via WAIRED_DEV_CONTROL_URL). Tier 1
# uses --no-init (enrol is a separate Tier-2 step).
#
# --skip-ollama by default, dropped for --inference. Since #138 that flag no
# longer decides whether install.sh downloads an engine — it never does — it
# decides what install.sh would tell `waired init`, and with --no-init here
# even that never leaves the script. It stays because the leg's own enrol step
# runs init directly, and the flag's two arms (opt-out wording vs the
# "installed at sign-in" banner) are still worth walking through as a pair.
#
# HOW the installer is started is itself under test (waired-agent#990). The
# shape a person gets from the documented one-liner is `curl … | sh` as an
# ordinary user: install.sh starts un-rooted, common_elevate takes its
# SUDO=sudo arm (install.sh:299-301), and every privileged step after that is
# a real sudo exec with env_reset. Starting it as `sudo env … sh install.sh`
# takes the OTHER arm — id -u is 0, SUDO is the empty word — so the ~50 $SUDO
# call sites, the "Ask for administrator rights" notice, and everything
# downstream of sudo's environment reset were never exercised at all.
#
# So --local now starts it un-rooted, and IT_INSTALL_AS_ROOT=1 selects the
# already-root shape. Both are real deployments (`curl | sh` as a user vs. a
# root shell), and the leg runs both: the un-rooted one carries the full
# assert suite, the root one re-installs at the end. The LXD arm is a true
# root login (no SUDO_USER at all) and stays as it is — a third real shape.
run_install() {
  local guest="$1"; shift
  local notray=WAIRED_NO_TRAY=1
  [ "$WITH_TRAY" = 1 ] && notray=
  local ollama_arg=--skip-ollama
  [ "$INFER" = 1 ] && ollama_arg=   # the CPU-inference journey wants an engine
  local as_root="${IT_INSTALL_AS_ROOT:-0}" how=un-rooted
  [ "$as_root" = 1 ] && how="root shell"
  it_log "running install.sh${IT_LOCAL:+ (local, $how)} in $guest (--dev -> $CONTROL_URL${ollama_arg:+, }${ollama_arg:-, +ollama})"
  # The env knobs point install.sh at the harness-built signed apt repo; it
  # then runs its real `apt-get install waired` path (#328 update path on a
  # re-run). --local runs it straight on this host; LXD pushes + execs.
  #
  # The output is teed to INSTALL_LOG because two asserts read it: what the
  # installer PRINTED is the only place the un-rooted arm announces itself
  # (install.sh:1086-1087), and a notice is not observable through the
  # installed state afterwards.
  if [ "$IT_LOCAL" = 1 ]; then
    local sudo_prefix=()
    [ "$as_root" = 1 ] && sudo_prefix=(sudo)
    # shellcheck disable=SC2086  # $notray/$ollama_arg are word-split on purpose
    "${sudo_prefix[@]}" env \
      WAIRED_APT_BASE_URL="$WAIRED_APT_BASE_URL" \
      WAIRED_APT_KEY_URL="$WAIRED_APT_KEY_URL" \
      WAIRED_APT_SUITE="$WAIRED_APT_SUITE" \
      WAIRED_APT_COMPONENT="$WAIRED_APT_COMPONENT" \
      WAIRED_DEV_CONTROL_URL="$CONTROL_URL" \
      $notray \
      sh "$ROOT/packaging/install/install.sh" --no-init $ollama_arg --dev "$@" \
      2>&1 | tee "$INSTALL_LOG"
    return
  fi
  lxc file push "$ROOT/packaging/install/install.sh" "$guest/root/install.sh" >/dev/null
  # shellcheck disable=SC2086  # $notray/$ollama_arg are word-split on purpose
  lxc exec "$guest" -- env \
    WAIRED_APT_BASE_URL="$WAIRED_APT_BASE_URL" \
    WAIRED_APT_KEY_URL="$WAIRED_APT_KEY_URL" \
    WAIRED_APT_SUITE="$WAIRED_APT_SUITE" \
    WAIRED_APT_COMPONENT="$WAIRED_APT_COMPONENT" \
    WAIRED_DEV_CONTROL_URL="$CONTROL_URL" \
    $notray \
    sh /root/install.sh --no-init $ollama_arg --dev "$@" \
    2>&1 | tee "$INSTALL_LOG"
}

# Poll systemctl is-active up to ~15s (the daemon may take a beat to
# settle after `enable --now`).
wait_service_active() {
  local guest="$1" _ st
  for _ in $(seq 1 15); do
    st=$(gx "$guest" systemctl is-active waired-agent 2>/dev/null || true)
    [ "$st" = active ] && return 0
    sleep 1
  done
  return 1
}

# wait_log_level echoes the daemon's answer to a log-level read once it is
# answering over its IPC socket, or nothing if it never does.
#
# `waired config log-level` adds a parenthetical ("(saved; the background
# service isn't running)") when it fell back to reading agent.json — the case that must not be
# mistaken for a live read, since it is also the branch that would write the
# file from the wrong user. Polling the real read (rather than
# `systemctl is-active`) is what makes the socket, not just the process, the
# thing under test.
wait_log_level() {
  local guest="$1" _ out
  for _ in $(seq 1 30); do
    out=$(gx "$guest" waired config log-level 2>/dev/null || true)
    case "$out" in
      "Log level: "*"("*) : ;;
      "Log level: "*) printf '%s' "$out"; return 0 ;;
    esac
    sleep 1
  done
  return 1
}

assert_tier1() {
  local guest="$1" v
  gx "$guest" dpkg -s waired >/dev/null 2>&1 && ok "package waired installed" || bad "package waired NOT installed"
  gx "$guest" test -f /lib/systemd/system/waired-agent.service && ok "systemd unit present" || bad "systemd unit missing"
  v=$(gx "$guest" systemctl is-enabled waired-agent 2>/dev/null || true)
  [ "$v" = enabled ] && ok "service enabled (is-enabled=$v)" || bad "service not enabled (is-enabled=$v)"
  if wait_service_active "$guest"; then ok "service active"; else
    bad "service not active"; gx "$guest" systemctl --no-pager -l status waired-agent 2>&1 | sed 's/^/    /' || true
    gx "$guest" journalctl -u waired-agent --no-pager -n 30 2>&1 | sed 's/^/    /' || true
  fi
  gx "$guest" id waired >/dev/null 2>&1 && ok "service user 'waired' exists" || bad "service user 'waired' missing"
  v=$(gx "$guest" stat -c '%U:%G' /var/lib/waired 2>/dev/null || true)
  [ "$v" = "waired:waired" ] && ok "state dir owned by waired:waired" || bad "state dir owner = $v (want waired:waired)"
  # postinst creates 0750; the daemon tightens the tree to 0700 at boot
  # (secrets.go). Either is fine — the invariant is "no world access".
  v=$(gx "$guest" stat -c '%a' /var/lib/waired 2>/dev/null || true)
  case "$v" in
    700|750) ok "state dir not world-accessible (mode 0$v)" ;;
    *) bad "state dir mode 0$v exposes world bits (want owner-only: 0700/0750)" ;;
  esac
  if gx "$guest" grep -q "^WAIRED_CONTROL_URL=$CONTROL_URL\$" /etc/waired/agent.env 2>/dev/null; then
    ok "control URL written to agent.env"
  else
    bad "control URL not in agent.env"; gx "$guest" cat /etc/waired/agent.env 2>&1 | sed 's/^/    /' || true
  fi
  # waired-agent#801: --log-level must NOT reach /etc/waired/agent.env. The
  # unit reads that file as its EnvironmentFile, and $WAIRED_LOG_LEVEL
  # outranks agent.json at every boot — which is what made a runtime
  # `waired config log-level` revert on every restart. The install-time level
  # is a persisted setting now, so the three asserts are "it did not land in
  # the service definition", "it did land in the daemon", and — the
  # regression bar — "a runtime change survives a restart".
  if gx "$guest" grep -q '^WAIRED_LOG_LEVEL=' /etc/waired/agent.env 2>/dev/null; then
    bad "agent.env still pins WAIRED_LOG_LEVEL; a runtime change will revert on the next restart (waired-agent#801)"
  else
    ok "agent.env pins no log level (waired-agent#801)"
  fi
  v=$(wait_log_level "$guest" || true)
  case "$v" in
    "Log level: debug") ok "--log-level debug reached the daemon as the persisted level" ;;
    "") bad "the daemon never answered a log-level read, so the install-time level could not be checked" ;;
    *) bad "--log-level debug did not become the persisted level: [$v]" ;;
  esac
  # A third value — not the installed debug, not the built-in info — so
  # neither "nothing changed" nor "fell back to the default" can pass.
  gx "$guest" waired config log-level warn >/dev/null 2>&1 || true
  gx "$guest" systemctl restart waired-agent || true
  v=$(wait_log_level "$guest" || true)
  if [ "$v" = "Log level: warn" ]; then
    ok "a runtime log-level choice survives a service restart (waired-agent#801)"
  else
    bad "a runtime log-level choice did not survive a restart: [$v] (waired-agent#801)"
  fi
  # Leave the guest at the level the rest of the leg was written against.
  gx "$guest" waired config log-level debug >/dev/null 2>&1 || true
}

# assert_start_shape <guest> <elevated|un-rooted> — assert install.sh took the
# branch this arm meant it to take, read from what it printed.
#
# show_install_summary prints "Ask for administrator rights" only when
# id -u is non-zero (install.sh:1086-1087), and confirm_proceed prints the
# summary only on a FRESH install (:1101-1104) — so this has to read the log
# of the install it belongs to, before assert_idempotent's re-run overwrites
# it. The literal is product output: if that line is reworded, this assert
# moves with it in the same change.
#
# Nothing in the installed state records which branch ran, which is exactly
# why the two shapes could diverge unnoticed for as long as they did.
assert_start_shape() {
  local want="$1" seen=absent
  grep -q 'Ask for administrator rights' "$INSTALL_LOG" 2>/dev/null && seen=present
  case "$want" in
    un-rooted)
      [ "$seen" = present ] \
        && ok "install.sh started un-rooted and said so (it elevates itself)" \
        || bad "install.sh did not print the administrator-rights notice — it was already root, so the SUDO=sudo arm never ran (waired-agent#990)"
      ;;
    elevated)
      [ "$seen" = absent ] \
        && ok "install.sh started from a root shell (no administrator-rights notice)" \
        || bad "install.sh printed the administrator-rights notice from a root shell"
      ;;
  esac
}

# it_ensure_tray_pkg installs waired-tray on demand for the #1031 asserts.
#
# The leg installs with WAIRED_NO_TRAY=1 (run_install), so the package is not
# there by default -- and an assert that silently skipped on that would be the
# exact "green because it did nothing" shape CLAUDE.md warns about; it is how
# the first version of this assert passed while testing nothing. Pulling the
# package in here rather than flipping the whole leg to --with-tray keeps the
# install shape the other 47 asserts were written against.
#
# Echoes nothing; returns non-zero when the package cannot be had, so the
# caller reports a skip with a reason instead of a false pass.
it_ensure_tray_pkg() {
  local guest="$1"
  [ -x /usr/bin/waired-tray ] && return 0
  gx "$guest" env DEBIAN_FRONTEND=noninteractive apt-get install -y waired-tray \
    >/tmp/it-tray-install.log 2>&1 || return 1
  [ -x /usr/bin/waired-tray ]
}

# assert_root_shell_install <guest> — the OTHER real deployment.
#
# The leg's primary install starts un-rooted, the way the documented
# `curl … | sh` one-liner does. A root shell (`sudo sh install.sh`, or a
# provisioning script running as root) is just as real and takes a different
# branch of common_elevate, so it gets a fresh install of its own here rather
# than only ever being reasoned about.
#
# Last in the leg, and lean-config only: it purges the host to get back to a
# fresh-install state, which would throw away the engine and weights the
# --inference/--daemon-engine/--engine-only legs spent their run building.
assert_root_shell_install() {
  local guest="$1"
  if [ "$IT_LOCAL" != 1 ]; then
    skip "root-shell install arm needs --local (the LXD arm is already a root login)"
    return
  fi
  # Hand the device back before purging it, so the primary arm's enrolment
  # does not outlive this leg on the Control Plane.
  command -v it_logout_guest >/dev/null 2>&1 && it_logout_guest "$guest"

  # waired-agent#1031: the uninstall has to take the running Waired app with
  # it. Start a real tray first, so the assert below is on a live process and
  # not on the plan.
  #
  # dbus-run-session is required rather than nice-to-have. With no session bus
  # fyne.io/systray's nativeStart logs and gives up, and the shutdown then
  # takes a degenerate path through a nil connection -- so a tray started
  # without one would be measuring something other than what a desktop user
  # has. A runner without it reports the skip rather than a false pass.
  #
  # The tray runs as the invoking user, not under gx/sudo: it is a desktop
  # process, and the point is that the ROOT uninstaller reaches another user's
  # tray. Its per-user autostart entry goes in beside it -- nothing owned that
  # file, so every uninstall used to leave it pointing at a binary that was
  # about to be deleted.
  local tray_pid='' tray_desktop="$HOME/.config/autostart/waired-tray.desktop"
  if ! command -v dbus-run-session >/dev/null 2>&1; then
    skip "tray-stop assert needs dbus-run-session (waired-agent#1031)"
  elif ! it_ensure_tray_pkg "$guest"; then
    bad "could not install waired-tray for the #1031 assert"
    sed 's/^/    /' /tmp/it-tray-install.log >&2 || true
  else
    mkdir -p "$(dirname "$tray_desktop")"
    printf '[Desktop Entry]\nType=Application\nExec=waired-tray\n' > "$tray_desktop"
    setsid dbus-run-session -- /usr/bin/waired-tray >/tmp/it-tray.log 2>&1 &
    for _ in $(seq 1 20); do
      tray_pid="$(pgrep -x waired-tray | head -1 || true)"
      [ -n "$tray_pid" ] && break
      sleep 0.5
    done
    if [ -n "$tray_pid" ]; then
      it_log "started waired-tray (PID $tray_pid) before the uninstall"
    else
      bad "could not start waired-tray for the #1031 assert"; sed 's/^/    /' /tmp/it-tray.log >&2 || true
    fi
  fi

  it_log "purging waired from $guest to get back to a fresh-install state"
  if ! gx "$guest" sh "$ROOT/packaging/install/uninstall.sh" --clean --yes >/tmp/it-uninstall.log 2>&1; then
    bad "uninstall.sh --clean --yes failed (exit $?)"; sed 's/^/    /' /tmp/it-uninstall.log >&2 || true
    return
  fi
  if gx "$guest" dpkg -s waired >/dev/null 2>&1; then
    bad "waired is still installed after uninstall.sh --clean"
    return
  fi
  ok "uninstall.sh --clean removed the waired package"

  # THE REGRESSION BAR for waired-agent#1031: the process, not the plan.
  if [ -n "$tray_pid" ]; then
    if kill -0 "$tray_pid" 2>/dev/null; then
      bad "waired-tray (PID $tray_pid) survived uninstall.sh --clean (waired-agent#1031)"
      pkill -x waired-tray 2>/dev/null || true
    else
      ok "uninstall.sh --clean stopped the running Waired app (waired-agent#1031)"
    fi
    # And it said which, rather than doing it silently -- the owner ruling in
    # docs/decisions/20260821/0228-uninstall-removes-what-is-running.md.
    if grep -qF "Stopping the Waired app (waired-tray, PID $tray_pid)" /tmp/it-uninstall.log; then
      ok "the uninstall named the app it stopped and its PID"
    else
      bad "the uninstall stopped the app without saying so (PID $tray_pid)"
    fi
    if [ -e "$tray_desktop" ]; then
      bad "the per-user autostart entry survived the uninstall: $tray_desktop"
      rm -f "$tray_desktop"
    else
      ok "uninstall.sh removed the per-user autostart entry (waired-agent#1031)"
    fi
  fi

  IT_INSTALL_AS_ROOT=1 run_install "$guest" --log-level debug
  assert_start_shape elevated
  if gx "$guest" systemctl is-active --quiet waired-agent; then
    ok "root-shell install leaves waired-agent active"
  else
    bad "waired-agent is not active after the root-shell install"
  fi

  # The OTHER uninstall route, and the one install.sh's own done banner
  # prints: `sudo apt purge waired waired-tray`. No script runs on that path,
  # so the stop has to come from the tray package's prerm (waired-agent#1031).
  # Last in the leg, because it takes waired-tray away.
  if ! command -v dbus-run-session >/dev/null 2>&1; then
    skip "apt-remove tray assert needs dbus-run-session"
  elif ! it_ensure_tray_pkg "$guest"; then
    bad "could not install waired-tray for the apt-remove assert (waired-agent#1031)"
    sed 's/^/    /' /tmp/it-tray-install.log >&2 || true
  else
    setsid dbus-run-session -- /usr/bin/waired-tray >/tmp/it-tray-apt.log 2>&1 &
    local apt_tray_pid=''
    for _ in $(seq 1 20); do
      apt_tray_pid="$(pgrep -x waired-tray | head -1 || true)"
      [ -n "$apt_tray_pid" ] && break
      sleep 0.5
    done
    if [ -z "$apt_tray_pid" ]; then
      skip "apt-remove tray assert: could not start a tray"
    else
      gx "$guest" env DEBIAN_FRONTEND=noninteractive apt-get remove -y waired-tray \
        >/tmp/it-apt-remove.log 2>&1 || true
      # The prerm sends SIGTERM and does not wait; give the tray its own
      # shutdown budget before deciding it ignored the signal.
      for _ in $(seq 1 30); do
        kill -0 "$apt_tray_pid" 2>/dev/null || break
        sleep 0.5
      done
      if kill -0 "$apt_tray_pid" 2>/dev/null; then
        bad "waired-tray (PID $apt_tray_pid) survived apt-get remove waired-tray (waired-agent#1031)"
        pkill -x waired-tray 2>/dev/null || true
      else
        ok "apt-get remove waired-tray stopped the running app (waired-agent#1031)"
      fi
    fi
  fi
}

# Re-run install.sh: with waired already installed and the repo candidate
# equal to installed, this takes the update path and must be a clean
# no-op ("already the latest"). This is the real-flow #328 FLAG_YES path.
assert_idempotent() {
  local guest="$1"
  it_log "re-running install.sh in $guest (idempotency / update path)"
  if run_install "$guest" >/tmp/it-reinstall.log 2>&1; then
    ok "second install.sh run is a clean no-op (exit 0)"
  else
    bad "second install.sh run failed (exit $?)"; sed 's/^/    /' /tmp/it-reinstall.log >&2 || true
  fi
}

# Simulate the #335 breakage (a pre-#340 root `waired init` left the
# identity/secrets root-owned, crash-looping the User=waired daemon) and
# assert that re-running postinst configure — what an `apt upgrade` does —
# reclaims the whole tree for the service user.
assert_postinst_selfheal() {
  local guest="$1" stray
  it_log "simulating root-owned state tree in $guest (#335), re-running postinst"
  gx "$guest" install -d -m 0700 /var/lib/waired/secrets
  gx "$guest" sh -c 'echo tok > /var/lib/waired/secrets/access_token && chmod 0600 /var/lib/waired/secrets/access_token'
  gx "$guest" chown -R root:root /var/lib/waired
  if ! gx "$guest" dpkg-reconfigure -fnoninteractive waired >/dev/null 2>&1; then
    bad "dpkg-reconfigure waired failed"; return
  fi
  stray=$(gx "$guest" find /var/lib/waired ! -user waired 2>/dev/null || true)
  if [ -z "$stray" ]; then
    ok "postinst re-run reclaims root-owned state tree (self-heal)"
  else
    bad "root-owned paths survive postinst re-run:"
    printf '%s\n' "$stray" | sed 's/^/    /' >&2
  fi
}

# --- drive the requested tier -----------------------------------------
GUEST="$IT_PREFIX-$NAME"

cleanup() {
  if [ "$KEEP" = 1 ]; then
    it_warn "leaving guest(s) up (--keep): $GUEST${TIER_GUESTS:+ $TIER_GUESTS}"
  elif [ "$IT_LOCAL" = 1 ]; then
    # The runner is disposable, so we don't uninstall; just best-effort drop
    # the device's identity so it doesn't linger on the CP.
    command -v it_logout_guest >/dev/null 2>&1 && it_logout_guest "$GUEST"
  else
    for g in "$GUEST" ${TIER_GUESTS:-}; do
      # Best-effort deregister so disposable guests don't pile up on the CP.
      command -v it_logout_guest >/dev/null 2>&1 && it_logout_guest "$g"
      lxc delete --force "$g" >/dev/null 2>&1 || true
    done
  fi
}
TIER_GUESTS=""
trap cleanup EXIT

it_step "Tier $TIER run (guest=$GUEST)"

if [ "$TIER" -le 2 ]; then
  launch_guest "$GUEST"
  # --log-level debug on purpose (waired-agent#801): the install-time level is
  # the configuration whose runtime override used to be silently undone by
  # every restart, and a host installed without it cannot exercise that at
  # all. It also gives this leg its first log-level coverage.
  run_install "$GUEST" --log-level debug
  # Immediately, before assert_idempotent's re-run overwrites INSTALL_LOG —
  # and before anything else, because a re-run is not a fresh install and
  # prints no summary at all. --local starts it un-rooted; the LXD guest is a
  # root login.
  if [ "$IT_LOCAL" = 1 ]; then assert_start_shape un-rooted; else assert_start_shape elevated; fi
  apply_agent_env_extra "$GUEST"
  assert_tier1 "$GUEST"
  assert_idempotent "$GUEST"
  assert_postinst_selfheal "$GUEST"
  if [ "$TIER" -ge 2 ]; then
    # shellcheck source=scripts/dev/lib/installtest-enroll.sh
    source "$ROOT/scripts/dev/lib/installtest-enroll.sh"
    if [ "$DAEMON_ENGINE" = 1 ]; then
      # shellcheck source=scripts/dev/lib/installtest-daemon-engine.sh
      source "$ROOT/scripts/dev/lib/installtest-daemon-engine.sh"
      it_enroll_daemon_path "$GUEST"   # daemon-path enrol via out-of-band OIDC completion
      assert_tier2 "$GUEST"            # identity chain still applies (the daemon owns it)
      assert_reinit_resumes "$GUEST"   # a second init resumes, it does not fail (waired-agent#313)
      assert_claude_route "$GUEST"     # init is the single decider of Claude routing (#294)
      assert_daemon_engine "$GUEST"    # the waired#835 §9/§11 executor engine install
    else
      it_enroll_guest "$GUEST"   # enrol (IT_ENROLL_MODE) against the Control Plane
      assert_tier2 "$GUEST"
      assert_reinit_resumes "$GUEST"   # a second init resumes, it does not fail (waired-agent#313)
      # An opt-out is not a failed install (waired-agent#551). Only where the
      # guest still has no engine: with --inference one is already installed,
      # so daemonWantsEngine would answer false and the probe would pass
      # without reaching the arm it exists to test.
      if [ "$INFER" != 1 ]; then
        assert_reinit_engine_optout "$GUEST"
        # The step-4 twin's other half (waired-agent#590): the flagless
        # default on a deterministically below-spec host ends disabled,
        # exit 0. Runs here for the same reason the opt-out probe does —
        # this guest still has no engine, so both arms are reachable.
        assert_reinit_default_unfit "$GUEST"
        # The `waired models pull` half of the same twin (waired-agent#590):
        # --yes alone declines a model that does not fit, --yes --force is
        # honoured. Here for a THIRD reason on top of the two above — an
        # engine-less guest makes the honoured row free, because the daemon
        # refuses the handed-on pull at its own admission check (#307)
        # instead of fetching weights this gate has no business fetching.
        assert_models_pull_confirm "$GUEST"
      fi
      assert_claude_route "$GUEST"     # init is the single decider of Claude routing (#294)
      # LAST of the engine-less probes, because it is the one that ends
      # the guest's engine-less life: it installs one (waired-agent#590).
      [ "$ENGINE_ONLY" = 1 ] && assert_engine_only_install "$GUEST"
      [ "$INFER" = 1 ] && assert_inference "$GUEST"
      if [ "$INTEG" = 1 ]; then
        # shellcheck source=scripts/dev/lib/installtest-integration.sh
        source "$ROOT/scripts/dev/lib/installtest-integration.sh"
        assert_integration "$GUEST"
      fi
    fi
    # Last: it toggles pause/resume, so keep it clear of the asserts above.
    assert_mgmt_socket "$GUEST"
  fi
  # After everything else: it purges the host to reach a fresh-install state,
  # so it must not run before the asserts that need one installed. Lean
  # configuration only — the engine-bearing legs would lose what they built.
  if [ "$INFER" != 1 ] && [ "$DAEMON_ENGINE" != 1 ] && [ "$ENGINE_ONLY" != 1 ] && [ "$INTEG" != 1 ]; then
    assert_root_shell_install "$GUEST"
  fi
else
  # Tier 3: two VMs, full installer + enrol on each, then overlay ping.
  # shellcheck source=scripts/dev/lib/installtest-enroll.sh
  source "$ROOT/scripts/dev/lib/installtest-enroll.sh"
  A="$IT_PREFIX-${NAME}-a"; B="$IT_PREFIX-${NAME}-b"
  TIER_GUESTS="$A $B"
  for g in "$A" "$B"; do
    launch_guest "$g"; run_install "$g"; assert_tier1 "$g"; it_enroll_guest "$g"; assert_tier2 "$g"
  done
  assert_tier3_ping "$A" "$B"
fi

echo
it_step "Tier $TIER summary: $PASS passed, $FAIL failed, $SKIP skipped"

# Assert-count floor (#215). Zero failures is not the same as having tested
# anything: a block that stops running — an early `return`, a guard that
# silently opts out, a helper that stops being called — subtracts asserts
# without ever printing FAIL, and the leg reports success. This is the shape
# behind "the leg said ok while the reason sat in the same log".
#
# The floors are MEASURED from a green run of the leanest configuration for
# each tier, not estimated: tier 1 = the 10 package/unit/state-dir asserts,
# tier 2 = those plus the 8 enrol + mgmt-socket ones and the 2 Claude-routing
# ones (#294 — assert_claude_route runs both ways round, so it contributes
# the same 2 whether or not the leg set IT_SKIP_INTEGRATION).
#
# "Options only ever ADD asserts, so a floor keyed on the tier alone holds"
# was true until waired-agent#551, and is not any more: the engine-less
# probes run ONLY on the lean leg, because --inference and --daemon-engine
# leave an engine on the host and those probes would then pass without
# reaching the arms they test. One number for both shapes held while the
# lean-only block was 4 asserts — it is 13 now, and a floor of 27 stopped
# covering the block it exists to cover the moment waired-agent#605 added
# to it without raising anything. So the tier-2 floor is PER CONFIGURATION,
# the way installtest-windows.ps1 has kept its since #215.
#
# Raise these when you add an assert that always runs; lower them, in the
# same commit and with the reason, if a leg legitimately becomes conditional.
#
# waired-agent#801 added 3 always-running asserts to assert_tier1 (the level
# did not reach agent.env / it did reach the daemon / it survives a restart),
# so every floor below is its measured predecessor plus 3.
#
# waired-agent#990 added assert_start_shape after the primary install, which
# always runs, so every floor below is its predecessor plus 1. The root-shell
# arm's 3 are added separately at the end: it needs --local (the LXD guest is
# already a root login, so there it skips), and a skip does not count.
#
# waired-agent#1051 added a fifth assert to assert_reinit_engine_optout. That
# probe is in the engine-less block, so only the two floors that keep the
# block move: the lean leg and --engine-only. The INFER / DAEMON_ENGINE floor
# stays where it is, because those legs trade the block away.
case "$TIER" in
  1) floor=14 ;;
  # 23 shared + the lean-only engine-less block:
  #   +5  assert_reinit_engine_optout   (waired-agent#551, +1 for #1051)
  #   +4  assert_reinit_default_unfit   (waired-agent#590 / #605)
  #   +5  assert_models_pull_confirm    (waired-agent#590)
  # A richer leg trades that block for assert_inference / assert_daemon_engine
  # and their own tails; 27 was the measured floor across both.
  #
  # --engine-only keeps the whole engine-less block (it does not set INFER)
  # and adds its own 6 on top, so it is the lean floor plus 6
  # (waired-agent#590).
  #
  # waired-agent#573's host-speed assert does NOT move these: it is soft while
  # waired-agent#579 is open (it warns rather than failing when no measurement
  # was published), so on the leg that hits that case it contributes 0, not 1.
  # The #579 fix flips it to blocking and raises INFER to 28 then.
  *) if [ "$INFER" = 1 ] || [ "$DAEMON_ENGINE" = 1 ]; then floor=31
     elif [ "$ENGINE_ONLY" = 1 ]; then floor=47
     else floor=41; fi ;;
esac
# The root-shell install arm (waired-agent#990): 3 asserts, on the lean
# --local configuration only. Keyed on IT_LOCAL rather than folded into the
# numbers above, because the LXD path legitimately skips it — and a floor set
# to the LXD minimum would stop noticing if the arm quietly stopped running
# on the leg that is actually the CI one.
if [ "$IT_LOCAL" = 1 ] && [ "$INFER" != 1 ] && [ "$DAEMON_ENGINE" != 1 ] \
   && [ "$ENGINE_ONLY" != 1 ] && [ "$INTEG" != 1 ]; then
  floor=$((floor + 3))
fi
executed=$((PASS + FAIL))
if [ "$executed" -lt "$floor" ]; then
  printf '\033[1;31m[installtest] FAIL\033[0m only %d asserts ran at tier %s; at least %d must (a block stopped executing — see the assert-count floor in %s)\n' \
    "$executed" "$TIER" "$floor" "$(basename "$0")" >&2
  exit 1
fi

[ "$FAIL" -eq 0 ] || exit 1
