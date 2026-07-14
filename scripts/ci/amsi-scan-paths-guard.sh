#!/usr/bin/env bash
# amsi-scan-paths-guard.sh — keep amsi-scan.yml's path filter honest.
#
# .github/workflows/amsi-scan.yml (Gate A of #553) runs the AMSI static scan of
# the installer scripts on a windows runner. It is PATHS-GATED to the AMSI
# surface — the two scripts that ship to users through the `iex` path plus the
# scanner + the workflow itself — so it stays off unrelated PRs. Because a
# paths-gated job produces no check run when the surface is untouched, it CANNOT
# be a required status check; this guard (run in ci.yml's always-on lint job)
# fails if the filter loses one of those anchors, which would silently drop the
# AMSI gate for a change to install.ps1 / ollama-windows.ps1 / the scanner —
# the exact class of regression (#552) the gate exists to catch.
#
# Same belt-and-braces pattern as installtest-paths-guard.sh /
# codeui-multios-paths-guard.sh. Run from the repository root.
set -euo pipefail

wf=".github/workflows/amsi-scan.yml"
[[ -f "${wf}" ]] || { echo "::error::${wf} not found (run from repo root)" >&2; exit 1; }

missing=()

# The AMSI surface the gate must always cover: the two scripts that reach the
# AMSI path in the user's hands (install.ps1 via `iwr | iex`, ollama-windows.ps1
# fetched + run), the scanner itself, and the workflow's own self-reference.
for anchor in \
  'packaging/install/install.ps1' \
  'scripts/install/ollama-windows.ps1' \
  'scripts/dev/amsi-scan.ps1' \
  '.github/workflows/amsi-scan.yml'; do
  grep -qF "${anchor}" "${wf}" || missing+=("${anchor}")
done

if (( ${#missing[@]} )); then
  echo "::error::${wf} paths is missing required AMSI-scan-surface anchors:" >&2
  printf '  - %s\n' "${missing[@]}" >&2
  echo "Add them under the workflow's on.*.paths so the AMSI gate re-runs when an installer script or the scanner changes." >&2
  exit 1
fi

echo "OK: ${wf} paths carry the AMSI-scan-surface anchors (install.ps1, ollama-windows.ps1, the scanner)."
