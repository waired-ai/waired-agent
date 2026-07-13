#!/usr/bin/env bash
# testnet-gate.sh — decide whether a PR's changed files warrant dispatching
# the real-NAT testnet (.github/workflows/testnet-pr.yml `gate` job). Same
# design as the monorepo's gate (waired#738); the harness itself lives in
# the private monorepo and is reached via cross-repo dispatch.
#
# stdin:  changed file paths, one per line (the PR's full base..head diff).
# stdout: the matched (testnet-relevant) paths, if any.
# exit 0: at least one changed file matches scripts/ci/testnet-relevant-paths.txt
#         -> run testnet.
# exit 1: no relevant file -> skip testnet.
# exit 2: usage/config error (missing allowlist) — callers must treat this as
#         "run" (fail open toward testing).
#
# Matching is a plain string-prefix test against the allowlist entries — no
# globs, no regex — so the policy file stays trivially auditable. Membership
# policy and the classification guard are documented in the allowlist header.
set -euo pipefail

list="$(dirname "${BASH_SOURCE[0]}")/testnet-relevant-paths.txt"
if [[ ! -f "${list}" ]]; then
  echo "testnet-gate: allowlist not found: ${list}" >&2
  exit 2
fi

# Strip comments (full-line and trailing) and blanks.
mapfile -t prefixes < <(sed -e 's/[[:space:]]*#.*$//' -e 's/[[:space:]]*$//' "${list}" | grep -v '^$')
if (( ${#prefixes[@]} == 0 )); then
  echo "testnet-gate: allowlist is empty: ${list}" >&2
  exit 2
fi

matched=()
while IFS= read -r f; do
  [[ -z "${f}" ]] && continue
  for p in "${prefixes[@]}"; do
    if [[ "${f}" == "${p}"* ]]; then
      matched+=("${f}")
      break
    fi
  done
done

if (( ${#matched[@]} > 0 )); then
  printf '%s\n' "${matched[@]}"
  exit 0
fi
exit 1
