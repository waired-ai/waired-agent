#!/usr/bin/env bash
# harness-failure-strings-guard.sh — the installtest harnesses fail a leg when
# `waired init`'s transcript contains an engine-install failure line (#215).
# That assert is a grep for product WORDING, and a grep for wording the product
# no longer prints is green forever — the same "test that cannot fail" shape
# #178 shipped through in the first place.
#
# So for each alternation this checks two things:
#
#   1. The three harnesses declare the SAME alternation. They are separate
#      files with no shared runtime, and a fix applied to one of them is the
#      normal way this drifts.
#   2. Every branch of that alternation still appears in the Go source that
#      prints it. A rename lands here as a lint failure, in the same PR.
#
# Deliberately NOT a search of the whole tree for "install failed": the point
# is that the harness and the producer agree on an exact string, so the check
# has to be the exact string.
#
# Ten alternations are covered. Six are keyed on `waired init`'s transcript;
# one is a `waired models pull` transcript, which is the same problem on
# a different command — the harnesses grep it as a present-assert AND an
# absent-assert, so a rename would half-pass silently; and the last is not
# transcript wording at all but JSON field names, for the same reason one step
# further out (see status-fields below):
#
#   install-failure     the engine install did not succeed (#215/#178)
#   bench-not-ready     the end-of-init benchmark never ran because the model
#                       was not ready (#382) — the arm that decides whether the
#                       red names the download or the engine
#   engine-opt-out      the executor reached the WAIRED_NO_OLLAMA arm (#551)
#   install-failure-box the block init must NOT print for that arm (#551).
#                       Asserted ABSENT, which is why it is here: an
#                       absent-assert for wording the product no longer prints
#                       passes forever — #178 with the sign flipped.
#   role-guidance       the #756 inference-role block, asserted ABSENT on the
#                       engine-opt-out host (#1051) — the same sign-flipped
#                       shape as install-failure-box, one ending along
#   unfit-skip-note     the step-4 non-interactive default's skip note
#                       (#584/#590) — the anti-vacuity assert of the
#                       below-spec default probe
#   no-model-line       the model picker acknowledging "don't download a model
#                       now" (#586/#590) — the anti-vacuity assert of the
#                       engine-only probe, and the one that tells an install
#                       where the operator declined from one where the picker
#                       never ran at all
#   pull-decline        `waired models pull --yes` declining a model that does
#                       not fit (#583/#590), asserted PRESENT for --yes and
#                       ABSENT for --yes --force
#   status-fields       the /waired/v1/inference/status field NAMES the
#                       model-readiness verdict is built from (#573). Not
#                       wording — but the harnesses match these literals out of
#                       a JSON payload, so a renamed field reads as "nothing
#                       decided yet" and the leg goes red blaming the download
#                       for a rename. All three are `omitempty`, so the
#                       harnesses cannot assert them present at runtime without
#                       failing on a healthy host; this check is the only place
#                       the rename can be caught, and it catches it in the PR
#                       that makes it
#   daemon-evidence     the daemon-log lines the three not-ready dumps grep
#                       for (#540/#579) — the boot pre-pull's hold, the model
#                       selection, and the #496 host-speed measurement that
#                       runs in front of the download. Not an assert: a dump
#                       that matches nothing prints one "(no lines)" line and
#                       the leg still goes red, just unreadably, which is
#                       exactly how run 31316731884 ended
#
# Run: bash scripts/ci/harness-failure-strings-guard.sh
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "${root}"

sh_lib='scripts/dev/lib/installtest-enroll.sh'
sh_mac='scripts/dev/installtest-macos.sh'
ps_win='scripts/dev/installtest-windows.ps1'

# Every branch must still be printed by the product. Searched as a literal in
# the Go sources that own the first-run narration.
#
# cmd/waired-agent is in the list because the daemon narrates half of the
# install too: the boot pre-pull and the #496 host-speed measurement print from
# there, not from the CLI, and the daemon-evidence set below greps for those
# lines. Widening the search can only make more branches findable — a branch
# passes when ANY producer contains it — so the existing sets are unaffected.
producers=(cmd/waired cmd/waired-agent internal)

# Read each declaration. Single-quoted, one line, one per file — matching the
# assignment rather than a comment marker keeps the value that the harness
# actually greps with as the single source of truth. The variable NAME is a
# parameter so a second alternation costs one call, not a second copy of the
# checks: two near-identical blocks is how the checks themselves would drift.
read_sh() {
  sed -n "s/^$2='\([^']*\)'.*/\1/p" "$1" | head -1
}
read_ps() {
  sed -n "s/^\\\$$2 = '\([^']*\)'.*/\1/p" "$1" | head -1
}

# check_set <label> <sh-var> <ps-var> — the whole guard for one alternation.
# Returns 1 rather than exiting, so a run reports BOTH sets instead of hiding
# the second behind the first.
check_set() {
  local label="$1" shvar="$2" psvar="$3"
  local lib_re mac_re win_re b
  lib_re="$(read_sh "${sh_lib}" "${shvar}")"
  mac_re="$(read_sh "${sh_mac}" "${shvar}")"
  win_re="$(read_ps "${ps_win}" "${psvar}")"

  local absent=0 pair f v
  for pair in "${sh_lib}|${lib_re}" "${sh_mac}|${mac_re}" "${ps_win}|${win_re}"; do
    f="${pair%%|*}"; v="${pair#*|}"
    if [ -z "${v}" ]; then
      echo "::error file=${f}::no ${label} alternation found (expected ${shvar}='...' or \$${psvar} = '...')" >&2
      absent=1
    fi
  done
  [ "${absent}" -eq 0 ] || return 1

  if [ "${lib_re}" != "${mac_re}" ] || [ "${lib_re}" != "${win_re}" ]; then
    {
      echo "::error::the three installtest harnesses disagree on the ${label} strings"
      echo "  ${sh_lib}: ${lib_re}"
      echo "  ${sh_mac}: ${mac_re}"
      echo "  ${ps_win}: ${win_re}"
      echo "They assert the same product behaviour on three OSes; a leg that greps"
      echo "for something the others do not is a hole, not a platform difference."
    } >&2
    return 1
  fi

  local missing=() branches=()
  IFS='|' read -r -a branches <<<"${lib_re}"
  for b in "${branches[@]}"; do
    [ -n "${b}" ] || continue
    # -e for the pattern, --include BEFORE the paths: after a `--` terminator
    # grep reads --include as a filename and errors on it, which made the search
    # exit non-zero for the wrong reason.
    if ! grep -rqF --include='*.go' -e "${b}" "${producers[@]}"; then
      missing+=("${b}")
    fi
  done

  if [ "${#missing[@]}" -gt 0 ]; then
    {
      echo "::error::the harnesses grep for ${label} wording the product no longer prints"
      printf '  %s\n' "${missing[@]}"
      echo "Searched: ${producers[*]} (*.go)."
      echo
      echo "A grep that can never match is a leg that can never go red — which is"
      echo "exactly how #178 stayed green for five days. Update the alternation in"
      echo "all three harnesses to the wording the product prints now."
    } >&2
    return 1
  fi

  echo "  ${label}: ok — ${#branches[@]} strings, agreed across 3 harnesses and present in the product"
}

fail=0
check_set 'install-failure'     'IT_INSTALL_FAILURE_RE'     'InstallFailureRe'     || fail=1
check_set 'bench-not-ready'     'IT_BENCH_NOT_READY_RE'     'BenchNotReadyRe'      || fail=1
check_set 'engine-opt-out'      'IT_ENGINE_OPTOUT_RE'       'EngineOptOutRe'       || fail=1
check_set 'install-failure-box' 'IT_INSTALL_FAILURE_BOX_RE' 'InstallFailureBoxRe'  || fail=1
check_set 'unfit-skip-note'     'IT_UNFIT_SKIP_RE'          'UnfitSkipRe'          || fail=1
check_set 'role-guidance'       'IT_ROLE_GUIDANCE_RE'       'RoleGuidanceRe'       || fail=1
check_set 'no-model-line'       'IT_NO_MODEL_RE'            'NoModelRe'            || fail=1
check_set 'pull-decline'        'IT_PULL_DECLINE_RE'        'PullDeclineRe'        || fail=1
check_set 'status-fields'       'IT_STATUS_FIELDS_RE'       'StatusFieldsRe'       || fail=1
check_set 'daemon-evidence'     'IT_DAEMON_EVIDENCE_RE'     'DaemonEvidenceRe'     || fail=1
[ "${fail}" -eq 0 ] || exit 1

echo "harness-failure-strings-guard: ok"
