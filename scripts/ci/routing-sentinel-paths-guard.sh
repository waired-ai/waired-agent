#!/usr/bin/env bash
# routing-sentinel-paths-guard.sh — keep routing-sentinel.yml's path filter honest.
#
# .github/workflows/routing-sentinel.yml runs the per-PR coding-agent routing
# sentinel (#496): install → enrolled daemon → gateway, then the Go harness
# (internal/e2e/integration, -tags integration) drives each leg and asserts a
# locally-served response via the observability event ring. It is PATHS-GATED so
# it only fires when its surface changes; that filter must cover the code the
# harness (and the routing path it exercises) actually compiles, or a change to
# an uncovered input would silently skip the gate (the rot deploy-dev / codeui /
# installtest guards all defend against for their workflows).
#
# Model: the codeui-multios guard (go-list-deps-derived). Every internal/<pkg>
# the routing harness transitively compiles must be EITHER covered by a `**`
# glob in the workflow OR listed in ALLOW below — a broad shared package with
# its own coverage and no routing-specific surface. A NEW harness dependency
# that is neither covered nor allowlisted fails the guard, forcing a conscious
# choice: widen the filter, or add it to ALLOW.
#
# Run from the repository root (CI does this in ci.yml's lint job).
set -euo pipefail

wf=".github/workflows/routing-sentinel.yml"
mod="$(go list -m)"

[[ -f "${wf}" ]] || { echo "::error::${wf} not found (run from repo root)" >&2; exit 1; }

missing=()

# 1) Fixed anchors the filter must always carry: the routing surface (the
#    integration adapters, the proxy/gateway the requests flow through, the
#    catalog + router + agentconfig that resolve/route the model, the codeui
#    OpenCode leg, the harness itself), the daemon/CLI that host the surfaces,
#    the installer surface the sentinel drives, the module files, and the
#    workflow's own self-reference.
for anchor in \
  'internal/integration/**' \
  'internal/proxy/**' \
  'internal/gateway/**' \
  'internal/agentconfig/**' \
  'internal/router/**' \
  'internal/catalog/**' \
  'internal/runtime/codeui/**' \
  'internal/e2e/integration/**' \
  'cmd/waired/**' \
  'cmd/waired-agent/**' \
  'go.mod' \
  'go.sum' \
  'scripts/dev/installtest-*.sh' \
  'scripts/dev/lib/installtest-*.sh' \
  '.github/workflows/routing-sentinel.yml'; do
  grep -qF "${anchor}" "${wf}" || missing+=("${anchor}")
done

# 2) Derived guard. Broad shared packages carry no routing-specific surface and
#    have their own tests, so they are watched via ALLOW rather than widening
#    this gate's trigger to fire on every unrelated change to them.
ALLOW=(
  internal/controlplane/signer   # CP signing; not a routing surface
  internal/download              # HTTP/progress download utils (linux dep of internal/runtime since #615); shared, own tests
  internal/hardware              # hardware profiling; shared, own tests
  internal/inferencemesh         # mesh routing; shared, own tests
  internal/management            # mgmt server (event-ring endpoint); own tests + smoke-control
  internal/management/observabilityclient
  internal/observability         # event-ring types; own tests
  internal/platform/elevation    # elevation predicate + per-OS elevation-hint wording (dep of internal/runtime since waired#752); no routing surface
  internal/platform/keychain
  internal/platform/localipc     # mgmt write socket / named pipe listener (waired#838); loopback IPC only, no routing surface
  internal/platform/paths
  internal/platform/secrets
  internal/platform/securestore
  internal/runtime               # engine supervision; shared, own tests (codeui watched via its **)
  internal/runtime/state
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
  go list -deps -tags integration ./internal/e2e/integration \
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
  echo "::error::new routing-harness dependency not covered by ${wf} paths nor allowlisted in this guard:" >&2
  printf '  - %s\n' "${uncovered[@]}" >&2
  echo "Add 'internal/<pkg>/**' under the workflow's paths (so the sentinel re-runs on its changes)," >&2
  echo "or add the package to ALLOW in scripts/ci/routing-sentinel-paths-guard.sh if it carries no routing surface." >&2
fi
if (( ${#missing[@]} || ${#uncovered[@]} )); then
  exit 1
fi

echo "OK: ${wf} paths cover all ${#deps[@]} routing-harness deps (anchors + allowlist)."
