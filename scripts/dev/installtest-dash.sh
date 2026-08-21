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
# Sections 1-6 run install.sh in --dry-run. Section 7 runs it FOR REAL
# (see its header): the branch it guards sits on the far side of a
# --dry-run guard and cannot be reached any other way.
#
# There is deliberately no WAIRED_FORCE_OS-style test hook in install.sh:
# the `uname` stub below already forces the OS from out here, so the
# subject stays free of test-only branches (CLAUDE.md "Test discipline":
# put the seam below the behaviour under test).
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

# The shells the case helpers iterate. Sections 6-7 narrow it (see there);
# everything else runs the full matrix.
MATRIX_SHELLS=("${SHELLS[@]}")

# --- stub host-state probes + neutralise mutating commands -------------
STUBDIR="$(mktemp -d)"
trap 'rm -rf "$STUBDIR"' EXIT

# Functional: report waired install state from IT_STUB_INSTALLED /
# IT_STUB_TRAY so we can pick the install vs update dispatch arm.
cat > "$STUBDIR/dpkg-query" <<'STUB'
#!/bin/sh
case "$*" in
  *gnome-shell*) [ -n "${IT_STUB_GNOME:-}" ] && exit 0 || exit 1 ;;
  *waired-tray*) [ -n "${IT_STUB_TRAY:-}" ] && exit 0 || exit 1 ;;
  *waired*)      [ -n "${IT_STUB_INSTALLED:-}" ] && printf '%s' "$IT_STUB_INSTALLED"; exit 0 ;;
esac
exit 0
STUB
# Functional, two subcommands. `policy` feeds install.sh's
# `awk '/Candidate:/{print $2}'`. `madison` feeds apt_has_version, which
# decides which spelling of a pinned release apt actually holds — the two
# are both live in one suite, so that cannot be assumed (waired-agent#811).
# IT_STUB_VERSIONS is a COMMA-separated version list (the runner passes env
# assignments through an unquoted expansion, so a space would end the
# value); empty = the package is not in the index at all.
cat > "$STUBDIR/apt-cache" <<'STUB'
#!/bin/sh
case "$1" in
  madison)
    IFS=','
    for v in ${IT_STUB_VERSIONS:-}; do
      printf '   waired | %s | https://example.invalid/apt suite/main amd64 Packages\n' "$v"
    done
    ;;
  *)
    printf 'waired:\n  Installed: %s\n  Candidate: %s\n' \
      "${IT_STUB_INSTALLED:-(none)}" "${IT_STUB_CANDIDATE:-(none)}"
    ;;
esac
STUB
# Safety no-ops: even if a --dry-run guard ever regresses, the matrix must
# never mutate the host. None of these are reached in --dry-run today.
for c in apt-get systemctl gpg dpkg; do
  printf '#!/bin/sh\nexit 0\n' > "$STUBDIR/$c"
done
# curl is a no-op like the rest, with ONE functional case: the done banners
# ask the running daemon whether the local AI engine is installed, over the
# loopback Management API (#663 — the probe used to be `$SUDO test -x` on the
# root-owned state dir, which re-authenticated sudo after the long init).
# Answer "not installed" by default, which is the fresh-install state the
# matrix drives; IT_STUB_ENGINE=1 picks the "installed (local AI engine)" arm.
# There is no daemon on this runner, so without the stub every case would take
# the default arm and the other one would go untested.
# The second functional case is the GitHub Releases API, which
# resolve_latest_version queries for the stable channel. Without an answer
# every stable update case dies at "could not determine the latest
# version" and the version compare below it is unreachable — which is why
# the darwin cases used to be able to run only on WAIRED_VERSION=edge.
# IT_STUB_LATEST_TAG names the tag the feed reports; unset = no answer,
# the network-failure arm.
cat > "$STUBDIR/curl" <<'STUB'
#!/bin/sh
case "$*" in
  */waired/v1/inference/runtimes*)
    if [ -n "${IT_STUB_ENGINE:-}" ]; then
      printf '{"runtimes":[{"name":"ollama","installed":true,"state":"ready"}]}\n'
    else
      printf '{"runtimes":[{"name":"ollama","installed":false,"state":"absent"}]}\n'
    fi
    ;;
  *api.github.com*releases/latest*)
    [ -n "${IT_STUB_LATEST_TAG:-}" ] || exit 22
    printf '{"tag_name":"%s","name":"%s"}\n' "$IT_STUB_LATEST_TAG" "$IT_STUB_LATEST_TAG"
    ;;
esac
exit 0
STUB
# sudo is a no-op like the rest, with ONE functional case: install.sh probes
# enrolment with `$SUDO test -e <state-dir>/identity.json`, and a blanket
# `exit 0` answered "already enrolled" — so linux_maybe_init short-circuited
# and no case ever reached the sign-in arm. Report not-enrolled by default
# (the fresh-install state the matrix is meant to drive); IT_STUB_ENROLLED=1
# picks the other arm. Everything else still exits 0 without running.
#
# The engine arm no longer rides this stub: it used to, because both facts
# went through `$SUDO test`, so IT_STUB_ENROLLED silently decided the Ollama
# line too. IT_STUB_ENGINE drives that one now, and the two are independent.
#
# `sudo launchctl …` is forwarded to the launchctl stub below rather than
# swallowed, because darwin_install_complete reads its exit status to decide
# install-vs-update; a blanket `exit 0` would make every host look complete.
cat > "$STUBDIR/sudo" <<'STUB'
#!/bin/sh
# IT_SUDO_TRACE echoes every call so a case can assert WHERE privileged work
# happens, not just that it succeeded (run_case_no_sudo_after_done, #663).
if [ -n "${IT_SUDO_TRACE:-}" ]; then printf '[stub-sudo] %s\n' "$*"; fi
case "$1" in
  test) if [ -n "${IT_STUB_ENROLLED:-}" ]; then exit 0; else exit 1; fi ;;
  launchctl) shift; exec launchctl "$@" ;;
esac
exit 0
STUB

# --- darwin stubs: let the macOS dispatch run on a Linux runner ---------
# Without these, install.sh's darwin arm is only ever `sh -n`-parsed here,
# and the real macOS leg (installtest.yml, runs-on macos-14) only ever sees a
# clean host — so nothing executed the install-vs-update decision on a
# half-installed one, which is exactly the state that got stuck.
REAL_UNAME="$(command -v uname)"
cat > "$STUBDIR/uname" <<STUB
#!/bin/sh
# Passes through to the real uname unless a case asks for a specific answer,
# so the Linux cases above are untouched.
case "\$1" in
  -s) if [ -n "\${IT_STUB_UNAME_S:-}" ]; then printf '%s\n' "\$IT_STUB_UNAME_S"; exit 0; fi ;;
  -m) if [ -n "\${IT_STUB_UNAME_M:-}" ]; then printf '%s\n' "\$IT_STUB_UNAME_M"; exit 0; fi ;;
esac
exec $REAL_UNAME "\$@"
STUB
printf '#!/bin/sh\nprintf %%s\\\\n 15.0\n' > "$STUBDIR/sw_vers"

# --- /etc/waired probe (waired-agent#792) -------------------------------
# uninstall.sh's --clean path decides what to say (and what to remove) from
# one `find /etc/waired`. Stubbing that here is what lets the leftover case
# run on a host that has no /etc/waired, and keeps the subject free of a
# test-only branch -- the same reasoning as the `uname` stub above.
# IT_STUB_ETC_WAIRED is a newline-separated listing; unset means "no such
# directory", which is what a runner really has.
REAL_FIND="$(command -v find)"
cat > "$STUBDIR/find" <<STUB
#!/bin/sh
if [ "\$1" = /etc/waired ] && [ -n "\${IT_STUB_ETC_WAIRED+set}" ]; then
  printf '%s' "\$IT_STUB_ETC_WAIRED"
  exit 0
fi
exec $REAL_FIND "\$@"
STUB
# `print` is the one functional verb: darwin_install_complete uses it as
# launchd's own view of whether the job exists.
cat > "$STUBDIR/launchctl" <<'STUB'
#!/bin/sh
case "$1" in
  print) if [ -n "${IT_STUB_LAUNCHD_LOADED:-}" ]; then exit 0; else exit 1; fi ;;
esac
exit 0
STUB
# Present-on-PATH only: common_require_cmd checks for them, and every use is
# behind the --dry-run guard.
for c in shasum tar; do
  printf '#!/bin/sh\nexit 0\n' > "$STUBDIR/$c"
done

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
  for sh in "${MATRIX_SHELLS[@]}"; do
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

# run_case_grep <zero|nonzero|any> <label> <env-assignments> <expect-regex> -- <args...>
# Same checks as run_case plus "the output proves which arm ran". Needed for
# the darwin dispatch cases, where a wrong-but-successful dispatch (update
# where a fresh install was required) exits 0 and would pass run_case.
run_case_grep() {
  local expect="$1" label="$2" envs="$3" want="$4"; shift 4; shift
  local sh out rc
  for sh in "${MATRIX_SHELLS[@]}"; do
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
    if ! printf '%s' "$out" | grep -Eq "$want"; then
      fail "[$sh] $label — output does not match /$want/:"
      printf '%s\n' "$out" | tail -n 12 | sed 's/^/        /' >&2
      continue
    fi
    ok "[$sh] $label (exit $rc)"
  done
}

# run_case_asserts <zero|nonzero|any> <label> <env-assignments> <asserts> -- <args...>
# Same checks as run_case plus a LIST of assertions, newline-separated, ALL of
# which must hold. A pattern prefixed with '!' must NOT match.
#
# One regex is not enough for the section-7 cases: "the installer kept going
# after a failed step" is a claim about several later lines of output, and an
# installer that printed the warning and then died would satisfy any single one
# of them. The negative form is what stops the positive assertions from passing
# vacuously — a case that asserts a warning appears is only meaningful next to
# one asserting it does not appear when the failure is not injected.
run_case_asserts() {
  local expect="$1" label="$2" envs="$3" asserts="$4"; shift 4; shift
  local sh out rc want bad
  for sh in "${MATRIX_SHELLS[@]}"; do
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
    bad=""
    while IFS= read -r want; do
      [ -n "$want" ] || continue
      case "$want" in
        '!'*)
          if printf '%s' "$out" | grep -Eq "${want#!}"; then bad="present: /${want#!}/"; fi ;;
        *)
          if ! printf '%s' "$out" | grep -Eq "$want"; then bad="missing: /$want/"; fi ;;
      esac
      [ -z "$bad" ] || break
    done <<< "$asserts"
    if [ -n "$bad" ]; then
      fail "[$sh] $label — $bad"
      printf '%s\n' "$out" | tail -n 20 | sed 's/^/        /' >&2
      continue
    fi
    ok "[$sh] $label (exit $rc)"
  done
}

# run_case_no_sudo_after_done <label> <env-assignments> -- <args...>
# Pins the #663 contract: the summary must not be the thing that asks for a
# password. `waired init` runs for as long as the engine + model downloads and
# the benchmark take, which is far past sudo's default timestamp window, so
# ANY privileged call in the done banner re-authenticates — under a bare
# prompt, below the "Done" rule, with nothing said about why.
#
# This is a claim about the ORDER of two lines, which the pattern-list helper
# above cannot express: it matches each pattern against the whole output. So
# the sudo stub traces its calls and this splits the output at the separator.
run_case_no_sudo_after_done() {
  local label="$1" envs="$2"; shift 2; shift
  local sh out rc done_ln after
  for sh in "${MATRIX_SHELLS[@]}"; do
    out="$(env $envs IT_SUDO_TRACE=1 $sh "$INSTALL_SH" "$@" 2>&1)" && rc=0 || rc=$?
    if printf '%s' "$out" | grep -Eq "$FAIL_RE"; then
      fail "[$sh] $label — set -u/syntax signature:"
      printf '%s\n' "$out" | grep -E "$FAIL_RE" | sed 's/^/        /' >&2
      continue
    fi
    [ "$rc" -eq 0 ] || { fail "[$sh] $label — expected exit 0, got $rc"; continue; }
    # Without this the assertion below passes on any run that never called
    # sudo at all — including one where the stub or the trace itself broke.
    if ! printf '%s\n' "$out" | grep -q '\[stub-sudo\]'; then
      fail "[$sh] $label — no privileged call was traced anywhere; stub or trace is broken"
      continue
    fi
    done_ln="$(printf '%s\n' "$out" | grep -n -m1 -E '^(-|─)(-|─)(-|─) Done ' | cut -d: -f1)"
    if [ -z "$done_ln" ]; then
      fail "[$sh] $label — no 'Done' section in the output"
      continue
    fi
    after="$(printf '%s\n' "$out" | tail -n "+$done_ln" | grep '\[stub-sudo\]' || true)"
    if [ -n "$after" ]; then
      fail "[$sh] $label — privileged call(s) after the Done separator:"
      printf '%s\n' "$after" | sed 's/^/        /' >&2
      continue
    fi
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

# 3a. The engine is NOT installed before consent (#138). linux_apt_install used
#     to run `waired runtimes install ollama` in its own "AI engine (Ollama)"
#     section, BEFORE linux_maybe_init — so a `curl | sh` had already pulled
#     ~1.4 GB by the time the operator was asked whether this computer should
#     run models at all. macOS and Windows dropped their pre-install in #55/#73
#     and leave the decision AND the install to `waired init`; these two cases
#     pin Linux to the same contract.
#
#     The two `!` patterns are the removal itself; the positive lines next to
#     them are what stops those from passing vacuously — an installer that died
#     before either section would satisfy the negatives on their own.
run_case_asserts zero "fresh: engine deferred to sign-in (#138)" "$FRESH" \
  "!AI engine \(Ollama\)
!Installing waired's bundled Ollama
Sign you in
Install the Ollama AI engine during sign-in
Ollama: +installed by sign-in" -- --dry-run
# The opt-out arm keeps its own wording: --skip-ollama still reaches `waired
# init` as WAIRED_NO_OLLAMA, so the banner must say "skipped", not "at sign-in".
run_case_asserts zero "fresh --skip-ollama: banner still says skipped" "$FRESH" \
  "!AI engine \(Ollama\)
!Installing waired's bundled Ollama
!Install the Ollama AI engine during sign-in
Ollama: +skipped \(--skip-ollama" -- --dry-run --skip-ollama
# NEGATIVE CONTROL for #310's local-AI warning. A run where `waired init` was
# never even called must not carry it, and the banner around it must be
# byte-for-byte what it always was — the note is spliced INTO the heredoc, so
# a mistake there shows up as a stray blank line on every healthy install.
#
# The set side cannot be reached here: every case in this file is --dry-run,
# which returns before the sign-in call, and turning that off would make the
# first host-mutating case in the matrix. It is covered on real hosts instead
# (rc8 checklist).
run_case_asserts zero "fresh: no local-AI warning when init never ran" "$FRESH" \
  "!Local AI is not running on this device
!Sign-in is finished; only local AI is missing
Waired is installed" -- --dry-run

# waired-agent#801. The install-time level is a PERSISTED setting now, so on
# every OS branch it must reach `waired config log-level` and must NOT reach
# the service definition. The two `!` patterns are the removal itself — an
# agent flag baked into the plist/ExecStart outranks agent.json at every boot,
# which is what made a runtime change revert on every restart — and the
# positive line is what stops them from passing vacuously on a run that died
# before the step. `--log-level` is validated far earlier, so a case that
# never reached the seed would still exit 0.
run_case_asserts zero "fresh --log-level: seeded, not pinned (waired-agent#801)" "$FRESH" \
  "!waired-agent install.*--log-level
!WAIRED_LOG_LEVEL=debug
would: waired config log-level debug" -- --dry-run --skip-ollama --no-init --log-level debug
run_case_asserts zero "fresh WAIRED_LOG_LEVEL: same path as the flag" "$FRESH WAIRED_LOG_LEVEL=warn" \
  "!waired-agent install.*--log-level
would: waired config log-level warn" -- --dry-run --skip-ollama --no-init

# 3a-bis. The done banner asks the DAEMON whether the engine is installed
#     (#663), over the loopback Management API, instead of stat'ing the
#     root-owned state dir through sudo. Three things to pin:
#
#     1. the "installed" arm is still reachable — it is the arm the old
#        `$SUDO test -x` selected, and losing it would be a silent regression
#        that no existing assertion notices (they all drive the other arm);
#     2. enrolment no longer decides it. Both facts used to go through
#        `$SUDO test`, so the single sudo stub answered both and
#        IT_STUB_ENROLLED quietly moved the Ollama line too;
#     3. nothing privileged runs after the "Done" rule at all — the defect
#        itself, rather than its symptom.
run_case_asserts zero "fresh: engine arm reads the daemon's answer" "$FRESH IT_STUB_ENGINE=1" \
  "Ollama: +installed \(local AI engine\)
!Ollama: +installed by sign-in" -- --dry-run
run_case_asserts zero "fresh enrolled: enrolment does not decide the engine line" "$FRESH IT_STUB_ENROLLED=1" \
  "Enrolled — the agent service is running
Ollama: +installed by sign-in
!Ollama: +installed \(local AI engine\)" -- --dry-run
run_case_no_sudo_after_done "fresh: nothing privileged runs after Done (#663)" "$FRESH" -- --dry-run
run_case_no_sudo_after_done "fresh enrolled: nothing privileged runs after Done (#663)" "$FRESH IT_STUB_ENROLLED=1" -- --dry-run

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

# 4d. Prerelease ordering on the apt arm (waired-agent#780 / #781). The
#     installed and candidate strings here are .deb versions, which spell
#     the prerelease separator "~". Every one of these compared EQUAL
#     under the old numeric-prefix compare, so the first said nothing was
#     available and the last two announced an update to an older build.
run_case_grep zero "apt rc -> next rc is offered" \
  "IT_STUB_INSTALLED=0.0.3~rc1 IT_STUB_CANDIDATE=0.0.3~rc2" \
  'Update available: 0\.0\.3~rc1 -> 0\.0\.3~rc2' -- --dry-run --check --skip-ollama
run_case_grep zero "apt rc -> its release is offered" \
  "IT_STUB_INSTALLED=0.0.3~rc9 IT_STUB_CANDIDATE=0.0.3" \
  'Update available: 0\.0\.3~rc9 -> 0\.0\.3' -- --dry-run --check --skip-ollama
# rc10 is NEWER than rc2. A lexical compare of the prerelease would call
# this backwards; dpkg reads the digit run as a number, and so must this.
run_case_grep zero "apt rc2 -> rc10 is offered" \
  "IT_STUB_INSTALLED=0.0.3~rc2 IT_STUB_CANDIDATE=0.0.3~rc10" \
  'Update available: 0\.0\.3~rc2 -> 0\.0\.3~rc10' -- --dry-run --check --skip-ollama
run_case_grep zero "apt same rc is not an update" \
  "IT_STUB_INSTALLED=0.0.3~rc1 IT_STUB_CANDIDATE=0.0.3~rc1" \
  'already the latest available' -- --dry-run --check --skip-ollama
# The inversion the campaign hit: an older build sat at the top of the
# package index and --check announced it as the update.
run_case_grep zero "apt backwards candidate is not an update" \
  "IT_STUB_INSTALLED=0.0.3~rc9 IT_STUB_CANDIDATE=0.0.3~rc8" \
  'already the latest available' -- --dry-run --check --skip-ollama
run_case_grep zero "apt release is not downgraded to its rc" \
  "IT_STUB_INSTALLED=0.0.3 IT_STUB_CANDIDATE=0.0.3~rc9" \
  'already the latest available' -- --dry-run --check --skip-ollama
# A channel switch is still a switch, not an up-to-date state: it crosses
# suites and is a downgrade in apt's eyes, so the gate above must not
# swallow it.
run_case_grep zero "stable -> edge still switches" \
  "IT_STUB_INSTALLED=0.0.3 IT_STUB_CANDIDATE=$EDGE_VER WAIRED_VERSION=edge" \
  'Update available' -- --dry-run --check --skip-ollama
# An operator writes the pin the way the release is named; apt holds the
# tilde form for anything published from v0.0.3-rc1 on (waired-agent#780).
# Both spellings, with or without the tag's leading v, reach it.
PINNED_NEW="IT_STUB_VERSIONS=0.0.3~rc1"
run_case_grep zero "pin is translated to the deb spelling" \
  "$UPD IT_STUB_CANDIDATE=0.0.3~rc1 $PINNED_NEW WAIRED_VERSION=0.0.3-rc1" \
  'waired=0\.0\.3~rc1' -- --dry-run --skip-ollama --no-init --yes
run_case_grep zero "pin accepts the tag spelling too" \
  "$UPD IT_STUB_CANDIDATE=0.0.3~rc1 $PINNED_NEW WAIRED_VERSION=v0.0.3-rc1" \
  'waired=0\.0\.3~rc1' -- --dry-run --skip-ollama --no-init --yes

# ...but the translation must not be applied blind. Everything published
# BEFORE v0.0.3-rc1 is in the hyphen form and was not renamed, so both
# spellings are live in the one suite. Translating unconditionally made
# every one of those releases unpinnable — apt answered
# `E: Version '0.0.1~rc10' for 'waired' was not found` (waired-agent#811).
PINNED_OLD="IT_STUB_VERSIONS=0.0.1-rc10,0.0.2-rc8-dev,0.0.2-rc9"
run_case_grep zero "a pre-tilde release is pinned as published" \
  "$UPD IT_STUB_CANDIDATE=0.0.2-rc8-dev $PINNED_OLD WAIRED_VERSION=0.0.2-rc9" \
  'waired=0\.0\.2-rc9' -- --dry-run --skip-ollama --no-init --yes
run_case_grep zero "a pre-tilde release accepts the tag spelling too" \
  "$UPD IT_STUB_CANDIDATE=0.0.2-rc8-dev $PINNED_OLD WAIRED_VERSION=v0.0.2-rc9" \
  'waired=0\.0\.2-rc9' -- --dry-run --skip-ollama --no-init --yes
# The two-hyphen preview tag, whose deb spelling would be 0.0.2~rc8~dev.
run_case_grep zero "a multi-hyphen release is pinned as published" \
  "$UPD IT_STUB_CANDIDATE=0.0.2-rc8-dev $PINNED_OLD WAIRED_VERSION=0.0.2-rc8-dev" \
  'waired=0\.0\.2-rc8-dev' -- --dry-run --skip-ollama --no-init --yes
# Preference when a suite somehow holds both: the .deb spelling wins.
run_case_grep zero "the deb spelling wins when both are present" \
  "$UPD IT_STUB_CANDIDATE=0.0.3~rc1 IT_STUB_VERSIONS=0.0.3~rc1,0.0.3-rc1 WAIRED_VERSION=0.0.3-rc1" \
  'waired=0\.0\.3~rc1' -- --dry-run --skip-ollama --no-init --yes
# Neither spelling in the index: send the current form, so apt names the
# version the operator most likely meant rather than a legacy spelling.
run_case_grep zero "an unknown pin falls back to the deb spelling" \
  "$UPD IT_STUB_CANDIDATE=0.0.3~rc1 WAIRED_VERSION=9.9.9-rc1" \
  'waired=9\.9\.9~rc1' -- --dry-run --skip-ollama --no-init --yes
# A release with no prerelease has one spelling; no probe, no rewrite.
run_case_grep zero "a plain release pin is untouched" \
  "$UPD IT_STUB_CANDIDATE=1.2.3 IT_STUB_VERSIONS=1.2.3 WAIRED_VERSION=1.2.3" \
  'waired=1\.2\.3' -- --dry-run --skip-ollama --no-init --yes

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

# --clean says it removes "ALL local state: config, keys". apt purge cannot
# deliver that: the deb's postrm rmdir's /etc/waired with
# --ignore-fail-on-non-empty, so an operator-placed /etc/waired/authkey
# survived the wipe silently, while /var/lib/waired next to it was an
# unconditional rm -rf (waired-agent#792). The uninstaller now takes the whole
# directory and names what it is taking, because these are by definition files
# waired did not install.
out="$(env IT_STUB_INSTALLED= IT_STUB_ETC_WAIRED="$(printf '/etc/waired\n/etc/waired/agent.env\n/etc/waired/agent.env.bak-verify20260810\n/etc/waired/authkey\n')" \
  sh "$INSTALL_SH" --dry-run --skip-ollama --no-init --clean --yes 2>&1)" || true
if printf '%s' "$out" | grep -q '/etc/waired/authkey'; then
  ok "clean --yes names the non-package files it is about to remove from /etc/waired (#792)"
else
  fail "clean --yes — /etc/waired/authkey not named in the removal log (#792)"
fi
if printf '%s' "$out" | grep -qE '\[dry-run\].*rm -rf /etc/waired'; then
  ok "clean --yes removes /etc/waired whole (#792)"
else
  fail "clean --yes — no rm -rf /etc/waired in the dry-run plan (#792)"
fi
# The opposite host: nothing there, nothing said, nothing removed.
out="$(env IT_STUB_INSTALLED= IT_STUB_ETC_WAIRED= \
  sh "$INSTALL_SH" --dry-run --skip-ollama --no-init --clean --yes 2>&1)" || true
if printf '%s' "$out" | grep -qE 'Removing (the empty )?/etc/waired'; then
  fail "clean --yes announces removing /etc/waired on a host that has none (#792)"
else
  ok "clean --yes stays quiet about /etc/waired when it does not exist (#792)"
fi
# The plain remove tier must not touch it at all: that is the whole
# remove/purge split (--clean is what wipes config).
out="$(env IT_STUB_INSTALLED=0.0.2 IT_STUB_ETC_WAIRED="$(printf '/etc/waired\n/etc/waired/authkey\n')" \
  sh "$ROOT/packaging/install/uninstall.sh" --dry-run 2>&1)" || true
if printf '%s' "$out" | grep -qE 'rm -rf /etc/waired'; then
  fail "uninstall without --clean removes /etc/waired (#792 must stay inside --clean)"
else
  ok "uninstall without --clean leaves /etc/waired alone (#792)"
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

# 4d. GNOME tray host extension (#295). The extension is added to the apt
#     transaction at RUNTIME, gated on gnome-shell already being installed,
#     because apt cannot express "only on a GNOME host": the package name is a
#     virtual one provided by gnome-shell-ubuntu-extensions on Ubuntu 26.04,
#     which `Depends: gnome-shell` — so a Depends/Recommends in the tray package
#     would install a desktop onto every server. That makes the negative case
#     below the safety-critical one, and it is asserted on every host.
run_case_asserts zero "tray host: no GNOME, no extension package" "$FRESH" \
  "$(printf 'Installing packages: waired waired-tray\n!%s\n!GNOME detected' 'gnome-shell-extension-appindicator')" \
  -- --dry-run --skip-ollama --no-init

run_case_asserts zero "tray host: WAIRED_NO_TRAY never adds it" "$FRESH IT_STUB_GNOME=1 WAIRED_NO_TRAY=1" \
  "$(printf '!%s\n!GNOME detected' 'gnome-shell-extension-appindicator')" \
  -- --dry-run --skip-ollama --no-init

# The positive case needs a host with no AppIndicator extension already
# installed, which is the state of a CI runner but not of a real GNOME desktop
# (Ubuntu Desktop ships ubuntu-appindicators, where doing nothing is correct).
# Assert whichever of the two this host actually is, rather than skipping: both
# are real behaviours worth pinning.
GNOME_EXT_PRESENT=0
for uuid in appindicatorsupport@rgcjonas.gmail.com ubuntu-appindicators@ubuntu.com; do
  [ -d "/usr/share/gnome-shell/extensions/$uuid" ] && GNOME_EXT_PRESENT=1
done
if [ "$GNOME_EXT_PRESENT" -eq 0 ]; then
  run_case_asserts zero "tray host: GNOME without an extension pulls one in" "$FRESH IT_STUB_GNOME=1" \
    "$(printf 'GNOME detected\nInstalling packages: waired waired-tray %s' 'gnome-shell-extension-appindicator')" \
    -- --dry-run --skip-ollama --no-init
else
  run_case_asserts zero "tray host: GNOME that already has one is left alone" "$FRESH IT_STUB_GNOME=1" \
    "$(printf 'Installing packages: waired waired-tray\n!%s' 'gnome-shell-extension-appindicator')" \
    -- --dry-run --skip-ollama --no-init
fi

# 5. Bad flag — clean failure, not a set -u error.
run_case nonzero "unknown flag" "$FRESH" -- --bogus

# 6. darwin dispatch (install vs update), driven on this Linux runner via the
# uname / launchctl stubs. The regression this guards: "installed" used to be
# decided by the binary alone, so an install that aborted after the binaries
# landed was sent to the update path — which installs none of the pieces it
# was missing, so the host never converged however many times it ran.
#
# The darwin cases can only run under a shell that resolves `uname` through
# PATH. busybox's shell may run applets — uname among them — without consulting
# PATH at all, depending on how it was built; where it does, the stub never
# takes effect and every case below quietly executes the LINUX arm instead.
# Ask each shell rather than special-casing busybox by name: what decides this
# is whether the stub is reachable, not which shell it is. Narrowing the matrix
# here (with a log line) beats reporting install.sh failures it did not cause.
DARWIN_SHELLS=()
for sh in "${SHELLS[@]}"; do
  if [ "$(env "PATH=$STUBDIR:$PATH" IT_STUB_UNAME_S=Darwin $sh -c 'uname -s' 2>/dev/null)" = Darwin ]; then
    DARWIN_SHELLS+=("$sh")
  else
    log "$sh resolves uname without PATH — skipping the darwin cases for it"
  fi
done
[ "${#DARWIN_SHELLS[@]}" -gt 0 ] || { echo "no shell can drive the darwin cases" >&2; exit 1; }
MATRIX_SHELLS=("${DARWIN_SHELLS[@]}")

DWORK="$(mktemp -d)"
trap 'rm -rf "$STUBDIR" "$DWORK"' EXIT

mkdir -p "$DWORK/bin-empty" "$DWORK/bin-installed"
# The installed binary darwin_detect_installed asks for its version.
# IT_STUB_WAIRED_VERSION drives it so a case can describe the installed
# build; 0.0.1 keeps the pre-existing cases as they were.
cat > "$DWORK/bin-installed/waired" <<'STUB'
#!/bin/sh
case "$*" in
  "version --json") printf '{"version":"%s"}\n' "${IT_STUB_WAIRED_VERSION:-0.0.1}" ;;
esac
exit 0
STUB
printf '#!/bin/sh\nexit 0\n' > "$DWORK/bin-installed/waired-agent"
chmod +x "$DWORK"/bin-installed/*
: > "$DWORK/present.plist"

# A PATH without /usr/local/bin, so darwin_detect_installed's `command -v
# waired` fallback cannot pick up a real install on a developer's own Mac —
# the cases below must describe the host, not the machine running them.
DPATH="PATH=$STUBDIR:/usr/bin:/bin:/usr/sbin:/sbin"
DBASE="$DPATH IT_STUB_UNAME_S=Darwin IT_STUB_UNAME_M=arm64 WAIRED_NO_EMOJI=1"
D_FRESH="$DBASE WAIRED_DARWIN_BINDIR=$DWORK/bin-empty WAIRED_DARWIN_PLIST=$DWORK/absent.plist"
D_HALF="$DBASE WAIRED_DARWIN_BINDIR=$DWORK/bin-installed WAIRED_DARWIN_PLIST=$DWORK/absent.plist"
D_FULL="$DBASE WAIRED_DARWIN_BINDIR=$DWORK/bin-installed WAIRED_DARWIN_PLIST=$DWORK/present.plist IT_STUB_LAUNCHD_LOADED=1"

# Nothing installed → fresh install.
run_case_grep zero "darwin fresh -> install" "$D_FRESH" \
  'Waired is installed \(macOS' -- --dry-run --skip-ollama --no-init --yes
# Binary present but no plist: the aborted-install state. MUST take the fresh
# arm, not the update prompt.
run_case_grep zero "darwin half-install -> install" "$D_HALF" \
  'Waired is installed \(macOS' -- --dry-run --skip-ollama --no-init --yes
# Binary + plist + a job launchd knows about → genuinely installed, update.
run_case_grep zero "darwin complete -> update" "$D_FULL WAIRED_VERSION=edge" \
  'waired updated to edge' -- --dry-run --skip-ollama --no-init --yes
# An explicit --check still reaches the update path on a half-installed host
# (the flag is the operator saying what they want), as on Linux.
run_case_grep zero "darwin half-install --check -> update" "$D_HALF WAIRED_VERSION=edge" \
  'Update available' -- --dry-run --skip-ollama --check

# 6b. The stable channel's version compare (waired-agent#781). This is the
#     arm the campaign found refusing every in-place update on macOS and
#     Windows: the numeric parts of "0.0.2-rc8-dev" and "0.0.2-rc9" match,
#     the prerelease was dropped before comparing, and the installer said
#     "already up to date" to a host eight builds behind. The first four
#     cases fail against that compare.
D_STABLE="$D_FULL"
run_case_grep zero "darwin rc -> next rc is offered" \
  "$D_STABLE IT_STUB_WAIRED_VERSION=0.0.3-rc1 IT_STUB_LATEST_TAG=v0.0.3-rc2" \
  'Update available: 0\.0\.3-rc1 -> 0\.0\.3-rc2' -- --dry-run --skip-ollama --check
run_case_grep zero "darwin rc -> its release is offered" \
  "$D_STABLE IT_STUB_WAIRED_VERSION=0.0.3-rc9 IT_STUB_LATEST_TAG=v0.0.3" \
  'Update available: 0\.0\.3-rc9 -> 0\.0\.3' -- --dry-run --skip-ollama --check
run_case_grep zero "darwin rc2 -> rc10 is offered" \
  "$D_STABLE IT_STUB_WAIRED_VERSION=0.0.3-rc2 IT_STUB_LATEST_TAG=v0.0.3-rc10" \
  'Update available: 0\.0\.3-rc2 -> 0\.0\.3-rc10' -- --dry-run --skip-ollama --check
# The exact pair the campaign was stuck on, in the direction it was stuck.
run_case_grep zero "darwin rc8-dev -> rc9 is offered" \
  "$D_STABLE IT_STUB_WAIRED_VERSION=0.0.2-rc8-dev IT_STUB_LATEST_TAG=v0.0.2-rc9" \
  'Update available: 0\.0\.2-rc8-dev -> 0\.0\.2-rc9' -- --dry-run --skip-ollama --check
run_case_grep zero "darwin same version is up to date" \
  "$D_STABLE IT_STUB_WAIRED_VERSION=0.0.3-rc1 IT_STUB_LATEST_TAG=v0.0.3-rc1" \
  'already up to date' -- --dry-run --skip-ollama --check
run_case_grep zero "darwin release is not offered its own rc" \
  "$D_STABLE IT_STUB_WAIRED_VERSION=0.0.3 IT_STUB_LATEST_TAG=v0.0.3-rc9" \
  'already up to date' -- --dry-run --skip-ollama --check
# The release tag's "v" is not part of the version: printing "0.0.3-rc1 ->
# v0.0.3-rc2" made one line disagree with itself (waired-agent#781 D-1).
run_case_grep zero "darwin latest is printed without the tag v" \
  "$D_STABLE IT_STUB_WAIRED_VERSION=0.0.3-rc1 IT_STUB_LATEST_TAG=v0.0.3-rc2" \
  'waired updated to 0\.0\.3-rc2' -- --dry-run --skip-ollama --no-init --yes
# A pin has to reach the asset base, or the run downloads whatever
# releases/latest happens to be and says nothing about it.
run_case_grep zero "darwin pin drives the release asset base" \
  "$D_STABLE WAIRED_VERSION=0.0.3-rc1" \
  'releases/download/v0\.0\.3-rc1' -- --dry-run --skip-ollama --no-init --yes
run_case_grep zero "darwin pin accepts the tag spelling too" \
  "$D_STABLE WAIRED_VERSION=v0.0.3-rc1" \
  'releases/download/v0\.0\.3-rc1' -- --dry-run --skip-ollama --no-init --yes
# The update path must retire the newsyslog drop-in too (#331), or a host that
# only ever updates keeps a second rotator racing the agent's own — renaming
# the log file out from under the descriptor the agent writes through, which is
# the whole failure this replaced.
run_case_grep zero "darwin update retires the legacy log rotation" "$D_FULL WAIRED_VERSION=edge" \
  'legacy newsyslog rotation at /etc/newsyslog.d/waired-agent.conf' -- --dry-run --skip-ollama --no-init --yes

# 7. darwin registration failure (#193) — the one darwin branch --dry-run
# cannot reach, so these cases run install.sh FOR REAL.
#
# #193: a `waired-agent install` that failed took the whole installer with it
# (`set -eu`), so the rotation step, the control-URL write, `waired init` and
# the next-steps block were all skipped and the last thing on screen was a raw Go
# error. Linux warns and continues; macOS was the outlier. The fix made the
# step non-fatal — but the failure arm lives past `if [ "$DRY_RUN" = 1 ]; then
# ... return 0; fi`, so no --dry-run case above can execute it, and only a real
# macOS host with a genuinely broken launchd would otherwise catch a
# regression.
#
# So this section swaps in a second stub layer, ahead of $STUBDIR on PATH, that
# makes the un-dry-run path survivable on a Linux runner: curl/shasum/tar
# materialise a "release" instead of fetching one, and sudo passes through
# (a real `install`, `chmod`, `mkdir` into temp dirs) instead of swallowing the
# command. Sections 1-6 keep the safe no-op stubs — nothing here changes them.
#
# Still no network and no root: the only thing that reaches outside a temp dir
# is the newsyslog drop-in removal, which is a no-op when the file is absent
# (it is, on a Linux runner).
DSTUB="$(mktemp -d)"
DREAL="$(mktemp -d)"
trap 'rm -rf "$STUBDIR" "$DWORK" "$DSTUB" "$DREAL"' EXIT

# Functional: create whatever -o names, and put the same constant the shasum
# stub prints into the .sha256, so darwin_install_binaries' checksum compare
# passes on its own terms rather than being bypassed.
cat > "$DSTUB/curl" <<'STUB'
#!/bin/sh
out=""; prev=""
for a in "$@"; do [ "$prev" = "-o" ] && out="$a"; prev="$a"; done
[ -n "$out" ] || exit 0
case "$out" in
  *.sha256) printf '%s  x\n' "$IT_STUB_SHA" > "$out" ;;
  *)        printf 'not-a-real-tarball\n' > "$out" ;;
esac
exit 0
STUB
cat > "$DSTUB/shasum" <<'STUB'
#!/bin/sh
printf '%s  -\n' "$IT_STUB_SHA"
STUB
# Functional: stand in for the release tarball. The waired-agent it writes is
# the injection point — `waired-agent install` is what darwin_register_agent
# runs, and IT_STUB_REGISTER_RC is its exit status.
cat > "$DSTUB/tar" <<'STUB'
#!/bin/sh
dir=""; prev=""
for a in "$@"; do [ "$prev" = "-C" ] && dir="$a"; prev="$a"; done
[ -n "$dir" ] || exit 0
cat > "$dir/waired" <<'W'
#!/bin/sh
# `config log-level` is answered because darwin_install seeds the install-time
# level through it (waired-agent#801). Without an answer here the installer
# would poll for its full 30s budget and then warn, on every darwin case.
case "$*" in
  "version --json")     printf '{"version":"0.0.1"}\n' ;;
  "config log-level")   printf 'Log level: info\n' ;;
  "config log-level "*) printf 'Log level set to %s (applied live).\n' "$3" ;;
esac
exit 0
W
# `waired-agent install` creates and locks down the state dir first, then
# bootstraps the LaunchDaemon. #193's failure is the bootstrap one, which
# happens AFTER the dir exists — so create it even when exiting nonzero, or the
# steps that legitimately skip on a missing state dir would look like the
# regression.
cat > "$dir/waired-agent" <<'W'
#!/bin/sh
# The argv echo is what makes the registered service definition observable
# from the matrix: everything after `--` is baked into the plist's
# ProgramArguments, and asserting on what is NOT there is the only way to
# keep a pin from creeping back (waired-agent#801).
printf 'STUB waired-agent argv: %s\n' "$*" >&2
prev=""
for a in "$@"; do [ "$prev" = "--state-dir" ] && mkdir -p "$a"; prev="$a"; done
exit ${IT_STUB_REGISTER_RC:-0}
W
printf '#!/bin/sh\nexit 0\n' > "$dir/waired-tray"
chmod +x "$dir"/waired "$dir"/waired-agent "$dir"/waired-tray
exit 0
STUB
# Pass-through, unlike $STUBDIR's swallowing sudo: the real install/chmod/mkdir
# have to run for the steps after the failed registration to be observable at
# all. `test` is spelled out because $STUBDIR's version answers the enrolment
# probe from IT_STUB_ENROLLED, and here we want the real answer about real dirs.
cat > "$DSTUB/sudo" <<'STUB'
#!/bin/sh
case "$1" in test) shift; exec test "$@" ;; esac
exec "$@"
STUB
chmod +x "$DSTUB"/*

# $DSTUB first, $STUBDIR second: uname / launchctl / sw_vers still come from the
# section-6 layer.
R_PATH="PATH=$DSTUB:$STUBDIR:/usr/bin:/bin:/usr/sbin:/sbin"
R_BASE="$R_PATH IT_STUB_UNAME_S=Darwin IT_STUB_UNAME_M=arm64 WAIRED_NO_EMOJI=1 IT_STUB_SHA=0000000000000000000000000000000000000000000000000000000000000000"
# An empty bindir + absent plist is the fresh-install dispatch, so these run the
# install arm rather than the update one.
# WAIRED_DARWIN_APPDIR joins the other two path overrides for the same reason:
# darwin_install_app really builds Waired.app here (waired-agent#833), and
# /Applications on the runner is neither writable nor ours to touch.
r_env() {  # r_env <case-dir> [extra env...]
  mkdir -p "$DREAL/$1/bin" "$DREAL/$1/apps"
  printf '%s WAIRED_DARWIN_BINDIR=%s/%s/bin WAIRED_DARWIN_APPDIR=%s/%s/apps WAIRED_DARWIN_PLIST=%s/%s/absent.plist WAIRED_STATE_DIR=%s/%s/state %s' \
    "$R_BASE" "$DREAL" "$1" "$DREAL" "$1" "$DREAL" "$1" "$DREAL" "$1" "${2:-}"
}

# The contract: a failed registration warns, says how to recover, and lets
# EVERY later step run. "Waired is installed (macOS" is darwin_next_steps, i.e.
# the far end of the function list darwin_install calls.
run_case_asserts zero "darwin register fails -> install continues" \
  "$(r_env reg-fail IT_STUB_REGISTER_RC=1)" \
  'could not register the background service \(exit 1\)
Retry with: sudo .*/waired-agent install
Waired is installed \(macOS
The background service is NOT registered' \
  -- --skip-ollama --no-init --yes

# The same run without the injected failure. Without this, the case above would
# still pass if the warning became unconditional, and "continues after a
# failure" would stop meaning anything.
run_case_asserts zero "darwin register succeeds -> no failure warning" \
  "$(r_env reg-ok IT_STUB_REGISTER_RC=0)" \
  '!could not register the background service
!The background service is NOT registered
Waired is installed \(macOS' \
  -- --skip-ollama --no-init --yes

# waired-agent#801 on the darwin arm, where the pin used to live in the plist.
# This case runs the registration for real (stubbed binary), so the negative is
# asserted against the argv the stub actually received rather than against a
# dry-run echo: everything after `--` becomes a ProgramArguments token, and a
# log level there outranks agent.json at every boot. The positive lines are
# what stop the negative from passing on a run that never registered anything.
run_case_asserts zero "darwin --log-level: seeded, not baked into the plist (waired-agent#801)" \
  "$(r_env loglevel IT_STUB_REGISTER_RC=0)" \
  '!STUB waired-agent argv:.*--log-level
STUB waired-agent argv: install --state-dir
Setting the agent log level to debug
Waired is installed \(macOS' \
  -- --skip-ollama --no-init --yes --log-level debug

# Configuration lands too, not just console output: agent.env is what a later
# bare `sudo waired init` reads the Control Plane URL back from (#42), and it is
# written two steps past the registration that failed. Asserted on the file, and
# run once — darwin_write_control_url deliberately leaves an existing active
# setting alone, so a second shell would take that arm instead.
R_CTRL_DIR="$DREAL/reg-fail-control"
mkdir -p "$R_CTRL_DIR/bin"
env $(r_env reg-fail-control IT_STUB_REGISTER_RC=1) sh "$INSTALL_SH" \
  --skip-ollama --no-init --yes --control http://127.0.0.1:9479 >/dev/null 2>&1 || true
if grep -q '^WAIRED_CONTROL_URL=http://127.0.0.1:9479$' "$R_CTRL_DIR/state/agent.env" 2>/dev/null; then
  ok "darwin register fails -> agent.env still written"
else
  fail "darwin register fails — no WAIRED_CONTROL_URL in $R_CTRL_DIR/state/agent.env"
fi

# --- 7b. macOS: the Waired app (waired-agent#833) -----------------------
# The tray used to be a bare Mach-O in /usr/local/bin: nothing in Spotlight,
# nothing in Launchpad, nothing to double-click, and an install banner telling
# the user to run a terminal one-liner instead. It now ships as a real bundle.
# These run on the reg-ok tree the case above already built.
R_APP="$DREAL/reg-ok/apps/Waired.app"
if [ -x "$R_APP/Contents/MacOS/waired-tray" ]; then
  ok "darwin install builds Waired.app around the tray binary"
else
  fail "darwin install — no executable at $R_APP/Contents/MacOS/waired-tray"
fi
# LSUIElement is what makes it a menu-bar accessory rather than a Dock app.
if grep -q 'LSUIElement' "$R_APP/Contents/Info.plist" 2>/dev/null &&
   grep -q 'ai.waired.tray' "$R_APP/Contents/Info.plist" 2>/dev/null; then
  ok "darwin Info.plist declares the bundle id and LSUIElement"
else
  fail "darwin Info.plist missing LSUIElement / bundle id ($R_APP/Contents/Info.plist)"
fi
# One binary, reachable by both names.
if [ -L "$DREAL/reg-ok/bin/waired-tray" ]; then
  ok "darwin bindir waired-tray is a symlink into the bundle"
else
  fail "darwin bindir waired-tray is not a symlink (two copies would drift)"
fi

# WAIRED_NO_TRAY still means no app at all.
run_case_asserts zero "darwin WAIRED_NO_TRAY skips the app" \
  "$(r_env no-tray WAIRED_NO_TRAY=1)" \
  'Waired app:  skipped \(WAIRED_NO_TRAY\)
!Installing the Waired app' \
  -- --skip-ollama --no-init --yes
if [ -e "$DREAL/no-tray/apps/Waired.app" ]; then
  fail "darwin WAIRED_NO_TRAY still built $DREAL/no-tray/apps/Waired.app"
else
  ok "darwin WAIRED_NO_TRAY leaves /Applications alone"
fi

# The launch decision. The runner has no Aqua session (the launchctl stub
# answers `print` from IT_STUB_LAUNCHD_LOADED, unset here), which is the same
# answer an SSH install gets on a real Mac -- and the banner must SAY so
# rather than repeat the old "launch it once; it then returns at every login",
# which was describing a mechanism that did not exist.
run_case_asserts zero "darwin without a GUI session says so instead of claiming autostart" \
  "$(r_env no-gui)" \
  'No GUI login session detected
not started .* no GUI login session was detected
!it then returns at every login' \
  -- --skip-ollama --no-init --yes

# The uninstaller takes the bundle back. Without this, the one line that
# removes what darwin_install_app creates is touched by no test at all —
# which is the shape of defect this whole change is about. Dry-run, so the
# assert is on the plan; the harness has no macOS to remove anything from.
U_APPS="$DREAL/uninst/apps"
mkdir -p "$U_APPS/Waired.app/Contents/MacOS" "$DREAL/uninst/bin"
: > "$DREAL/uninst/bin/waired-agent"
chmod +x "$DREAL/uninst/bin/waired-agent"
out="$(env $R_PATH IT_STUB_UNAME_S=Darwin IT_STUB_UNAME_M=arm64 WAIRED_NO_EMOJI=1 \
  WAIRED_DARWIN_BINDIR="$DREAL/uninst/bin" WAIRED_DARWIN_APPDIR="$U_APPS" \
  sh "$ROOT/packaging/install/uninstall.sh" --dry-run 2>&1)" || true
if printf '%s' "$out" | grep -qE '\[dry-run\].*rm -rf .*Waired\.app'; then
  ok "darwin uninstall removes the Waired.app the installer built (#833)"
else
  fail "darwin uninstall — no rm -rf of Waired.app in the dry-run plan (#833)"
fi
# Existence-gated: a host that never had one is not told about it.
rm -rf "$U_APPS/Waired.app"
out="$(env $R_PATH IT_STUB_UNAME_S=Darwin IT_STUB_UNAME_M=arm64 WAIRED_NO_EMOJI=1 \
  WAIRED_DARWIN_BINDIR="$DREAL/uninst/bin" WAIRED_DARWIN_APPDIR="$U_APPS" \
  sh "$ROOT/packaging/install/uninstall.sh" --dry-run 2>&1)" || true
if printf '%s' "$out" | grep -q 'Waired\.app'; then
  fail "darwin uninstall announces a Waired.app that is not there (#833)"
else
  ok "darwin uninstall stays quiet about Waired.app when there is none (#833)"
fi

# And the pure decision itself, over all four states, driven directly. The
# `launch` arm is the one no end-to-end case here can reach: the runner has no
# Aqua session and never will, which is exactly why the decision is a function
# and not an inline chain of tests.
#
# The body is lifted out of the shipped file rather than copied, the way
# installtest-swap.ps1 lifts install.ps1's functions -- a copy drifts, and this
# is a guard against drift. install.sh cannot simply be sourced: it runs main.
DTP_FN="$(awk '/^darwin_tray_launch_plan\(\) \{$/,/^\}$/' "$INSTALL_SH")"
if [ -z "$DTP_FN" ]; then
  fail "install.sh has no darwin_tray_launch_plan to lift (#833)"
fi
dtp() { # dtp <no_tray> <shipped> <gui> <want>
  got="$(printf '%s\ndarwin_tray_launch_plan "$1" "$2" "$3"\n' "$DTP_FN" | sh -s -- "$1" "$2" "$3")"
  if [ "$got" = "$4" ]; then
    ok "darwin_tray_launch_plan('$1','$2','$3') = $4"
  else
    fail "darwin_tray_launch_plan('$1','$2','$3') = '$got', want '$4'"
  fi
}
dtp 1 1 1 skip:no-tray
dtp '' 0 1 skip:not-installed
dtp '' 1 0 skip:no-gui-session
dtp '' 1 1 launch

# The update path's login-item notice (waired-agent#832 follow-up). It must
# speak ONLY when it positively knows the login item is missing for a user who
# has a desktop -- an update that registered the item itself would silently
# overturn a user who switched "Start Waired on login" off, since the plist's
# absence is the only record either way.
DTN_FN="$(awk '/^darwin_tray_autostart_notice\(\) \{$/,/^\}$/' "$INSTALL_SH")"
if [ -z "$DTN_FN" ]; then
  fail "install.sh has no darwin_tray_autostart_notice to lift (#832 follow-up)"
fi
dtn() { # dtn <no_tray> <gui> <state> <expect-empty|expect-notice>
  got="$(printf '%s\ndarwin_tray_autostart_notice "$1" "$2" "$3" alice\n' "$DTN_FN" | sh -s -- "$1" "$2" "$3")"
  case "$4" in
    empty)
      if [ -z "$got" ]; then ok "notice('$1','$2','$3') stays quiet"
      else fail "notice('$1','$2','$3') spoke when it should not: [$got]"; fi ;;
    notice)
      if printf '%s' "$got" | grep -q 'not set to start when alice'; then
        ok "notice('$1','$2','$3') names the user and what to do"
      else fail "notice('$1','$2','$3') = [$got]"; fi ;;
  esac
}
dtn ''  1 absent  notice
dtn ''  1 present empty
dtn ''  1 unknown empty
dtn ''  0 absent  empty
dtn 1   1 absent  empty

echo
log "summary: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] || exit 1
