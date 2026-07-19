#!/usr/bin/env bash
# Compares the working tree's proto/ against the latest published
# proto/v* tag and fails on any non-additive change (removed / retyped /
# retagged exported API, non-omitempty additions to existing structs,
# changed const values). Rules and rationale: scripts/ci/protoguard and
# docs/decisions.md (20260719). Run from the repository root; CI entry
# point is .github/workflows/proto-guard.yml.
set -euo pipefail

latest=$(git tag -l 'proto/v*' --sort=-v:refname | head -1)
if [ -z "${latest}" ]; then
  # Shallow/tagless clones: try the remote once before giving up.
  git fetch --tags --quiet origin 2>/dev/null || true
  latest=$(git tag -l 'proto/v*' --sort=-v:refname | head -1)
fi
if [ -z "${latest}" ]; then
  echo "proto-additive-guard: no proto/v* tag found; nothing to compare against"
  exit 0
fi

old=$(mktemp -d)
trap 'rm -rf "${old}"' EXIT
git archive "${latest}" proto | tar -x -C "${old}"

echo "proto-additive-guard: comparing proto/ against ${latest}"
go run ./scripts/ci/protoguard "${old}/proto" proto
