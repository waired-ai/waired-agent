#!/usr/bin/env bash
# installtest-model-ready-asserts.sh — run the three copies of the
# model-readiness verdict over one shared fixture table and require all three
# to agree, per PR.
#
# WHY THIS EXISTS
# ---------------
# it_model_ready_state (lib/installtest-enroll.sh), its twin in
# installtest-macos.sh and Get-ModelReadyState (installtest-windows.ps1) decide
# whether an `install+inference` leg saw the bundled model arrive. They run
# ONLY in installtest-inference, which is schedule/dispatch-only — so a copy
# that had quietly stopped being able to fail would stay green for a long time,
# and nothing on a PR would say so.
#
# That is not hypothetical here. It is waired-agent#573: all three copies read
# `models.ready` and reported the first entry, and #496's host-cutoff probe
# lands in `models.ready` like any other pull — so a host that selected nothing
# and downloaded only the 1 GB measurement probe printed
#
#     [installtest]  ok  bundled model ready in waired store :9475 (qwen3.5-0.8b; via mgmt API)
#
# for months. A green `ok`, naming the probe, on exactly the state the leg
# exists to catch. This file is the instrument that stops that class from
# recurring: it is the sibling of installtest-serving-asserts.sh, which exists
# for the same reason one assert along.
#
# The functions are LIFTED from the harnesses (sourced / sed-extracted / AST-
# extracted), never copied here: what this exercises is what the legs run. The
# payloads live in scripts/dev/testdata/inference-status/ and are read by all
# three, so there is exactly ONE copy of each scenario — three copies of a
# fixture agreeing with each other would prove nothing.
#
# Run: bash scripts/dev/installtest-model-ready-asserts.sh
set -uo pipefail

ROOT="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)"
cd "$ROOT"

FIXTURES='scripts/dev/testdata/inference-status'

# The verdict every copy must reach, for every fixture. Read it as the
# specification: the left column is the payload, the right is what the leg
# concludes from it.
#
#   ready <id>  the daemon is COMMITTED to serving <id> and <id> is on disk
#   probe <id>  the only weights on disk are the host-cutoff probe and nothing
#               was selected — a probe, not a pick (#573)
#   none        the operator's standing "no model now" choice (#586)
#   pending     nothing decided yet
read -r -d '' EXPECTED <<'EOF'
01-pick ready qwen3.5-9b
02-subsystem-ready ready qwen3.5-9b
03-probe-only probe qwen3.5-0.8b
04-no-model-selected none
05-active-not-yet-ready pending
06-probe-plus-another pending
07-nothing-yet pending
08-no-payload pending
EOF

# Why each fixture is in the table, in one line each:
#
#   01-pick                a real selection. Also the adversarial payload:
#                          `active_endpoints` sits BEFORE `active`, and
#                          `available_update` carries its own `model_id`, so a
#                          reader that matched either would name the wrong
#                          model here.
#   02-subsystem-ready     the second accepting arm, kept from before #573.
#   03-probe-only          #573 itself — probe on disk, nothing selected.
#   04-no-model-selected   #586's standing choice. Terminal: nothing is coming.
#   05-active-not-yet-ready  a pick whose 20-45 GB has not landed. The ordinary
#                          state for most of the poll, and the one a careless
#                          #573 fix reports as "a probe, not a pick" — which
#                          would put a false claim in the red on every leg
#                          whose download simply ran long.
#   06-probe-plus-another  probe plus something else: not the probe-only shape.
#   07-nothing-yet         cold start.
#   08-no-payload          an unreachable daemon must not read as a verdict.

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fixtures() { find "$FIXTURES" -name '*.json' | sort; }

# --- Linux: source the library ---------------------------------------------
# Sourced under `set -euo pipefail`, the flags installtest-run.sh runs it with,
# so an errexit trap the function sets off here would also fire in the leg.
linux_transcript() {
  (
    set -euo pipefail
    ok() { :; }; bad() { :; }; it_log() { :; }; it_warn() { :; }
    IT_LOGDIR="$WORK"
    IT_BUNDLED_OLLAMA_BIN=/var/lib/waired/runtimes/ollama/bin/ollama

    # shellcheck source=scripts/dev/lib/installtest-enroll.sh
    . scripts/dev/lib/installtest-enroll.sh

    for f in $(fixtures); do
      printf '%s %s\n' "$(basename "$f" .json)" "$(it_model_ready_state "$(cat "$f")")"
    done
  ) 2>/dev/null
}

# --- macOS: lift the functions out (the script installs; it cannot be sourced)
macos_transcript() {
  (
    set -uo pipefail        # installtest-macos.sh's own flags — no -e
    local fn
    for fn in it_json_object it_json_str it_json_true it_models_ready it_model_ready_state; do
      # Brace at column 0 closes the function, which is how that file is written.
      eval "$(sed -n "/^$fn() {\$/,/^}\$/p" scripts/dev/installtest-macos.sh)"
      if ! declare -F "$fn" >/dev/null; then
        echo "could not extract $fn from installtest-macos.sh" >&2
        exit 1
      fi
    done

    for f in $(fixtures); do
      printf '%s %s\n' "$(basename "$f" .json)" "$(it_model_ready_state "$(cat "$f")")"
    done
  ) 2>/dev/null
}

# --- Windows: pwsh, which is preinstalled on the runner that runs this ------
windows_transcript() {
  pwsh -NoProfile -File scripts/dev/installtest-model-ready-asserts.ps1 2>/dev/null
}

command -v pwsh >/dev/null 2>&1 || {
  echo "error: pwsh not found in PATH — the Windows copy cannot be checked, and a" >&2
  echo "       two-of-three run would report agreement it did not establish." >&2
  exit 1
}

count="$(fixtures | wc -l | tr -d ' ')"
want="$(printf '%s\n' "$EXPECTED" | wc -l | tr -d ' ')"
if [ "$count" != "$want" ]; then
  echo "::error::$FIXTURES holds $count fixtures but the expected transcript has $want lines." >&2
  echo "A fixture with no expected verdict is a scenario nothing checks — add the line." >&2
  exit 1
fi

printf '%s\n' "$EXPECTED" > "$WORK/expected"
linux_transcript   > "$WORK/linux"
macos_transcript   > "$WORK/macos"
windows_transcript > "$WORK/windows"

rc=0
for os in linux macos windows; do
  if diff -u "$WORK/expected" "$WORK/$os" > "$WORK/$os.diff"; then
    echo "installtest-model-ready-asserts: $os matches the expected transcript"
  else
    echo "installtest-model-ready-asserts: $os DIFFERS from the expected transcript" >&2
    sed 's/^/    /' "$WORK/$os.diff" >&2
    rc=1
  fi
done
exit "$rc"
