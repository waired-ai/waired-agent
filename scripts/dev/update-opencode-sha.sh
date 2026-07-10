#!/usr/bin/env bash
# Recompute the per-platform sha256 set in the `artifacts` map of
# internal/runtime/codeui/install.go for a given OpenCode release.
# sst/opencode publishes no per-asset checksum file, so the 6 digests are
# self-pinned and MUST be recomputed on every version bump. Invoked by
# Renovate's postUpgradeTasks (renovate.json) right after it bumps
# OpenCodePinnedVersion, so the version and its checksums land in the same
# PR commit (Renovate cannot derive these SHAs itself). Mirrors
# scripts/dev/update-uv-sha.sh.
#
# Usage: scripts/dev/update-opencode-sha.sh <opencode-version>   (e.g. 1.17.8)
# Run from the repo root.
set -euo pipefail

ver="${1:?usage: update-opencode-sha.sh <opencode-version>}"
base="https://github.com/sst/opencode/releases/download/v${ver}"
file="internal/runtime/codeui/install.go"

# The 6 release assets keyed in the artifacts map. Filenames are stable
# across OpenCode releases; isZip is irrelevant here (we hash the raw
# download, not the extracted binary).
assets=(
  "opencode-linux-x64.tar.gz"
  "opencode-linux-arm64.tar.gz"
  "opencode-darwin-x64.zip"
  "opencode-darwin-arm64.zip"
  "opencode-windows-x64.zip"
  "opencode-windows-arm64.zip"
)

if [ ! -f "$file" ]; then
  echo "update-opencode-sha: $file not found (run from repo root)" >&2
  exit 1
fi

for asset in "${assets[@]}"; do
  url="${base}/${asset}"
  sha="$(curl -fsSL "$url" | sha256sum | awk '{print $1}')"
  if [ "${#sha}" -ne 64 ]; then
    echo "update-opencode-sha: failed to compute a 64-hex sha256 for $url (got ${#sha} chars)" >&2
    exit 1
  fi

  # Escape regex metacharacters (the dots) in the asset filename.
  asset_re="$(printf '%s' "$asset" | sed -E 's/[.]/\\./g')"

  # Replace ONLY the 64-hex value that follows  "<asset>", "  on that line,
  # keeping the asset name + quotes + trailing fields intact.
  sed -i -E "s|(\"${asset_re}\", \")[0-9a-fA-F]{64}(\")|\1${sha}\2|" "$file"

  if ! grep -q "\"${asset}\", \"${sha}\"" "$file"; then
    echo "update-opencode-sha: sed did not update $asset in $file" >&2
    exit 1
  fi
  echo "update-opencode-sha: ${asset} sha256=${sha}"
done

echo "update-opencode-sha: updated all ${#assets[@]} opencode shas for v${ver}"
