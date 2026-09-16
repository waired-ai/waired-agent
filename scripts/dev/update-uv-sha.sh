#!/usr/bin/env bash
# Recompute the uv tarball pins in internal/runtime/uv.go for a given uv
# version: UVPinnedSHA256Linux64 (x86_64) and UVPinnedSHA256LinuxARM64
# (aarch64). Invoked by Renovate's postUpgradeTasks (renovate.json) right
# after it bumps UVPinnedVersion, so the version and its checksums land in
# the same PR commit. Renovate cannot derive a tarball SHA itself (#290).
#
# Each digest is computed from the downloaded tarball AND checked against
# the release's own .sha256 sidecar; a disagreement stops the script
# rather than pinning either value. The "Verified against" URL comment
# above each constant is moved to the new version too.
#
# Usage: scripts/dev/update-uv-sha.sh <uv-version>   (e.g. 0.12.15)
# Run from the repo root.
set -euo pipefail

ver="${1:?usage: update-uv-sha.sh <uv-version>}"
file="internal/runtime/uv.go"
base="https://github.com/astral-sh/uv/releases/download/${ver}"

if [ ! -f "$file" ]; then
  echo "update-uv-sha: $file not found (run from repo root)" >&2
  exit 1
fi

update() {
  local const="$1" triple="$2"
  local asset="${triple}.tar.gz"
  local sha sidecar
  sha="$(curl -fsSL "${base}/${asset}" | sha256sum | awk '{print $1}')"
  if [ "${#sha}" -ne 64 ]; then
    echo "update-uv-sha: failed to compute a 64-hex sha256 for ${base}/${asset} (got ${#sha} chars)" >&2
    exit 1
  fi
  sidecar="$(curl -fsSL "${base}/${asset}.sha256" | awk '{print $1}')"
  if [ "${sha}" != "${sidecar}" ]; then
    echo "update-uv-sha: ${asset} hashes to ${sha} but its .sha256 sidecar says ${sidecar}" >&2
    exit 1
  fi

  # Swap only the 64-hex value, keeping the const name + quotes intact.
  sed -i -E "s|(${const} = \")[0-9a-fA-F]{64}(\")|\1${sha}\2|" "$file"
  if ! grep -q "${const} = \"${sha}\"" "$file"; then
    echo "update-uv-sha: sed did not update ${const} in $file" >&2
    exit 1
  fi
  # The comment naming the sidecar the pin was checked against.
  sed -i -E "s|(releases/download/)[^/]+(/${asset}\.sha256)|\1${ver}\2|" "$file"
  echo "update-uv-sha: ${const}=${sha} for uv ${ver}"
}

update UVPinnedSHA256Linux64 uv-x86_64-unknown-linux-gnu
update UVPinnedSHA256LinuxARM64 uv-aarch64-unknown-linux-gnu
