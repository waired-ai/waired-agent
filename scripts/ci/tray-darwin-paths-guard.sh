#!/usr/bin/env bash
# tray-darwin-paths-guard.sh — keep tray-darwin.yml's path filter honest.
#
# .github/workflows/tray-darwin.yml is the ONLY PR-time gate that compiles the
# tray subtree for macOS with cgo (make verify-cross skips it — see
# DARWIN_VET_PKGS). It is paths-gated, so a change to an input that is not in
# its filter would silently skip that gate and let a darwin-only break reach
# main, which is exactly the gap waired#901 I1 opened this workflow to close.
#
# The guard is deliberately narrower than codeui-multios-paths-guard.sh: this
# job builds the tray PACKAGES themselves, so what matters is that the two
# directories holding darwin-tagged tray code (plus the module files and the
# Makefile target it runs) are watched. Transitive deps are shared with the
# agent and are already vetted for darwin by `make verify-cross`; only the
# systray-dependent subtree needs the native leg.
#
# Run from the repository root (CI does this in ci.yml's lint job).
set -euo pipefail

wf=".github/workflows/tray-darwin.yml"

[[ -f "${wf}" ]] || { echo "::error::${wf} not found (run from repo root)" >&2; exit 1; }

missing=()
for anchor in \
  'internal/gui/tray/**' \
  'cmd/waired-tray/**' \
  'Makefile' \
  'go.mod' \
  'go.sum' \
  '.github/workflows/tray-darwin.yml'; do
  grep -qF "${anchor}" "${wf}" || missing+=("${anchor}")
done

# The workflow exists to cover what verify-cross cannot. If the Makefile ever
# stops excluding these packages from the darwin vet (i.e. they became
# cgo-free), this guard should be revisited rather than silently kept.
if ! grep -q 'DARWIN_VET_PKGS' Makefile; then
  echo "::error::Makefile no longer defines DARWIN_VET_PKGS — re-check whether ${wf} is still the tray's only darwin gate." >&2
  exit 1
fi
for excluded in 'cmd/waired-tray' 'internal/gui/tray'; do
  grep -q "DARWIN_VET_PKGS.*${excluded}" Makefile || {
    echo "::error::${excluded} is no longer excluded from DARWIN_VET_PKGS; ${wf}'s reason for existing changed — re-check the filter." >&2
    exit 1
  }
done

# make build-tray-darwin is the artifact gate the workflow runs; a rename would
# leave the job green while building nothing.
grep -q '^build-tray-darwin:' Makefile || {
  echo "::error::Makefile has no build-tray-darwin target, but ${wf} runs it." >&2
  exit 1
}

if (( ${#missing[@]} )); then
  echo "::error::${wf} paths is missing required anchors:" >&2
  printf '  - %s\n' "${missing[@]}" >&2
  echo "Add them under BOTH the pull_request and push path filters." >&2
  exit 1
fi

echo "OK: ${wf} watches the tray subtree and the Makefile target it gates."
