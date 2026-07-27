#!/usr/bin/env bash
# harness-failure-strings-guard.sh — the installtest harnesses fail a leg when
# `waired init`'s transcript contains an engine-install failure line (#215).
# That assert is a grep for product WORDING, and a grep for wording the product
# no longer prints is green forever — the same "test that cannot fail" shape
# #178 shipped through in the first place.
#
# So this checks two things:
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
# Run: bash scripts/ci/harness-failure-strings-guard.sh
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "${root}"

sh_lib='scripts/dev/lib/installtest-enroll.sh'
sh_mac='scripts/dev/installtest-macos.sh'
ps_win='scripts/dev/installtest-windows.ps1'

# Read each declaration. Single-quoted, one line, one per file — matching the
# assignment rather than a comment marker keeps the value that the harness
# actually greps with as the single source of truth.
read_sh() {
  sed -n "s/^IT_INSTALL_FAILURE_RE='\([^']*\)'.*/\1/p" "$1" | head -1
}
read_ps() {
  sed -n "s/^\\\$InstallFailureRe = '\([^']*\)'.*/\1/p" "$1" | head -1
}

lib_re="$(read_sh "${sh_lib}")"
mac_re="$(read_sh "${sh_mac}")"
win_re="$(read_ps "${ps_win}")"

fail=0
for pair in "${sh_lib}|${lib_re}" "${sh_mac}|${mac_re}" "${ps_win}|${win_re}"; do
  f="${pair%%|*}"; v="${pair#*|}"
  if [ -z "${v}" ]; then
    echo "::error file=${f}::no install-failure alternation found (expected IT_INSTALL_FAILURE_RE='...' or \$InstallFailureRe = '...')" >&2
    fail=1
  fi
done
[ "${fail}" -eq 0 ] || exit 1

if [ "${lib_re}" != "${mac_re}" ] || [ "${lib_re}" != "${win_re}" ]; then
  {
    echo "::error::the three installtest harnesses disagree on the install-failure strings"
    echo "  ${sh_lib}: ${lib_re}"
    echo "  ${sh_mac}: ${mac_re}"
    echo "  ${ps_win}: ${win_re}"
    echo "They assert the same product behaviour on three OSes; a leg that greps"
    echo "for something the others do not is a hole, not a platform difference."
  } >&2
  exit 1
fi

# Every branch must still be printed by the product. Searched as a literal in
# the Go sources that own the first-run narration.
producers=(cmd/waired internal)
missing=()
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
    echo "::error::the harnesses grep for install-failure wording the product no longer prints"
    printf '  %s\n' "${missing[@]}"
    echo "Searched: ${producers[*]} (*.go)."
    echo
    echo "A grep that can never match is a leg that can never go red — which is"
    echo "exactly how #178 stayed green for five days. Update the alternation in"
    echo "all three harnesses to the wording the product prints now."
  } >&2
  exit 1
fi

echo "harness-failure-strings-guard: ok — ${#branches[@]} strings, agreed across 3 harnesses and present in the product"
