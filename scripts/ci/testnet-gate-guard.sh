#!/usr/bin/env bash
# testnet-gate-guard.sh — keep the testnet gate's classification honest.
#
# The testnet gate (scripts/ci/testnet-gate.sh) skips the cross-repo
# real-NAT testnet dispatch for PRs that touch no path in
# scripts/ci/testnet-relevant-paths.txt. That allowlist is a JUDGEMENT
# call, not derived from the build graph — so the risk is silent
# staleness: a new internal/ package lands, nobody decides whether it is
# mesh-relevant, and the gate silently skips testnet for it.
#
# This guard forces the decision: every package under internal/ must be
# covered by a prefix in EXACTLY ONE of
#   scripts/ci/testnet-relevant-paths.txt      (arms testnet), or
#   scripts/ci/testnet-nonrelevant-packages.txt (declared non-mesh, with reason)
# Unclassified or doubly-classified packages fail the lint job. It also pins
# a set of fixed anchors that must never drop out of the relevant list.
#
# Run from the repository root (CI does this in ci.yml's lint job).
set -euo pipefail

relevant="scripts/ci/testnet-relevant-paths.txt"
nonrelevant="scripts/ci/testnet-nonrelevant-packages.txt"
mod="$(go list -m)"

strip_list() {
  sed -e 's/[[:space:]]*#.*$//' -e 's/[[:space:]]*$//' "$1" | grep -v '^$'
}

mapfile -t rel < <(strip_list "${relevant}")
mapfile -t nonrel < <(strip_list "${nonrelevant}")

matches_any() { # $1=path, rest=prefixes
  local path="$1"; shift
  local p
  for p in "$@"; do
    [[ "${path}" == "${p}"* ]] && return 0
  done
  return 1
}

unclassified=()
dual=()
while IFS= read -r pkg; do
  dir="${pkg#"${mod}"/}/"
  in_rel=0
  in_non=0
  matches_any "${dir}" "${rel[@]}" && in_rel=1
  matches_any "${dir}" "${nonrel[@]}" && in_non=1
  if (( in_rel && in_non )); then
    dual+=("${dir}")
  elif (( !in_rel && !in_non )); then
    unclassified+=("${dir}")
  fi
done < <(go list ./internal/...)

failed=0

if (( ${#unclassified[@]} )); then
  echo "::error::internal/ packages not classified for the testnet gate:" >&2
  printf '  - %s\n' "${unclassified[@]}" >&2
  echo "Decide whether a regression there could change what testnet verifies" >&2
  echo "(v4 punch / v6 STUN / enrollment / relay<->direct fallback) and add a" >&2
  echo "prefix to ${relevant} (arms testnet) or ${nonrelevant} (skip, with a" >&2
  echo "one-line reason). See the membership rule in ${relevant}." >&2
  failed=1
fi

if (( ${#dual[@]} )); then
  echo "::error::internal/ packages matched by BOTH testnet gate lists (ambiguous):" >&2
  printf '  - %s\n' "${dual[@]}" >&2
  echo "Remove the overlapping prefix from ${relevant} or ${nonrelevant}." >&2
  failed=1
fi

# Fixed anchors the relevant list must always carry: the shared proto
# module (the wire contract the private CP/relay import), the deployed
# daemon's main package, the module/build inputs the testnet harness
# builds from, and the gate/dispatch machinery itself.
missing_anchors=()
for anchor in \
  'proto/' \
  'cmd/waired-agent/' \
  'go.mod' \
  'go.sum' \
  'Makefile' \
  'build/cloudbuild-agent-images.yaml' \
  '.github/workflows/testnet-pr.yml' \
  'scripts/ci/testnet-require-green-remote.sh'; do
  grep -qxF "${anchor}" <(printf '%s\n' "${rel[@]}") || missing_anchors+=("${anchor}")
done

if (( ${#missing_anchors[@]} )); then
  echo "::error::${relevant} is missing fixed anchors the testnet gate depends on:" >&2
  printf '  - %s\n' "${missing_anchors[@]}" >&2
  failed=1
fi

if (( failed )); then
  exit 1
fi

total="$(go list ./internal/... | wc -l | tr -d ' ')"
echo "OK: all ${total} internal packages classified for the testnet gate (+ fixed anchors present)."
