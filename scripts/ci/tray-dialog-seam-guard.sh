#!/usr/bin/env bash
# tray-dialog-seam-guard.sh — keep every tray dialog / host-integration call
# behind its test seam.
#
# On darwin, internal/gui/tray's ShowError / ShowConfirm / ShowAbout /
# ConfirmYesNo run `osascript display dialog`, which puts up a REAL modal and
# does not return until a human clicks it. The four *ViaElevation helpers raise
# a real administrator-password prompt. A unit test that reaches any of them
# does not fail on a headless macOS runner — it hangs until the job timeout,
# with no failing test named. That is #152, and it is why tray-darwin.yml could
# vet the tray subtree but not test it.
#
# The fix has two halves, and BOTH have to hold or the hang comes back:
#   1. tray.go declares a package-level seam per helper (`showError = ShowError`)
#      and every handler calls the lowercase one.
#   2. seams_test.go's TestMain replaces each seam with a recording no-op, so
#      the suite is hermetic by construction rather than per-test opt-in.
# Half 1 rots the moment someone adds a handler and types the exported name: it
# compiles, and it is green on Linux, where no dialog backend exists. Half 2
# rots when someone adds a seam and forgets the stub. This guard fails lint on
# either.
#
# The seam list is DERIVED from tray.go rather than restated here — a guard
# carrying its own copy of the list is a third thing to keep in sync.
#
# SCOPE — two kinds of file are exempt, for opposite reasons:
#
#   * The implementation layer (any file that DEFINES one of these helpers:
#     actions_*.go, dialog_*.go, browser.go). It sits below the seam, and its
#     internal calls — e.g. InstallOllamaViaElevation falling back to
#     OpenBrowser — are the behaviour, not a bypass of it. Derived, not listed.
#
#   * `_test.go` files tagged `//go:build linux` or `//go:build windows`.
#     These are the per-OS table tests ON the real helper that CLAUDE.md
#     §"Test discipline" requires ("a `var xFn = realFn` seam needs a table
#     test on realFn, or the real one is never called by any test"), and
#     neither leg can hang on them: the Linux helpers fall through to stderr
#     when no zenity/kdialog is on PATH, and the Windows one in scope
#     (CopyToClipboard) is non-modal and self-skips without a session. If a
#     windows-tagged test ever needs one of the MessageBoxW helpers, that
#     exemption has to be revisited rather than widened.
#
# Untagged tests and darwin-tagged tests are NOT exempt — that is exactly where
# the hang lives.
#
# Run from the repository root (CI does this in ci.yml's lint job).
set -euo pipefail

pkg="internal/gui/tray"
seamfile="${pkg}/tray.go"
stubfile="${pkg}/seams_test.go"

for f in "${seamfile}" "${stubfile}"; do
  [ -f "${f}" ] || { echo "::error::missing ${f} (run from repo root)" >&2; exit 1; }
done

# A seam is a package-level `<lowerName> = <ExportedName>` line in tray.go.
pairs="$(grep -oE '^[[:space:]]+[a-z][A-Za-z0-9]*[[:space:]]+= [A-Z][A-Za-z0-9]*$' "${seamfile}" \
  | awk '{print $1" "$3}')"

count="$(printf '%s\n' "${pairs}" | grep -c . || true)"
if [ "${count}" -lt 8 ]; then
  echo "::error::${seamfile} declares only ${count} dialog seams (expected the full set)." >&2
  echo "Either the seam block was gutted, or its shape changed and this guard stopped seeing it." >&2
  exit 1
fi

# Implementation layer: every file defining at least one of the seamed helpers.
impl="$(printf '%s\n' "${pairs}" | awk '{print $2}' | while read -r u; do
  grep -lE "^func ${u}\(" "${pkg}"/*.go 2>/dev/null || true
done | sort -u)"

# Files this guard actually polices.
subjects=()
for f in "${pkg}"/*.go; do
  printf '%s\n' "${impl}" | grep -qxF "${f}" && continue
  case "${f}" in
    *_test.go)
      # Exempt only the per-OS tests of the real helper (see SCOPE above).
      head -1 "${f}" | grep -qE '^//go:build (linux|windows)$' && continue
      ;;
  esac
  subjects+=("${f}")
done

if [ "${#subjects[@]}" -eq 0 ]; then
  echo "::error::no ${pkg} files left to check — the exemption rules swallowed the package." >&2
  exit 1
fi

fail=0

while read -r lower upper; do
  [ -n "${upper}" ] || continue

  # Invariant 1: the handler layer never calls the exported helper directly.
  hits="$(grep -nE "(^|[^[:alnum:]_.])${upper}\(" "${subjects[@]}" || true)"
  if [ -n "${hits}" ]; then
    echo "::error::${upper} is called directly; call the ${lower} seam instead (#152)." >&2
    printf '%s\n' "${hits}" | sed 's/^/  /' >&2
    fail=1
  fi

  # Invariant 2: TestMain stubs it. A seam with no stub is a seam that still
  # opens a real modal under `go test`.
  if ! grep -qE "^[[:space:]]*${lower} = " "${stubfile}"; then
    echo "::error::seam ${lower} (=${upper}) has no stub in ${stubfile}." >&2
    echo "  Add it to installSeamStubs, or a darwin unit test reaching it hangs to the job timeout (#152)." >&2
    fail=1
  fi
done <<EOF
${pairs}
EOF

[ "${fail}" -eq 0 ] || exit 1

echo "tray-dialog-seam-guard: ok — ${count} seams, all routed and all stubbed (${#subjects[@]} files checked)"
