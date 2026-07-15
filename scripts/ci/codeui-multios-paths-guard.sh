#!/usr/bin/env bash
# codeui-multios-paths-guard.sh — keep codeui-multios.yml's path filter honest.
#
# .github/workflows/codeui-multios.yml runs the bundled-OpenCode "codeui"
# multi-OS coverage (L1 unit + L2 real `opencode serve` smoke, #501) on
# GitHub-hosted Windows/macOS runners, which cost real minutes. To keep that
# off every PR it is PATHS-GATED — it only runs when one of the entries in its
# `paths:` include-list changes. That list must cover the code the codeui
# tests actually compile, or a change to an uncovered input would silently skip
# the multi-OS gate (the exact rot deploy-dev-paths-guard.sh guards against for
# its workflow).
#
# The expensive matrix should NOT fire on every change to broad shared packages
# (e.g. all of internal/runtime, which also holds ollama/vllm). So instead of
# forcing `internal/<pkg>/**` for every transitive dep, this guard requires each
# dep to be EITHER covered by a path glob in the workflow OR explicitly listed in
# ALLOW below — an intentionally-not-watched package with its own coverage and no
# codeui-specific OS surface. A NEW codeui dependency that is neither covered nor
# allowlisted fails the guard, forcing a conscious choice: widen the filter, or
# add it to ALLOW.
#
# Run from the repository root (CI does this in ci.yml's lint job).
set -euo pipefail

wf=".github/workflows/codeui-multios.yml"
mod="$(go list -m)"

[[ -f "${wf}" ]] || { echo "::error::${wf} not found (run from repo root)" >&2; exit 1; }

missing=()

# 1) Fixed anchors the filter must always carry. These are the codeui surface
#    (package + the opencode pin live under internal/runtime/codeui), the
#    OS-split spawner the L2 smoke drives directly, the plugin package, the
#    module files, and the workflow's own self-reference.
for anchor in \
  'internal/runtime/codeui/**' \
  'internal/integration/opencode/**' \
  'internal/runtime/spawner_unix.go' \
  'internal/runtime/spawner_windows.go' \
  'go.mod' \
  'go.sum' \
  '.github/workflows/codeui-multios.yml'; do
  grep -qF "${anchor}" "${wf}" || missing+=("${anchor}")
done

# 2) Derived guard. Every internal/<...> package the codeui + opencode tests
#    transitively compile must be covered by a `**` glob in the workflow OR be
#    in ALLOW. Broad shared packages are intentionally watched only via specific
#    files (internal/runtime via the two spawner files) or not at all (they
#    carry no codeui-specific OS behaviour and have their own tests), so they
#    live in ALLOW rather than widening this expensive matrix's trigger.
ALLOW=(
  internal/runtime               # watched via spawner_{unix,windows}.go only
  internal/download              # HTTP/progress download utils (dep of internal/runtime since #615); OS-split ollama paths but no codeui surface
  internal/integration           # plugin parent; no OS-specific surface here
  internal/platform/elevation    # elevation predicate + per-OS elevation-hint wording (dep of internal/runtime since waired#752); OS-split but no codeui surface
  internal/platform/keychain
  internal/platform/paths        # per-user state-dir resolution; OS-split but no codeui surface
  internal/platform/secrets
  internal/platform/securestore
  internal/version
)

is_allowed() {
  local d="$1"
  for a in "${ALLOW[@]}"; do [[ "${d}" == "${a}" ]] && return 0; done
  return 1
}

# A dep dir is "covered" when the workflow filter contains `<dir>/**` or any
# ancestor `<prefix>/**`.
is_covered() {
  local d="$1"
  while [[ "${d}" == internal/* || "${d}" == internal ]]; do
    grep -qF "${d}/**" "${wf}" && return 0
    [[ "${d}" == */* ]] || break
    d="${d%/*}"
  done
  return 1
}

mapfile -t deps < <(
  go list -deps ./internal/runtime/codeui ./internal/integration/opencode \
    | sed -n "s|^${mod}/\(internal/.*\)|\1|p" \
    | sort -u
)

uncovered=()
for d in "${deps[@]}"; do
  is_covered "${d}" && continue
  is_allowed "${d}" && continue
  uncovered+=("${d}")
done

if (( ${#missing[@]} )); then
  echo "::error::${wf} paths is missing required anchors:" >&2
  printf '  - %s\n' "${missing[@]}" >&2
fi
if (( ${#uncovered[@]} )); then
  echo "::error::new codeui test dependency not covered by ${wf} paths nor allowlisted in this guard:" >&2
  printf '  - %s\n' "${uncovered[@]}" >&2
  echo "Add 'internal/<pkg>/**' under the workflow's paths (so the multi-OS gate re-runs on its changes)," >&2
  echo "or add the package to ALLOW in scripts/ci/codeui-multios-paths-guard.sh if it carries no codeui OS surface." >&2
fi
if (( ${#missing[@]} || ${#uncovered[@]} )); then
  exit 1
fi

echo "OK: ${wf} paths cover all ${#deps[@]} codeui/opencode test deps (anchors + allowlist)."
