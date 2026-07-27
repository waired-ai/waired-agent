#!/usr/bin/env bash
# base-ref-guard.sh — a pull request must target `main`.
#
# Why this exists: a PR whose base is a feature branch merges into that
# branch, not into main, and GitHub still shows the purple "Merged"
# badge. The work looks shipped and is not. Measured on this repo,
# 7 of 112 merged PRs had a non-main base, and one of them — #146,
# the host-fit producer — was actually lost: its squash commit sits on
# an orphaned branch that never reached main, which is why `vendor` /
# `unified_memory` / `usable_vram_mb` had a consumer in the control
# plane and no producer in the agent for weeks (#180). The private repo
# lost waired#911 the same way; that squash commit cannot even be
# fetched from a fresh clone any more.
#
# There is a second, quieter cost. testnet-pr.yml and proto-tag.yml both
# trigger on `branches: [main]`, so a stacked PR silently loses the
# cross-repo testnet gate and proto auto-tagging BEFORE it merges, not
# after. Nothing reports that today.
#
# Stacking is legitimate, so it is opt-out rather than prohibition. One
# line in the PR body:
#
#     stacked-on: <branch> (#N)
#
# names the branch this sits on and the PR that lands it. That is the
# whole ask: make the intent visible, so "merged" and "on main" stop
# being the same word for two different things.
#
# Environment (set by .github/workflows/base-ref-guard.yml):
#   BASE_REF  — the PR's target branch name
#   PR_BODY   — the pull request body, verbatim
set -euo pipefail

base_ref="${BASE_REF:-}"
if [ -z "${base_ref}" ]; then
  echo "::error::BASE_REF is not set (this script expects the PR context)" >&2
  exit 1
fi

if [ "${base_ref}" = "main" ]; then
  echo "base-ref-guard: base is main — OK"
  exit 0
fi

# Body opt-out. Case-insensitive, must name something after the colon.
# The workflow listens for `edited`, so adding the line re-runs this
# check without a push. Parser mirrors docs-surface-guard.sh's
# `docs-not-needed:`.
declared=$(printf '%s\n' "${PR_BODY:-}" \
  | tr -d '\r' \
  | grep -ioE '^[[:space:]]*stacked-on:[[:space:]]*.+' \
  | head -1 \
  | sed -E 's/^[[:space:]]*[Ss][Tt][Aa][Cc][Kk][Ee][Dd]-[Oo][Nn]:[[:space:]]*//' \
  || true)

if [ -n "${declared}" ]; then
  echo "base-ref-guard: base is '${base_ref}', declared stacked on — ${declared}"
  exit 0
fi

cat >&2 <<EOF
::error::This PR targets '${base_ref}', not 'main'.

A PR merged into a feature branch never reaches main, and GitHub shows
the same purple "Merged" badge either way. That is how #146 was lost:
its squash commit is on an orphaned branch, so the host-fit producer
never shipped while the control plane consumed the fields (#180). The
private repo lost waired#911 identically.

Before it even merges, a non-main base also silently drops:

  * the cross-repo testnet gate  (testnet-pr.yml: branches: [main])
  * proto auto-tagging           (proto-tag.yml:  branches: [main])

If this is deliberately stacked, retarget it to main once the parent
lands, or declare it now by adding one line to the PR body:

  stacked-on: ${base_ref} (#<parent PR number>)

Otherwise, change the base to main: the "Edit" button next to the PR
title, or

  gh pr edit <PR#> --base main
EOF
exit 1
