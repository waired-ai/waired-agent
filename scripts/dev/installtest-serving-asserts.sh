#!/usr/bin/env bash
# installtest-serving-asserts.sh — run every branch of the three
# serving-engine asserts (#494) and require all three to agree, per PR.
#
# WHY THIS EXISTS
# ---------------
# assert_serving_ollama (lib/installtest-enroll.sh), assert_serving_ollama_macos
# (installtest-macos.sh) and Assert-ServingEngine (installtest-windows.ps1) are
# the CI proof that the engine SERVING requests is waired's own at the pinned
# version. They execute only in installtest-inference, which is
# schedule/dispatch-only — so a copy that had quietly stopped being able to
# fail would stay green for a long time, and nothing on a PR would say so.
# That is the exact shape #178/#215/#505 already cost this repo, in these same
# files.
#
# So: drive each copy through the same seven scenarios with its host commands
# stubbed, normalize away the three things that legitimately differ per OS, and
# require the three transcripts to be byte-identical AND to match the expected
# transcript below. A wrong verdict, a mixed-up message, a branch that stopped
# being reachable, and a wording drift between the copies all land as a diff.
#
# The functions are LIFTED from the harnesses (sourced / sed-extracted / AST-
# extracted), never copied here: what this exercises is what the legs run.
#
# Normalized away, and only these:
#   <BIN>    the state-dir engine path      (/var/lib/waired… vs /Library/… vs C:\ProgramData\…)
#   <OTHER>  a foreign engine's path
#   <TOOL>   ss | lsof | Get-NetTCPConnection
#   --       the em dash the .sh harnesses use, the ASCII pair the .ps1 uses
#
# Run: bash scripts/dev/installtest-serving-asserts.sh

# File-level, with its reason, rather than one per stub: every ok()/bad()/gx()
# below is called from the harness code this file sources or evals. That call
# graph is invisible to static analysis, so each stub is reported unreachable.
# shellcheck disable=SC2317
set -uo pipefail

ROOT="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)"
cd "$ROOT"

# The transcript every copy must produce. Read it as the specification: each
# line is one assert the leg will print, and the scenario above it is the host
# state that produces it. `all-good` is the only scenario with no FAIL.
read -r -d '' EXPECTED <<'EOF'
# all-good
ok   the process serving :9475 is the state-dir binary (ctx; pid 4242)
ok   the serving engine is the pinned release (ctx; /api/version = 0.31.1)
ok   waired spawned the serving engine (ctx; mode=spawned)
# foreign-binary-on-the-port
FAIL the process serving :9475 is not waired's engine (ctx): pid=4242 exe=<OTHER>, expected <BIN>
ok   the serving engine is the pinned release (ctx; /api/version = 0.31.1)
ok   waired spawned the serving engine (ctx; mode=spawned)
# version-mismatch
ok   the process serving :9475 is the state-dir binary (ctx; pid 4242)
FAIL the serving engine is not the pinned release (ctx): /api/version = 0.24.0, pinned 0.31.1
ok   waired spawned the serving engine (ctx; mode=spawned)
# adopted-engine
ok   the process serving :9475 is the state-dir binary (ctx; pid 4242)
ok   the serving engine is the pinned release (ctx; /api/version = 0.31.1)
FAIL waired did not spawn the serving engine (ctx; mode=adopted) -- it adopted a process it does not supervise
# daemon-silent
ok   the process serving :9475 is the state-dir binary (ctx; pid 4242)
FAIL the daemon published no pinned_version (ctx) -- the version comparison would be vacuous
FAIL the daemon published no engine mode (ctx) -- cannot tell a spawned engine from an adopted one
# listener-unidentifiable
FAIL could not identify the process listening on :9475 (ctx) -- <TOOL> found no listening process
ok   the serving engine is the pinned release (ctx; /api/version = 0.31.1)
ok   waired spawned the serving engine (ctx; mode=spawned)
# engine-not-answering
FAIL nothing is serving on :9475 after 180 s (ctx) -- the engine is installed but not answering
EOF

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

GOOD_STATUS='{"subsystem_state":"ready","runtimes":{"ollama":{"installed":true,"state":"ready","mode":"spawned","live_version":"0.31.1","pinned_version":"0.31.1"},"vllm":{"installed":false,"state":"absent"}}}'
ADOPTED_STATUS="${GOOD_STATUS/\"mode\":\"spawned\"/\"mode\":\"adopted\"}"

# normalize <bin> <other> <tool> — stdin to stdout. The em dash goes last so a
# path containing one (there are none, but the order should not matter) is
# already gone.
normalize() {
  sed -e "s|$1|<BIN>|g" -e "s|$2|<OTHER>|g" -e "s|\\b$3\\b|<TOOL>|g" -e 's/—/--/g'
}

# --- Linux: source the library and stub gx() -------------------------------
# Sourced under `set -euo pipefail`, the flags installtest-run.sh runs it with,
# so an errexit trap the function sets off here would also fire in the leg.
linux_transcript() {
  (
    set -euo pipefail
    IT_LOGDIR="$WORK"
    IT_BUNDLED_OLLAMA_BIN=/var/lib/waired/runtimes/ollama/bin/ollama
    FOREIGN=/usr/local/bin/ollama
    LINES=()
    ok()      { LINES+=("ok   $*"); }
    bad()     { LINES+=("FAIL $*"); }
    it_warn() { :; }
    it_log()  { :; }
    sleep()   { :; }        # the not-answering scenario must not take 90 s

    VERSION_BODY='' STATUS_BODY='' LISTENER_PID='' LISTENER_EXE=''
    gx() {
      shift                 # the guest name
      case "$1" in
        curl)
          case "$*" in
            *9475/api/version*) [ -n "$VERSION_BODY" ] && printf '%s' "$VERSION_BODY" || return 1 ;;
            *)                  [ -n "$STATUS_BODY"  ] && printf '%s' "$STATUS_BODY"  || return 1 ;;
          esac ;;
        readlink) [ -n "$LISTENER_EXE" ] && printf '%s\n' "$LISTENER_EXE" || return 1 ;;
        sh)       case "$*" in *ss\ -Hltpn*) printf '%s\n' "$LISTENER_PID" ;; *) : ;; esac ;;
        *) : ;;
      esac
    }

    # shellcheck source=scripts/dev/lib/installtest-enroll.sh
    . scripts/dev/lib/installtest-enroll.sh

    run() { LINES=(); assert_serving_ollama guest0 ctx; printf '# %s\n' "$1"; printf '%s\n' "${LINES[@]}"; }

    VERSION_BODY='{"version":"0.31.1"}'; STATUS_BODY="$GOOD_STATUS"
    LISTENER_PID=4242; LISTENER_EXE="$IT_BUNDLED_OLLAMA_BIN"
    run all-good
    LISTENER_EXE="$FOREIGN";                       run foreign-binary-on-the-port
    LISTENER_EXE="$IT_BUNDLED_OLLAMA_BIN"
    VERSION_BODY='{"version":"0.24.0"}';           run version-mismatch
    VERSION_BODY='{"version":"0.31.1"}'
    STATUS_BODY="$ADOPTED_STATUS";                 run adopted-engine
    STATUS_BODY='';                                run daemon-silent
    STATUS_BODY="$GOOD_STATUS"; LISTENER_PID='';   run listener-unidentifiable
    LISTENER_PID=4242; VERSION_BODY='';            run engine-not-answering
  ) 2>/dev/null | normalize /var/lib/waired/runtimes/ollama/bin/ollama /usr/local/bin/ollama ss
}

# --- macOS: lift the function out (the script installs; it cannot be sourced)
macos_transcript() {
  (
    set -uo pipefail        # installtest-macos.sh's own flags — no -e
    STATE_DIR='/Library/Application Support/waired'
    BIN="$STATE_DIR/runtimes/ollama/bin/ollama"
    FOREIGN=/opt/homebrew/bin/ollama
    LINES=()
    ok()    { LINES+=("ok   $*"); }
    bad()   { LINES+=("FAIL $*"); }
    sleep() { :; }

    VERSION_BODY='' STATUS_BODY='' LISTENER_PID='' LISTENER_EXE=''
    curl() {
      case "$*" in
        *9475/api/version*) [ -n "$VERSION_BODY" ] && printf '%s' "$VERSION_BODY" || return 1 ;;
        *)                  [ -n "$STATUS_BODY"  ] && printf '%s' "$STATUS_BODY"  || return 1 ;;
      esac
    }
    sudo() {
      case "$1" in
        lsof) printf '%s\n' "$LISTENER_PID" ;;
        ps)   [ -n "$LISTENER_EXE" ] && printf '%s\n' "$LISTENER_EXE" || return 1 ;;
        *)    : ;;
      esac
    }

    # Brace at column 0 closes the function, which is how this file is written.
    eval "$(sed -n '/^assert_serving_ollama_macos() {$/,/^}$/p' scripts/dev/installtest-macos.sh)"
    if ! declare -F assert_serving_ollama_macos >/dev/null; then
      echo "could not extract assert_serving_ollama_macos from installtest-macos.sh" >&2
      exit 1
    fi

    run() { LINES=(); assert_serving_ollama_macos ctx; printf '# %s\n' "$1"; printf '%s\n' "${LINES[@]}"; }

    VERSION_BODY='{"version":"0.31.1"}'; STATUS_BODY="$GOOD_STATUS"
    LISTENER_PID=4242; LISTENER_EXE="$BIN"
    run all-good
    LISTENER_EXE="$FOREIGN";                     run foreign-binary-on-the-port
    LISTENER_EXE="$BIN"
    VERSION_BODY='{"version":"0.24.0"}';         run version-mismatch
    VERSION_BODY='{"version":"0.31.1"}'
    STATUS_BODY="$ADOPTED_STATUS";               run adopted-engine
    STATUS_BODY='';                              run daemon-silent
    STATUS_BODY="$GOOD_STATUS"; LISTENER_PID=''; run listener-unidentifiable
    LISTENER_PID=4242; VERSION_BODY='';          run engine-not-answering
  ) 2>/dev/null | normalize '/Library/Application Support/waired/runtimes/ollama/bin/ollama' /opt/homebrew/bin/ollama lsof
}

# --- Windows: pwsh, which is preinstalled on the runner that runs this ------
# The two paths come back on marker lines rather than being repeated here: the
# .ps1 has to spell them without a drive letter to run under pwsh on Linux, and
# a second copy of that spelling is one more thing to drift.
windows_transcript() {
  local raw bin other
  raw="$(pwsh -NoProfile -File scripts/dev/installtest-serving-asserts.ps1 2>/dev/null)"
  bin="$(printf '%s\n' "$raw" | sed -n 's/^#BIN //p')"
  other="$(printf '%s\n' "$raw" | sed -n 's/^#OTHER //p')"
  if [ -z "$bin" ] || [ -z "$other" ]; then
    echo "the .ps1 produced no #BIN/#OTHER markers — rerun it directly to see why" >&2
    return 1
  fi
  printf '%s\n' "$raw" | grep -v '^#BIN \|^#OTHER ' | normalize "$bin" "$other" Get-NetTCPConnection
}

command -v pwsh >/dev/null 2>&1 || {
  echo "error: pwsh not found in PATH — the Windows copy cannot be checked, and a" >&2
  echo "       two-of-three run would report agreement it did not establish." >&2
  exit 1
}

printf '%s\n' "$EXPECTED" > "$WORK/expected"
linux_transcript   > "$WORK/linux"
macos_transcript   > "$WORK/macos"
windows_transcript > "$WORK/windows"

rc=0
for os in linux macos windows; do
  if diff -u "$WORK/expected" "$WORK/$os" > "$WORK/$os.diff"; then
    echo "installtest-serving-asserts: $os matches the expected transcript"
  else
    echo "installtest-serving-asserts: $os DIFFERS from the expected transcript" >&2
    sed 's/^/    /' "$WORK/$os.diff" >&2
    rc=1
  fi
done
exit "$rc"
