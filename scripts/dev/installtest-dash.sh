#!/usr/bin/env bash
# installtest-dash.sh — fast, hermetic, infra-free guard for the
# install.sh shell layer. Runs packaging/install/install.sh under
# multiple POSIX shells (dash is the production `curl | sh` interpreter)
# across every CLI/env dispatch path in --dry-run, asserting it never
# trips `set -u` ("unbound variable" / "parameter not set") or a syntax
# error.
#
# This is the class of bug that shipped twice — #311 (FLAG_CHECK /
# FLAG_UPDATE, fresh dispatch) and #328 (FLAG_YES, deep in the update
# prompt) — each caught only when a user ran the one-liner on real
# hardware. shellcheck (CI's install-script-lint) catches some of it;
# this actually *executes* every branch.
#
# Hermetic: install.sh's only un-dry-run-guarded probes of host state are
# `dpkg-query` (is waired installed?) and `apt-cache policy` (candidate
# version). We stub both on PATH so the matrix drives the install path
# AND the update->prompt path deterministically, regardless of whether
# the dev box already has waired installed. No LXD, no root, no network.
#
# Usage:
#   bash scripts/dev/installtest-dash.sh
#   INSTALL_SH=/path/to/install.sh bash scripts/dev/installtest-dash.sh
set -euo pipefail

ROOT="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)"
INSTALL_SH="${INSTALL_SH:-$ROOT/packaging/install/install.sh}"
[ -f "$INSTALL_SH" ] || { echo "install.sh not found: $INSTALL_SH" >&2; exit 1; }

SHELLS=()
for s in dash bash; do command -v "$s" >/dev/null 2>&1 && SHELLS+=("$s"); done
command -v busybox >/dev/null 2>&1 && SHELLS+=("busybox sh")
[ "${#SHELLS[@]}" -gt 0 ] || { echo "no POSIX shell available" >&2; exit 1; }

# --- stub host-state probes + neutralise mutating commands -------------
STUBDIR="$(mktemp -d)"
trap 'rm -rf "$STUBDIR"' EXIT

# Functional: report waired install state from IT_STUB_INSTALLED /
# IT_STUB_TRAY so we can pick the install vs update dispatch arm.
cat > "$STUBDIR/dpkg-query" <<'STUB'
#!/bin/sh
case "$*" in
  *waired-tray*) [ -n "${IT_STUB_TRAY:-}" ] && exit 0 || exit 1 ;;
  *waired*)      [ -n "${IT_STUB_INSTALLED:-}" ] && printf '%s' "$IT_STUB_INSTALLED"; exit 0 ;;
esac
exit 0
STUB
# Functional: feed install.sh's `awk '/Candidate:/{print $2}'`.
cat > "$STUBDIR/apt-cache" <<'STUB'
#!/bin/sh
printf 'waired:\n  Installed: %s\n  Candidate: %s\n' \
  "${IT_STUB_INSTALLED:-(none)}" "${IT_STUB_CANDIDATE:-(none)}"
STUB
# Safety no-ops: even if a --dry-run guard ever regresses, the matrix must
# never mutate the host. None of these are reached in --dry-run today.
for c in apt-get systemctl curl gpg dpkg; do
  printf '#!/bin/sh\nexit 0\n' > "$STUBDIR/$c"
done
# sudo is a no-op like the rest, with ONE functional case: install.sh probes
# enrolment with `$SUDO test -e /var/lib/waired/identity.json`, and a blanket
# `exit 0` answered "already enrolled" — so linux_maybe_init short-circuited
# and no case ever reached the sign-in arm. Report not-enrolled by default
# (the fresh-install state the matrix is meant to drive); IT_STUB_ENROLLED=1
# picks the other arm. Everything else still exits 0 without running.
cat > "$STUBDIR/sudo" <<'STUB'
#!/bin/sh
case "$1" in
  test) if [ -n "${IT_STUB_ENROLLED:-}" ]; then exit 0; else exit 1; fi ;;
esac
exit 0
STUB
chmod +x "$STUBDIR"/*
export PATH="$STUBDIR:$PATH"

# --- runner ------------------------------------------------------------
FAIL_RE='unbound variable|parameter not set|[Bb]ad substitution|[Ss]yntax error|: not found'
PASS=0; FAIL=0
log()  { printf '\033[1;36m[dash-matrix]\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m[dash-matrix] FAIL\033[0m %s\n' "$*" >&2; FAIL=$((FAIL+1)); }
ok()   { printf '\033[1;32m[dash-matrix]  ok \033[0m %s\n' "$*"; PASS=$((PASS+1)); }

# run_case <zero|nonzero|any> <label> <env-assignments> -- <args...>
run_case() {
  local expect="$1" label="$2" envs="$3"; shift 3; shift   # drop the literal --
  local sh out rc
  for sh in "${SHELLS[@]}"; do
    out="$(env $envs $sh "$INSTALL_SH" "$@" 2>&1)" && rc=0 || rc=$?
    if printf '%s' "$out" | grep -Eq "$FAIL_RE"; then
      fail "[$sh] $label — set -u/syntax signature:"
      printf '%s\n' "$out" | grep -E "$FAIL_RE" | sed 's/^/        /' >&2
      continue
    fi
    case "$expect" in
      zero)    [ "$rc" -eq 0 ] || { fail "[$sh] $label — expected exit 0, got $rc"; continue; } ;;
      nonzero) [ "$rc" -ne 0 ] || { fail "[$sh] $label — expected nonzero, got 0"; continue; } ;;
    esac
    ok "[$sh] $label (exit $rc)"
  done
}

log "install.sh = $INSTALL_SH"
log "shells     = ${SHELLS[*]}"

# 1. Syntax (parse only).
for sh in "${SHELLS[@]}"; do
  if $sh -n "$INSTALL_SH" 2>/dev/null; then ok "[$sh] syntax (-n)"; else fail "[$sh] syntax (-n)"; fi
done

# 2. Help.
run_case zero "--help" "" -- --help

# 3. Fresh-install dispatch (IT_STUB_INSTALLED unset -> linux_apt_install).
FRESH="IT_STUB_INSTALLED="    # explicit empty: not installed
run_case zero "fresh (base)"        "$FRESH"                                 -- --dry-run --skip-ollama --no-init
run_case zero "fresh --dev"         "$FRESH"                                 -- --dry-run --skip-ollama --no-init --dev
run_case zero "fresh --control"     "$FRESH"                                 -- --dry-run --skip-ollama --no-init --control http://127.0.0.1:9479
run_case zero "fresh --control="    "$FRESH"                                 -- --dry-run --skip-ollama --no-init --control=http://127.0.0.1:9479
run_case zero "fresh --yes"         "$FRESH"                                 -- --dry-run --skip-ollama --no-init --yes
run_case zero "fresh -y"            "$FRESH"                                 -- --dry-run --skip-ollama --no-init -y
run_case zero "fresh (maybe-init)"  "$FRESH"                                 -- --dry-run --skip-ollama
run_case zero "fresh NO_TRAY"       "$FRESH WAIRED_NO_TRAY=1"                -- --dry-run --skip-ollama --no-init
run_case zero "fresh CONTROL_URL"   "$FRESH WAIRED_CONTROL_URL=http://h:9479" -- --dry-run --skip-ollama --no-init
run_case zero "fresh NO_OLLAMA env" "$FRESH WAIRED_NO_OLLAMA=1"              -- --dry-run --no-init
run_case zero "fresh --non-interactive" "$FRESH"                             -- --dry-run --skip-ollama --non-interactive
run_case zero "fresh init answers"  "$FRESH"                                 -- --dry-run --skip-ollama --inference-enabled=false --share-with-mesh=true
run_case zero "fresh init answers (space form)" "$FRESH"                     -- --dry-run --skip-ollama --inference-enabled false --share-with-mesh true
run_case zero "fresh enrolled arm"  "$FRESH IT_STUB_ENROLLED=1"              -- --dry-run --skip-ollama

# 3b. The `waired init` hand-off (#165, #166). These assert OUTPUT, not just
#     exit status, so they run under setsid: without a controlling terminal
#     the terminal gate is live and deterministic whether this harness is run
#     from a dev terminal or from CI.
#
#     The contract under test:
#       * no terminal          -> sign-in is skipped, with a "finish later" note
#       * --non-interactive    -> sign-in is attempted anyway, stdin from
#                                 /dev/null (</dev/tty cannot open without one)
#       * --yes                -> init gets --non-interactive, but does NOT
#                                 clear the terminal gate
#       * the two answers      -> forwarded in the `=` form. The space form is
#                                 a trap: they are Go bool flags, so
#                                 `--inference-enabled false` would leave
#                                 "false" as a positional arg and `waired init`
#                                 (cobra.NoArgs) would reject it outright.
init_out() {  # init_out <args...> -> install.sh output with no controlling tty
  setsid env IT_STUB_INSTALLED= sh "$INSTALL_SH" --dry-run --skip-ollama "$@" </dev/null 2>&1 || true
}
if command -v setsid >/dev/null 2>&1; then
  out="$(init_out --non-interactive --inference-enabled=false --share-with-mesh=true)"
  if printf '%s' "$out" | grep -q -- 'waired init .*--non-interactive .*--inference-enabled=false .*--share-with-mesh=true'; then
    ok "--non-interactive runs sign-in with no tty, answers in = form"
  else
    fail "--non-interactive init hand-off wrong: $(printf '%s' "$out" | grep -i 'init\|terminal' | tr '\n' '|')"
  fi
  if printf '%s' "$out" | grep -q -- '</dev/null'; then
    ok "no-tty sign-in reads stdin from /dev/null (</dev/tty cannot open)"
  else
    fail "no-tty sign-in did not switch stdin off /dev/tty"
  fi
  if ! printf '%s' "$out" | grep -qE 'init .*(--inference-enabled|--share-with-mesh) (true|false)'; then
    ok "no bare true/false left as a positional arg (cobra.NoArgs would reject it)"
  else
    fail "a bool value was passed in the space form"
  fi

  out="$(init_out --inference-enabled=false)"
  if printf '%s' "$out" | grep -q 'No terminal detected'; then
    ok "no terminal + no --non-interactive skips sign-in (unchanged default)"
  else
    fail "the terminal gate did not hold: $(printf '%s' "$out" | grep -i 'init\|terminal' | tr '\n' '|')"
  fi

  out="$(init_out --yes)"
  if printf '%s' "$out" | grep -q 'No terminal detected'; then
    ok "--yes does not override the terminal gate"
  else
    fail "--yes cleared the terminal gate: $(printf '%s' "$out" | grep -i 'init\|terminal' | tr '\n' '|')"
  fi

  out="$(init_out --non-interactive --yes)"
  if printf '%s' "$out" | grep -q -- 'waired init .*--non-interactive'; then
    ok "--yes forwards init --non-interactive (install.sh --help has always said so)"
  else
    fail "--yes did not forward --non-interactive to init"
  fi
else
  log "setsid unavailable -- skipping the init hand-off cases"
fi

# 3c. Bad values die at parse time, before any privileged step.
run_case nonzero "--inference-enabled bogus" "$FRESH" -- --dry-run --inference-enabled bogus
run_case nonzero "--share-with-mesh bogus"   "$FRESH" -- --dry-run --share-with-mesh=maybe
run_case nonzero "--inference-enabled (no value)" "$FRESH" -- --dry-run --inference-enabled

# 4. Update dispatch (IT_STUB_INSTALLED set -> linux_apt_update). This is
#    the arm where #328's FLAG_YES lived, reached only when a package is
#    already installed. We drive: already-latest, update-available (declined
#    via no-tty no-yes — the exact FLAG_YES read), update --yes (proceeds),
#    --check, tray-pin branch, and the no-candidate die.
UPD="IT_STUB_INSTALLED=0.0.1"
run_case zero "update already-latest" "$UPD IT_STUB_CANDIDATE=0.0.1"          -- --dry-run --skip-ollama
run_case zero "update avail (declined)" "$UPD IT_STUB_CANDIDATE=9.9.9"        -- --dry-run --skip-ollama --no-init
run_case zero "update avail --yes"    "$UPD IT_STUB_CANDIDATE=9.9.9"          -- --dry-run --skip-ollama --no-init --yes
run_case zero "update --check avail"  "$UPD IT_STUB_CANDIDATE=9.9.9"          -- --dry-run --check --skip-ollama
run_case zero "update tray-pin --yes" "$UPD IT_STUB_CANDIDATE=9.9.9 IT_STUB_TRAY=1 WAIRED_VERSION=9.9.9" -- --dry-run --skip-ollama --no-init --yes
run_case nonzero "update no-candidate" "$UPD IT_STUB_CANDIDATE="             -- --dry-run --update --skip-ollama

# 4b. Edge channel (--edge / --latest / WAIRED_VERSION=edge). The unified
#     opt-in must drive the edge apt suite + edge asset base without
#     tripping set -u, keep `edge` a *channel* (never `waired=edge`), and
#     handle the stable->edge switch — a downgrade — on the update arm.
EDGE_VER="0.0.1~edge.20260610120000+abc1234"
run_case zero "fresh --edge"              "$FRESH"                       -- --dry-run --skip-ollama --no-init --edge
run_case zero "fresh --latest"            "$FRESH"                       -- --dry-run --skip-ollama --no-init --latest
run_case zero "fresh WAIRED_VERSION=edge" "$FRESH WAIRED_VERSION=edge"   -- --dry-run --skip-ollama --no-init
run_case zero "edge switch stable->edge"  "$UPD IT_STUB_CANDIDATE=$EDGE_VER" -- --dry-run --skip-ollama --no-init --yes --edge
run_case zero "edge already-latest"       "IT_STUB_INSTALLED=$EDGE_VER IT_STUB_CANDIDATE=$EDGE_VER WAIRED_VERSION=edge" -- --dry-run --skip-ollama

# 4c. Clean install (--clean): consent gate, wipe delegation to the
#     sibling uninstall.sh (hermetic — install.sh runs from a file here,
#     so the sibling is always found and its host probes hit the same
#     stubs), and the forced fresh-install dispatch even when a package
#     looks installed.
run_case zero    "clean --yes (fresh)"       "$FRESH"                       -- --dry-run --skip-ollama --no-init --clean --yes
run_case zero    "clean --yes (installed)"   "$UPD IT_STUB_CANDIDATE=9.9.9" -- --dry-run --skip-ollama --no-init --clean --yes
run_case zero    "clean WAIRED_CLEAN env"    "$FRESH WAIRED_CLEAN=1"        -- --dry-run --skip-ollama --no-init --yes
run_case nonzero "clean + --check rejected"  "$FRESH"                       -- --dry-run --clean --yes --check
run_case nonzero "clean + --update rejected" "$FRESH"                       -- --dry-run --clean --yes --update

# The wipe child must actually run, not just exit 0: the uninstall.sh
# --clean path always logs the apt-source removal on Linux.
out="$(env IT_STUB_INSTALLED= sh "$INSTALL_SH" --dry-run --skip-ollama --no-init --clean --yes 2>&1)" || true
if printf '%s' "$out" | grep -q 'Removing the Waired apt source'; then
  ok "clean --yes delegates to uninstall.sh (wipe log present)"
else
  fail "clean --yes — no uninstall.sh wipe log in output"
fi

# Consent gate: non-interactive without --yes must die, never wipe. setsid
# detaches the controlling terminal so install.sh's /dev/tty prompt is
# unavailable even when this harness runs from a dev terminal.
if command -v setsid >/dev/null 2>&1; then
  if setsid sh "$INSTALL_SH" --dry-run --clean </dev/null >/dev/null 2>&1; then
    fail "clean (no tty, no --yes) — expected nonzero, got 0"
  else
    ok "clean (no tty, no --yes) dies before wiping"
  fi
else
  log "setsid unavailable — skipping the no-tty consent-gate case"
fi

# 5. Bad flag — clean failure, not a set -u error.
run_case nonzero "unknown flag" "$FRESH" -- --bogus

echo
log "summary: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] || exit 1
