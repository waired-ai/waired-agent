#!/usr/bin/env bash
# Recompute UVPinnedSHA256Linux64 in internal/runtime/uv.go for a given uv
# version. Invoked by Renovate's postUpgradeTasks (renovate.json) right
# after it bumps UVPinnedVersion, so the version and its checksum land in
# the same PR commit. Renovate cannot derive a tarball SHA itself (#290).
#
# Usage: scripts/dev/update-uv-sha.sh <uv-version>   (e.g. 0.11.9)
# Run from the repo root.
set -euo pipefail

ver="${1:?usage: update-uv-sha.sh <uv-version>}"
url="https://github.com/astral-sh/uv/releases/download/${ver}/uv-x86_64-unknown-linux-gnu.tar.gz"
file="internal/runtime/uv.go"

if [ ! -f "$file" ]; then
  echo "update-uv-sha: $file not found (run from repo root)" >&2
  exit 1
fi

sha="$(curl -fsSL "$url" | sha256sum | awk '{print $1}')"
if [ "${#sha}" -ne 64 ]; then
  echo "update-uv-sha: failed to compute a 64-hex sha256 for $url (got ${#sha} chars)" >&2
  exit 1
fi

# Swap only the 64-hex value, keeping the const name + quotes intact.
sed -i -E "s|(UVPinnedSHA256Linux64 = \")[0-9a-fA-F]{64}(\")|\1${sha}\2|" "$file"

if ! grep -q "UVPinnedSHA256Linux64 = \"${sha}\"" "$file"; then
  echo "update-uv-sha: sed did not update the constant in $file" >&2
  exit 1
fi
echo "update-uv-sha: UVPinnedSHA256Linux64=${sha} for uv ${ver}"
